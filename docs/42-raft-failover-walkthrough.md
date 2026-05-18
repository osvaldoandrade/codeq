# Raft failover walkthrough

This is the operational companion to [`40-raft-replication.md`](./40-raft-replication.md).
That doc describes the architecture; this one shows what failover
actually looks like from outside the cluster — the commands you type,
the JSON you see, the seconds you wait. The scenario is the textbook
one: the raft leader dies, the surviving followers run an election,
clients keep submitting tasks.

We use the `deploy/docker-compose/raft-cluster/` template so the steps
are reproducible on any developer box. All ports below are container
host-mapped: HTTP on `:8080`, `:8081`, `:8082`; raft mux on `:7000`
inside each container (not published — peers talk on the docker bridge
network).

## 1. Setup

```bash
# Build the image (once).
docker build -f deploy/docker-compose/cluster/Dockerfile -t codeq-service:cluster .

# Bring the cluster up.
docker compose -f deploy/docker-compose/raft-cluster/compose.yaml up -d
```

Three codeq processes start: `node-a`, `node-b`, `node-c`. Only
`node-a` has `RAFT_BOOTSTRAP=true` — the other two come up with the
flag false and wait for AppendEntries from a leader. The template
defaults to a single Pebble shard, so there is exactly one raft group
covering the whole keyspace; with `numShards: N` in
`PERSISTENCE_CONFIG` you'd see N groups, each with its own leader.

After ~2 seconds, `node-a` has bootstrapped and won the initial
election. Query any node's status endpoint to confirm:

```bash
curl -s http://localhost:8080/v1/codeq/raft/status | jq .
```

```json
{
  "enabled": true,
  "numGroups": 1,
  "groups": [
    {
      "shardIdx": 0,
      "isLeader": true,
      "selfId": "node-a",
      "selfAddr": "node-a:7000",
      "leaderId": "node-a",
      "leaderAddr": "node-a:7000",
      "leaderHTTPAddr": "http://node-a:8080",
      "hasLeader": true
    }
  ]
}
```

The `leaderHTTPAddr` comes from `RAFT_PEER_HTTP_ADDRS` in the compose
file. It's how followers know where to redirect writes. Implementation:
`pkg/config/config.go` (`RaftConfig.PeerHTTPAddrs`) and
`internal/controllers/raft_status_controller.go:18`.

## 2. Submit a task — leader path

Send a write to the leader (`:8080`):

```bash
curl -s -X POST http://localhost:8080/v1/codeq/tasks \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"command":"GENERATE_MASTER","payload":{"k":"v"},"priority":5}' | jq .
```

```json
{
  "id": "7e84c2c0-fc9f-4cbd-9e2a-1a0b1c2d3e4f",
  "status": "PENDING",
  "command": "GENERATE_MASTER",
  "priority": 5,
  "attempts": 0
}
```

The write went through `raft.Apply` on `node-a`, was replicated to
`node-b` and `node-c` via AppendEntries, and committed when a majority
(2 of 3) acked. Now read the task back from a follower:

```bash
curl -s http://localhost:8081/v1/codeq/tasks/7e84c2c0-fc9f-4cbd-9e2a-1a0b1c2d3e4f \
  -H 'Authorization: Bearer dev-token' | jq .id
```

```json
"7e84c2c0-fc9f-4cbd-9e2a-1a0b1c2d3e4f"
```

The follower returns the task without contacting the leader — reads
are local on every node, served straight out of that node's Pebble.
That's the design point in `40-raft-replication.md` § Leadership:
"Reads are local on every node, with no consensus round; followers may
serve stale data." Stale-tolerant reads scale linearly with replicas.

## 3. Submit a task — follower path with 307 redirect

Now try a write against a follower (`:8081`). Use `-i` to see the
response headers:

```bash
curl -i -s -X POST http://localhost:8081/v1/codeq/tasks \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"command":"GENERATE_MASTER","payload":{"who":"follower"}}'
```

```
HTTP/1.1 307 Temporary Redirect
Location: http://node-a:8080/v1/codeq/tasks
Content-Type: application/json

{"error":"not leader","leader":"http://node-a:8080"}
```

The follower didn't try to forward the write. It returned `307` with
`Location` pointing at the leader's HTTP base URL. Per RFC 7231,
clients re-send the same method and body to the new URL (unlike the
older 302 which historically degraded POST to GET). Go's `net/http`
follows automatically when the request body is replayable
(`bytes.Reader`, `strings.Reader`, or `GetBody`); curl follows with
`-L`.

