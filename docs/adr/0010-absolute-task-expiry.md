# ADR 0010: Opt-in absolute task expiration (CFP-115)

Status: implementation authorized; deployment and activation pending.

## Context and constraints

Managed Logs batches need an immutable accepted time and deadline. Legacy task
retention deletes only terminal task bodies and renews its index on lease/result
activity. It does not bound queued payloads, results or DLQ references. This work
is T4: a hot-path, persistence and additive protocol change spanning HTTP, stream,
shard and Raft boundaries. The root ADR-0023 owns the product decision and its
ordered architecture/contract/security/migration/operations design approvals.
No production mutation, deployment or change to ordinary topic behavior occurs.

## Decision and written implementation plan

Add `Policy.absoluteExpiry` (default false), optional monotonic
`Policy.visibilityFloor` and nullable `Task.expiresAt`. Absolute topics require
an explicit RFC3339Nano deadline at ingress, bounded by current retention.
Resolve policy from the canonical topic catalog, shared across local shards.
Only an absolute task uses strict content checks on named-ID retries: identical
content returns its original task metadata; different command/content conflicts.
Legacy task IDs, renewals and cleanup retain their existing contract.

Pass requested expiration through a typed request context and explicit additive
gRPC fields, preserving existing Go interfaces and old wire field numbers.
Before an absolute task write commits, revalidate its deadline and current
policy; keep its fixed expiry TTL entry and reverse references in the same
replicated batch. A bounded reaper removes expired payload, result, lease and
all queue/TTL/idempotency/deduplication references, regardless of task state.
Reads and claims enforce visibility immediately; physical cleanup is observed
and asynchronous. Policy shortening uses the minimum deadline and visibility
floor. Increasing retention cannot extend an existing task's own expiration.

Affected files: domain task/context; QueueTopic policy and topic store; HTTP
create/batch; producer/worker/cluster protobufs and codecs; Pebble task/result,
batch guard/reaper; runtime catalog wiring; memory plugin where its contract
applies; focused repository, HTTP, restart, concurrency and protocol tests.

## Verification before release

Establish HTTP/persistence RED before behavior edits. Run targeted `go test` for
domain, topic, Pebble, controllers, streams/cluster and application integration;
then `go test -short -race ./...`, `go vet ./...`, `go build ./...`. Use real
temporary Pebble stores, process restart and the existing real Raft test harness.
Exercise pending/delayed/in-progress/completed/failed, heartbeat/result/requeue,
same-ID replay, tenant isolation, floor reduce/increase, malformed deadlines,
concurrent cleanup/result commits, and legacy compatibility. Record outcomes
outside this ADR; an unexecuted command is not a pass.

## Replay boundary and operational budget

After removing every trace of an ID, a later request proposing a new deadline
cannot be distinguished from a new task. Permanent no-resurrection therefore
requires retained non-payload identity metadata or a publisher that always
reuses the original absolute deadline. Keep a tombstone for **48 hours after
actual retirement**, retaining only tenant, ID, topic, content hash, original
creation/expiry and retirement metadata. Purge payload, result and references
at retirement; never extend the tombstone on a retry. This is a bounded replay
guarantee, not permanent deduplication. The managed Logs ingress separately
rejects newly collected CRI records older than 24 hours or over 5 minutes in the
future; accepted batches may process until their original deadline.

Reserve a metadata identity slot with initial acceptance, so cleanup cannot be
blocked by a full tombstone budget. The default operational budget is 1,000,000
identities **per local shard**, configurable through
`absoluteExpiryMaxIdentitiesPerShard` /
`ABSOLUTE_EXPIRY_MAX_IDENTITIES_PER_SHARD`. It is not a product quota or a claim
that a given daily byte volume fits. Expose usage/capacity/refusal/purge metrics;
refuse a new identity with actionable HTTP 429 and `Retry-After: 30` at capacity.
Existing accepted replays and legacy work remain available. Topic disable/delete
returns 409 until all active tasks and tombstones drain. Check every local
shard; unknown Raft leadership/custody fails closed.

## Durable acknowledgement

The existing `persistenceConfig.fsyncOnCommit` setting now controls actual
standalone writes and Raft log, stable state, FSM commits and snapshot restores.
Its default stays false for legacy installations. **Absolute expiry requires
true**, both at topic activation and task acceptance. Non-durable activation
returns the existing validation status 422. Every Raft voter must run the
qualified capable version with true before activation; local configuration
does not remotely attest other voters. The memory plugin refuses expiring tasks.

Acceptance waits for the storage commit/quorum and local durable FSM apply.
A same-ID retry also commits an opaque durable fence before returning its old
receipt; a prior failed commit may have left visible but unacknowledged memory
state. Pebble fatal Sync errors terminate the process before acknowledgement.
Do not claim power-loss protection beyond the filesystem/storage fsync contract.

## Compatibility and rollback

Expansion is opt-in: old topics and task JSON without the field are unchanged;
no historical backfill. Deploy protocol-capable readers on every peer, then
enable fsync everywhere before enabling Logs topics. The unreplicated legacy
multi-process Cluster topology is not qualified for global topic retirement;
managed Logs uses standalone or the qualified Raft/sharded installation.
Disable collection first on rollback. Do not roll an active absolute queue back
to an expiry-unaware writer or turn off fsync; retain capable durable writers
and cleanup until tasks and the explicit 48-hour tombstone window have drained.
Never delete unrelated data or clear indexes as a rollback. Independent review,
measured load and end-to-end retention acceptance remain release obligations.
