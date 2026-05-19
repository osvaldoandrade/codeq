# Consensus And Replication

Replication answers a question single-node deployments never have to ask: when this machine dies, where does the queue live next. The answer in codeQ is raft consensus over a Pebble-backed state machine. Every write that lands in the storage engine on the leader node also lands in the storage engine on every follower node, applied in the same order, through the same code path. A node can crash; the cluster keeps serving. A network partition can split followers from a leader; the majority side keeps serving and the minority side stops accepting writes until it rejoins. The cost is one extra round trip on every write, and a handful of milliseconds of failover latency when a leader actually dies.

This page covers the conceptual model: what raft guarantees, how AppendEntries replicates the log, what fault tolerance the f=1 / N=3 configuration buys, how the finite state machine turns a committed log entry into a Pebble batch, and why an Apply coalescer at the raft layer mirrors the group commit coalescer at the engine layer. The wire-level mechanics — log store layout, snapshot streaming, mux transport framing — live in [IO Raft Replication](IO-Raft-Replication) and [Mux Transport](IO-Mux-Transport). The failover walkthrough is in [Cluster Level Failover](Concepts-Cluster-Level-Failover).

## What raft is

Raft is a consensus algorithm. Given a cluster of N nodes, raft produces a single ordered log of operations that every non-failed node agrees on, even when some nodes are slow, restarting, or partitioned away. The agreement is durable: an operation that raft reports as "committed" survives the loss of any minority of the cluster. The agreement is also linearisable on the leader's view: operations appear in the order the leader received them, and reads on the leader reflect every committed write.

The mechanism is small. One node is the leader; the others are followers. The leader accepts client writes, appends them to its local log, and immediately ships each new entry to every follower via the AppendEntries RPC. A follower that receives an AppendEntries appends the entry to its own log and replies with an ack. When the leader has received acks from a majority (including itself), the entry is committed. The leader then notifies the followers of the new commit index on the next AppendEntries (commits piggyback on the next heartbeat or write). Each node, leader and follower alike, applies committed entries to its local state machine in log order.

Two safety invariants make the algorithm work. First, only the leader appends to the log — followers reject entries that conflict with their own log and force the leader to back up and resend. Second, a node will only vote for a candidate whose log is at least as up-to-date as its own. The second invariant means a leader, once elected, contains every entry that was committed under any previous term. The combination is enough to guarantee that the committed log is identical on every non-failed node.

codeQ uses `github.com/hashicorp/raft` for the algorithm itself. The wrapping in `internal/raft/db.go` adapts hashicorp/raft's `FSM`, `LogStore`, `StableStore`, and `SnapshotStore` interfaces to codeQ's Pebble store, so the entire raft state — log entries, voted-for, current term, snapshots, and the replicated keyspace — lives in one embedded LSM tree. There is no second daemon, no external etcd, no ZooKeeper.

## State machine replication via the FSM

The state machine is what raft applies its log entries to. In codeQ that state machine is a thin shim at `internal/raft/fsm.go:43-62`:

```go
func (f *fsm) Apply(log *hraft.Log) any {
    if log == nil || log.Type != hraft.LogCommand || len(log.Data) == 0 {
        return nil
    }
    repr := make([]byte, len(log.Data))
    copy(repr, log.Data)

    batch := f.pebble.NewBatch()
    defer batch.Close()
    if err := batch.SetRepr(repr); err != nil {
        return fmt.Errorf("fsm apply: SetRepr: %w", err)
    }
    if err := batch.Commit(pebbledb.NoSync); err != nil {
        return fmt.Errorf("fsm apply: commit: %w", err)
    }
    return nil
}
```

Every committed raft log entry carries the serialised representation of a Pebble batch (`batch.Repr()`) as its payload. The FSM reconstructs the batch with `SetRepr`, commits it to the local Pebble with `NoSync`, and returns. Every replica runs the same Apply on the same input — raft guarantees identical log content in identical order — so every replica's Pebble converges to the same state.

The choice of payload format matters. A Pebble batch repr is the engine's native serialisation: a sequence of (op, key, value) records prefixed with a header. It is compact, validated by Pebble itself on `SetRepr`, and trivially mergeable — two batch reprs concatenated through `Batch.Apply` produce a third batch with all ops in order. That mergeability is what makes the Apply coalescer below possible.

