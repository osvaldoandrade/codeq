# Persistence Engine

The persistence engine is the layer below every controller, every service, every claim and heartbeat. It is also the layer that decides whether a deployment survives a crash, how many writes per second the system can sustain, and how much disk a busy queue costs to operate. codeQ stores all of its durable state in a single log-structured merge tree (an LSM tree), embedded in-process, addressed through a thin `*pebblerepo.DB` wrapper at `internal/repository/pebble/db.go`. Reads are point lookups and prefix iterations against that tree; writes are batches submitted to a group commit coalescer that amortises the engine's internal commit lock across many concurrent submitters.

This page covers the engine in computer-science terms: what an LSM tree is, what it costs, how the write-ahead log relates to durability, why group commit matters, and how the fsync trade decides between throughput and crash tolerance. The on-disk key schema and per-domain repositories are described in [Tasks And Results](Concepts-Tasks-And-Results) and [IO Persistence Engine](IO-Persistence-Engine); this page is about the storage primitive itself.

## Why an LSM tree

A queue workload looks like this: a burst of writes for new tasks (one row per task plus pending-index entries), a burst of updates as workers claim and heartbeat (overwrite of the same key), a burst of deletes when tasks complete and the reaper sweeps history, and a steady-state read pattern that is mostly point lookups by task ID plus a few range scans over the pending priority buckets. Writes outnumber reads by a wide margin under sustained load — every task generates at minimum one create, one claim update, one complete update, and one delete from the pending queue index.

An LSM tree optimises for exactly this profile. Writes never modify a disk page in place; they append to an in-memory structure (the memtable) and to a sequential log on disk (the write-ahead log, or WAL). When the memtable fills, it is sealed and flushed to a new sorted file on disk (a sorted-string table, or SST) at level 0. Background compaction then merges level-0 files into level 1, level 1 into level 2, and so on down through L6, rewriting and pruning duplicates as it goes. Each level is roughly an order of magnitude larger than the one above it, so most of the data ends up in the deepest level and most of the writes touch only memtable plus WAL.

The cost is read amplification. A point lookup must check the memtable, every L0 file (they overlap), and one file per lower level (they do not overlap within a level). codeQ controls that cost with two mechanisms: a 256 MiB block cache (`pebble.NewCache(256 << 20)` in `internal/repository/pebble/db.go:147`) that holds hot SST blocks in memory, and a bloom filter per level (`bloom.FilterPolicy(10)` configured in `db.go:151-153`) that lets the engine skip files that definitely do not contain the key. Bloom filters are probabilistic — a positive answer requires a real lookup, a negative answer is certain — and at ten bits per key the false-positive rate sits near 1%. For the negative-lookup hot path (worker polling for tasks that do not exist yet, idempotency checks against the ghost index) the bloom is the difference between one in-memory bit test and a full filesystem fan-out.

The B-tree alternative — Redis with on-disk persistence, PostgreSQL, an embedded SQLite — pays in-place update costs on every write. For a workload dominated by short-lived writes those costs dominate. An LSM trades read-side complexity for write-side cheapness, and codeQ's reads are mostly bloom-filtered point lookups, which is the case the LSM read path is built for.

## The write-ahead log

Every batch the engine commits is first appended to a sequential WAL on disk. The WAL is the durability anchor: a crash mid-flight loses the contents of the memtable but not the WAL, and on restart the engine replays the WAL into a fresh memtable so committed writes survive. The memtable itself is volatile — it lives in process memory and is reconstructed from the WAL on startup.

WAL writes are sequential appends, which is the cheapest pattern an SSD or even a spinning disk can produce. There is no seek, no random read, no write amplification at the WAL layer. The expensive part is `fsync`, the system call that asks the kernel to flush the page cache to the underlying device. Without `fsync`, a crash can lose writes that were "committed" only as far as the page cache. With `fsync` on every commit, throughput drops to whatever the device can deliver in synchronous round trips — for a consumer SSD that is on the order of a few thousand commits per second.

codeQ defaults to `NoSync` mode at the engine layer. `db.Set`, `db.Delete`, and `db.CommitBatch` all call `pebble.NoSync` when there is no replicator attached (`db.go:287, 303, 395`). The trade is explicit: writes are durable to the OS but not to the device on crash. A clean process exit flushes; a kernel panic or power loss can lose the most recent few milliseconds of commits. For workloads where the cost of replaying a small window of work is lower than the cost of synchronous commits, this is the right default. For workloads that cannot tolerate any loss — financial settlement, audit logs, regulated event sourcing — the `FsyncOnCommit` option flips the engine to durable mode, at the throughput cost noted above.

The fsync trade is one of the few config knobs the engine exposes. Everything else about durability — replication, leader election, log compaction — happens at the layer above (see [Consensus And Replication](Concepts-Consensus-And-Replication)). The engine's job is to make local writes as fast and as durable as the operator asked it to be.

