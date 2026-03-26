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