Implementation:
- `internal/controllers/respond.go:34` (`maybeRedirectLeader`)
- `pkg/domain/errors.go:11` (`LeaderHint` interface, satisfied by raft
  errors that carry a leader URL hint)

With `-L`, curl follows the redirect transparently:

```bash
curl -L -s -X POST http://localhost:8081/v1/codeq/tasks \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"command":"GENERATE_MASTER","payload":{"who":"follower"}}' | jq .id
```

```json
"3d51f8ea-1234-4abc-9def-0123456789ab"
```

Note: the compose template's `Location` URL uses the container
hostname (`node-a:8080`), only resolvable from inside the docker
bridge. From the host curl sees the redirect but can't reach it — a
deploy artifact, not a protocol issue. Real deployments set
`RAFT_PEER_HTTP_ADDRS` to externally-reachable URLs.

## 4. Kill the leader

Identify the current leader:

```bash
curl -s http://localhost:8080/v1/codeq/raft/status \
  | jq -r '.groups[] | select(.isLeader) | .selfId'
# node-a
```

Stop it:

```bash
docker compose -f deploy/docker-compose/raft-cluster/compose.yaml stop node-a
```

The two survivors form a majority of the 3-node cluster, and raft
guarantees one of them holds the most up-to-date log — so one will
win the next election. A partitioned (rather than killed) ex-leader
would discover on rejoin that the cluster moved to a higher term and
step down: no split-brain.

## 5. Election timeline

With the library defaults from `internal/raft/db.go:53-97`
(`HeartbeatMS=1000`, `ElectionMS=1000`, `LeaderLeaseMS=500`), the
sequence of events after killing the leader is:

| Time | Event |
|---|---|
| `t = 0.0s` | `node-a` process killed. Followers still consider it leader (lease held). |
| `0 → heartbeatMS` | Followers stop receiving AppendEntries. Heartbeat timer ticks toward expiry. |
| `~heartbeatMS` | Heartbeat timeout fires on followers. Leader lease (`LeaderLeaseMS = 500ms`) expires. |
| `~heartbeatMS + jitter` | One follower transitions Follower → Candidate, increments `currentTerm`, votes for itself, broadcasts `RequestVote` RPCs. The jitter (random fraction of `ElectionMS`) prevents two followers from becoming candidates simultaneously and split-voting. |
| `~heartbeatMS + RTT` | Other follower grants the vote (it has no higher-term candidate to prefer). Candidate has 2 votes ≥ majority of 3 → wins. |
| `~heartbeatMS + RTT` | Winner transitions Candidate → Leader, broadcasts an empty `AppendEntries` (heartbeat) to claim leadership. |
| `~heartbeatMS + RTT + small` | Clients hitting any node now see the new `leaderId` in `/v1/codeq/raft/status`. Writes against the old leader's URL fail (process down); writes against followers get a 307 to the new leader. |

End-to-end with defaults: ~1-3 seconds of write unavailability. For
faster failover in tests, drop the values to e.g. 200ms/200ms via
`RAFT_HEARTBEAT_MS` / `RAFT_ELECTION_MS`. Trade-off: tighter timeouts
trigger more spurious elections under transient network hiccups, so
production usually keeps the 1-second defaults and budgets the wait
on the client side.

Reads are unaffected throughout this window — `node-b` and `node-c`
keep serving GETs from their local Pebble.

## 6. Observe the new leader

```bash
curl -s http://localhost:8081/v1/codeq/raft/status \
  | jq '.groups[] | {selfId, isLeader, leaderId, leaderHTTPAddr}'
```

```json
{
  "selfId": "node-b",
  "isLeader": true,
  "leaderId": "node-b",
  "leaderHTTPAddr": ""
}
```

(`leaderHTTPAddr` is empty on a node that is *itself* the leader — the
field is for redirect hints to other peers.)

Submit a fresh write to confirm the cluster is writable again:

```bash
curl -s -X POST http://localhost:8081/v1/codeq/tasks \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"command":"GENERATE_MASTER","payload":{"after":"failover"}}' | jq .id
```

```json
"a1b2c3d4-5678-49ab-bcde-f01234567890"
```

`node-c` is still a follower:

```bash
curl -s http://localhost:8082/v1/codeq/raft/status \
  | jq '.groups[] | {selfId, isLeader, leaderId}'
```

```json
{
  "selfId": "node-c",
  "isLeader": false,
  "leaderId": "node-b"
}
```

If you POST to `:8082` now, you'll get a 307 to `http://node-b:8080`.

