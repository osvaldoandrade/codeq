# ADR 0013: Retire expired absolute rows in batches (CFP-070)

- **Status**: Proposed
- **Date**: 2026-11-06
- **Deciders**: Osvaldo Andrade (CFP-070 coordinator request)
- **Amends**: ADR 0010 (retirement mechanics only; no contract change)
- **Ships in**: CodeQ v0.5.13

## Context

On 2026-11-06 the Conveste MASTER `local-tenant` `cf-logs` queue stopped
draining after a leader election. Every single-task claim
(`POST /v1/codeq/tasks/claim`, `waitSeconds: 0`) ran past the followers' 10 s
leader-forward timeout and the client's 12 s timeout, so nothing was claimed.
The collectors kept publishing, so the backlog and the voter volumes grew
(about 1.1 to 2.3 GiB of 3.9 GiB in 8 hours). Tenant `conveste`, with an
empty queue, claimed normally.

In v0.5.12 every expired absolute row cost one replicated commit, wherever it
was met:

- `Claim` → `completeClaim` called `purgeAbsoluteRow` for each expired head:
  one Raft round trip per row, up to the 200-hint inspection budget. With a
  FIFO whose head is older than the custody window, every claim spent its
  whole time on serial round trips and never reached a live task.
- `ClaimMany`, the claim prelude's `moveDueDelayedForTenant`, the reaper's
  lease sweep and TTL cleanup did the same.
- `sweepAbsolute` committed each of up to 512 identities separately while
  holding `absoluteMu` for the whole page. Absolute claims take that mutex to
  commit their lease, so they waited behind the entire page.

The leader was CPU-starved (50m request on a node at 94% CPU), so each commit
was slow and the per-row pattern turned a backlog into an outage.

## Decision

- Expired absolute rows retire through `DB.purgeAbsoluteRows`, which takes
  `absoluteMu` once, stages every row into one Pebble batch and replicates it
  in one commit per `absoluteRetireBatch` (256) rows. The identity count is
  read when the retirement starts and reread after every commit, and carried
  across the rows staged in between (`+1` for a restored row, `-1`, clamped
  at zero, for an identity whose replay window closed), so the stored count
  ends where the per-row path left it. Each row is staged in a scratch batch,
  so a row that fails to stage adds nothing; it stays, the rows after it
  still retire, and the first staging error is returned after the commit.
- A retirement belongs to one leadership term. If the replicator reports a
  new leadership epoch before a commit (this node lost and regained
  leadership, so another leader may have written the rows and the count it
  read), the remaining rows are abandoned unretired with
  `errAbsoluteRetirementTermChanged`, or `NotLeaderError` when this node is
  no longer leader, and a later pass rereads them.
- `Claim` holds the hints of expired rows in flight instead of purging them
  one by one. It commits the live task's lease first, with the lease start
  stamped immediately before that commit, and then retires the held rows in
  one batch. A claim that meets N expired heads before a live task commits
  twice (lease, then retirement), not N+1 times. Held rows are in flight, so
  no other claimer can take them meanwhile; leasing first keeps the gap
  between reading the live row and leasing it as short as it was in v0.5.12.
- A live row whose deadline passes between its read and its lease commit
  (`prepareAbsoluteBatch` refuses it with `task_expired`) is held as an
  expired row, and the claim moves on to the next hint within its budget.
  The refused lease writes nothing, so a moving expiry frontier costs reads,
  never commits; the claim still commits once for its lease and once per
  256 retired rows at return.
- If the claim finds no live task, or fails or is canceled, the held rows
  still retire when it returns. That commit does not depend on the request
  context, so a client that gave up still leaves progress behind. A held hint
  whose row did not retire (failed commit, staging error, or a row no longer
  expired) is republished, as a failed per-row purge was. After a committed
  lease, a retirement failure is logged and the lease is returned: the lease
  is durable, and the unretired rows keep their hints for a later claim.
- `ClaimMany` reads its popped rows, commits the leases of the live ones in
  one batch and then retires the expired ones in one batch. A candidate that
  expired before the lease batch committed is moved to the expired rows and
  the rest is retried; each retry drops at least one row and none commits.
  `ClaimMany` and the delayed move no longer commit an empty batch when every
  row was expired.
- `moveDueDelayedForTenant`, the reaper lease sweep and `dropExpiredBodies`
  collect their expired rows and call `purgeAbsoluteRows` once.
- `sweepAbsolute` stages its page into the same batched retirement: one
  commit per 256 identities, and `absoluteMu` is held for one or two commits
  instead of hundreds.

## Consequences

- Claim latency behind an expired backlog no longer scales with the backlog.
  Benchmark `BenchmarkClaimBehindExpiredHeads` (200 expired heads, then a
  live task; Apple M4 Max): standalone Pebble with fsync went from about
  840 ms to 11 ms. A replicator charging 2 ms per commit went from 1.6–2.0 s
  to 17 ms, and from 201 to 2 commits.
