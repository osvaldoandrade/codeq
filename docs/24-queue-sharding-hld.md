# Queue Sharding: High-Level Design and RFC

## Overview

This document describes the technical design for implementing horizontal queue sharding in codeQ to overcome single-node KVRocks scalability limits. The design considers three architectural options: vertical scaling only, master-replica per tenant, and RAFT-based distributed consensus. It proposes a phased approach that begins with client-side sharding and establishes a path toward RAFT-based coordination when KVRocks matures.

## Current Architecture and Limitations

### Existing System

CodeQ stores all task queues and state in a single KVRocks instance. Each queue operation—enqueue, claim, NACK—executes against this single storage node. The system achieves multi-tenancy by using tenant-namespaced keys (e.g., `codeq:q:{command}:{tenantID}:pending:{priority}`), but all tenants share the same physical KVRocks instance.

KVRocks implements the Redis protocol on top of RocksDB, providing persistent queues with atomic single-key operations. Each command is linearizable, but multi-key workflows (such as moving a task between queues) rely on Lua scripts for atomicity. The architecture trades strict ordering for availability and throughput, delivering at-least-once semantics without global coordination.

### Scalability Bottlenecks

Single-node KVRocks imposes three critical constraints:

**Memory capacity**: RocksDB's block cache and write buffers share a fixed memory allocation. Large working sets cause cache evictions, increasing read amplification and degrading latency. Current deployments allocate 8-16 GB per instance, limiting total queue depth to millions of tasks before memory pressure causes performance collapse.

**CPU throughput**: All queue operations contend for CPU on a single machine. Even with concurrent workers, enqueue, claim, and background compaction compete for processing cycles. At sustained rates above 1000 operations per second, CPU saturation introduces queueing delays in KVRocks itself, cascading latency to API responses.

**Storage I/O**: RocksDB background compactions and write-ahead log (WAL) flushes share disk bandwidth. Write-heavy workloads trigger frequent compactions, increasing read and write amplification. Single-disk configurations cannot scale beyond the I/O capacity of one storage device, even with SSDs.

### Impact on Production

Without sharding, production deployments face two scaling paths:

**Vertical scaling**: Upgrading to larger instances increases memory, CPU, and I/O capacity but hits diminishing returns. A 64-core, 256 GB machine with NVMe storage costs significantly more than four 16-core, 64 GB machines with comparable aggregate capacity. Vertical scaling also creates a single point of failure with no graceful degradation.

**Regional isolation**: Deploying separate codeQ+KVRocks pairs per region or customer provides horizontal capacity but prevents cross-region task routing. A task enqueued in US-East cannot be claimed by a worker in EU-West. This approach works for isolated workloads but fails when tasks need global visibility or load balancing across geographies.

The fundamental constraint is that all state for all tenants resides on one storage node. Horizontal scaling requires distributing this state across multiple nodes while preserving atomicity and consistency guarantees.

## Problem Statement

Design a sharding architecture that allows codeQ to distribute queue state across multiple KVRocks instances while maintaining:

- Atomic claim operations within a shard
- Predictable task routing based on command or tenant
- Backward compatibility with existing single-node deployments
- A migration path that allows phased adoption without breaking existing clients

The design must address Lua script execution, Redis Cluster hash-slot semantics, and the possibility of using RAFT consensus when KVRocks gains support for distributed coordination.

## Requirements and Goals

### Functional Requirements

**Shard assignment**: Tasks must route to a specific shard based on a deterministic function of command and tenant. The routing logic must be consistent across all API servers to ensure workers query the correct shard.

**Atomic operations**: Within a shard, claim and NACK operations must remain atomic. A claim that moves a task from pending to in-progress cannot split across shards or leave the task in an inconsistent state.

**Tenant isolation**: Sharding must preserve tenant isolation. A tenant's tasks may span multiple shards, but each task belongs to exactly one shard. Cross-tenant operations are not required.

**Backward compatibility**: Existing single-shard deployments must continue to work without configuration changes. Multi-shard deployments should add configuration rather than replace existing keyspace design.

### Non-Functional Requirements

**Performance**: Sharding should increase aggregate throughput proportionally to the number of shards. A four-shard deployment should handle roughly four times the throughput of a single shard, accounting for coordination overhead.

**Operational simplicity**: Adding or removing shards should not require rewriting task data. Shard membership changes should be configuration-driven and tolerate gradual rollout.

**Failure isolation**: A shard failure should impact only tasks routed to that shard. Other shards should continue processing tasks without degradation.

### Non-Goals

**Global ordering**: Tasks across shards do not have a global sequence. FIFO ordering is preserved within a command and priority tier on a single shard, but cross-shard ordering is undefined.

