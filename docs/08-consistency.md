# Consistency, failures, and clocks

This document is the canonical reference for the consistency, durability, and delivery guarantees codeQ provides — and, just as important, the ones it does not. Everything below is grounded in the Pebble-backed code path; the Redis backend has been retired.

## 1. Headline guarantee

codeQ provides **at-least-once** task delivery. Idempotency on submission is best-effort via a per-tenant idempotency-key map (`KeyIdempo`, see `internal/repository/pebble/keys.go:25-59`). Strict exactly-once delivery would require a transactional consumer (the worker commits its side effect in the same transaction that acks the task). codeQ does not implement that layer and does not pretend to.

The remainder of this document derives every claim from the code path:

- Enqueue: `internal/services/scheduler_service.go:94-143` → `internal/repository/pebble/sharded_task_repository.go:80-97` → `internal/repository/pebble/task_repository.go:202-290`.
- Result submission and worker-ownership check: `internal/services/results_service.go:200-330`.
- Lease enforcement: `internal/repository/pebble/reaper.go` (`sweepLeases`).

## 2. At-least-once — formal definition

> A task created via Enqueue **will** be claimed by at least one worker, and **may** be claimed and processed by more than one worker if a previous claimant fails to complete within the lease (or returns Nack/Abandon). The system never silently drops a task.

This holds for three reasons, each rooted in the Pebble storage layer:

1. **Enqueue writes are durable.** `EnqueueWithID` builds a `pebble.Batch` containing `KeyTask`, `KeyPending` (or `KeyDelayed`), `KeyTTL`, and optionally `KeyIdempo`, then calls `CommitBatch` (`internal/repository/pebble/task_repository.go:245-279`). Pebble's WAL is appended on commit; on Open after a crash the WAL replays into the memtable, so a committed batch survives process death.
2. **Claims are durable.** When a worker claims a task, the same path moves the queue index (deletes `KeyPending`, writes `KeyInprog`) and sets `KeyLease` with `(workerID, leaseUntil)` in one batch. After restart, the in-progress index plus the lease key are visible to the reaper, which decides whether to extend or requeue.
3. **Stale leases are reaped.** `sweepLeases` walks `KeyLease` keys whose `until-unix` has passed. For each, it deletes the lease, deletes the in-progress index, restores `KeyPending`, and resets `Task.Status` to `pending`. A crashed worker therefore cannot strand a task; the task becomes claimable again within one reap cycle.

The cost of this reliability is duplicate processing: a worker that paused (long GC, network partition) past its lease will see another worker claim and complete the task. Section 5 walks through how the system arbitrates the duplicate completion attempt.

## 3. Idempotency key flow

The application layer can pass an `idempotencyKey` on Create. The repository uses it to short-circuit duplicate submissions:

```
Client (idempotencyKey=K)
   |
   v
shardOf(K) % N -> shard k                       (FNV-1a 64, modulo shard count)
   |
   v
Get KeyIdempo(K) -> existing task ID?
   |
   +-- yes -> Get KeyTask(existingID), return the original task
   |
   +-- no  -> pick new id; write { KeyTask(t), KeyPending(t) or KeyDelayed(t),
                                   KeyTTL(t), KeyIdempo(K) -> t } in one batch
```

The hot path in `ShardedTaskRepository.EnqueueWithReady` (`internal/repository/pebble/sharded_task_repository.go:80-97`):

```go
if idempotencyKey != "" {
    idShard := s.shardOf(idempotencyKey)
    if existing, err := s.shards[idShard].db.Get(KeyIdempo(idempotencyKey)); err == nil {
        existingID := string(existing)
        tShard := s.shardOf(existingID)
        task, ferr := s.shards[tShard].Get(ctx, existingID)
        if ferr == nil {
            return task, false, nil
        }
    }
}
```

Two important points:

- The lookup uses `shardOf(idempotencyKey)`, **not** `shardOf(taskID)`. An idempotency key always lands on the same shard regardless of the task ID generated, so `KeyIdempo` is a deterministic per-tenant dedup map.
- The original task is returned with `ready=false`. Callers that key off `ready` to send a queue-ready signal will not re-fire on dedup hits (`internal/services/scheduler_service.go:136-141`).

On the write side, `EnqueueWithID` performs the same lookup on the local shard, then sets `KeyIdempo(K) -> id` last in the batch (`internal/repository/pebble/task_repository.go:270-275`). Setting it last is a sequencing choice — the batch commits atomically, so order within the batch does not affect visibility, but it documents the intent: the mapping should be the final visible artifact of a successful enqueue.

## 4. Concurrency / collision window

The dedup check is **read-then-write**, not a compare-and-swap. Two concurrent submitters with the same `idempotencyKey` arriving within microseconds may both see the empty read and both proceed to write.

What actually happens in that race:

1. Submitter A and Submitter B both Get `KeyIdempo(K)` → `ErrNotFound`.
2. A builds Batch_A: `{KeyTask(idA), KeyPending(idA), KeyTTL(idA), KeyIdempo(K) -> idA}`.
3. B builds Batch_B: `{KeyTask(idB), KeyPending(idB), KeyTTL(idB), KeyIdempo(K) -> idB}`.
4. Both call `CommitBatch`. Pebble serializes commits through its WAL; whichever LSN is assigned second overwrites `KeyIdempo(K)` with its task ID.
5. **Both** `KeyTask(idA)` and `KeyTask(idB)` exist. **Both** `KeyPending(...)` entries exist. Two workers may claim and process; two results may be submitted.
6. Subsequent submits of the same idempotency key see whichever ID won the last write.

In CS terms: `KeyIdempo` follows last-writer-wins semantics with no CAS, and the queue inserts are independent writes that are not conditioned on the dedup state. The window is bounded by the time between the `Get` and the `CommitBatch` — at single-process latencies in the low microseconds for memtable hits, longer when the WAL fsync stalls.

This is acceptable for the typical workload (idempotency keys collide rarely and are usually retries from the same client serially). For applications that must guarantee zero collisions, the client side should serialize submits per idempotency key — codeQ does not provide that ordering for free. Both alternatives are heavier than the current design:

- A CAS on `KeyIdempo` would require either a single-shard transaction (Pebble supports indexed batches but the contention would serialize the whole shard) or a Lua-equivalent atomic block — neither matches the current lock-free hot path.
- Pre-allocating idempotency keys server-side would push the de-dup decision earlier but would not help concurrent reuse of the same key.

The documented contract is therefore **best-effort idempotency at very low collision risk**.

## 5. Worked examples

### Example 1 — Process crash after Enqueue ack

- Client → server: `Create(cmd, payload, idemp=K)`.
- Server: builds the batch; `CommitBatch` returns success; WAL is fsynced (controlled by `fsyncOnCommit`).
- Server: writes the response; ack reaches the client.
- Server: crashes.
- On restart: Pebble Open replays the WAL into the memtable; `KeyTask`, `KeyPending`, `KeyIdempo` are visible. The task is in `pending`, same as if nothing had happened.

No work lost, no duplicates.

### Example 2 — Process crash mid-batch

- Client → server: `Create`.
- Server: assembling the batch (`KeyTask`, `KeyPending`, `KeyTTL`); `CommitBatch` has **not** been called.
- Server: crashes.
- Client: sees a connection error, no ack. The Create result is unknown to the client.
- On restart: nothing for this task exists in Pebble. The batch was never committed; the WAL has no record of it.

