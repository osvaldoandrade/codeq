# Observability: Profiling

Profiling is the pillar of internal introspection. Where metrics tell you that something is slow and tracing tells you which step is slow, profiling tells you *why* that step is slow at the level of Go functions, goroutines, and synchronization primitives. It is the only one of the four pillars that reaches inside the runtime — into the call stack, the mutex contention, the heap allocation, the goroutine state — and reports back what it found. For a broker like CodeQ that pushes tens of thousands of tasks per second through a single process, profiling is not optional; it is what turns "the broker is at 60% CPU and I cannot get more throughput" into a specific lock to redesign.

This page covers the `pprof` endpoints CodeQ exposes, the bench harnesses in `pkg/app/raft_profile_bench_test.go` and `pkg/app/raft_grpc_profile_test.go` that capture profiles under controlled load, the mechanics of `runtime.SetMutexProfileFraction` and `runtime.SetBlockProfileRate`, the `go tool pprof` workflow for reading the resulting profiles, and two real performance investigations whose findings shipped as code changes in this codebase.

## The pprof endpoints

Go's `net/http/pprof` package registers a family of HTTP handlers under `/debug/pprof/*` on the default mux when imported for its side effects. CodeQ does the import in `cmd/server/main.go:8`:

```go
_ "net/http/pprof" // Registers /debug/pprof/* on http.DefaultServeMux when CODEQ_PPROF=1.
```

The import alone is not enough; the broker also has to bind the default mux to a listener. CodeQ does that on a *separate* listener from the API server, gated by the `CODEQ_PPROF=1` environment variable, in `cmd/server/main.go:69-79`:

```go
if getenv("CODEQ_PPROF", "") == "1" {
    runtime.SetMutexProfileFraction(1)
    runtime.SetBlockProfileRate(1)
    pprofAddr := getenv("CODEQ_PPROF_ADDR", ":6060")
    go func() {
        pprofSrv := &http.Server{Addr: pprofAddr, ReadHeaderTimeout: 5 * time.Second}
        if err := pprofSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
            fmt.Fprintln(os.Stderr, "[WARN] pprof server:", err)
        }
    }()
}
```

The choice to put pprof on its own port matters. Mounting `/debug/pprof/*` on the API mux would expose the profiles on the same port that handles task traffic, which is both a security concern (profiles can leak heap contents and stack traces) and a contention concern (a long pprof read shares Gin middleware with real requests). Separating onto `:6060` lets you firewall the port to a jump host or service mesh, and lets `go tool pprof` open a long-lived connection without burning a Gin worker. The two `runtime.Set*` calls inside the same gate are deliberate: mutex and block profiling are off by default because they cost a small amount of CPU on every lock acquire and channel send. Flipping the gate enables both at full sampling (`1`) so that profiles captured immediately after start show contention without further setup.

The endpoints under `/debug/pprof/` are the standard set:

- `GET /debug/pprof/profile?seconds=30` — 30-second CPU sample.
- `GET /debug/pprof/heap` — current heap snapshot, also doubles as the allocation profile.
- `GET /debug/pprof/mutex` — contended mutex stacks (only useful with `SetMutexProfileFraction > 0`).
- `GET /debug/pprof/block` — goroutine block events on channels and primitives.
- `GET /debug/pprof/goroutine` — full goroutine dump (great for deadlock diagnosis).

You can stream any of these directly into `go tool pprof`:

```bash
go tool pprof -http=:0 'http://codeq-broker:6060/debug/pprof/profile?seconds=30'
```

The `-http=:0` flag launches a local web server that renders flame graphs, source views, and the top-cum tables in a browser. For terminal-only environments, the equivalent table view is `go tool pprof -top -cum <profile>`.

## The bench harnesses

For controlled, repeatable profile captures, CodeQ ships two profile-bench tests under `pkg/app/`. They are not regular unit tests — they take tens of seconds to run and skip under `-short` — but they are the canonical way to capture profiles from a running cluster without needing a live deployment to attach to.

`pkg/app/raft_profile_bench_test.go` starts a three-node, four-shard Raft cluster on loopback, warms it up so every shard has an elected leader, then opens a 15-second measurement window during which 32 concurrent goroutines push the full create-claim-complete cycle. While the window is open, the harness has already toggled mutex and block profiling at full sampling:

```go
runtime.SetBlockProfileRate(1)
defer runtime.SetBlockProfileRate(0)
prevMutex := runtime.SetMutexProfileFraction(1)
defer runtime.SetMutexProfileFraction(prevMutex)
```

