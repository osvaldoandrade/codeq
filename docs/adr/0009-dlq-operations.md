# ADR 0009: Requeue and delete dead-lettered tasks

- **Status**: Proposed
- **Date**: 2026-10-08
- **Deciders**: @osvaldoandrade (maintainer); proposed by @fernandomarcius
- **Tracks**: —

## Context

A task whose attempts run out (nack or lease expiry) becomes `FAILED` and
gets an entry in the dead-letter index `q/<cmd>/<tenant>/dlq/<id>`. Nothing
takes it out again. After fixing the cause of a failure an operator cannot
retry the task: the only option is to create a new one, which loses the ID,
the history and any idempotency mapping pointing at it. A poison task cannot
be removed either; it waits for the retention sweep, which deletes the
`FAILED` body about a day after the task was created but leaves its
dead-letter entry behind, so `QueueStats.dlq` keeps counting it.

## Decision

Three admin routes, in the existing `admin` group (`RequireAdmin`). The
tenant comes only from the validated token. A task that does not exist or
belongs to another tenant answers `404 {"error":"not found"}`, exactly like
`GET /tasks/{id}`, so the routes are no existence oracle. Binding-scoped
tokens are refused as on every `/admin` route (ADR 0003).

**`POST /v1/codeq/admin/tasks/{id}/requeue`** → `200` with the task.

- Only a dead-lettered task qualifies: status `FAILED` *and* a dead-letter
  entry. Anything else, including a task failed through a result submit,
  answers `409 {"error":"task_not_in_dlq"}`.
- The task restarts as a new run: `PENDING`, `attempts` 0, and no `error`,
  worker, lease, `resultKey` or stored result. Payload, priority, webhook,
  `maxAttempts`, tenant and `createdAt` are kept.
- It joins the tail of the ready bucket of its own priority (a requeue is a
  new arrival; the old pending sequence is not recorded anywhere), and its
  dead-letter entry is removed in the same replicated batch. Workers
  subscribed to the command are notified as on a create.

**`POST /v1/codeq/admin/queues/{command}/dlq/requeue?limit=N`** → `200
{"requeued": n, "remaining": true|false}`.

- Requeues up to `limit` tasks of the caller tenant's dead-letter queue for
  the command, each exactly as above, in task ID order. `limit` defaults to
  100 and accepts 1–1000 (`400` otherwise); the repository commits in chunks
  of 64 tasks, which bounds one replicated log entry and how long a task is
  held.
- `remaining` is a boolean, not a count. Counting would scan the whole
  dead-letter queue on every call, making a drain loop quadratic; the
  boolean costs one seek. A client drains the queue by repeating the call
  while `remaining` is true.
- An entry whose body is gone, is no longer `FAILED` or belongs to another
  queue is stale: it is dropped in the same batch and not counted. This also
  clears the entries the retention sweep leaves behind, so a drain loop
  always ends. An entry whose task another operation holds is skipped and
  keeps `remaining` true.

**`DELETE /v1/codeq/admin/tasks/{id}`** → `204`.

- Refused with `409 {"error":"task_in_progress"}` while a worker holds the
  task (or a claim of it is in flight).
- Removes, in one replicated batch, the task body, its stored result and
  the queue entry its state names: the pending entry in its priority bucket,
  the delayed entry, or the dead-letter entry (plus a stray in-progress
  entry, if any). The pending and delayed entries carry a sequence or score
  that the body does not record, so they are located by scanning the task's
  own bucket: O(depth of that bucket), on an admin path only.
- It deliberately leaves two kinds of entries, both of which the existing
  code already treats as harmless leftovers:
  - **TTL schedule entries** (`ttl/<expire>/<id>`). Enqueue, every claim,
    heartbeat and completion write one; they are keyed by time, not by task,
    so finding them means scanning every entry any task wrote during this
    task's lifetime. The retention sweep drops an entry whose body is gone
    and does nothing else; it leaves the same leftovers itself after
    reaping a body. They are gone within the retention window.
  - **The idempotency mapping** (`idempo/<key> → id`). The key is not
    recorded on the task, and mappings are never deleted (not by retention
    either), so finding it means scanning all of them. A create with that
    key finds no body and creates a new task, overwriting the mapping: the
    key is free after the delete, as it is after retention today.

**Concurrency.** Writes happen only on the leader, through
`ensureLeaderDispatch` and `CommitBatch` like every other mutator; a Raft
follower forwards the by-ID routes like a single write (`fwd.Single`) and
the bulk route like a batch (`fwd.Batch`). The existing mutators are
read-check-write without compare-and-swap. What keeps them apart is the
in-memory dispatch state: a claim marks the task ID in flight from the
moment it pops the hint until its commit, and `popHint` skips an in-flight
ID; the delayed sweep is single-flighted per (command, tenant) by a CAS flag.
The new operations join that scheme instead of adding a lock to the hot
path:

- They mark the task in flight for their whole read-check-write, waiting
  (bounded by the request context) while a claim or another admin operation
  holds it. A reserved task cannot be claimed; a claim in flight makes a
  delete wait and then answer `409`; a requeue and a delete of the same task
  run one after the other. A claim that pops the hint of a reserved task
  drops it, so a delete that fails after locating the pending entry puts
  the hint back.
- A delete of a delayed task also takes the delayed-move flag of its bucket
  and re-reads the body under it, so the sweep cannot turn the task ready
  in between.

Windows that remain, none of them introduced by this ADR:

- *Retention sweep vs requeue.* The sweep reads a `FAILED` body and deletes
  it in a later commit without any lock. A requeue that commits in between
  is undone (the body goes; the pending entry left behind is discarded by
  the claim path as a ghost). This needs the requeue to land in the same
  instant the task reaches its retention limit.
- *Late result submission.* `Submit` checks `IN_PROGRESS`, uploads
  artifacts, then writes the outcome without re-checking. If the lease
  expired meanwhile and the task was dead-lettered and then requeued or
  deleted, the late write lands on it. Today the same window lets a late
  result overwrite a dead-lettered task.
- *Visibility check on a lagging replica.* The routes check the tenant
  against the local copy of the task, like `GET /tasks/{id}`. A follower
  forwards up front whenever one peer leads every group (every current
  installation); only with split shard leadership can a lagging follower
  answer `404` for a task it has not applied yet.

**Partitions.**

- Sharded: the by-ID routes go to `shardOf(id)`. The bulk requeue walks the
  shards in order on the shards this node leads; a shard led elsewhere still
  counts toward `remaining`, and when the led shards moved nothing while
  such a shard holds entries, its not-leader error is returned so the call
  is forwarded to that shard's leader. A client repeating the call therefore
  always reaches a node that can make progress.
- Cluster: `RequeueTask` and `DeleteTask` RPCs go to the owner of the ID
  (same bloom short-circuit as `Nack`); the `RequeueDLQ` RPC is sent to each
  node in ring order with the part of `limit` still unspent, like
  `ListTasks` (ADR 0005). A node that cannot answer fails the call instead of
  being taken for empty; tasks already requeued stay requeued and a repeated
  call continues from there.

## Consequences

- Operators can retry a dead-lettered task in place, drain a dead-letter
  queue, and discard a task, without a side tool.
- Requeued tasks keep their ID, so idempotency replays and stored
  references keep pointing at them.
- New wire surface: three HTTP routes and three `TaskNode` RPCs with six new
  messages; no existing message changes. `repository.TaskRepository` gains
  three methods; `pkg/persistence.TaskStorage` is unchanged.
- `DELETE` leaves TTL entries and the idempotency mapping (above). Recording
  the idempotency key and the TTL score on the task would let both `DELETE`
  and the retention sweep remove them exactly; that changes the hot write
  path and is left to a later decision. It becomes more pressing if client
  chosen task IDs (ADR 0008) land: a reused ID could then meet a leftover
  TTL entry of its deleted namesake and be reaped early.
- Interplay with ADR 0004 (deduplication), carried here: a delete of a
  waiting task releases the deduplication mapping in its batch, through the
  same release the claim uses, which deletes the mapping only while it still
  names the task. A requeue does not re-acquire the key: it retries the
  task, like a nack retry, and is not a new create. A requeued task keeps
  `DeduplicationKey` on its body as a record while a newer task may hold the
  key, so neither its next claim nor its delete touches that newer mapping.
- Interplay with proposals still open, for whichever lands after this ADR:
  with ADR 0007 (progress) a requeue should clear the progress of the
  previous run; with ADR 0008 (client-chosen task IDs) see the TTL note
  above.

## Alternatives considered

- **Requeue into the delayed index with backoff, keeping attempts**: the
  task would fail again on its first nack; an operator requeueing a
  dead-lettered task asks for a fresh run.
- **Requeue at the head of its bucket**: the original sequence is not
  recorded, and jumping ahead of waiting tasks is not obviously right.
- **A count in the bulk response**: quadratic drain loop (above).
- **One unbounded bulk call**: unbounded request time and replicated entry.
- **A per-task lock taken by every mutator, or compare-and-swap writes**:
  a structural change of the claim hot path; the in-flight set already
  serializes the operations that matter here.
- **Answer `503` for the bulk route in cluster mode**: one RPC per node, as
  for `ListTasks`, is proportionate.
- **Scan for TTL and idempotency entries on delete**: cost proportional to
  all traffic, not to the task (above).
- **Soft delete with a tombstone status**: every reader would learn a new
  status.

## References

- ADR 0003 (binding-scoped tokens; `/admin` refused).
- ADR 0005 (listing a queue state; the node walk the bulk requeue reuses).
- Platform ADR-0022 C1.5 (in-process leader forwarding).
