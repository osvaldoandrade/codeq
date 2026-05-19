# Performance Overview

codeQ's performance story is not a marketing slogan. It is a sequence of measurements taken on real hardware against named bench files, followed by a sequence of mutex-profile-driven patches to whichever lock dominated. Every number in this section traces to a Go test in the source tree. Every optimization traces to a profile entry that was, before the patch, a measurable fraction of mutex or block time. There is no "we made it fast." There is only "we identified the lock at 96% mutex profile and merged N acquisitions into one."

This section frames the rest of the Performance pages. It says what was measured, what was not, what the numbers mean, and what they do not mean. The detail pages — single-node throughput, the cost of HA, multi-shard scaling, tuning knobs, the bench harness — each pick up one slice of the story.

## What the numbers look like

Three measurements anchor the section. Each has a source file you can run yourself.

Single-node Pebble plus gRPC streaming sustains roughly seventy-six thousand full task cycles per second. The cycle is create-then-claim-then-complete, and the figure comes from `internal/bench/profile_full_cycle_test.go::TestProfile_FullCycle` on a twelve-core Linux box with thirty-two producer goroutines, one hundred twenty-eight worker slots, and a single Pebble shard at `NoSync`. The exact reading on the box used to write this page was 76,639 tasks/s in a twenty-second measurement window. That is the single-node ceiling we publish.

Turning on three-node RAFT and running the same cycle against the gRPC stream path drops throughput to roughly nine to ten thousand cycles per second steady-state. The bench is `pkg/app/raft_grpc_bench_test.go::TestRaftBench_GRPC`, run with three codeq processes on the same WSL2 host bound to loopback. Variance is high: peaks past twenty thousand cycles per second appear when the WSL2 scheduler cooperates and Pebble's block cache is warm. The number to cite is the steady-state floor, not the peak. The Apply coalescer in `internal/raft/db.go:149` (constant `raftMergeBatch = 128`) is responsible for thirty to fifty percent of that floor; without it the same bench measures correspondingly lower.

The HTTP REST baseline against the same three-node RAFT cluster lands at about 3,949 cycles per second on a single shard and 3,883 cycles per second on four shards (`pkg/app/raft_smart_routing_bench_test.go::TestRaftBench_SmartRouting`). The four-shard number does not scale because the bottleneck is not the storage engine: it is the Go `http.Transport` idle-connection mutex on the client. Multi-shard scaling reveals itself only when the client-side bottleneck is removed, which is why the gRPC stream bench supersedes the REST bench for any throughput claim past four thousand cycles per second.

## How the numbers were earned

Each measurement above exists because a previous version of the system was slower, and a profile said why. The pattern repeats.

Phase 0 profiling at twenty-six thousand requests per second pinned ninety-six percent of mutex time on Pebble's internal `commitPipeline` mutex. Every batch commit acquired the same global lock. The fix was the group-commit coalescer at `internal/repository/pebble/db.go:71-82`: concurrent writers hand their batches to a single coalescer goroutine, which merges up to sixty-four of them (`maxMergeBatch = 64`, `db.go:122`) into one Pebble commit. N mutex acquisitions become one. The benchmark `internal/repository/pebble/bench_test.go::BenchmarkEnqueueParallel_Coalesced` versus `BenchmarkEnqueueParallel_Direct` measures the delta directly.

The HTTP REST bench showed the next bottleneck. At about four thousand cycles per second the mutex profile on the client moved to `http.Transport.tryPutIdleConn` at 28.74% of total contention. Every cycle was a fresh HTTP request, every request churned through the connection pool, and the pool's idle-list lock was the contended resource. The fix was not a code change to the lock — it was a topology change. The bidirectional gRPC streams at `pkg/app/producer_stream.go` and `pkg/app/worker_stream.go` open one persistent HTTP/2 connection per session and multiplex frames over it with no per-request idle-pool round-trip. Switching the bench to gRPC streams moved the same workload from about four thousand cycles per second to about seventy-six thousand on the single-node path.

The third bottleneck appeared once RAFT replication was wired in. Every write paid a fixed cost: one `AppendEntries` round-trip to the majority quorum plus one FSM apply on every replica. At three nodes on loopback that is two network round-trips and three Pebble commits per logical operation. The Apply coalescer at `internal/raft/db.go:125-149` is the dual of the Pebble coalescer: concurrent `Replicate` callers queue on `applyCh` and the apply loop merges up to one hundred twenty-eight of them into a single submission to the underlying raft library. Without it, every concurrent writer pays its own round-trip. With it, the three-node loopback bench measures thirty to fifty percent higher throughput than baseline.

## What this section covers

Five detail pages follow. Each picks up one specific question.

`Performance-Single-Node-Throughput` walks through what `TestProfile_FullCycle` actually measures: producer fan-out, worker drainage, the cycle composition, and the difference between the gRPC stream path and the HTTP REST path. It explains what changes when `fsyncOnCommit` flips to true and what changes when the bench is run with batched producers and batched workers. It is honest about what is not measured: cold-start latency, network round-trip on real NICs, and the long-tail GC pauses that twenty-second windows do not catch.

`Performance-Cost-Of-HA` covers the order-of-magnitude drop from single-node Pebble to three-node RAFT. It names the costs — `AppendEntries` round-trip, three FSM applies per commit, loopback CPU contention between three codeq processes on the same host — and quantifies how much of the gap the Apply coalescer recovers. It states the open question explicitly: what would three real hosts with separate NICs and disks look like? Loopback distorts in both directions (lower latency than a real link, higher CPU contention than a dedicated host), and the WSL2 environment used for the bench distorts further. The honest answer is unmeasured.