## Compaction

Background compaction is what keeps read amplification bounded as the database grows. The engine runs compaction in dedicated goroutines that pick a level, select a set of overlapping files, merge them by key, and write the result back at the next level. During the merge, duplicate keys are resolved (the newer value wins), tombstones (deletes) get propagated until they reach the bottom level and can be dropped, and the resulting SSTs are written in sorted order. Compaction is the LSM's housekeeping; without it, levels grow unboundedly, read amplification climbs, and the bloom filters stop helping because each level holds too many overlapping files.

The cost of compaction is write amplification: the same logical key may be rewritten several times as it migrates through levels. For a queue workload where most rows live briefly (claim, heartbeat once or twice, complete, delete), most rows are tombstoned before they reach the deepest level — they get filtered out during early compactions and never accumulate on disk. The reaper's deletion sweep at lease expiry and TTL boundary is what makes this work; if the reaper falls behind, dead rows survive longer in the LSM and compaction does more work. The reaper's design (one goroutine per Pebble shard, leader-gated under raft) is covered in [Architecture Overview](Concepts-Architecture-Overview) and [Multi-Tenancy](Concepts-Multi-Tenancy).

Compaction is throttled internally so it does not starve foreground writes. codeQ does not expose compaction tuning at the configuration layer — the engine's defaults are tuned for queue-shaped workloads, and the cost lever operators actually need is the number of independent Pebble shards (see [Deployment Modes](Concepts-Deployment-Modes)).

## The group commit coalescer

The single biggest performance discovery during the engine's productionisation was that the bottleneck under load was not disk, not CPU, not memtable size — it was the engine's internal `commitPipeline` mutex. Profile data captured at 26k requests per second showed that mutex pinned at 96% of the mutex profile and 44% of the block profile (recorded in the source comment at `db.go:71-82`). Every `Batch.Commit()` call acquires that lock to serialise WAL append and memtable insert; under N concurrent committers the per-commit cost grows with contention, not with the work the commit actually does.

The fix is a coalescer goroutine. Producers do not call `Commit()` directly. They populate a `*pebble.Batch`, hand it to `db.CommitBatch(b)`, and that helper enqueues the batch onto a buffered channel (`commitCh chan *commitReq`, sized at `commitChanBuf = 1024`). One goroutine, `commitLoop` (`db.go:351`), owns the receive side. It pops the first request, opens a fresh empty batch as the merge target, applies the first request's ops into it, then opportunistically drains additional requests already queued — up to `maxMergeBatch = 64` (`db.go:71-82`) — applies each one into the merge target, and issues a single `Commit()` for the whole pile. Errors fan back out to every joined submitter on their `done` channel.

The result: N concurrent commits cost one lock acquisition instead of N. At the 64-way merge upper bound the per-submitter mutex contention drops by 64×, which is what unlocks the 76,639 tasks-per-second number measured in `internal/bench/profile_full_cycle_test.go` for the single-node gRPC path.

There is a latency cost. A submitter that arrives one nanosecond after the coalescer kicked off a merge waits for the in-flight commit to finish before its own merge starts. With merge cycles on the order of tens of microseconds and merge sizes capped at 64, the tail-latency hit is small — and crucially, smaller than the lock-wait the submitter would have paid pre-coalescer under the same load. The bet is throughput; the loss is a few microseconds at p99. For queue workloads with thousands of concurrent producers and workers, the bet pays.