`SetMutexProfileFraction(1)` means "sample every contention event"; the documented default of zero means no mutex events are recorded at all. The bench harness then calls `pprof.StartCPUProfile` against a file in `/tmp/codeq-raft-profiles/cpu.pb.gz`, runs the load loop, calls `pprof.StopCPUProfile`, and snapshots the other profiles (`mutex`, `block`, `allocs`, `goroutine`) by looking each up with `pprof.Lookup(name).WriteTo(file, 0)`. The deferred restore of the previous mutex fraction is important: leaving full-sampling enabled across a test binary slows subsequent tests, and the careful save/restore keeps the harness composable.

The companion test, `pkg/app/raft_grpc_profile_test.go`, runs the same pattern but specifically against the gRPC streaming paths rather than the HTTP REST paths. The output lands in `/tmp/codeq-raft-grpc-profiles/`. Together the two tests give you symmetric capture for both code paths.

To run a profile capture locally:

```bash
go test -v -run='^TestRaftProfile_4Shard$' -count=1 -timeout=180s ./pkg/app
go tool pprof -top -cum /tmp/codeq-raft-profiles/cpu.pb.gz
go tool pprof -top -cum /tmp/codeq-raft-profiles/mutex.pb.gz
go tool pprof -top -cum /tmp/codeq-raft-profiles/block.pb.gz
go tool pprof -alloc_space -top -cum /tmp/codeq-raft-profiles/allocs.pb.gz
```

The `-cum` flag is the one most operators do not know they want. By default pprof's `-top` view ranks functions by their *flat* sample count — time spent directly in that function. `-cum` ranks by *cumulative* count — time spent in that function and everything it called. For finding bottlenecks, cumulative is usually what you want, because the deepest leaf in a stack is rarely the function you can change; what you can change is the caller that decided to spend so much time in that subtree. `-flat` is useful when you suspect a single hot function (a parser, a hash, a marshal) is the offender; `-cum` is useful when you are working top-down through a call graph.

## Mutex profiles and why they matter

The mutex profile records, for each call stack that ever blocked waiting on a `sync.Mutex` or `sync.RWMutex`, the cumulative time spent blocked. Two real CodeQ optimizations came directly out of reading this profile, and they are worth walking through both for the engineering content and for the methodology.

### Finding 1: 96% time in `pebble.commitPipeline`

In the early days of the Pebble persistence path, the profile-bench test above showed mutex contention dominated by a single call stack with `(*pebble.commitPipeline).Commit` at the top of the cumulative view, accounting for roughly 96% of all contended-mutex time. The interpretation: every goroutine that called `Commit` on a Pebble batch was serializing through a single mutex deep inside Pebble's commit pipeline, and at high concurrency the queue in front of that mutex was the dominant cost. The throughput symptom was that adding more producer goroutines past about eight did not increase tasks-per-second — they all queued at the same lock.

The fix was structural rather than algorithmic. CodeQ introduced a group-commit coalescer in front of Pebble: a single goroutine that owns the `Commit` call, with a small unbounded channel that callers send their batches into. The coalescer waits a microsecond or two, collects however many batches arrive in that window (up to `maxMergeBatch = 64`), merges them into a single `pebble.Batch`, and issues a single `Commit`. Each caller's goroutine blocks on a per-batch completion signal until the merged commit finishes. The result is that the contended mutex is hit once per *group* of commits instead of once per commit, and group sizes of 30-60 are typical under load. A re-run of the same bench showed the mutex contention had collapsed and end-to-end throughput on the full producer-worker cycle climbed to the cited 76,639 tasks/s figure (`internal/bench/profile_full_cycle_test.go::TestProfile_FullCycle`). That is the number quoted throughout the architecture docs as the single-node ceiling.

The methodological lesson: a 96% mutex profile is not a number you negotiate with. You either redesign so the lock is held less often, or you live with the throughput it permits. Trying to make the lock faster is rarely the right answer; finding a way to call it less often almost always is.

### Finding 2: 28.74% time in `http.Transport.tryPutIdleConn`

The second investigation came from the HTTP REST bench (`internal/bench/http_bench_test.go`) where the broker was acting as a client to itself over the Raft replication path. The mutex profile showed `net/http.(*Transport).tryPutIdleConn` at 28.74% of contended time. That stack is the part of Go's HTTP client that returns a finished connection to the idle pool; the pool itself is protected by a single mutex, and at very high request rates every completing request fights for that mutex when it returns its connection.

The fix here was again structural: the Raft Apply path was coalesced. Instead of issuing one HTTP request per task to replicate the entry, the path batches multiple Apply calls into one replication request when they arrive close together. That is conceptually the same pattern as the Pebble coalescer — a single owner goroutine, a small wait window, batched issuance — but applied at the HTTP transport boundary rather than the storage boundary. After the coalescer landed, the `tryPutIdleConn` line dropped off the top-cum view entirely.