**Cross-shard atomicity**: Transactions spanning multiple shards are not supported. Tasks cannot move between shards except by completing on one shard and re-enqueuing on another.

**Automatic rebalancing**: The initial design does not redistribute tasks when shard membership changes. Existing tasks remain on their current shard until completion or expiration.

## Sharding Strategies

### Hash-Based Sharding

Assign tasks to shards using a hash function applied to a routing key. The routing key could be `{command}`, `{tenantID}`, or `{command}:{tenantID}`.

**Example**: `shard = hash(command) mod N`

**Advantages**:
- Uniform distribution across shards when hash function is well-distributed
- Simple implementation with no external coordination
- Stateless routing logic that any API server can compute

**Disadvantages**:
- Changing shard count (N) invalidates all existing key mappings unless using consistent hashing
- Consistent hashing adds complexity and still requires rebalancing when membership changes
- Tenants with high task volume can overload a single shard if routing is purely by command

**Use case**: Best for workloads with many small tenants and uniform command distribution. Not suitable when a single tenant dominates throughput.

### Range-Based Sharding

Divide the routing key space into contiguous ranges and assign each range to a shard.

**Example**: Tenants A-G go to shard 0, H-N to shard 1, O-Z to shard 2.

**Advantages**:
- Predictable mapping that allows manual balancing
- Adding shards only affects boundary ranges
- Natural for alphabetically sorted tenants or time-based partitioning

**Disadvantages**:
- Requires coordination to define range boundaries
- Manual rebalancing when tenants grow unevenly
- Range boundaries become hotspots if tenant activity is not uniformly distributed

**Use case**: Suitable for systems with a small number of large tenants where manual partitioning is acceptable.

### Explicit Sharding

Maintain a mapping table that explicitly assigns each command or tenant to a shard. The mapping is stored in configuration or a coordination service.

**Example**:
```yaml
shards:
  - id: shard-0
    addr: kvrocks-0:6666
    commands: [GENERATE_MASTER]
    tenants: [tenant-a, tenant-b]
  - id: shard-1
    addr: kvrocks-1:6666
    commands: [GENERATE_CREATIVE]
    tenants: [tenant-c, tenant-d]
```

**Advantages**:
- Complete control over task placement
- Can isolate high-volume tenants onto dedicated shards
- Easy to understand and debug
- Supports gradual migration by moving one command or tenant at a time

**Disadvantages**:
- Requires maintaining and distributing the mapping configuration
- Does not scale to thousands of tenants without automation
- Manual intervention required to balance load

**Use case**: Best for systems with a small number of known workloads and explicit isolation requirements.

### Recommendation

For codeQ, we recommend **explicit sharding** as the initial implementation because:

1. CodeQ has a small number of command types (GENERATE_MASTER, GENERATE_CREATIVE) that can be explicitly assigned
2. Operators can isolate high-volume tenants onto dedicated shards without complex hashing logic
3. The explicit mapping supports backward compatibility by defaulting all traffic to a single "default" shard
4. Migration becomes declarative: move one command or tenant from shard-0 to shard-1 by updating configuration

A ShardSupplier interface will abstract the routing logic, allowing future implementations to support hash-based or range-based strategies if workload patterns change.

## Proposed Design

### Key Format

Queue keys will include an optional shard segment:

**Single-shard (current)**:
```
codeq:q:{command}:pending:{priority}
codeq:q:{command}:{tenantID}:pending:{priority}
```

**Multi-shard (proposed)**:
```
codeq:q:{shardID}:{command}:pending:{priority}
codeq:q:{shardID}:{command}:{tenantID}:pending:{priority}
```

The `{shardID}` segment is inserted after the `codeq:q:` prefix and before the command. When `shardID` is empty or omitted, the key format remains backward-compatible with the current single-shard layout.

Task records and results remain in global hashes to simplify cross-shard inspection:
```
codeq:tasks -> HASH (field=taskID, value=JSON with shardID annotation)
codeq:results -> HASH (field=taskID, value=result JSON)
```

Each task JSON includes a `shardID` field indicating which shard owns its queue state. Admin operations can inspect tasks without knowing the shard mapping.

### ShardSupplier Interface

Introduce a ShardSupplier interface that maps commands and tenants to shard connections:

```go
type ShardConfig struct {
    ID       string
    Addr     string
    Password string
    DB       int
}

type ShardMapping struct {
    Command  string
    TenantID string
    ShardID  string
}

type ShardSupplier interface {
    // GetShard returns the Redis client for the given command and tenant.
    GetShard(ctx context.Context, command string, tenantID string) (*redis.Client, string, error)
    
    // AllShards returns all shard clients for admin operations.
    AllShards() []*redis.Client
    
    // ShardID returns the shard ID for a given command and tenant without returning the client.
    ShardID(command string, tenantID string) string
}
```

