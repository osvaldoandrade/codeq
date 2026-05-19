# Leases and Ownership

A task in IN_PROGRESS belongs to exactly one worker for exactly as long as that worker's lease is valid. The lease is the contract that lets the server hand out work without losing track of who is doing it, and lets the worker do work without holding open a connection. This page covers how the lease is represented, why it lives in memory, how it survives a server restart, the heartbeat race between a slow worker and the reaper, and the ownership-transfer race that the `WorkerID` guard on Submit prevents.

## The in-memory lease table

The lease table is `leaseTable` in `internal/repository/pebble/lease_table.go:39-80`. The structure is a plain Go map under a `sync.RWMutex`:

```go
type leaseTable struct {
    mu sync.RWMutex
    m  map[string]leaseEntry
}

type leaseEntry struct {
    workerID string
    cmd      domain.Command
    tenantID string
    untilU   int64    // unix seconds
}
```

Per-task footprint is about 32 bytes for the entry plus the map overhead. A million concurrent leases consumes on the order of 32 to 64 MiB, which is well within any production sizing. The reaper sweeps the table every second; iteration cost is linear in the table size, which is the reason the entry is kept small. A `sync.Map` was tried and rejected — the load-then-CAS-replace pattern makes `ForEach` significantly more expensive than a plain map under `RWMutex`, and reaper iteration is the hot path the design tries to keep cheap.

The five operations are:

- `Set(taskID, workerID, cmd, tenantID, untilU)`. Installs the lease unconditionally, replacing any prior entry. Called by Claim, ClaimMany, and `completeClaim`.
- `Delete(taskID)`. Drops the entry. Idempotent. Called on Submit, Nack, Abandon, and reaper requeue.
- `Get(taskID)`. Read-only lookup. The reaper uses this just before issuing a requeue to double-check that a heartbeat hasn't extended the lease between the sweep snapshot and the requeue commit.
- `Extend(taskID, workerID, untilU)`. Extends the expiry only if the caller still owns the entry; returns `false` if the workerID has changed (i.e. another worker has reclaimed). This is the heartbeat path's atomic check.
- `SnapshotExpired(now, limit)`. Returns up to `limit` task IDs whose `untilU <= now`. The reaper iterates this slice and requeues each one. The cap keeps a single sweep from monopolising the lock under a million-lease backlog; subsequent sweeps drain the rest.

## Why the lease table is volatile

The lease table is in memory by design. The earlier implementation used an on-disk `codeq/lease/<id>` index, which paid one Pebble Set per Claim, one Get per reaper tick, and one Delete per Submit. None of these are free. The lease lookup is on the heartbeat hot path; every worker heartbeat would hit Pebble even when there was nothing to commit, and the Pebble compaction load from constant lease churn was a measurable share of total IO.

The in-memory design is correct for the same reason that any cache-of-durable-data design is correct: the cache can be lost without losing correctness, because the underlying durable state remains the source of truth. In codeQ's case, the durable state is the inprog index and the `LeaseUntil` field in the task body. The inprog key tells you a task is currently held by some worker; the `LeaseUntil` field tells you when that hold expires. The lease table is a serving cache over those two facts.

What the lease table buys is the reaper's ability to find expired leases in O(N expired) time rather than O(N total). A disk-resident lease index would have to scan every entry on every sweep; the in-memory map can iterate just the entries that are actually expired by checking `untilU <= now` per entry. For a system where leases live for 30 to 120 seconds and the reaper ticks every second, most entries are unexpired on most ticks, and the in-memory iteration is nearly free.

## Recovery on Open

A server restart wipes the in-memory table. The recovery path rebuilds it from durable state. The function is `recoverLeases` in `internal/repository/pebble/lease_table.go:131-172`, called once from the repository constructor (`task_repository.go:167-171`):

```go
if err := r.recoverLeases(); err != nil {
    panic(fmt.Sprintf("pebble lease recovery: %v", err))
}
```

The recovery scan walks every key under `codeq/q/` and picks out those whose path contains `/inprog/`. For each, it reads the task body via `KeyTask(id)`, unmarshals it, and seeds a lease entry from the persisted `LeaseUntil` timestamp. The semantics are:

- A task whose `LeaseUntil` is in the future gets installed in the table with its original expiry. The worker that owns it can continue heartbeating; from its perspective the restart was invisible.
- A task whose `LeaseUntil` is in the past gets installed with an already-expired expiry. The reaper's first sweep picks it up and requeues it via `requeueExpiredOne`. From the worker's perspective, the lease expired one reaper tick later than it would have without the restart — indistinguishable from the no-restart case.
- A task whose body fails to unmarshal, or whose `WorkerID` or `LeaseUntil` is empty, is skipped. The inprog key remains; the next state transition reconciles it.

This is a deliberately gentle recovery. There is no synchronous remediation; the reaper is the mechanism that fixes drift, and it gets a chance once per tick. The cost of an Open scan is proportional to the number of currently-in-progress tasks across all shards, which is bounded by the worker count times the average concurrent task count per worker, typically well under a million.

The trade-off is that any worker holding a task at the moment of the crash sees its lease honoured up to whatever was already written to the task body. If the worker had heartbeated `LeaseUntil` to t+60 and the server crashed at t+5, the recovered lease still expires at t+60 even though no part of the system has any record of the worker between t+5 and t+60. That is fine in practice — the heartbeat that wrote `LeaseUntil` to t+60 was durable; the worker chose to commit to that horizon.

A note on `KeyLease`: the on-disk key prefix `codeq/lease/<id>` is still defined in `keys.go:33` and used as the recovery side-channel; it does not carry runtime traffic. The header comments call out the trade-off clearly — the in-memory table is the active lease store, while the inprog scan rebuilds it from task bodies on Open.

## The heartbeat race

Consider a worker A that claims task T at t=0 with a 60-second lease. The lease entry says `untilU = 60`. At t=10, worker A pauses (GC, kernel context switch, disk stall — whatever) for 70 seconds. Meanwhile:

At t=60, the reaper sweeps. `SnapshotExpired(now=60, ...)` returns `T` because `untilU == 60 <= 60`. The reaper calls `requeueExpiredOne(T)`, which atomically deletes the inprog key, writes a new pending key with a fresh sequence, increments `Attempts`, clears `WorkerID` and `LeaseUntil`, and finally deletes the lease entry.

At t=62, worker B calls Claim. The pending key for T is at the head of its priority bucket (because the requeue assigned a fresh seq), so worker B claims it. The Claim batch deletes the pending key, writes a new inprog key (same task ID), writes a new lease entry with `workerID=B` and `untilU=122`, and rewrites the task body with `WorkerID=B, LeaseUntil=t=122`.

At t=80, worker A wakes up. From A's perspective, nothing has changed locally — A still believes it owns T. A's next action determines what happens.

If A calls Heartbeat(T, A, 60), the call lands at `Extend(T, A, 80+60=140)`. The lease table's check is:

```go
e, ok := t.m[taskID]
if !ok || e.workerID != workerID {
    return false
}
```

The entry exists (B installed it), but `e.workerID == "B"`, not `"A"`. Extend returns false. The Heartbeat RPC propagates `not-owner` to A. A can then choose to Abandon (the call would also fail with `not-owner`, but A learns the truth) or to give up locally and stop working on T.

If A calls Submit(T, A, COMPLETED, result), the results service runs the ownership check at `internal/services/results_service.go:60`. It reads the task body, finds `WorkerID = "B"`, sees `req.WorkerID = "A"`, and rejects with `not-owner`. A's result is discarded. B is still working on T (or has already finished it); whichever submission lands with the matching WorkerID wins, and only that one is durable.

The race is therefore resolved in B's favour, which is the correct answer: B legitimately claimed the task after A's lease expired. A's claim was effectively abandoned by inactivity, and codeQ's job is to make sure no result from an abandoned claim overrides a result from the legitimate owner.

## Ownership transfer race

A subtler race appears when the reaper is sweeping concurrently with a Heartbeat. Suppose worker A's `untilU = 60` and the wall clock is now 60. The reaper's `SnapshotExpired` runs and returns `T`. Before the reaper can execute `requeueExpiredOne(T)`, worker A's Heartbeat arrives and calls `Extend(T, A, 120)`. The Extend acquires the write lock, sees `e.workerID == A`, sets `e.untilU = 120`, and returns true. A is happy.

Then the reaper proceeds to call `requeueExpiredOne(T)`. If the reaper trusts its earlier snapshot, it would clobber A's now-extended lease. To prevent this, `requeueExpiredOne` re-checks the in-memory lease at requeue time. The recheck is the reason `leaseTable.Get` exists. If the entry's `untilU` is in the future at the moment of requeue, the operation is aborted — the snapshot was stale, the lease has been extended, and no requeue is needed. The reaper sweep merely missed this round; the next sweep will see the entry as either still-valid (if A keeps heartbeating) or properly expired (if A goes silent again).

