# 3-node codeq cluster with raft replication

Spins up three codeq processes joined into a raft cluster. Every Pebble
write is consensus-replicated across the three replicas; the leader
fails over automatically when a node dies.

The template uses the **mux transport** (`RAFT_MUX_ENABLED=true`) so
every shard's raft group shares one TCP port per node — only port
8080 (HTTP) and 7000 (raft) are exposed on each container, regardless
of how many shards are configured.

It also wires `RAFT_PEER_HTTP_ADDRS` so server-side 307 redirects work
out of the box: a write that lands on a follower comes back as
`307 Temporary Redirect` with `Location: <leader URL>`. Standard HTTP
clients follow automatically.

See [`docs/40-raft-replication.md`](../../../docs/40-raft-replication.md)
for the architecture, configuration knobs, status endpoint, and current
limitations.

## Quick start

```bash
# Bring the cluster up. The compose file defaults to
# ghcr.io/osvaldoandrade/codeq-service:latest, pulled automatically.
# node-a bootstraps; node-b and node-c join via raft replication of
# the initial configuration.
docker compose -f deploy/docker-compose/raft-cluster/compose.yaml up -d

# To run against a locally-built image instead:
#   docker build -f deploy/docker-compose/cluster/Dockerfile -t codeq-service:local .
#   CODEQ_IMAGE=codeq-service:local docker compose -f ... up -d

# All three nodes are reachable:
curl -s http://localhost:8080/v1/codeq/raft/status | jq .   # node-a
curl -s http://localhost:8081/v1/codeq/raft/status | jq .   # node-b
curl -s http://localhost:8082/v1/codeq/raft/status | jq .   # node-c
```

The status endpoint reports which node is leader for each raft group.
With the default single-shard config there is exactly one group.

## Producing tasks

Submit a task to the leader and watch it replicate:

```bash
curl -s -X POST http://localhost:8080/v1/codeq/tasks \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"command":"GENERATE_MASTER","payload":{"k":"v"},"priority":5}'

# Read the same task back from node-b (local read, replicated state):
curl -s http://localhost:8081/v1/codeq/tasks/<id> \
  -H 'Authorization: Bearer dev-token' | jq .
```

If you hit a follower with the write, you'll get `400 not leader` —
client retries on a different node (`http://localhost:8081` /
`:8082`) until it finds the leader. Future server-side forwarding will
make this automatic; today the client retries.

## Testing failover

```bash
# Find the current leader.
curl -s http://localhost:8080/v1/codeq/raft/status | jq '.groups[] | select(.isLeader)'

# Kill it.
docker compose -f deploy/docker-compose/raft-cluster/compose.yaml stop node-a

# Wait ~2 seconds (default election timeout).
sleep 2

# The surviving nodes elected a new leader.
curl -s http://localhost:8081/v1/codeq/raft/status | jq .

# Submit a write against the new leader; it lands and replicates.
```

## Going multi-shard (M2)

For per-shard raft groups (one group per Pebble shard), add
`"numShards": 4` to `PERSISTENCE_CONFIG`. With the mux transport
already enabled (`RAFT_MUX_ENABLED=true`), no port-range changes are
needed — every shard's raft group shares port 7000, demuxed by group
ID prefix. The 12 raft groups (3 nodes × 4 shards) come up on just
3 listeners total.

## Mutual exclusion

Raft mode is mutually exclusive with the legacy static-ring cluster
mode (`CLUSTER_ENABLED=true`). Don't enable both — startup will
reject the config.
