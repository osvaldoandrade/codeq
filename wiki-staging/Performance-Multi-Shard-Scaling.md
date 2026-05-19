# Multi-Shard Scaling

codeQ scales inside a single process by partitioning storage across N Pebble shards. Each shard is an independent Pebble directory with its own LSM tree, its own commit pipeline, its own group-commit coalescer, and its own lease reaper. Writes that hash to different shards run in parallel; writes that hash to the same shard serialize the same way they would in a single-shard deployment. The shape of the scaling curve is therefore set by two things: the distribution of work across shards (which is what hashing decides) and the per-shard ceiling (which is what the single-node throughput page measures). This page covers both, names the empirical sweet spot, and gives the rule for when multi-sharding does not help.

## How a task lands on a shard

The router is FNV-1a over the task ID. The code lives in `internal/repository/pebble/sharded_task_repository.go` and is described in `docs/08b-pebble-sharding-internals.md` and in the chapter doc at `docs/chapter/02-concepts-and-architecture.md:77`. The hash is computed once per operation and the result modulo `numShards` selects which underlying `*pebble.DB` handles the write. Reads use the same hash, so a task always lands on the same shard for its entire lifecycle.

The choice of FNV-1a is deliberate. It is a non-cryptographic hash with good avalanche behavior over short inputs (task IDs are UUIDs, sixteen bytes after decoding), it is branch-free and inlines tightly, and at the request rates we are interested in (tens of thousands per second) the routing decision must not be visible in the profile. FNV-1a achieves all three.

The distribution is statistically uniform but not exactly uniform. Over a measurement window of millions of tasks, the load per shard converges to one over N within sub-percent. Over a window of a few thousand, occasional imbalances of five or ten percent are normal. The bench windows used in this section run long enough for the distribution to converge.

Cross-shard operations exist but they are administrative. The DLQ scan in `internal/repository/pebble/sharded_task_repository.go` walks every shard in sequence to assemble a unified listing — O(N) shard touches for one query. The same is true for tenant-wide list endpoints and for the `/v1/codeq/stats` aggregate view. None of these are on the hot path; they are operator-facing endpoints whose latency is set by the slowest shard plus the cost of merging N result sets. The tradeoff is that the hot path (per-task enqueue, claim, complete) is independent across shards, and the cold path (admin queries) pays the O(N) walk.

## What changes when you add shards

Each shard has its own `commitPipeline` mutex inside Pebble. Single-shard deployments serialize all commits through that one mutex; the group-commit coalescer at `internal/repository/pebble/db.go:71-82` amortizes the lock by merging up to sixty-four concurrent batches into one acquisition, but at sufficiently high concurrency even the coalesced acquisitions become the wall. Adding a second shard halves the work going through any one `commitPipeline`. Adding a fourth shard quarters it. The cap is set by CPU and disk.

Each shard also has its own reaper goroutine. The reaper scans the `KeyInprog` index for expired leases and re-enqueues them. Single-shard, one reaper handles the entire workload; multi-shard, each reaper handles its own slice. A long compaction or a slow reaper scan on one shard does not stall any other shard. This matters for tail latency more than for throughput, but it is a property of the architecture that the multi-shard configuration buys.

Each shard maintains its own block cache. The `Cache: pebbledb.NewCache(256 << 20)` allocation at `internal/repository/pebble/db.go:147` is per shard. Four shards therefore allocate four 256 MiB caches, one per shard. The reader concerned about memory should adjust either the cache size or the shard count accordingly — the default sizing assumes a single-digit shard count on a host with at least a few gigabytes of available RAM.

Each shard has its own compaction worker. Compactions on different shards run in parallel, contending only for the disk. On a disk with sufficient IOPS this is a win — more total bytes get compacted per unit time. On a disk that is already saturated by compaction, adding shards moves the bottleneck nowhere because the device queue is the wall.

## The empirical sweet spot

On a twelve-core Linux box at NoSync, four shards is the empirical sweet spot for full-cycle throughput. The reason is the joint product of CPU and the per-shard commit ceiling.

A single shard saturates roughly four to six cores at peak load — the coalescer goroutine, the bench's request-handling goroutines, the Pebble compaction worker, the lease reaper, and the gRPC stream handler all share work. Two shards spread that across eight to ten cores, with the second shard's goroutines filling otherwise-idle scheduler slots. Four shards saturate roughly the full twelve cores; the throughput improvement from one to four shards on this hardware is approximately two times for the full create-claim-complete cycle, with the exact ratio varying by measurement window. Going to eight shards on twelve cores does not improve throughput because the cores are already saturated and the additional shards' coalescer and reaper goroutines mostly contend for CPU rather than doing useful additional work.

The relationship is not "more shards is always better." Beyond the core count, additional shards spend CPU on coalescer overhead with no commit-pipeline win, because the lock contention has already been reduced to where it is no longer the bottleneck. The recommendation in `docs/41-deployment-modes.md` of `numShards: 4` is calibrated for a twelve-core host. For a four-core host, two shards is closer to the sweet spot. For a thirty-two-core host, eight or sixteen shards is plausible — but the bench has not been run on a thirty-two-core host so the page does not publish a number for that scale.

The HTTP smart-routing bench (`pkg/app/raft_smart_routing_bench_test.go::TestRaftBench_SmartRouting`) measures the multi-shard ratio at 3,949 cycles per second on one shard versus 3,883 on four shards — flat. This is not evidence that multi-sharding does not help. It is evidence that the bench was bottlenecked on the client's `http.Transport` idle-pool mutex, which is independent of shard count. The same workload over the gRPC stream path (`pkg/app/raft_grpc_bench_test.go::TestRaftBench_GRPC`) shows a clear improvement from one to four shards even under RAFT replication, because the gRPC stream removes the client-side bottleneck.