The commit inside Apply uses `NoSync` deliberately. Durability in raft mode is the responsibility of the raft log, not the FSM's local fsync — once raft commits an entry, that entry survives any minority failure, and replaying the log on restart re-applies it to the FSM. Calling fsync on every FSM apply would add disk-flush latency to every replicated write without buying any additional safety. The engine-level `FsyncOnCommit` flag is also ignored under raft for the same reason; the raft log is where durability lives.

## AppendEntries and the commit index

AppendEntries is the workhorse RPC of the algorithm. The leader calls it on every follower, with payload that includes any new log entries since the follower's last position, the leader's current commit index, and metadata that lets the follower detect a log mismatch and force a rewind. In codeQ the RPC travels over `hashicorp/raft`'s `NetworkTransport`, which serialises it over a TCP connection to the follower's bind address. The default election timeout is one second and the default heartbeat interval is one second (`internal/raft/db.go:53-90`); the heartbeat is just an AppendEntries with no new entries.

The commit index is the high-water mark of agreement. When the leader's local log contains an entry, that entry is replicated but not yet committed; when a majority of followers (counting the leader itself) have acked it, the leader advances its commit index and the entry is durable across the cluster. Followers learn about the new commit index on the next AppendEntries and apply through their FSM up to that point. A follower lags the leader by at most one AE round-trip plus one FSM apply — typically a few milliseconds on a healthy network.

The size of the AE payload is one of the things the coalescer below tunes. Small AEs (one entry, one batch) waste network round-trips; oversized AEs blow past raft's preferred entry size and cause hashicorp/raft to throttle them. The sweet spot for codeQ's queue workload is in the hundreds of small entries per AE, which is exactly what `raftMergeBatch=128` produces.

## Majority quorum and fault tolerance

The safety guarantee scales with cluster size. Define f as the number of node failures the cluster can tolerate while still making progress; then the cluster needs N = 2f+1 nodes. With three nodes, f=1: one node can be down and the remaining two form a majority. With five nodes, f=2: two can be down. With one node, f=0: any failure stops the cluster, which is the non-replicated single-node case.

codeQ's default replicated topology is three nodes, f=1. This is the smallest configuration that gives meaningful fault tolerance. A two-node configuration is explicitly worse than a single node — the majority of two is two, so a single failure halts the cluster, but now there are two boxes to maintain. The configuration validation in `pkg/config/config.go:662-683` does not reject N=2 at parse time (it cannot tell intent from configuration shape), but [Deployment Modes](Concepts-Deployment-Modes) calls this out as a non-recommendation: pick one or three, never two.

The trade above three is operational. Five nodes tolerate two failures but cost more, take longer to commit (the majority is three acks instead of two), and have larger AE fanout. Seven and up are rare outside identity-style workloads where the read load is high enough to benefit from more local replicas. For a queue workload three is almost always the right number; the bottleneck is throughput, not read scale, and adding nodes does not help throughput once writes are already serialised through the leader.

## Leader election

Every node starts as a follower. If a follower receives no AppendEntries within the election timeout (default 1000ms in codeQ, with raft adding randomised jitter to spread retries), it transitions to candidate, increments its current term, votes for itself, and asks every other node for a vote. A node grants a vote if the candidate's log is at least as up-to-date as its own and it has not already voted in this term. A candidate that wins a majority becomes leader; a candidate that splits the vote times out and retries at a higher term. The randomised election timeout is what breaks the split-vote tie — two candidates timing out at slightly different points means the next election almost certainly produces one winner.

The leader lease (`LeaderLeaseMS=500`, `db.go:67-72`) is a tighter bound on how long a leader trusts its own status. A leader that cannot communicate with a majority within the lease window steps down voluntarily, even before its election timeout would force an election. The lease prevents the split-brain edge where a partitioned leader keeps acking writes for a few hundred milliseconds after a new leader has been elected on the majority side.

