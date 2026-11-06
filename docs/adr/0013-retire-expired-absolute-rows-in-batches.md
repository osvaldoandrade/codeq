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
  read once and carried across the staged rows (`+1` for a restored row,
  `-1`, clamped at zero, for an identity whose replay window closed), so the
  stored count ends where the per-row path left it. Each row is staged in a
  scratch batch, so a failing row adds nothing. Staging stops at the first
  error. Rows staged before it commit and the error is returned, as with the
  per-row path.
- `Claim` holds the hints of expired rows in flight instead of purging them
  one by one. Before it commits a live task's lease, the held rows retire in
  one batch. A claim that meets N expired heads before a live task commits
  twice (retirement, then lease), not N+1 times. If it finds no live task, or
  fails or is canceled, the held rows still retire when it returns. That
  commit does not depend on the request context, so a client that gave up
  still leaves progress behind. A held hint whose row did not retire (failed
  commit, staging error, or a row no longer expired) is republished, as a
  failed per-row purge was.
- `ClaimMany`, `moveDueDelayedForTenant`, the reaper lease sweep and
  `dropExpiredBodies` collect their expired rows and call `purgeAbsoluteRows`
  once. `ClaimMany` and the delayed move no longer commit an empty batch when
  every row was expired.
- `sweepAbsolute` stages its page into the same batched retirement: one
  commit per 256 identities, and `absoluteMu` is held for one or two commits
  instead of hundreds.

## Consequences

- Claim latency behind an expired backlog no longer scales with the backlog.
  Benchmark `BenchmarkClaimBehindExpiredHeads` (200 expired heads, then a
  live task; Apple M4 Max): standalone Pebble with fsync went from about
  840 ms to 11 ms. A replicator charging 2 ms per commit went from 1.6–2.0 s
  to 17 ms, and from 201 to 2 commits.
- One Raft entry carries up to 256 retirements. A measured 200-row
  retirement replicates 117 KB (about 600 bytes per row), so the largest
  entry is about 150 KB. The Raft apply loop already merges up to 32
  submitter batches into one entry.
- Expired hints stay in flight a little longer, until the claim's retirement
  commit. Another claimer that pops a duplicate hint for the same ID drops
  it, as it already did for any in-flight hint. The holder republishes it if
  the row does not retire.
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
- Tests: `internal/repository/pebble/claim_expired_batch_test.go`; benchmark
  `claim_expired_batch_bench_test.go`.
- Installation notes: `code-cloud-deployment`
  `docs/code-logs-platform-sources.md` (2026-10-21, v0.5.7) already recorded
  serialized absolute-expiry commit latency.
