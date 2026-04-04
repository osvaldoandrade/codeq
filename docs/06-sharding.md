# Sharding

## Current Status

codeQ includes a pluggable **ShardSupplier** interface and a static configuration-based implementation that enables horizontal scaling across multiple KVRocks instances. Sharding is optional — single-shard deployments continue to work without any configuration changes.

## ShardSupplier Interface

The `ShardSupplier` interface (`pkg/domain/shard.go`) defines the contract for shard routing:

```go
type ShardSupplier interface {
    QueueShards(ctx context.Context, command string, tenantID string) ([]string, error)
    CurrentShard(ctx context.Context, command string, tenantID string) (string, error)
}
```

- **`CurrentShard`** — returns the shard for enqueue and claim operations.
- **`QueueShards`** — returns all shards that may hold queues for a command-tenant pair (used for stats aggregation and migration visibility).

## Static Shard Supplier

The `StaticShardSupplier` (`internal/shard/static_supplier.go`) routes commands and tenants to shards using YAML configuration with three-layer precedence:

1. **Tenant overrides** (highest priority) — isolate specific tenants on dedicated shards.
2. **Command mappings** — route commands to designated shards.
3. **Default shard** (fallback) — handles all unmatched command-tenant pairs.

### Configuration

```yaml
sharding:
  enabled: true
  defaultShard: "primary"

  # Command-to-shard mapping
  commandMappings:
    GENERATE_MASTER: "compute-heavy"
    GENERATE_CREATIVE: "compute-heavy"
    SEND_EMAIL: "notification"

  # Tenant overrides take highest priority
  tenantOverrides:
    "tenant-premium-abc": "premium-shard"
```

When `sharding.enabled` is `false` or the section is absent, codeQ behaves as a single-shard deployment with no changes to queue key format.

## Repository Layer

The repository layer implements shard-aware task operations through the **ShardedTaskRepository** pattern.

### ClientMap

The `ClientMap` (`internal/shard/client_map.go`) manages the mapping of shard identifiers to Redis clients:

```go
// Manages shard-to-client routing
type ClientMap struct {
    clients      map[string]*redis.Client  // Shard ID → Redis client
    defaultShard string                    // Fallback shard ID
}
```

**Key methods:**
- `NewClientMap(clients, defaultShard)` — creates a map with validation that all references exist
- `Client(shardID)` — returns the Redis client for a shard (falls back to default if not found)
- `DefaultClient()` — returns the client for the default shard
- `ShardIDs()` — returns all configured shard identifiers
- `Close()` — gracefully closes all Redis connections

**Single-shard mode:** `NewSingleClientMap(client)` creates a ClientMap with a single Redis client, maintaining backward compatibility.

### ShardedTaskRepository

The `ShardedTaskRepository` (`internal/repository/sharded_task_repository.go`) wraps per-shard repositories and routes operations based on ShardSupplier resolution:

```go
type shardedTaskRepository struct {
    shardSupplier domain.ShardSupplier      // Shard routing logic
    repos         map[string]TaskRepository // One repo per shard
    defaultShard  string                    // Fallback shard ID
}
```

**Routing pattern:**

1. For each operation (Enqueue, Claim, etc.):
   - Call `ShardSupplier.CurrentShard(ctx, command, tenantID)` to determine target shard
   - Route to the corresponding `TaskRepository` instance
   - If shard resolution fails, fall back to default shard

2. For cross-shard operations (QueueStats):
   - Call `ShardSupplier.QueueShards(ctx, command, tenantID)` to get all relevant shards
   - Aggregate results across all shard repositories
   - Return consolidated view to caller

**Error handling:**

- If a shard is not found in `repos`, operations transparently fall back to the default shard
- "Not-found" errors from individual shards are properly propagated
- Network or Redis errors from any shard are returned directly

### Example: Multi-Shard Setup

```go
// Create per-shard Redis clients
primaryClient := redis.NewClient(&redis.Options{Addr: "kvrocks-primary:6379"})
heavyClient := redis.NewClient(&redis.Options{Addr: "kvrocks-compute:6379"})

// Create ClientMap
clientMap, _ := shard.NewClientMap(
    map[string]*redis.Client{
        "primary": primaryClient,
        "compute": heavyClient,
    },
    "primary", // default shard
)

// Create StaticShardSupplier with routing rules
supplier := shard.NewStaticShardSupplier(
    map[string]string{"GENERATE_MASTER": "compute"},      // command mappings
    map[string]string{"tenant-vip": "compute"},           // tenant overrides
    "primary",                                             // default
)

// Create sharded task repository
taskRepo := repository.NewShardedTaskRepository(
    clientMap,
    time.UTC,
    "exp_full_jitter",
    5, 900,
    supplier,
)

// Operations now transparently route across shards
task, _ := taskRepo.Enqueue(ctx, "GENERATE_MASTER", payload, 1, webhook, maxAttempts, idempKey, visibleAt, "tenant-vip")
// → routed to "compute" shard via ShardSupplier
```

## Queue Key Format

Queue keys include an optional shard segment inserted after the tenant identifier:

```
# Single-shard (legacy, fully backward compatible)
codeq:q:<command>[:<tenantID>]:<queue-type>[:<priority>]

# Multi-shard
codeq:q:<command>[:<tenantID>]:s:<shardID>:<queue-type>[:<priority>]
```

The shard segment (`:s:<shardID>`) is omitted when the shard is `"default"` or empty, preserving backward compatibility with existing deployments. Key generation utilities are in `internal/shard/key.go`.

## Shard Key Strategies

Three strategies are supported through different `ShardSupplier` implementations:

| Strategy | Description | Use Case |
|----------|-------------|----------|
| **Explicit** (implemented) | Configuration maps commands/tenants to named shards | Maximum operational control, gradual rollout |
| **Hash-based** (future) | Hash of (command, tenant) selects shard | Automatic load balancing |
| **Range-based** (future) | Key-space partitioned into ranges | Predictable data locality |

Hash and range strategies can be implemented as alternative `ShardSupplier` implementations without changing the rest of the system.

## Migration from Single to Multi-Shard

1. **Upgrade** to a sharding-aware release. Without configuration, codeQ defaults to single-shard behavior — no migration needed.
2. **Add sharding configuration** mapping low-risk commands to a new shard.
3. **Gradually migrate** commands by updating `commandMappings`. During migration, `QueueShards` returns both old and new shards for visibility.
4. **Decommission** the old shard once it drains.

Each step is reversible: removing a command from `commandMappings` reverts it to the default shard.

## Validation

The `StaticShardSupplier.Validate()` method checks that all shard references (default, command mappings, tenant overrides) correspond to configured backends. The service refuses to start if validation fails, preventing tasks from routing to non-existent shards.

## Design Document

For the full sharding design including architecture diagrams, atomicity analysis, Redis Cluster considerations, and alternative approaches, see **[Queue Sharding HLD](24-queue-sharding-hld.md)**.