Action: the client retries with the same idempotency key. If the prior attempt had partially completed (somehow it landed but the ack was lost — Example 1's case), the retry is deduplicated by `KeyIdempo`. Otherwise the retry creates a fresh task.

### Example 3 — Lease race (the duplicate-processing case)

- Worker A claims task X at t=0, lease = 60s. `KeyLease(X) = (A, t=60)`.
- A's process pauses 70s (long GC, scheduler starvation, kernel pause).
- At t=60, reaper finds `KeyLease(X)` expired → moves X back to pending; clears `KeyLease`, `KeyInprog`. `Task.Status = pending`, `Task.WorkerID = ""`.
- At t=62, worker B claims X, lease = 60s. `KeyLease(X) = (B, t=122)`. `Task.WorkerID = B`.
- B completes at t=70, calls `SubmitResult(taskID=X, workerID=B, status=COMPLETED, ...)`. `results_service.go` checks `task.WorkerID == req.WorkerID` → B == B → accept; result stored; task moves to `completed`.
- At t=72, A wakes up and calls `SubmitResult(taskID=X, workerID=A, ...)`. Same check: `task.WorkerID = B`, `req.WorkerID = A` → mismatch → reject with `"not-owner"` (`internal/services/results_service.go:248-252`).

Outcome: A's work is silently discarded by the server. If A had already produced external side effects (HTTP POST to a third party, a row in another database), those side effects are not undone — that is the standard cost of at-least-once. The application owner has to make worker side effects idempotent.

### Example 4 — Same idempotencyKey, two submitters

- Client 1 and Client 2 both submit `Create(idemp="K1")` within the same microsecond on different RPC connections.
- Both reach `shardOf("K1") = 3`.
- Both call `Get(KeyIdempo("K1"))` → `ErrNotFound`.
- Both build their own batch with distinct task IDs (`id1`, `id2`).
- Both call `CommitBatch`; Pebble serializes them; both succeed; `KeyIdempo("K1")` ends up pointing to whichever Commit got the later LSN.
- Both `KeyTask(id1)` and `KeyTask(id2)` exist; both are in `pending`. Two workers may pick them up.
- A third client that issues `Create(idemp="K1")` later reads `KeyIdempo("K1")` → gets the winner → returns it without enqueueing again.

This is the case Section 4 describes. The "loser" task body is durable storage residue; the application can ignore it by deduping its own work, or it can accept duplicate work as the price of the race window.

## 6. Consistency model summary table

| Operation | Single-node Pebble | Raft (any group) |
|---|---|---|
| Enqueue write durability | WAL append per `CommitBatch`; fsync per `fsyncOnCommit` setting | Majority quorum required for ack; entry replicated to a quorum's WAL before apply |
| Enqueue read-after-write (same client, same node) | Linearizable: Pebble is sequentially consistent inside one process | Linearizable on the leader; up to one heartbeat-interval stale on followers (`HeartbeatMS=1000` default, `pkg/config/config.go:147`) |
| Cross-client read consistency | Read-your-writes; no cross-process anomalies because there is only one writer per shard | Linearizable on leader; bounded staleness on followers; no read-index enforcement |
| Concurrent `idempotencyKey` collisions | Best-effort dedup with the race window from Section 4 | Same — dedup runs **before** raft submission, so two routers can both decide to enqueue |
| Task completion | At-least-once with worker-ownership check; loser writes rejected | Same; the ownership check runs on whichever node owns the shard |
| Lease expiry | Pebble lease table + reaper sweep; persisted `LeaseUntil` survives restart | Same; reaper runs on the shard leader |
| Delayed task promotion | `MoveDueDelayed` fast-path on the shard leader | Replicated through raft; followers see the move when AppendEntries lands |

A few caveats on the raft column:

- codeQ does not currently enforce **linearizable reads** by issuing a raft read-index before serving reads from the leader. A reader on the leader sees its own writes and writes that have been applied locally, but in a split-brain transient (very rare, bounded by election timeout `ElectionMS`) a stale leader could serve a stale read. To get strong linearizable reads the caller should write a no-op and then read, or route the read through a known fresh leader.
- Follower staleness is bounded by `HeartbeatMS` plus AppendEntries RTT. Under healthy networks the lag is sub-second.
- Idempotency dedup happens at the router boundary, not inside the raft state machine. A two-router race (Section 4) is therefore possible even with raft; raft only linearizes the **writes**, not the read-modify-write decision.

## 7. What codeQ does NOT guarantee

- **Exactly-once delivery.** A task may be processed more than once when a worker fails to complete inside its lease. The worker-ownership check (`results_service.go:248-252`) ensures only one result is recorded, but the side effects of the duplicate processing are visible to the world.
- **Strict total order across tasks.** Order is FIFO per `(command, tenant, priority)` shard inside Pebble's queue layout (`KeyPending` keys are sorted by `priority_be1 / seq_be8 / id`). Across shards, across priorities, or across tenants, there is no order.
- **Cross-shard atomicity.** There is no two-phase commit, no SAGA coordinator. A multi-task workflow that spans shards is the application's responsibility.
- **Linearizable reads on followers.** Reads on followers can be up to one heartbeat stale. If the application requires linearizable reads, route the read to the leader and accept the latency cost.
- **Clock-skew-bounded leases.** Lease expiry is driven by the server's monotonic-ish clock (`time.Now()` inside the reaper). Workers receive `leaseUntil` as a service-clock advisory and should not rely on it for cross-machine ordering decisions. The persisted `LeaseUntil` on the task body is authoritative on the server.
- **Strict idempotency.** Section 4 above.

## 8. Complexity evidence

Let N be the number of tasks in a shard and L the configured scan limit.

| Operation | Complexity | Source |
|---|---|---|
| Enqueue | O(1) batched writes + O(log N) Pebble memtable insert | `task_repository.go:245-279` |
| Claim | O(L) range scan over `KeyPending` until first hit + O(1) batch move | `task_repository.go` (claim path) |
| Reap stale leases | O(K) where K is the number of expired leases this sweep | `reaper.go::sweepLeases` |
| Move due delayed | O(L) per shard, bounded by `MoveDueDelayed` limit argument | `sharded_task_repository.go:158-170` |
| Cleanup expired (TTL) | O(K) over `KeyTTL` keys with `expire_unix < now` | `reaper.go::sweepTTL` |

These bounds follow directly from Pebble's LSM properties (ordered range scans, batched writes, memtable then SSTable lookup). They are not affected by the choice of single-node versus raft, except that raft adds the constant quorum-replication latency to every write commit.

## 9. Putting it together

If you only remember three things:

1. codeQ is **at-least-once**. Workers must be idempotent in their side effects, or accept that duplicates can leak.
2. The `idempotencyKey` map deduplicates the common case (sequential retries of the same key) and degrades gracefully under concurrent submissions of the same key (Section 4). It is not a substitute for client-side serialization when zero collisions are required.
3. Raft gives you durable, linearizable writes on the leader. It does not give you exactly-once, it does not give you linearizable follower reads, and it does not change the at-least-once contract — those are properties of the queue semantics, not of the replication layer.