`Performance-Multi-Shard-Scaling` covers the move from one to N shards inside a single process. Each shard is an independent Pebble directory with its own `commitPipeline`, its own coalescer, and its own reaper. The FNV-1a hash routes by task ID at `internal/repository/pebble/sharded_task_repository.go`. The page explains why four shards is the empirical sweet spot, why eight shards typically does not help on a twelve-core box, and the trade: cross-shard admin queries (DLQ scans, tenant lists) now walk O(N) shards instead of one. It also names the failure mode of multi-sharding low-concurrency workloads: each shard's coalescer needs concurrent submitters to amortize its mutex, so two shards each handling one writer is strictly worse than one shard handling two writers.

`Performance-Tuning-Knobs` is the field guide. Every knob that moves performance: `numShards`, `fsyncOnCommit`, the worker client's `Concurrency` and `BatchSize`, the RAFT heartbeat and election timers, and the compile-time constants `maxMergeBatch` and `raftMergeBatch`. For each knob: what it changes mechanically, what it costs, what a reasonable starting value is, and where in `pkg/config/config.go` it lives.

`Performance-Bench-Harness` is the operator's reference for reproducing the numbers above. It explains the test layout in `internal/bench/` and `pkg/app/`, how to run each bench, what the output means, and how to read pprof profiles. It is also where the WSL2 variance disclaimer lives in full: observed clock jumps of fifty minutes inside an eight-second measurement window are not edits to the bench — they are real WSL2 behavior, and the recommendation is to run benches three or five times and take the median.

## What this section deliberately does not claim

There are several plausible-sounding numbers we do not publish, because we have not measured them.

We do not publish a three-host RAFT number on a real network. The closest measurement is loopback on one WSL2 host, which is both faster (no NIC) and slower (three processes contending for the same CPU and disk) than the deployment most operators want to model. Until somebody runs `TestRaftBench_GRPC` against three machines with separate disks and a real network link, the order-of-magnitude statement stands as the only published figure.

We do not publish a p99 latency number under sustained load. The bench harness tracks throughput counters; it does not record per-request latency distributions. The Pebble commit path's tail latency is bounded (a submitter that arrives just after the coalescer committed pays one merge cycle of wait, which at `maxMergeBatch=64` and a 26k req/s commit rate is on the order of a few milliseconds) but bounded is not measured.

We do not publish a cold-start number. The benches all warm up for two seconds before starting the measurement window. Cold-start matters for autoscaling and for crash recovery; it is not part of the throughput story.

We do not publish a memory-under-load number. Heap allocation profiles exist (`/tmp/codeq-profiles/alloc.pb.gz` after `TestProfile_FullCycle`) but the published bench output is throughput, not memory residency.

The reader who wants any of these numbers can run the bench themselves and add them to the picture. The bench files are committed and reproducible. What this section commits to is: the numbers we publish are real, the numbers we do not publish we name, and the optimizations all have profile evidence behind them.

## Mental model

The shortest mental model is three concentric circles. The innermost circle is single-node Pebble — one process, one disk, one commit pipeline per shard, group-commit coalescer amortizing the lock. That ceiling is the LSM tree and the CPU, somewhere around seventy thousand full cycles per second on a twelve-core host. The middle circle is multi-shard — same process, N parallel commit pipelines, throughput grows sub-linearly with shard count up to roughly the number of available cores divided by some factor (empirically four on twelve-core). The outermost circle is multi-node RAFT — every write pays a quorum round-trip and an FSM apply on each replica. Throughput drops an order of magnitude on loopback; the Apply coalescer recovers a third to a half of that. Each circle is a different deployment mode with different trade-offs, documented in `docs/41-deployment-modes.md`.

Performance is not a single number. It is a profile-shaped trade among latency, durability, and availability, and the right point depends on the workload. The detail pages give you the data to pick.

## What "no buzzwords" means in practice

The Performance section is written under a strict voice rule: no marketing language, no superlatives without measurement, no claim that does not trace to a file path. The reason is not stylistic preference. It is that performance claims without provenance are noise, and noise compounds. When somebody asks "is codeQ fast?" the only honest answer is a number with a citation, on hardware described, in an environment named. Anything shorter than that is a sales pitch.

This section accordingly avoids superlatives and marketing adjectives. Where the section calls codeQ fast, it does so by reporting a measurement and pointing at the bench file. Where it calls the system scalable, it does so by reporting the ratio between one-shard and four-shard configurations and pointing at the bench file. Where it calls the architecture replicated, it does so by reporting the cost of replication and pointing at the bench file. The voice rule is the discipline that makes that possible.

The voice rule also extends to the trade-offs. Every optimization in the system has a cost, and the pages name that cost explicitly. The group-commit coalescer trades small tail-latency increases for a large throughput win; the page says so. The Apply coalescer trades the same tail-latency increase for a thirty-to-fifty percent RAFT throughput recovery; the page says so. The `fsyncOnCommit=false` default trades a one-millisecond durability window against a kernel panic for a meaningful throughput improvement; the page says so. The reader who picks codeQ for their workload should know exactly what they are picking, and the section is structured so that nothing is hidden behind a positive-sounding adjective.

## Where to go next

For the single-node ceiling and the producer-worker stream mechanics, see [Performance Single Node Throughput](Performance-Single-Node-Throughput). For the cost of replication, see [Performance Cost Of HA](Performance-Cost-Of-HA). For the scaling shape inside one process, see [Performance Multi Shard Scaling](Performance-Multi-Shard-Scaling). For the operator's field guide to every knob that moves performance, see [Performance Tuning Knobs](Performance-Tuning-Knobs). For reproducing every number on your own hardware, see [Performance Bench Harness](Performance-Bench-Harness).