The lock ordering is significant. Heartbeat takes the write lock briefly, then releases. The reaper's snapshot phase takes the read lock briefly to collect IDs, releases, then for each ID takes the write lock briefly inside `requeueExpiredOne` to do the recheck-and-delete. There is no transaction spanning the two operations, so the recheck is what makes the race safe rather than the locking.

## Capacity math

The lease table sets a soft ceiling on concurrent in-progress tasks. The per-entry cost is the struct itself plus map bookkeeping; a conservative estimate is 100 bytes per entry once Go's map overhead is included. The recovery scan cost on Open is one Pebble Get per inprog entry, which is a few microseconds per entry on local SSD, so a million leases recover in a few seconds.

The reaper iteration cost is what bounds practical scale. At one sweep per second, a million-entry table costs roughly 10 ms of CPU time per sweep (about 10 ns per entry for the comparison and the conditional append), plus contention on the RWMutex with concurrent Heartbeats. The `SnapshotExpired` cap of 256 entries per sweep keeps the write-lock hold time bounded; ten million entries simply take ten million / 256 sweeps to drain, which at one second per sweep is around 40,000 seconds — only relevant if you crashed with that many simultaneously-in-progress tasks, which would imply a workload mismatch elsewhere.

For typical deployments — tens of thousands of workers, lease durations in the 30 to 120 second range — the table stays under a hundred thousand entries with steady-state turnover. The reaper cost is negligible, the recovery cost is sub-second, and the memory footprint is in single-digit MiB.

## The two ownership checks

Two separate code paths enforce the "only the current owner can act on this lease" rule. They both exist because they protect different races.

The first is `leaseTable.Extend`'s `workerID` check (`lease_table.go:78-82`). This protects Heartbeat against the case where another worker has reclaimed the task. It is purely in-memory and runs on every Heartbeat. Its cost is one map lookup plus one string comparison.

The second is `results_service.go:60` (single Submit) and `results_service.go:249` (batch Submit):

```go
if task.WorkerID != "" && req.WorkerID != "" && task.WorkerID != req.WorkerID {
    return ..., "not-owner"
}
```

This protects Submit against the case where the worker's lease was stolen after work completed but before Submit reached the server. The Submit path reads the durable task body, so the comparison is against the most recently written `WorkerID` regardless of in-memory state. If the lease has changed hands, the task body's `WorkerID` will be the new owner's, the comparison will fail, and the stale submitter gets `not-owner`.

The conjunction `task.WorkerID != "" && req.WorkerID != ""` is worth understanding. If the task somehow has an empty `WorkerID` at submit time (which shouldn't happen for an IN_PROGRESS task, but the code is defensive), the check is skipped. If the request has an empty `WorkerID` (the SDK doesn't bother to pass it), the check is also skipped, on the assumption that the caller wants to submit as a trusted operator. The check is active only when both sides have identities to compare. This is a pragmatic choice: some test harnesses and admin tools intentionally bypass the worker identity, and forcing them to pass a synthetic WorkerID would be more friction than it's worth.

## Summary of guarantees

The leasing protocol gives codeQ the following properties:

- At any moment, an IN_PROGRESS task is associated with at most one worker. This is the lease invariant.
- A worker that stops heartbeating long enough has its task forcibly returned to PENDING by the reaper. The threshold is the lease duration.
- A worker's late Submit after lease theft is rejected. The result of work done past the lease window is discarded.
- A server restart preserves all unexpired leases. Workers do not need to be aware of the restart.
- Concurrent Heartbeat and reaper-sweep cannot produce a double-requeue. The recheck-on-requeue inside the lease table prevents it.

These are not absolute. A worker that completes work, has its lease stolen, but submits before the server's task body is rewritten can get `not-owner` returned even though the work was correct — the worker just lost the race. The result is durable on the second owner's submit (which happens later). In effect codeQ's contract is at-least-once delivery with the lease enforcing "at most one in-progress claim per task at a time", not "exactly one". Producers that need exactly-once semantics rely on the result rendezvous: a task either has a `ResultRecord` or it doesn't, and a duplicate result write from the second owner overwrites the first with identical content (or with different content, in which case the second wins). The combination of idempotency keys on the producer side and ownership checks on the worker side bounds the effective duplication rate to near zero in steady state.
