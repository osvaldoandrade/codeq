# Sharding

codeq supports two flavours of sharding, both single-binary, both
without external coordination:

| Flavour | Scope | When to reach for it | Reference |
|---|---|---|---|
| Intra-process (Phase 8) | One codeq process opens N independent Pebble stores | Single-node throughput hitting the commit pipeline ceiling on a multi-core box | [08b-pebble-sharding-internals.md](./08b-pebble-sharding-internals.md) |
| Multi-node cluster | N codeq processes joined by a consistent-hash ring | Single-machine resources exhausted; need horizontal scale | [05-cluster-architecture.md](./05-cluster-architecture.md) and [19b-cluster-grpc-protocol.md](./19b-cluster-grpc-protocol.md) |

The two are **mutually exclusive** in the current release: a codeq node
running cluster mode does not also intra-process-shard. The startup
path in `pkg/app/application_pebble.go` rejects `numShards > 1` with
`cluster.enabled=true`. Pick the one that matches the bottleneck.

## Picking a flavour

- If `internal/bench/profile_full_cycle_test.go::TestProfile_FullCycle`
  hits the commit/compaction wall on one box (typical at ≥ 40k tasks/s
  on single-shard Pebble), increase `numShards` first. Sweet spot on a
  12-core box is 4 (`83,420 tasks/s` in the bench).
- If a single box is CPU-, memory-, or storage-bound, move to cluster
  mode. Cluster mode hashes task IDs to nodes and gossips per-node
  bloom filters so cross-node `Get` short-circuits when the ID is
  definitely absent.

## Routing

Both flavours route by **task ID hash**:

- Intra-process: `fnv1a64(task_id) % numShards`, computed in
  `internal/repository/pebble/sharded_task_repository.go`.
- Cluster: `xxhash(task_id)` over 256 virtual nodes on the ring,
  computed in `internal/cluster/ring.go`.

Tenant ID is **not** part of the routing key in either flavour — task
keys carry the tenant prefix (`codeq/q/<cmd>/<tenant>/...`) but the
shard/node placement is task-ID-derived. That keeps a noisy tenant
from concentrating load on a single shard.

## Migration

- Adding shards (Phase 8): drain + reseed. The current implementation
  has no online resharding; see [32-shard-migration-guide.md](./32-shard-migration-guide.md).
- Adding cluster nodes: rolling restart with updated `cluster.peers`;
  the ring redistributes virtual nodes automatically.

## Historical note

Earlier releases of codeq exposed a `ShardSupplier` interface intended
for Redis-protocol multi-instance sharding. That interface remains in
`pkg/domain/shard.go` for backwards compatibility but is no longer the
recommended sharding strategy — Pebble Phase 8 and cluster mode have
both replaced it.

## See also

- [08b-pebble-sharding-internals.md](./08b-pebble-sharding-internals.md)
- [05-cluster-architecture.md](./05-cluster-architecture.md)
- [19b-cluster-grpc-protocol.md](./19b-cluster-grpc-protocol.md)
- [32-shard-migration-guide.md](./32-shard-migration-guide.md)
