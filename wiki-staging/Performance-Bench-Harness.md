# Bench Harness

Every throughput number in the Performance section comes from a Go test in `internal/bench/` or `pkg/app/`. This page is the operator's reference for reproducing those numbers on your own hardware. It walks through the three primary bench files, explains what each one drives, gives the exact `go test` invocation, shows what the output looks like, and documents the WSL2 variance behavior that motivates the recommendation to run benches three to five times and take medians.

The point of running these yourself is not to reproduce the exact number — different hardware will give different numbers. The point is to confirm the ratios. Single-node Pebble should outperform three-node RAFT by roughly an order of magnitude. Four shards should beat one shard on a many-core host driven by a non-HTTP client. The Apply coalescer should leave a thirty-to-fifty percent gap when removed. If your run reproduces those ratios, the system is healthy on your hardware. If it does not, something is wrong and a profile will tell you what.

## The three primary benches

`internal/bench/profile_full_cycle_test.go::TestProfile_FullCycle` is the single-node ceiling bench. It boots one codeq Application with Pebble persistence, opens one producer gRPC stream session with thirty-two pumping goroutines, opens one worker gRPC stream session with one hundred twenty-eight slots, and runs the full create-claim-complete cycle for twenty seconds. After the window it writes six pprof profiles to `/tmp/codeq-profiles/`. The reported number is the cycles-per-second over the measurement window.

The invocation is `go test -v -run='^TestProfile_FullCycle$' -count=1 -timeout=180s ./internal/bench/`. The `-count=1` defeats Go's test result caching. The `-timeout=180s` is generous; the bench itself runs for twenty-two seconds (two-second warmup plus twenty-second window). The `-v` flag is required to see the throughput log lines.

The output of a healthy run looks roughly like:

```
=== RUN   TestProfile_FullCycle
    profile_full_cycle_test.go:158: PROFILE WINDOW (20s):
    profile_full_cycle_test.go:159:   created   = 1532780    (76639/s)
    profile_full_cycle_test.go:160:   completed = 1532780    (76639/s)
    profile_full_cycle_test.go:161: PROFILES written to /tmp/codeq-profiles/
    profile_full_cycle_test.go:162: Next: go tool pprof -top -cum /tmp/codeq-profiles/cpu.pb.gz
--- PASS: TestProfile_FullCycle (22.04s)
```

The `created` and `completed` counters should be very close to each other. A gap means the producer is outrunning the worker (the queue is growing during the window), which usually indicates the worker concurrency or the bench's batch settings need adjustment. The throughput number is the smaller of the two divided by the elapsed seconds.

Two environment variables modify the bench. `PHASE6_BATCH=N` enables the worker batched path with batch size N (`internal/bench/profile_full_cycle_test.go:75`). `PHASE6_PROD_BATCH=N` enables the producer batched path with batch size N (`profile_full_cycle_test.go:186`). Both default to zero (single-task path). Running the bench at `PHASE6_BATCH=32 PHASE6_PROD_BATCH=8` measures the full batched-on-both-sides configuration; the number is meaningfully higher than the unbatched default. The published seventy-six thousand cycles per second is the unbatched configuration; the batched configuration measures higher and the canonical "what does codeQ peak at" figure is the batched run.

`pkg/app/raft_grpc_bench_test.go::TestRaftBench_GRPC` is the three-node RAFT bench. It boots three codeq processes on loopback, wires them into a single RAFT cluster with the mux transport, and runs the full cycle through their gRPC stream listeners. It runs the cycle twice — once with one shard, once with four shards — and reports a multi-over-single ratio.

The invocation is `go test -v -run='^TestRaftBench_GRPC$' -count=1 -timeout=180s ./pkg/app`. Same flag conventions as the single-node bench. The bench's measurement window is eight seconds (`pkg/app/raft_grpc_bench_test.go:42`) rather than twenty, because three nodes on loopback take longer to set up and tear down and the bench needs to fit inside the timeout.

Output of a healthy run includes two subtests:

```
=== RUN   TestRaftBench_GRPC
=== RUN   TestRaftBench_GRPC/3-node_×_1-shard_(raft_+_gRPC)
    raft_grpc_bench_test.go:131: 1-shard:  create=9400/s  complete=9380/s
=== RUN   TestRaftBench_GRPC/3-node_×_4-shard_(raft_+_gRPC)
    raft_grpc_bench_test.go:135: 4-shard:  create=10250/s  complete=10240/s
    raft_grpc_bench_test.go:141: multi/single create ratio:   1.09x
    raft_grpc_bench_test.go:142: multi/single complete ratio: 1.09x
```