The methodological lesson: mutex profiles surface contention regardless of which library it lives in. The Go standard library's HTTP transport is no more sacred than your own code; if it shows up at 28% of contended time, the right answer is to call it less often, by batching at a higher layer.

## Reading a profile

`go tool pprof -top -cum file.pb.gz` produces a table that looks roughly like this for a mutex profile:

```
      flat  flat%   sum%        cum   cum%
   23.45ms 41.2% 41.2%   42.10ms 74.0%  github.com/cockroachdb/pebble.(*commitPipeline).Commit
    8.12ms 14.3% 55.5%   10.30ms 18.1%  sync.(*Mutex).Lock
    ...
```

`flat` is samples in this function itself; `cum` is samples in this function plus everything it called. The percent columns are normalized to the total samples in the profile. The first row tells you that `Commit` spent 41% of its samples doing its own work and 74% if you include callees — a strong signal that the work this function does is itself the bottleneck, not just its leaf children. By contrast, a row with high `cum%` but low `flat%` is a function that mostly waits on something deeper.

For CPU profiles the same shape applies but the unit is CPU time. For heap profiles `-alloc_space -cum` ranks call stacks by total bytes ever allocated; `-inuse_space -cum` ranks by bytes currently live. The former finds allocation-rate hotspots; the latter finds memory-retention hotspots.

A heuristic: if your top-cum is at 70% or higher, you have a single dominant call path and that is the thing to fix. If your top-cum is at 20% and there is no clear hierarchy, you have either a well-balanced system or a profile that is too short to be representative. The 15-second window in the bench harness is usually enough; for production captures, 30 seconds is the typical floor.

## Goroutine and block profiles

The goroutine profile is the one you want when the broker has hung. `GET /debug/pprof/goroutine?debug=2` returns a text dump of every goroutine with its full stack and current state — running, blocked on channel send, blocked on mutex, waiting on I/O, etc. A few thousand goroutines blocked on the same channel is a smoking gun for backpressure. The block profile is the streaming version: it records, for every goroutine that *did* block on a channel or mutex during the sampling window, where it blocked and for how long. Block profiles are useful for finding lock-step backpressure (worker A waits on B waits on C) that does not show up in a single instantaneous goroutine dump.

A common workflow: scrape `/debug/pprof/goroutine?debug=2` periodically into a file. When the broker behaves strangely, diff the most recent dump against the previous one. Stacks that are present in both at the same line are stuck; stacks that have advanced are healthy.

## When *not* to enable profiling

Mutex and block profiling at full sampling cost a few percent of CPU. CPU profiles cost more — the sampler is interrupt-driven and during a profile run you should expect a measurable but small slowdown on the profiled process. The cost is not large enough that it forces you to disable profiling in production; the cost *is* large enough that you should not leave a CPU profile running indefinitely. The pattern is: capture a 30-second window when you need it, then go away. Heap profiles are cheap by comparison (they sample at allocation time, not continuously) and can run all the time.

Production deployments that are concerned about exposure can leave `CODEQ_PPROF=0`, keep the endpoint disabled, and use the bench harnesses on an off-the-rotation node when an investigation requires it. That is the conservative posture. The aggressive posture — `CODEQ_PPROF=1` everywhere, firewalled to the SRE network — gives you the profile when you need it without the lead time of a redeploy, and that lead time matters at 3 a.m.

## Where profiling fits in the workflow

Profiling is rarely the first pillar you reach for. The path is usually metrics → trace → profile. A Prometheus alert names a metric (see [Observability Metrics](Observability-Metrics)). A slow span in a Jaeger trace (see [Observability Tracing](Observability-Tracing)) names a function. A 30-second CPU or mutex profile from the broker named in the trace tells you what that function is doing. Skipping straight to a profile when the metric and trace would have told you what to look for wastes wall-clock time, because a profile of a healthy broker is just a lot of `runtime.netpoll`. The investigations that produce real code changes are the ones where the profile was opened to answer a specific question — "why is this commit slow," "why does this connection pool contend" — already raised by the upstream pillars.

The opposite is also true: a metric or a trace without a profile to explain it is often unactionable. You see latency rise, you see the slow span, but the span has no internal structure beyond what the OpenTelemetry instrumentation chose to emit. The profile is what tells you *how* to make the span faster. Together with the structured logs documented in [Observability Logging](Observability-Logging), the four pillars compose into the investigative loop that turns observations into fixes.