**Implementation: StaticShardSupplier**

The initial implementation reads configuration from YAML:

```yaml
shards:
  - id: default
    addr: kvrocks-0:6666
    password: ""
    db: 0
  - id: shard-1
    addr: kvrocks-1:6666
    password: ""
    db: 0

mappings:
  - command: GENERATE_MASTER
    tenantID: ""
    shardID: default
  - command: GENERATE_CREATIVE
    tenantID: ""
    shardID: default
  - command: GENERATE_MASTER
    tenantID: tenant-large
    shardID: shard-1
```

Routing logic:
1. Check for exact match: `(command, tenantID)`
2. Fall back to wildcard: `(command, "")`
3. Default to `default` shard if no mapping exists

The StaticShardSupplier maintains a pool of Redis clients and returns the appropriate client based on the configuration.

### Repository Changes

Update TaskRepository methods to accept and use the ShardSupplier:

```go
type TaskRepository interface {
    Enqueue(ctx context.Context, cmd domain.Command, payload string, priority int, 
            webhook string, maxAttempts int, idempotencyKey string, visibleAt time.Time, 
            tenantID string) (*domain.Task, error)
    
    Claim(ctx context.Context, workerID string, commands []domain.Command, 
          leaseSeconds int, inspectLimit int, maxAttemptsDefault int, 
          tenantID string) (*domain.Task, bool, error)
    
    // ... other methods
}
```

Internally, each method will:
1. Call `shardSupplier.GetShard(ctx, command, tenantID)` to get the shard client
2. Execute Redis operations against that client
3. Store the `shardID` in the task JSON for future routing

Example Enqueue implementation:

```go
func (r *taskRedisRepo) Enqueue(ctx context.Context, cmd domain.Command, payload string, ...) (*domain.Task, error) {
    client, shardID, err := r.shardSupplier.GetShard(ctx, string(cmd), tenantID)
    if err != nil {
        return nil, err
    }
    
    task := &domain.Task{
        ID:       uuid.NewString(),
        Command:  cmd,
        ShardID:  shardID,
        TenantID: tenantID,
        // ... other fields
    }
    
    pendingKey := r.keyQueuePending(shardID, cmd, priority, tenantID)
    
    // Execute operations against the sharded client
    pipe := client.Pipeline()
    pipe.HSet(ctx, r.keyTasksHash(), task.ID, marshal(task))
    pipe.LPush(ctx, pendingKey, task.ID)
    // ...
    
    return task, nil
}
```

### Claim and Cross-Shard Queries

Claim operations must query all shards because a worker may subscribe to commands distributed across multiple shards:

```go
func (r *taskRedisRepo) Claim(ctx context.Context, workerID string, commands []domain.Command, ...) (*domain.Task, bool, error) {
    for _, cmd := range commands {
        // Determine which shard owns this command for this tenant
        client, shardID, err := r.shardSupplier.GetShard(ctx, string(cmd), tenantID)
        if err != nil {
            continue
        }
        
        // Attempt claim on this shard
        task, found, err := r.claimFromShard(ctx, client, shardID, workerID, cmd, tenantID, ...)
        if found {
            return task, true, nil
        }
    }
    
    return nil, false, nil
}
```

This approach increases claim latency proportionally to the number of shards queried. For most deployments with 2-4 shards, the overhead is negligible. For larger deployments, workers should subscribe to specific commands rather than wildcards to reduce cross-shard queries.

### Admin Operations

Admin cleanup and statistics must aggregate across all shards:

```go
func (r *taskRedisRepo) CleanupExpired(ctx context.Context, limit int, before time.Time) (int, error) {
    totalCleaned := 0
    
    for _, client := range r.shardSupplier.AllShards() {
        cleaned, err := r.cleanupOnShard(ctx, client, limit, before)
        if err != nil {
            // Log error but continue with other shards
            continue
        }
        totalCleaned += cleaned
    }
    
    return totalCleaned, nil
}
```

Queue statistics combine results from all shards:

```go
func (r *taskRedisRepo) QueueStats(ctx context.Context, cmd domain.Command) (*domain.QueueStats, error) {
    stats := &domain.QueueStats{}
    
    for _, client := range r.shardSupplier.AllShards() {
        shardStats, err := r.queueStatsOnShard(ctx, client, cmd)
        if err != nil {
            continue
        }
        
        stats.Pending += shardStats.Pending
        stats.InProgress += shardStats.InProgress
        stats.Delayed += shardStats.Delayed
        stats.DLQ += shardStats.DLQ
    }
    
    return stats, nil
}
```