The cap of 64 is not arbitrary. Higher merge sizes increase the in-memory footprint of the merged batch (every submitter's ops live in the merge target until commit), and at very high cap values a single slow commit can hold up many submitters. 64 is the empirical sweet spot from the same benchmark that produced the 76k figure; the comment at `db.go:117-122` flags it as tuneable but warns that the gains plateau quickly.

## Replication interaction

When raft is enabled, `DB.AttachReplicator(r)` swaps the engine's write path. `Set`, `Delete`, and `CommitBatch` no longer call `commitLoop`; they call `r.Replicate(batch.Repr())` instead (`db.go:275-339`). The serialised batch goes through raft, gets applied on every replica's FSM, and each replica's FSM commits the batch to its local Pebble — with `NoSync`, because the durability anchor in raft mode is the replicated log on quorum, not the local fsync.

The coalescer is bypassed in raft mode because the raft layer does its own coalescing at the log-entry level (see [Consensus And Replication](Concepts-Consensus-And-Replication) for the `raftMergeBatch=128` knob). Layering one coalescer on top of another adds latency without adding throughput; the raft layer is the right place to merge because the cost it amortises (AppendEntries round-trip, quorum wait, FSM apply) is far higher than the Pebble commit-lock cost.

Reads are unaffected by replication. `Get`, `Has`, and `Iter` always go directly to the local Pebble, on every node, follower or leader. Follower reads may be stale by one applied-batch lag, but they are local and free. Strong-read semantics, where a read must reflect every committed write, are not provided — the queue model tolerates short-lived staleness on the read side because every action (claim, heartbeat, complete) goes back through the leader and is serialised there.

## Sharding the engine

A single Pebble instance has one commit pipeline and one set of compaction goroutines. Splitting the keyspace across multiple Pebble instances (intra-process sharding, configured by `numShards` in the persistence config) gives the operator N parallel commit pipelines, N parallel compactions, and N independent block caches. The wiring lives in `pkg/app/application_pebble.go:141-176`: the application opens `numShards` Pebble instances at `Path/shard0/`, `Path/shard1/`, ..., wraps each in its own task and result repository, and exposes them through a `ShardedTaskRepository` that hash-routes by task ID.

The throughput gain scales sub-linearly: at four shards on a single node the system runs at roughly 3× the single-shard rate, not 4×, because the producer and consumer ends still share a single Go runtime and a single network listener. Beyond four shards the contention shifts from the engine to the application's own goroutine scheduling, and adding more shards stops helping. The number is workload-dependent and the recommendation in [Deployment Modes](Concepts-Deployment-Modes) is to start at one shard and measure before adding more.

Sharding and raft compose. When both are enabled, each Pebble shard gets its own raft group with its own log, its own snapshot, its own leader election, and writes to different shards proceed in parallel through different raft logs. The mux transport at `internal/raft/mux_transport.go` lets all of those raft groups share a single TCP listener per node (see [Mux Transport](IO-Mux-Transport) for the wire shape), so the operational cost of adding shards stays flat at the network layer.

## Sequence numbers and recovery

The engine maintains an in-memory monotonic sequence counter (`DB.seq atomic.Uint64`) used to order entries within a priority bucket on the pending queue. On startup the engine scans the pending key range and resets the counter to the largest sequence number it finds (`recoverSeq` at `db.go:200-230`). This is bounded linear-time in the size of the pending queue, which is small in steady state because the reaper aggressively cleans completed tasks out of the pending index.

The recovery is the only startup work the engine does. There is no schema migration step, no consistency check across keys, no rebuild of secondary indexes — every secondary structure (pending priority index, due-time delayed index, lease table) is materialised in the keyspace and recovered by reading it. The only volatile state is the in-memory lease table (`DB.Leases`), which is rebuilt from the persisted `KeyInprog` rows during repository initialisation (see [Leases And Ownership](Concepts-Leases-And-Ownership)).

## Health and observability

The engine exposes a single liveness probe (`Health(ctx)`) that reads a non-existent reserved key (`codeq/__health__`) and returns nil on `ErrNotFound`. This exercises the read path without touching real data. Operators use it from the `/health` HTTP endpoint and from container orchestration liveness checks. Failure modes the probe catches: corrupted SST, locked directory (Pebble holds an exclusive lock on its data directory), out-of-disk during compaction.

Metrics for the engine are surfaced through the application's Prometheus exporter at `:9091/metrics` — see [Metrics](Observability-Metrics) for the full list. The high-signal ones are commit latency at p50 and p99, merge size histogram, WAL bytes written, and compaction bytes read/written. The merge size histogram is the first thing to look at if throughput stops scaling — if the histogram is bottom-heavy (most commits are 1-2 batches deep) the workload is below the contention threshold and adding more producers will help; if it sits at 64 (the cap) the engine is saturated and the next lever is sharding or moving to a larger box.

## What the engine does not do

There is no SQL. There is no secondary index defined declaratively; every index is materialised by the repository layer writing additional keys. There is no transaction across multiple batches; one batch is one commit. There is no MVCC visible to the application — a key holds one value, and concurrent writers to the same key resolve by last-writer-wins through the raft log (under replication) or through the coalescer's serialised commit order (without it). There is no streaming replication independent of raft; the only replication path is the raft log.

These are deliberate omissions. The cost of the missing features is paid at the application layer in code that materialises the indexes it needs and serialises through the leader for writes that must compose. The benefit is an engine that does one thing — durable, embedded, key-value with bloom-filtered reads and coalesced writes — and does it within a tight performance envelope that the rest of the system can plan around.

## See also

- [Tasks And Results](Concepts-Tasks-And-Results) for the on-disk key layout (`codeq/q/...`, `codeq/r/...`, lease prefix, ghost index)
- [Sharding](Concepts-Sharding) for the intra-process shard model
- [Consensus And Replication](Concepts-Consensus-And-Replication) for how `Replicate` interacts with the engine
- [Group Commit Coalescer](IO-Group-Commit-Coalescer) for the deep dive on `commitLoop`
- [Performance Single Node Throughput](Performance-Single-Node-Throughput) for the 76k tasks/s breakdown