In a steady-state cluster elections are rare. The most common cause is a leader process restart (e.g., during a rolling deploy); the next most common is a network partition; far behind those, the leader's disk gets slow and the follower side declares it dead. All three look the same to the algorithm: missed heartbeats trigger an election, the majority elects a new leader, the cluster keeps running. The full failover sequence — including how clients learn about the new leader — is walked through in [Cluster Level Failover](Concepts-Cluster-Level-Failover).

## Snapshots and log compaction

The raft log grows without bound if nothing prunes it. Snapshots are the pruning mechanism. Every `SnapshotEntries` entries (default 8192, `db.go:84-90`) raft asks the FSM for a snapshot of its current state. The FSM at `internal/raft/fsm.go:68-71` answers with a point-in-time view of the Pebble store (`pebble.NewSnapshot()`); raft then streams that view to a `SnapshotSink` and truncates the log up to the snapshotted point. Restarting nodes that come up far behind the leader's log first install the snapshot, then catch up by replaying only the post-snapshot entries.

The snapshot format is defined in `internal/raft/snapshot.go` and covered in [IO Raft Replication](IO-Raft-Replication). For the conceptual purpose: a snapshot is the entire codeq/ keyspace at a known log position, serialised to a single stream, restorable by wiping the keyspace and replaying the stream. Snapshots run concurrently with Apply because the Pebble snapshot is point-in-time at creation, not at persist; Apply keeps advancing the FSM while the snapshot streams out.

The snapshot interval is fixed at 120 seconds in `db.go:81-83`. The entry threshold is the soft limit; the interval is the hard limit. Either firing triggers a snapshot. The cost is a brief burst of disk read on the snapshot side and some extra network bandwidth on the install-snapshot side when a far-behind follower is catching up.

## The Apply coalescer

The leader's `Replicate` path looks deceptively simple: take a serialised batch, call `raft.Apply` with it, wait for the future to resolve, return the result. The trouble is that `raft.Apply` is expensive — it appends to the local log, ships an AppendEntries to every follower, waits for the majority ack, advances the commit index, dispatches to the FSM on every node, and returns. On a three-node cluster with healthy network the round trip is somewhere between a millisecond and a few milliseconds. At a thousand concurrent submitters each calling `raft.Apply` independently, the cluster spends most of its time in AE fan-out and FSM dispatch, and the achieved throughput collapses to a few thousand operations per second.

The fix is the same shape as the engine-layer group commit. `Replicate` does not call `raft.Apply` directly; it enqueues an `applyReq{repr, done}` onto a buffered channel (`applyCh chan *applyReq`, sized at `raftApplyChanBuf=1024`). A single goroutine, `applyLoop` at `db.go:634-709`, owns the dispatch. It pops the first request, opens a fresh Pebble batch as the merge target, sets the first request's repr into it, then opportunistically drains more requests already queued — up to `raftMergeBatch=128`. For each additional request it applies the batch onto the merge target. When the merge fills or the channel drains, it serialises the merged repr and calls `raft.Apply` once for the entire pile. Errors fan back out to every joined submitter through their `done` channels.

The savings are large. N submitters cost one log append, one AE round-trip, one FSM Apply, and one Pebble commit — instead of N of each. At 128-way merging the per-submitter overhead drops by roughly 128×, and the measured cluster throughput climbs from a few thousand cycles per second to roughly ten thousand on the gRPC path (`pkg/app/raft_grpc_bench_test.go` shows 9-10k cycles/s on a three-node loopback cluster). The HTTP path with smart routing measures ~3.9k cycles/s (`pkg/app/raft_smart_routing_bench_test.go`); the gap between HTTP and gRPC is mostly HTTP framing, not the coalescer.

The cap of 128 (versus 64 at the engine layer) reflects the different cost amortised. The engine-level merge collapses one mutex acquisition; the raft-level merge collapses one full quorum round-trip plus FSM apply. The raft round-trip costs more, so a larger merge size pays off — but only up to a point. Beyond 256-ish the merged batch exceeds raft's preferred entry size and hashicorp/raft starts paying retransmit penalties; throughput drops back to the pre-coalescer level. 128 is the empirical sweet spot, documented at `db.go:142-149`.

## Ordering inside a merge