## Lua Scripting and Atomicity

### Current Lua Usage

CodeQ uses Lua scripts for atomic multi-key operations within a single Redis instance. The primary script is `claimMoveScript`, which atomically moves a task from pending to in-progress:

```lua
local pending = KEYS[1]
local inprog = KEYS[2]
local taskID = redis.call('RPOP', pending)
if taskID then
    redis.call('SADD', inprog, taskID)
end
return taskID
```

This script executes as a single atomic operation because it runs on one Redis instance with no cross-key coordination.

### Sharding Impact

Once sharding is introduced, Lua scripts continue to work **within a shard**. The atomic claim operation remains valid because the pending and in-progress keys for a given command and tenant reside on the same shard.

However, operations that span multiple shards cannot use Lua for atomicity. For example, moving a task from one command's queue to another command's queue (if they reside on different shards) would require a two-phase approach:

1. Claim and complete the task on shard A
2. Re-enqueue with the new command on shard B

This is acceptable because codeQ's design already avoids cross-queue atomic moves. Tasks do not migrate between commands; they either complete or retry on the same command.

### Redis Cluster and Hash Slots

Redis Cluster partitions the key space into 16384 hash slots and requires that all keys in a Lua script map to the same slot. This constraint is enforced using hash tags: `{user123}:pending` and `{user123}:inprog` would both hash the `user123` segment, ensuring they land on the same slot.

CodeQ's current key format does not use hash tags, which would break Redis Cluster compatibility if we switched to it. However, we are not proposing Redis Cluster. Instead, we use client-side routing to direct requests to the correct standalone KVRocks instance based on the shard mapping.

If we later adopt Redis Cluster, we would need to introduce hash tags:

**Current**: `codeq:q:generate-master:tenant-a:pending:5`
**With hash tags**: `codeq:q:{generate-master:tenant-a}:pending:5`

The `{generate-master:tenant-a}` segment ensures all keys for that command-tenant pair hash to the same slot. This change would be part of a Redis Cluster migration, not the initial sharding implementation.

### Ensuring Atomicity

To preserve atomicity in a sharded deployment:

1. **Same-shard operations use Lua**: Claim, NACK, and delayed-to-pending moves continue to use Lua scripts because all involved keys reside on the same shard.

2. **Cross-shard operations avoid atomicity**: If a task must move between shards (e.g., re-enqueued on a different command after configuration change), it is completed on the source shard and created as a new task on the target shard. This is at-least-once semantics, consistent with codeQ's existing model.

3. **Global task hash remains single-instance**: The `codeq:tasks` and `codeq:results` hashes can remain on a "metadata shard" or be replicated to all shards. For simplicity, we recommend a dedicated metadata shard that all API servers query for task details. Queue operations remain sharded, but task data is centralized for admin visibility.

## Redis Cluster and Hash-Slot Implications

### Why Not Redis Cluster?

Redis Cluster provides automatic sharding with client-aware routing and cross-node coordination. However, it introduces complexity that does not align with codeQ's architecture:

**No multi-key transactions**: Redis Cluster does not support multi-key transactions across slots. Lua scripts must use hash tags to ensure all keys map to the same slot. CodeQ's key format would require significant changes.

**Client complexity**: Redis Cluster requires cluster-aware clients that handle MOVED and ASK redirects. The go-redis library supports this, but it adds failure modes and coordination overhead.

**Operational overhead**: Running a Redis Cluster requires managing cluster membership, handling slot migrations, and coordinating failover. This is more complexity than codeQ needs for 2-4 shards.

**No atomic visibility across cluster**: Even with Redis Cluster, operations spanning multiple slots are not atomic. CodeQ would still need client-side coordination for cross-shard claims.

### Client-Side Routing vs. Redis Cluster

**Client-side routing** (our proposal):
- API servers maintain a mapping of command/tenant to KVRocks instance
- Each KVRocks instance is a standalone Redis-compatible server
- No cluster coordination overhead
- Simple failure model: shard is up or down
- Manual rebalancing by changing configuration

**Redis Cluster**:
- Clients query the cluster for key ownership
- Cluster automatically redirects requests to the correct node
- Automatic failover with replica promotion
- Slot rebalancing requires coordination and data migration

For codeQ's use case—2-4 shards with explicit command/tenant assignment—client-side routing is simpler and avoids the operational complexity of Redis Cluster.

If codeQ later scales to dozens of shards with dynamic rebalancing needs, Redis Cluster could be reconsidered. The ShardSupplier abstraction allows swapping in a cluster-aware implementation without changing the repository layer.

## RAFT-Based Alternative Architecture

### RAFT and Distributed Consensus

