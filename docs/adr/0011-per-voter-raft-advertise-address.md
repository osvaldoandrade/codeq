# ADR 0011: Every Raft voter advertises its own peer address (CFP-070)

- **Status**: Accepted
- **Date**: 2026-10-28
- **Deciders**: Osvaldo Andrade (owner go, 2026-10-28: "resolvemos?")
- **Tracks**: CFP-070 CODE Logs incident of 2026-10-28

## Context

With `RAFT_MUX_ENABLED=true` every shard's Raft group shares one listener. The
CODE agent renders `RAFT_BIND_ADDR=:7000` and per-voter peers
(`codeq-N=codeq-N.codecloud-queue.svc.cluster.local:7000`). The mux
`StreamLayer.Addr()` returned the listener address, so every voter's
`hraft.NetworkTransport` advertised the same bytes, `[::]:7000`, in every RPC
header.

hashicorp/raft v1.7.3 persists a vote as the candidate's advertised address
bytes (`keyLastVoteCand`) and, for a second request in the same term, grants it
again when those bytes match ("duplicate requestVote from", `raft.go`
`requestVote`). With identical addresses a voter that already voted, even for
itself, also grants a competing candidate. Two concurrent candidates therefore
both reach a majority, and one term elects two leaders. The "reject votes while
a leader is known" guard compares addresses too, so it never fired.

Production evidence (Cloud Logging, `codecloud-queue`): hundreds of
"duplicate requestVote from" grants since 2026-10-21. In terms 11, 32, 153, 477,
493, 519 and 526 two voters entered leader state; in term 526 codeq-0 and
codeq-2 each won with tally 2. Two leaders in one term can append different
entries at the same index and term, which Raft's log matching then treats as
identical. That is an election-safety violation that can make replicas diverge.
Tests never hit it because they bind each voter to its own `127.0.0.1:port`.

## Decision

- `raft.NewMuxAcceptorAdvertising(bind, advertise, log)` keeps listening on the
  bind address. Every registered group advertises `advertise`, so the transport
  advertises it too (`StreamLayer.Addr()` → `MuxAcceptor.AdvertiseAddr()`).
  `NewMuxAcceptor` keeps the listener address (single voter, tests).
- In a group of more than one voter, `pkg/app` passes `Peers[SelfID]` as the
  advertise address. This is the address the other voters already dial, and it
  is in the replicated configuration. A single voter has no competing candidate
  and keeps the listener address, whatever its own peers entry says.
- An advertise address must be `host:port` with a non-wildcard host and a port
  from 1 to 65535 (`raft.ValidateAdvertiseAddr`). The check runs before
  listening.
- `Config.Validate` fails closed for a group of more than one voter whose peer
  addresses are not `host:port`, use a wildcard or empty host, have an invalid
  port, or share an address (compared case-insensitively).
- The startup log `raft replication enabled` reports `advertiseAddr`.

## Consequences

- One vote per voter per term again. Followers record the leader's real peer
  address, so the "rejecting vote request since we have a leader" guard in
  hashicorp/raft works as designed.
- No wire, storage or configuration-entry change. Peers are still dialed at the
  configured addresses. Persisted votes recorded under `[::]:7000` only affect
  the term in which they were cast; a later grant in that term to a fixed
  candidate is refused, which is the safe direction.
- Rolling upgrade: an upgraded voter is distinct from every other voter. Until
  the last voter restarts, two old voters can still double-vote, which is no
  worse than before. Rollback is the previous image and brings the defect back
  unchanged.
- Divergence that already happened is not repaired by this change. Most queue
  state is short-lived: tasks are claimed in seconds, and results and absolute
  identities expire within 48 hours. Long-lived catalog entries (topics,
  schedules) are not proven identical across voters. A replica consistency
  check is a separate follow-up.
- `/v1/codeq/raft/status` `leaderAddr` now shows the leader's peer name instead
  of `[::]:7000`; `selfAddr` stays the bind address. No consumer reads either
  field.

## Alternatives considered

- **Bind each voter to its pod IP:** the IP changes on reschedule and would
  leave the replicated configuration, which uses stable Service names.
- **A separate `RAFT_ADVERTISE_ADDR` variable:** it duplicates `Peers[SelfID]`,
  and the two could disagree; it would also need an agent change before taking
  effect.
- **Patching hashicorp/raft to record vote IDs:** it forks a consensus library,
  and other address comparisons would still be wrong.

## References

- hashicorp/raft v1.7.3 `raft.go` `requestVote` (vote persisted and compared by
  `RPCHeader.Addr`), `net_transport.go` `LocalAddr`.
- Root workspace `docs/rollouts/managed-logs-code-first.md` (2026-10-28
  checkpoint) and ADR-0025.