A natural question: if the merge batches N independent producer requests together, do they end up in the order the producers submitted them. The answer is "the order does not matter for codeQ's workload, and that is by design". Pebble batches are append-only collections of point operations — each (Set or Delete) on a distinct key — and merging two batches via `Batch.Apply` preserves all ops. Producer batches write to different task UUIDs and therefore different keys; the merge order inside one raft entry only matters if two producers write the same key in the same merge, which the application layer makes impossible by routing same-key operations through the same logical path.

Across merges, raft's log ordering provides the usual sequential semantics: entry N is applied before entry N+1, on every node, in identical order. The coalescer never reorders across the boundary between two raft.Apply calls; it only batches within a single one.

## The leader's local Pebble path

In raft mode the engine layer wires the replication delegate via `pebblerepo.DB.AttachReplicator(r)` (`internal/repository/pebble/db.go:108`). After attach, every `Set`, `Delete`, and `CommitBatch` on the engine checks `r.IsLeader()` and either calls `r.Replicate(batch.Repr())` or returns a typed `NotLeaderError` carrying the leader's HTTP URL (`db.go:275-339`). The error type satisfies `pkg/domain.LeaderHint`, which the controllers use to emit the 307 redirect described in [Cluster Level Failover](Concepts-Cluster-Level-Failover).

On the leader, `Replicate` enters `applyLoop`, gets merged with concurrent calls, and eventually fires `raft.Apply`. That call ships the AE to followers, waits for quorum, and on success the FSM applies the merged batch to the local Pebble. The original caller's `done` channel resolves and `Replicate` returns. The caller's `*pebble.Batch` object is still valid (the FSM applied a copy of the repr, not the caller's batch), so the standard `defer b.Close()` pattern works exactly the same as on the non-replicated path.

On a follower, the FSM applies committed entries asynchronously. Followers can read the local Pebble at any time, but the read may reflect state that is one merge cycle behind the leader. The repository layer's claim path enforces strict ownership via raft-routed writes, so a follower that hands out a claim to an "expired" lease will lose the race against the leader's authoritative claim — the second submitter's `raft.Apply` will fail-or-overwrite based on the leader's actual state, not the follower's local snapshot.

## Mutual exclusion with sharding and cluster mode

Raft, intra-process Pebble shards, and the gRPC cluster router are not mutually composable in every combination. The configuration validator at `pkg/config/config.go:662-683` enforces two rules: `raft.enabled` and `cluster.enabled` are mutually exclusive, and `raft.enabled` and `sharding.enabled` are mutually exclusive at the legacy `Sharding` knob layer.

The first rule reflects that the gRPC cluster router and raft replication solve different problems with conflicting assumptions: the router hash-partitions task IDs across N independent stores with no replication, while raft replicates one store across N replicas with no partitioning. Combining them would need a per-shard raft group plus a per-shard router, which is the multi-shard raft path covered in [Deployment Modes](Concepts-Deployment-Modes) and not the legacy `cluster.enabled` flag.

The second rule reflects that `Sharding` is the old per-tenant backend selector, which assumes external Redis shards. Raft is Pebble-only. Multi-shard raft (the per-shard raft groups configured via `numShards` in the persistence config) is a separate path and does compose with raft.

The takeaway: pick the topology up front. Single-node Pebble, single-node Pebble + multi-shard, three-node raft, three-node raft + multi-shard. Mixing flags across modes does not produce a working configuration.

## The deep dive lives elsewhere

The wire format of the raft log, the snapshot stream layout, the mux transport's 4-byte group ID prefix, and the recovery semantics across shard restarts are deferred to [IO Raft Replication](IO-Raft-Replication) and [Mux Transport](IO-Mux-Transport). The failover walkthrough — leader dies, new leader elected, client 307s to the new leader, retries succeed — is in [Cluster Level Failover](Concepts-Cluster-Level-Failover). The performance envelope (what 9-10k cycles/s buys you, where the bottleneck moves under load) is in [Performance Cost Of HA](Performance-Cost-Of-HA).

This page's claim is narrower: consensus and replication in codeQ are raft over Pebble, the FSM is a one-function shim that turns committed log entries into Pebble batches, and the Apply coalescer at the raft layer mirrors the group commit coalescer at the engine layer because both layers amortise the same shape of contention. Everything else is detail.