The lesson is that multi-shard scaling is visible only when the upstream client path can drive enough concurrent load to fill multiple coalescers. The gRPC stream path can. The HTTP REST path cannot, on the bench hardware, at the concurrency the bench uses.

## When multi-sharding does not help

The most common multi-sharding failure mode is splitting a low-concurrency workload across many shards. The coalescer's amortization works by merging concurrent batches. If each shard receives a steady trickle of one or two writers, the coalescer rarely has more than one batch to merge, and the per-batch overhead (one channel hop into the coalescer goroutine, plus the merge logic) becomes pure cost with no benefit.

The arithmetic is simple. Imagine a workload of four concurrent writers and one shard. The coalescer sees four concurrent batches per cycle, merges them, and pays one mutex acquisition for four operations. Now split into four shards. Each shard sees one writer at a time. Each coalescer pays one mutex acquisition per operation, plus the channel hop into the coalescer. Total mutex acquisitions: four instead of one. The single-shard deployment with the coalescer fully fed is strictly faster.

The threshold is roughly "are there enough concurrent writers to keep each shard's coalescer fed?" An empirical rule of thumb is to provision shards no faster than concurrent submitters divided by four. If the system sees thirty-two concurrent producer goroutines, up to eight shards keeps each coalescer busy. If the system sees four concurrent producer goroutines, stick with one shard.

The other failure mode is over-sharding a memory-constrained host. Each shard's 256 MiB block cache plus the LSM working set adds up. On a host with two gigabytes of RAM, eight shards is operationally fragile: GC pressure rises, the OS page cache shrinks, and tail latencies degrade. The recommended path is to either reduce the per-shard cache size by editing the open path or to keep shard count low.

## What the bench numbers say

The cleanest multi-shard measurement comes from the gRPC bench under RAFT, which removes both the HTTP client bottleneck and the single-node CPU saturation. `pkg/app/raft_grpc_bench_test.go` measures one-shard and four-shard configurations side by side. The one-shard reading sits near nine thousand cycles per second steady-state on loopback; the four-shard reading sits near ten thousand. The ratio is small because three nodes on loopback are CPU-bound — the per-shard improvement is partially absorbed by the cross-node CPU contention. On a real three-host cluster the four-shard win would be larger, because each host has its own twelve cores to give and the contention drops away.

Single-node multi-shard without RAFT shows the ratio more cleanly. The `TestProfile_FullCycle` bench at `numShards=4` (set via the `PHASE8_SHARDS=4` env var that the profile fixture reads) measures higher than at `numShards=1` on the same hardware. The exact ratio depends on the producer-worker fan-out, but the single-node four-shard configuration is the production recommendation for hosts that can spare the cores and memory.

A useful way to read the bench output is as two numbers: throughput and the multi-over-single ratio. The throughput is what the system delivered on the test box. The ratio is what the system would deliver if you added shards to a similar host. The ratio is more portable than the absolute number, because it abstracts over the box's CPU and disk. A ratio of two times from one to four shards is a healthy ratio; a ratio of one and a half is the regime where CPU has started to dominate; a ratio of one or below means something else (probably the client) is the bottleneck.

## Practical guidance

For a single-node deployment on a twelve-core host with NoSync writes, use four shards. The single-shard configuration leaves CPU on the table; the eight-shard configuration adds overhead with no commit-pipeline win.

For a single-node deployment on a four-core host, use two shards. The same reasoning applies, scaled.

For a three-node RAFT deployment, four shards per node is the most common configuration. Each node's commit pipelines are sharded the same way; the RAFT layer replicates across nodes orthogonally to the shard split. The cross-node CPU contention on loopback bench environments mutes the gain, but on a real three-host cluster the four-shard advantage shows up.

For workloads with low concurrency — handfuls of writers, infrequent submissions — one shard is correct. The coalescer is a no-op at this load anyway; multi-sharding just spreads no concurrency across more goroutines and costs more memory.

For workloads with high read traffic against admin endpoints, be aware that DLQ scans and tenant lists walk every shard. A scan against an eight-shard deployment is eight times the I/O of a scan against a one-shard deployment. The admin endpoints are not hot paths in any documented workload, but the trade is real if you start polling them aggressively.

The configuration knob is `numShards` inside the `PersistenceConfig` JSON for the Pebble provider (see `pkg/persistence/pebble` and the example config at `deploy/config/codeq.example.yml`). The compile-time constant for the within-shard coalescer cap (`maxMergeBatch = 64` at `internal/repository/pebble/db.go:122`) is fixed; the within-shard coalescer queue depth (`commitChanBuf = 1024`) is also fixed. Operators tune at the shard-count level; the coalescer's internal knobs are set by code and have not been observed to be the bottleneck on any measured hardware.

## Where to go next

For the single-node throughput baseline this scaling story sits on top of, see [Performance Single Node Throughput](Performance-Single-Node-Throughput). For how multi-shard interacts with RAFT replication, see [Performance Cost Of HA](Performance-Cost-Of-HA). For every knob mentioned on this page in catalog form, see [Performance Tuning Knobs](Performance-Tuning-Knobs). For the internal architecture of the sharded repository layer, see [Pebble Sharding](Pebble-Sharding).