## 7. Restart the dead node

```bash
docker compose -f deploy/docker-compose/raft-cluster/compose.yaml start node-a
```

What happens internally:

1. `node-a` starts; raft reads its persisted log + stable state from
   the Pebble store under `/var/lib/codeq/pebble/` (logs under the
   `raft/log/` key prefix, see `internal/raft/log_store.go`).
2. It tries to resume as leader at its old term. The first
   `AppendEntries` from `node-b` (the current leader) carries a higher
   term, so `node-a` immediately steps down to follower and adopts the
   new term.
3. Raft replays any log entries `node-a` missed during downtime. Short
   downtime → entries shipped via `AppendEntries`. Long downtime
   beyond the leader's log retention → leader sends `InstallSnapshot`
   to ship a Pebble snapshot of the FSM state (see
   `internal/raft/snapshot.go`, snapshot threshold is `SnapshotEntries
   = 8192` by default — `internal/raft/db.go:85-90`).
4. Once `node-a`'s `commitIndex` matches the leader's, it's a healthy
   follower again — serving local reads, voting in future elections.

Verify:

```bash
curl -s http://localhost:8080/v1/codeq/raft/status \
  | jq '.groups[] | {selfId, isLeader, leaderId}'
```

```json
{
  "selfId": "node-a",
  "isLeader": false,
  "leaderId": "node-b"
}
```

`node-a` is back, sees `node-b` as leader, no longer claims
leadership. The cluster is back to 3 healthy replicas.

## 8. Failure modes that don't recover automatically

Honest accounting:

- **Two nodes down (loss of majority quorum)**. The survivor cannot
  elect itself — raft requires `⌈N/2⌉+1 = 2` of 3. Writes fail until
  one of the others restarts. Reads still work on the survivor (stale-
  tolerant). Canonical raft availability trade for CP behavior.

- **Symmetric network partition (1 vs 2)**. The lone node can't reach
  a quorum, so its election attempts fail. The 2-node side elects a
  leader and accepts writes. On heal, the isolated node receives a
  higher-term `AppendEntries`, drops any uncommitted entries, and
  catches up. Majority quorum is what prevents split-brain.

- **All three nodes down**. Data is durable on every node's Pebble
  store. Starting one node back gives a 1-node cluster with no leader.
  Once a second comes up they elect together; the third joins on
  restart. No data-loss path unless disks are also lost.

- **Disk loss on one node**. The damaged node's raft log + Pebble
  state are gone. Fix: wipe its data directory
  (`docker volume rm codeq-raft_node-a-data`) and restart with
  `RAFT_BOOTSTRAP=false`. Raft sees an empty log and receives
  `InstallSnapshot` from the leader.

- **All disks lost**. No recovery — Pebble is the source of truth.
  Backups (Pebble checkpoint API, filesystem snapshots) are an
  operator concern; codeq does not currently ship a managed backup
  tool.

## 9. What clients should do

**HTTP clients**:
- Follow 3xx redirects. Go's `http.Client` does this by default (up to
  10 hops); pass `-L` to curl. Redirect is 307 so POST bodies replay
  per RFC 7231.
- Optionally cache the resolved leader URL between calls to skip the
  redirect round-trip.

**gRPC streaming clients** (`pkg/producerclient`, `pkg/workerclient`):
- gRPC has no 307 equivalent. A stream against a non-leader returns
  `pebble: not leader` at the application layer.
- On `not leader`: close the stream, reconnect to a different node
  from the member list, retry with the project's standard backoff. In
  multi-shard raft a single node leads only some shards, so the same-
  tenant traffic naturally fans out.
- Under multi-shard + high concurrency, `not leader` is a normal
  background condition — a routing hint, not a failure.

Server-side gRPC forwarding (where a follower transparently proxies
the request to the leader) is on the roadmap but not yet implemented;
see [`40-raft-replication.md`](./40-raft-replication.md) §
"What's NOT covered yet".

## See also

- [`40-raft-replication.md`](./40-raft-replication.md) — architecture,
  config knobs, mux transport, mutual exclusion with the legacy
  cluster mode
- [`29-operational-runbooks.md`](./29-operational-runbooks.md) — other
  ops procedures (backups, snapshots, capacity)
- [`05-cluster-architecture.md`](./05-cluster-architecture.md) — the
  legacy static-ring mode raft replaces
- [`deploy/docker-compose/raft-cluster/README.md`](../deploy/docker-compose/raft-cluster/README.md)
  — the compose template used in this walkthrough