- One retirement batch carries up to 256 rows. A measured 200-row
  retirement replicates 117 KB (about 600 bytes per row), so the largest
  retirement batch is about 150 KB. The Raft apply loop merges up to 128
  (`raftMergeBatch`) concurrent submitter batches into one entry. Retirement
  commits run under `absoluteMu`, so one merged entry carries at most one
  retirement batch: the worst case is about 150 KB of retirement plus up to
  127 other submitters' batches, whose sizes this ADR does not change.
- Expired hints stay in flight a little longer, until the claim's retirement
  commit after its lease. Another claimer that pops a duplicate hint for the
  same ID drops it, as it already did for any in-flight hint. The holder
  republishes it if the row does not retire. A process that stops between
  the lease and the retirement leaves the rows pending; they are recovered
  as hints on restart and retired by the next claim or the sweep.
- Review amendment (2026-11-06): the first cut retired the held rows before
  the lease commit. On a slow leader the first live row, the closest to its
  deadline, expired during that retirement and the claim failed with
  `task_expired` (HTTP 400), ending the long poll. Leasing first, holding a
  row that expires at lease time, rereading the count per flush, abandoning a
  retirement across a leadership change, and skipping a row that fails to
  stage close that gap. Tests: `claim_expiry_frontier_test.go`.
- The keyspace after a batched retirement equals the per-row result. The
  tests compare it key by key against a verbatim copy of the v0.5.12
  functions, including restored rows, idempotency and dedupe mappings,
  delayed, in-progress and failed rows, and a skewed stored count.
- There is no wire, storage format, configuration or metric change. Followers
  apply the same raw batch, so a rolling upgrade is safe. Rollback to v0.5.12
  restores per-row retirement and its latency.
- Not changed: `requeueExpired` still deletes stale in-progress keys and
  requeues expired leases one commit at a time. It runs at most every 500 ms
  per queue, only over in-progress tasks, and expired absolute rows there are
  bounded by the in-flight count, not the backlog. Batching it means
  deferring a delete that a concurrent claim could make stale, so it needs
  its own change.

### Known trade-offs and follow-ups (review of 9af454b)

- **Retirement after the lease delays the reply.** After the live task's lease
  commits, `Claim` and `ClaimMany` wait for `absoluteMu` and one retirement
  commit before they return the task. `sweepAbsolute`, enqueues and acks also
  take `absoluteMu`.
  - Absolute leases are capped at the task's deadline. If the caller gives up
    during that wait, a frontier task can expire while leased to nobody.
  - This is still far shorter than v0.5.12, which made one commit per expired
    head before leasing anything.
  - Follow-up (v0.5.14): after the lease, take `absoluteMu` with `TryLock`
    (republishing the held hints when it is busy), or retire in a detached
    `runOpen` task.
- **The retirement epoch check is sampled per retirement.** It does not wait
  for the new leader's Barrier. `Enqueue`'s count check has the same race, so
  this is not a regression. Follow-up: require the rebuild epoch.
- **A row that can never stage still fails a claim that leases nothing.** The
  same happens to `moveDueDelayedForTenant` on a corrupt delayed row, as in
  v0.5.12.
- **`requeueExpired` and `discardPendingHint` still commit once per row.** They
  are bounded by tasks in flight or by stale hints.
- **Stale comment.** The comment at `internal/raft/db.go` near
  `raftMergeBatch` still says 32; the constant is 128.

## Alternatives considered

- **One combined batch for retirement and the lease.** It would save one more
  round trip. But `prepareAbsoluteBatch` reserves the leased row's identity
  from the stored count, which would overwrite the retirement's count. Proving
  that safe needs a joint accounting path. Two commits meet the bound.
- **Skip expired hints and leave them to the reaper.** Claims would be fast,
  but the hints would be dropped with the pending keys still durable, and the
  reaper's own per-row sweep was part of the problem.
- **Raise the leader's CPU request.** It treats the symptom. Each commit stays
  serial, and the cost policy does not permit it as a fix.

## References

- ADR 0010 (absolute task expiry), ADR 0012 (acknowledged absolute tasks).
- `internal/repository/pebble/absolute_expiry.go` (`purgeAbsoluteRows`,
  `absoluteRetirement`, `sweepAbsolute`) and `task_repository.go` (`Claim`,
  `retireHeld`, `ClaimMany`, `moveDueDelayedForTenant`, `dropExpiredBodies`).
- Tests: `internal/repository/pebble/claim_expired_batch_test.go`,
  `claim_expiry_frontier_test.go`; benchmark
  `claim_expired_batch_bench_test.go`.
- Installation notes: `code-cloud-deployment`
  `docs/code-logs-platform-sources.md` (2026-10-21, v0.5.7) already recorded
  serialized absolute-expiry commit latency.