RAFT is a consensus algorithm that ensures multiple replicas agree on a sequence of operations even when some replicas fail. It provides:

- **Linearizable reads and writes**: All replicas see operations in the same order
- **Automatic leader election**: When the leader fails, replicas elect a new leader without operator intervention
- **Log replication**: The leader replicates operations to followers, ensuring durability

RAFT is used in systems like etcd, Consul, and CockroachDB to build strongly consistent distributed databases. KVRocks does not currently support RAFT, but it is built on RocksDB, which other projects have extended with RAFT (e.g., TiKV uses RocksDB with RAFT via Raft Engine).

### RAFT in KVRocks: Vision

Imagine a future where KVRocks gains native RAFT support, allowing multiple KVRocks instances to form a consensus group. Each write would be proposed to the leader, replicated to a majority of followers, and then acknowledged. Reads could be served from any replica with linearizable consistency.

In this world, codeQ could deploy a RAFT-backed KVRocks cluster with the following properties:

**Automatic failover**: If the leader crashes, followers elect a new leader and continue serving requests within seconds.

**Strong consistency**: All operations are serialized through the RAFT log, eliminating the at-least-once ambiguities of sharded deployments.

**Horizontal read scaling**: Followers can serve read-only queries, distributing load across the cluster.

**Simplified operations**: No manual failover, no stale read concerns, no lost writes due to replication lag.

### Why RAFT Is Not Available Today

KVRocks is a young project that prioritizes Redis protocol compatibility and RocksDB integration. Adding RAFT consensus would require:

1. **RAFT implementation**: Integrating a RAFT library (e.g., etcd/raft or Hashicorp's raft) with KVRocks' event loop and storage layer.

2. **State machine mapping**: Defining how Redis commands map to RAFT log entries and how to apply them deterministically across replicas.

3. **Leader election and membership**: Managing cluster membership, leader election, and replica addition/removal.

4. **Snapshot and log compaction**: Periodically snapshotting the RocksDB state and truncating the RAFT log to prevent unbounded growth.

5. **Protocol extensions**: Exposing leader/follower status to clients so they can route writes to the leader and optionally read from followers.

This is a multi-year engineering effort. KVRocks may eventually pursue RAFT, but it is not on the current roadmap.

### RAFT vs. Master-Replica

KVRocks already supports asynchronous master-replica replication, similar to Redis. A master instance streams write operations to replicas, which apply them with eventual consistency. This provides read scaling and disaster recovery but not automatic failover or strong consistency.

**Master-replica limitations**:
- Writes only go to the master; replicas lag by milliseconds to seconds
- If the master fails, manual promotion is required
- Replicas may have stale data, causing inconsistent reads
- No automatic failover without external orchestration (e.g., Sentinel)

**RAFT advantages**:
- Writes are durable once a majority of replicas acknowledge
- Automatic leader election on failure
- Linearizable reads from the leader or followers with read quorum
- No external orchestration required

The trade-off is complexity and latency. RAFT requires at least three nodes and adds latency for every write (one round-trip to a majority of replicas). Asynchronous replication is faster but less durable.

## Option Analysis

We now evaluate three architectural options for scaling codeQ beyond a single KVRocks instance.

### Option 1: Do Nothing (Vertical Scaling Only)

Continue deploying codeQ with a single KVRocks instance and scale vertically as needed.

**What this means**:
- Upgrade to larger instances with more CPU, memory, and faster disks
- Use KVRocks master-replica replication for disaster recovery
- Deploy separate codeQ+KVRocks pairs per region or large tenant

**Advantages**:
- No code changes required
- Simple deployment and operations
- No coordination overhead
- Vertical scaling works well up to 64-128 GB memory and 32-64 cores

**Disadvantages**:
- Eventually hits hardware limits (cost and availability of very large instances)
- Single point of failure without automatic failover
- Cannot distribute load across multiple storage nodes for a single tenant
- High-traffic tenants cannot be isolated without deploying separate environments

**When this is sufficient**:
- Task throughput remains below 1000/sec sustained
- Total queue depth stays under 10 million tasks
- Cost of large instances is acceptable
- Tenants can be regionally isolated

**When this breaks**:
- Single tenant generates 10x more traffic than all others combined
- Total memory footprint exceeds largest available instance
- Cost of vertical scaling exceeds cost of horizontal scaling
- Disaster recovery requires automatic failover within seconds

This option is the baseline. We do not need sharding until vertical scaling becomes cost-prohibitive or insufficient.

### Option 2: Master-Replica Per Tenant

Deploy one KVRocks master per high-traffic tenant, with asynchronous replicas for disaster recovery. Small tenants share a common "default" shard.

**What this means**:
- Each large tenant gets dedicated KVRocks instances
- CodeQ API servers route requests based on tenant ID
- Use KVRocks native master-replica replication for durability
- Manual failover when a master fails (promote a replica)

**Advantages**:
- Complete tenant isolation at the storage layer
- High-traffic tenants do not impact others
- Horizontal capacity scales with tenant count
- Straightforward routing logic (tenant ID → master address)

**Disadvantages**:
- Manual failover when a master fails (downtime until operator promotes replica)
- Replicas have stale data; cannot serve consistent reads
- Operational burden increases with shard count (each tenant needs monitoring and backups)
- Small tenants still share a shard; no isolation for them

**When this is appropriate**:
- A small number of large tenants dominate traffic
- Tenant isolation is a strict requirement (compliance, noisy neighbor concerns)
- Operators can manage manual failover for each shard
- Read consistency is not critical (at-least-once delivery already tolerates stale reads)

**When this breaks**:
- Dozens of tenants need dedicated shards (operational burden too high)
- Automatic failover is required for SLA
- Cross-tenant queries are needed (admin operations become complex)

This option is viable today with KVRocks' existing replication support. It is the most practical near-term solution for scaling beyond a single shard.

### Option 3: RAFT Consensus in KVRocks

Wait for KVRocks to implement RAFT consensus, then deploy a RAFT-backed cluster for automatic failover and strong consistency.

**What this means**:
- Deploy 3-5 KVRocks instances in a RAFT group
- Leader election happens automatically on failure
- Writes are durable once a majority of replicas acknowledge
- Reads can be served from leader or followers with linearizable consistency

**Advantages**:
- Automatic failover with no operator intervention
- Strong consistency eliminates stale reads
- Horizontal read scaling by distributing reads across followers
- Simplified disaster recovery (no manual promotion)

**Disadvantages**:
- RAFT is not implemented in KVRocks (multi-year effort)
- Increased write latency due to quorum replication
- Requires at least three nodes, increasing deployment cost
- More complex failure modes (split-brain, quorum loss)

**When this is appropriate**:
- KVRocks has production-ready RAFT support
- Strong consistency is required for business logic
- Automatic failover is mandatory for SLA
- Write latency increase (2-5ms) is acceptable

**When this breaks**:
- KVRocks never implements RAFT (have to migrate to different storage)
- Write latency is unacceptable for high-throughput workloads
- Deployment cost of 3-5 nodes exceeds budget

This option is aspirational. It depends on KVRocks evolving its architecture. If KVRocks adds RAFT, it becomes the best long-term solution. Until then, it is not actionable.

### Recommendation

**Near-term (next 12 months)**: Implement **Option 2** (master-replica per tenant) with explicit shard mappings. This provides horizontal scaling for large tenants without waiting for KVRocks to implement RAFT. Use the ShardSupplier abstraction to keep routing logic pluggable.

**Long-term (2+ years)**: Monitor KVRocks development and evaluate **Option 3** (RAFT consensus) if and when it becomes available. The ShardSupplier abstraction allows swapping in a RAFT-aware implementation without rewriting the repository layer.

**Fallback**: If KVRocks does not implement RAFT and vertical scaling becomes insufficient, consider migrating to a storage layer with native RAFT support (e.g., TiKV, Etcd, or FoundationDB). The domain and service layers remain unchanged; only the repository layer needs rewriting.

## Migration and Backward Compatibility

### Phased Rollout

Introduce sharding in phases to minimize risk and allow gradual adoption:

**Phase 1: ShardSupplier abstraction**
- Introduce the ShardSupplier interface and StaticShardSupplier implementation
- Default configuration uses a single "default" shard with the current key format
- All existing deployments continue to work without configuration changes
- Release and monitor for regressions

**Phase 2: Explicit shard mappings**
- Add configuration support for multiple shards
- Allow operators to specify which commands or tenants route to which shards
- Existing tasks remain on the default shard; new tasks route per configuration
- No data migration required; tasks age out naturally

**Phase 3: Admin tooling**
- Add CLI commands to inspect shard distribution
- Add metrics to monitor per-shard queue depth and throughput
- Add health checks per shard to detect failures

**Phase 4: Tenant isolation**
- Migrate high-volume tenants to dedicated shards one at a time
- Freeze enqueue for the tenant, wait for tasks to drain, update configuration, resume enqueue
- Monitor for cross-shard queries in claim operations

**Phase 5: Multi-region support**
- Extend ShardSupplier to support region-aware routing
- Allow shards to be deployed in different regions with local workers
- Consider task replication or cross-region failover if needed

### Backward Compatibility

The design preserves backward compatibility in three ways:

**Key format compatibility**: When `shardID` is empty, the key format matches the current single-shard layout. Existing keys do not need to be renamed.

**Configuration defaults**: If no shard configuration is provided, the system defaults to a single shard named "default" with the current Redis address. Existing deployments continue to work without changes.

**Gradual migration**: Operators can enable sharding for one command at a time. Other commands continue to use the default shard until explicitly migrated.

### Data Migration

For deployments that want to move existing tasks from the default shard to a new shard:

**Option A: Drain and re-enqueue**
1. Stop accepting new tasks for the command
2. Wait for all existing tasks to complete or expire
3. Update configuration to route the command to the new shard
4. Resume accepting tasks

**Option B: Manual key migration**
1. Dump all task IDs for the command from the default shard
2. Read each task JSON and re-write with the new shardID
3. Move queue keys using RENAME or by popping from old shard and pushing to new shard
4. Update configuration to route new tasks to the new shard

Option A is simpler and recommended. Option B is only necessary if draining is not acceptable (e.g., very long-running tasks).

### Configuration Evolution

Shard configuration starts simple and grows as deployments mature:

**Single-shard deployment (current state)**:
```yaml
redis:
  addr: kvrocks:6666
  password: ""
  db: 0
```

**Explicit multi-shard deployment**:
```yaml
shards:
  - id: default
    addr: kvrocks-0:6666
  - id: shard-1
    addr: kvrocks-1:6666

mappings:
  - command: GENERATE_MASTER
    shardID: default
  - command: GENERATE_CREATIVE
    shardID: shard-1
```

**Per-tenant sharding**:
```yaml
shards:
  - id: default
    addr: kvrocks-0:6666
  - id: tenant-large-shard
    addr: kvrocks-1:6666

mappings:
  - command: "*"
    tenantID: tenant-large
    shardID: tenant-large-shard
  - command: "*"
    tenantID: ""
    shardID: default
```

The configuration schema supports all three modes without breaking changes.

## Trade-Offs and Alternatives

### Trade-Offs in the Proposed Design

**Increased claim latency**: Workers that subscribe to commands across multiple shards must query each shard sequentially. This adds latency proportional to shard count. For most deployments (2-4 shards), the overhead is acceptable (<10ms). For larger deployments, workers should subscribe to specific commands rather than wildcards.

**Manual rebalancing**: The explicit mapping approach requires operators to decide which commands and tenants go to which shards. This is simpler than automatic rebalancing but requires monitoring and manual adjustments as tenants grow.

**No cross-shard atomicity**: Tasks cannot move atomically between shards. This is acceptable given codeQ's at-least-once delivery model, but it means shard boundaries are semi-permanent. Changing a command's shard requires draining tasks first.

**Admin operation complexity**: Cleanup and statistics must query all shards and aggregate results. This increases latency and failure surface area for admin operations. However, admin operations are infrequent and can tolerate higher latency.

### Alternatives Considered

**Hash-based sharding with consistent hashing**: Would distribute load more evenly but makes tenant isolation harder and complicates debugging. Rejected because codeQ's workload has a small number of commands and tenants, not millions of random keys.

**Redis Cluster**: Would provide automatic routing and failover but adds operational complexity and requires key format changes (hash tags). Rejected because client-side routing is simpler for 2-4 shards.

**PostgreSQL with partitioned tables**: Would provide strong consistency and mature tooling but sacrifices Redis-compatible operations and increases query latency. Rejected because KVRocks is foundational to codeQ's architecture.

**TiKV or FoundationDB**: Would provide RAFT-based consistency and horizontal scaling but requires rewriting the repository layer and introduces a different operational model. Reserved as a long-term option if KVRocks does not meet scaling needs.

### Why RAFT Matters

RAFT-based storage would fundamentally change the scaling story:

**Strong consistency**: Eliminates at-least-once ambiguities and stale reads.

**Automatic failover**: Removes manual intervention from disaster recovery.

**Horizontal scaling**: Distributes load across multiple nodes without manual sharding.

However, RAFT is not free:

**Increased latency**: Every write waits for a majority quorum, adding 2-5ms to enqueue operations.

**Operational complexity**: Running a RAFT cluster requires understanding split-brain, quorum loss, and log compaction.

**Resource cost**: Requires at least three nodes, increasing infrastructure cost by 3x compared to single-node deployments.

The RAFT option is aspirational because it depends on KVRocks (or a replacement) implementing distributed consensus. Until then, explicit sharding with master-replica replication is the pragmatic path forward.

## Phased Implementation Roadmap

### Phase 1: Foundation (Q2 2026)

**Goal**: Introduce sharding abstractions without changing deployment behavior.

**Tasks**:
- Define ShardSupplier interface and StaticShardSupplier implementation
- Add shard configuration schema to pkg/config
- Update TaskRepository to accept ShardSupplier dependency
- Default to single-shard configuration for backward compatibility
- Add unit tests for routing logic with multiple shards
- Document configuration examples in docs/06-sharding.md

**Success criteria**:
- All existing deployments continue to work without config changes
- No performance regressions in single-shard mode
- Unit tests cover routing edge cases (missing mapping, invalid shard ID)

### Phase 2: Multi-Shard Support (Q3 2026)

**Goal**: Enable operators to deploy multiple shards with explicit command mappings.

**Tasks**:
- Implement multi-shard Enqueue, Claim, NACK, and Heartbeat
- Update admin operations (CleanupExpired, QueueStats) to query all shards
- Add per-shard metrics in internal/metrics/redis_collector.go
- Add CLI command `codeq admin shards list` to inspect shard mapping
- Test with two-shard deployment in docker-compose.yml
- Document migration process in docs/06-sharding.md

**Success criteria**:
- Tasks route to correct shard based on configuration
- Workers can claim tasks across multiple shards
- Admin cleanup removes tasks from all shards
- Metrics show per-shard queue depth

### Phase 3: Tenant Isolation (Q4 2026)

**Goal**: Allow high-volume tenants to be isolated onto dedicated shards.

**Tasks**:
- Extend shard mappings to support tenant-specific overrides
- Add CLI command `codeq admin migrate-tenant` to move tenant between shards
- Document tenant isolation strategies in docs/06-sharding.md
- Add runbook for shard failure recovery
- Test with three-shard deployment (default + two tenant-specific)

**Success criteria**:
- Tenant-specific mappings override command defaults
- Tenant migration succeeds without data loss (drain and re-enqueue pattern)
- Shard failure affects only tasks on that shard

### Phase 4: Operational Maturity (Q1 2027)

**Goal**: Harden sharding for production with monitoring and disaster recovery.

**Tasks**:
- Add Grafana dashboard with per-shard panels
- Add alerting rules for shard health checks
- Document disaster recovery procedures (shard failure, data loss, split-brain scenarios with replicas)
- Add integration tests for claim with one shard down
- Add capacity planning guide (when to add shards, how to estimate load)

**Success criteria**:
- Operators can monitor shard health and load distribution
- Runbooks cover common failure scenarios
- Integration tests validate graceful degradation

### Phase 5: Advanced Routing (Future)

**Goal**: Support hash-based or range-based sharding if workload requires.

**Tasks**:
- Implement HashShardSupplier using consistent hashing
- Implement RangeShardSupplier with configurable boundaries
- Add CLI tool to analyze tenant distribution and recommend shard assignment
- Evaluate Redis Cluster if shard count exceeds 10

**Success criteria**:
- Hash-based routing distributes load evenly across shards
- Operators can choose routing strategy in configuration

### Phase 6: RAFT Evaluation (Future, Conditional)

**Goal**: Evaluate KVRocks RAFT support if and when it becomes available.

**Tasks**:
- Benchmark RAFT latency overhead vs. standalone KVRocks
- Test automatic failover and leader election
- Implement RaftShardSupplier that routes writes to leader
- Document trade-offs (latency vs. consistency vs. cost)
- Provide migration path from master-replica to RAFT

**Success criteria**:
- RAFT-backed deployment provides automatic failover
- Write latency increase is acceptable (<10ms p99)
- Documentation clearly explains when to use RAFT vs. master-replica

This roadmap is tentative and depends on production needs, KVRocks evolution, and operator feedback.

## Conclusion

This document proposes a phased approach to queue sharding in codeQ, beginning with explicit shard mappings and client-side routing. The design preserves backward compatibility, supports gradual migration, and provides a foundation for future enhancements such as hash-based routing or RAFT consensus.

The near-term path focuses on practical horizontal scaling using KVRocks' existing master-replica replication. This allows deploying dedicated shards for high-volume tenants without waiting for distributed consensus features.

The long-term vision anticipates RAFT-based storage, either through KVRocks evolving its architecture or by migrating to a system like TiKV or FoundationDB. The ShardSupplier abstraction keeps this option open without committing to a specific implementation.

Operators should begin with single-node vertical scaling (Option 1) and adopt sharding (Option 2) only when cost or capacity constraints make it necessary. RAFT (Option 3) remains aspirational until KVRocks or a replacement provides production-ready distributed consensus.

Implementation will proceed in phases, starting with the ShardSupplier abstraction and ending with operational maturity for multi-shard production deployments. Each phase builds on the previous one, allowing incremental rollout and validation before committing to the next step.