The numbers here will vary substantially. The bench has been observed peaking past twenty thousand cycles per second under optimal cache and CPU conditions; it has been observed dipping below six thousand under WSL2 contention. The healthy steady state is somewhere in the high single digits to low tens of thousands. The ratio between one-shard and four-shard is more stable than the absolute numbers; ratios near one indicate CPU saturation across the three loopback processes (which limits the multi-shard win), ratios above one and a half indicate a healthier run.

`pkg/app/raft_smart_routing_bench_test.go::TestRaftBench_SmartRouting` is the HTTP REST baseline against three-node RAFT. It uses the smart-routing HTTP client that follows the 307 redirect from a non-leader to the current leader. The invocation is `go test -v -run='^TestRaftBench_SmartRouting$' -count=1 -timeout=120s ./pkg/app`. The measurement window is five seconds.

Healthy output:

```
=== RUN   TestRaftBench_SmartRouting/3-node_×_1-shard_(raft_+_smart_routing)
    raft_smart_routing_bench_test.go:69: 1-shard cycles/s: 3949
=== RUN   TestRaftBench_SmartRouting/3-node_×_4-shard_(raft_+_smart_routing)
    raft_smart_routing_bench_test.go:73: 4-shard cycles/s: 3883
    raft_smart_routing_bench_test.go:80: multi-shard / single-shard ratio: 0.98x
```

The flat ratio is the diagnostic signature of an HTTP-client-bottlenecked bench. The server is not the wall; the `http.Transport.tryPutIdleConn` mutex on the client is. The bench is preserved because it documents the smart-routing redirect behavior, not because it is a useful throughput measurement at high loads.

## Reading the pprof output

The single-node bench writes six profile files to `/tmp/codeq-profiles/`:

```
/tmp/codeq-profiles/cpu.pb.gz       # CPU samples over the measurement window
/tmp/codeq-profiles/alloc.pb.gz     # Heap allocations over the window
/tmp/codeq-profiles/heap.pb.gz      # Heap residency snapshot at end of window
/tmp/codeq-profiles/block.pb.gz     # Goroutine blocking points
/tmp/codeq-profiles/mutex.pb.gz     # Mutex contention points
/tmp/codeq-profiles/goroutine.pb.gz # Goroutine stack snapshot
```

The two most important profiles for performance work are `cpu` and `mutex`. `go tool pprof -top -cum /tmp/codeq-profiles/cpu.pb.gz` shows where wall time goes — protobuf marshaling, sonic JSON decoding, Pebble batch construction, gRPC frame handling. A healthy CPU profile has no single function above roughly fifteen percent of total. `go tool pprof -top -cum /tmp/codeq-profiles/mutex.pb.gz` shows where lock contention lives. Post-coalescer, the Pebble `commitPipeline` mutex should sit at a few percent at most; if it is the top entry, something is wrong with the coalescer wiring.

The `alloc` and `heap` profiles are for memory work. The `block` profile shows where goroutines wait — at saturation most blocking is on the bench's own channels and the stream session's send buffers, which is expected. The `goroutine` snapshot is useful for catching goroutine leaks across multiple runs.

The RAFT bench does not write profiles. To profile a RAFT-mode run, edit the bench locally to add the same `pprof.StartCPUProfile` and `runtime.SetMutexProfileFraction` calls the single-node bench has. The relevant pattern is in `internal/bench/profile_full_cycle_test.go:95-148`.

## Why we run benches three times

A bench is a measurement, not a fact. Variance comes from many sources: GC scheduling, OS scheduler decisions, disk write timing, the state of the page cache, the temperature of the CPU. Running once gives a single sample; running three times gives a sense of the noise floor; running five gives a median that survives one outlier.

The WSL2 environment used to write this page has been observed to exhibit specific pathological behavior. The most striking instance is a fifty-minute clock jump occurring inside an eight-second measurement window — not an edit to the bench, not a debugger pause, real WSL2 clock behavior under specific virtualization conditions. Benches affected by such a jump report nonsense throughput (a window that "took" three thousand seconds completed in actual wall time of eight). The fix is not to trust single runs.

The discipline is to run any bench at least three times, discard obvious outliers, take the median, and report that. The Performance section's published numbers were collected this way.

A useful loop for the operator:

```bash
for i in 1 2 3 4 5; do
  go test -v -run='^TestProfile_FullCycle$' -count=1 -timeout=180s ./internal/bench/ \
    2>&1 | grep -E "created.*=|completed.*="
done
```

The output is five sets of created/completed numbers. Eyeball for obvious outliers, take the median of the rest. If the spread is wider than twenty percent, something on the host is interfering — another process, a thermal throttle, a background backup. Run on a quiet box.

## What to do when the numbers do not match

The most common diagnostic shape is "my throughput is lower than the published number." The first question is whether the ratio is preserved. If single-node gets fifty thousand on your box and three-node RAFT gets six thousand, the ratio is the same as the published one (roughly an order of magnitude) and your hardware is simply slower or more contested than the reference. That is not a bug.

If the ratio is inverted — three-node RAFT outperforming single-node, or four-shard worse than single-shard — something is wrong with the test setup. The most common cause is that the bench was run with a build that has bugs in the coalescer wiring (a `RUN_COALESCER=false` or similar test toggle accidentally set, for example). Recompile cleanly and re-run.

If the throughput numbers fluctuate by more than thirty percent across consecutive runs on the same hardware, the host is too noisy for benching. Move to a quieter host or quiesce the noise sources before re-running.

If the CPU profile shows a single function above forty percent of CPU, that function is the new bottleneck. Pre-coalescer benches showed Pebble's `commitPipeline.commit` dominating; post-coalescer benches should not show any single function dominating. If yours does, there is a regression somewhere, and the function name plus a quick read of the source usually pinpoints it.

If the mutex profile shows a contended lock above ten percent, that lock is the new bottleneck. The fix pattern is the one this section is built on: identify the lock, profile its callers, find a way to amortize the acquisition across more operations (group commit, batching, channel-hop-then-merge). The Pebble and RAFT coalescers are templates.

## Auxiliary benches

Beyond the three primary benches, several supporting bench files in `internal/bench/` and `internal/repository/pebble/` are useful for isolated measurements.

`internal/bench/sonic_bench_test.go` measures the JSON encode/decode path using `bytedance/sonic` versus `encoding/json`. The sonic path is roughly an order of magnitude faster on hot paths and is what the system uses for task payloads.

`internal/bench/producer_stream_vs_rest_test.go` and `internal/bench/worker_stream_vs_rest_bench_test.go` measure the stream-versus-REST gap in isolation, without the full cycle. These benches show the producer-only path peak at more than 130,000 creates per second on the stream and at three to four thousand on REST.

`internal/repository/pebble/bench_test.go::BenchmarkEnqueueParallel_Coalesced` versus `BenchmarkEnqueueParallel_Direct` measures the coalescer's contribution directly, in microbenchmark form. The Coalesced variant routes through the group-commit coalescer; the Direct variant bypasses it. The delta is the coalescer's value.

`internal/bench/gc_pressure_bench_test.go` is a deliberate stress test for GC behavior. It is not a throughput measurement; it is a way to confirm that long-running workloads do not develop pathological GC tail latency.

These auxiliary benches are useful for confirming specific hypotheses about a particular code path. None of them replace the three primary benches as the canonical performance measurements.

## Honest framing

The published numbers in the Performance section are reproducible. They were collected on twelve-core Linux hardware under WSL2 with the bench files in this repository. They are not marketing numbers, they are not synthetic, and they are not extrapolated. They are also not the only possible numbers — different hardware, different OS, different storage will give different absolute readings. The ratios (single-node versus RAFT, one-shard versus four-shard, stream versus REST) are what tells you the system is healthy. The absolute number tells you how much capacity your hardware has.

If you are deciding whether codeQ is fast enough for your workload, the right next step is to run `TestProfile_FullCycle` on your target hardware and read the throughput number. If you are deciding whether to enable RAFT, run `TestRaftBench_GRPC` on three real hosts (or three local processes, with the WSL2 caveat) and compare to your single-node number. The bench harness is the answer to "is this system fast enough for me?" — not the documentation, and not the marketing.

## Where to go next

For the architecture each bench exercises, see [Architecture Overview](Architecture-Overview), [Pebble Storage](Pebble-Storage), and [Architecture Raft](Architecture-Raft). For the throughput numbers the benches produce, see [Performance Single Node Throughput](Performance-Single-Node-Throughput) and [Performance Cost Of HA](Performance-Cost-Of-HA). For the knobs the benches let you measure, see [Performance Tuning Knobs](Performance-Tuning-Knobs).
