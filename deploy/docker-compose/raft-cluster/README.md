# 3-node codeq cluster with raft replication

Spins up three codeq processes joined into a raft cluster. Every Pebble
write is consensus-replicated across the three replicas; the leader
fails over automatically when a node dies.

See [`docs/40-raft-replication.md`](../../../docs/40-raft-replication.md)
for the architecture, configuration knobs, status endpoint, and current
limitations.

## Quick start

```bash
# Build the image once (any image with codeq compiled in works).
docker build -f deploy/docker-compose/cluster/Dockerfile -t codeq-service:cluster .

# Bring the cluster up. node-a bootstraps; node-b and node-c join via
# raft replication of the initial configuration.
docker compose -f deploy/docker-compose/raft-cluster/compose.yaml up -d

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
`"numShards": 4` to `PERSISTENCE_CONFIG` and reserve N consecutive
ports per node. The compose template uses container-local port 7000 as
the base; each shard binds 7000 + shardIdx. Adjust as needed.

## Mutual exclusion

Raft mode is mutually exclusive with the legacy static-ring cluster
mode (`CLUSTER_ENABLED=true`). Don't enable both — startup will
reject the config.
