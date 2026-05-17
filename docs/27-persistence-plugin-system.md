# Persistence Plugin System

## Overview

codeQ's persistence layer uses a plugin architecture that allows you to choose different storage backends without modifying core code. This enables organizations to use their existing database infrastructure and simplifies testing with in-memory storage.

## Available Plugins

### Redis Plugin (Default)

The Redis plugin provides persistence using Redis or KVRocks (Redis protocol compatible).

**Configuration:**

```yaml
persistenceProvider: redis
persistenceConfig:
  addr: localhost:6379
  password: "" # optional
```

**Environment Variables:**

```bash
PERSISTENCE_PROVIDER=redis
PERSISTENCE_CONFIG='{"addr":"localhost:6379","password":""}'
```

**Features:**
- Production-ready persistence
- Atomic operations using Redis transactions
- Bloom filters for performance optimization
- Full backward compatibility with existing deployments

### Pebble Plugin (Embedded, Single-Node)

The Pebble plugin provides embedded persistence using [Pebble](https://github.com/cockroachdb/pebble), a RocksDB-inspired key-value store. Ideal for single-node deployments where embedding a database simplifies infrastructure.

**Configuration:**

```yaml
persistenceProvider: pebble
persistenceConfig:
  path: ./codeq-pebble           # Directory for database files
  fsyncOnCommit: false           # fsync on every commit (durability vs throughput trade-off)
  numShards: 4                   # Intra-process shards for parallelism (Phase 8)
```

**Environment Variables:**

```bash
PERSISTENCE_PROVIDER=pebble
PERSISTENCE_CONFIG='{"path":"./codeq-pebble","fsyncOnCommit":false,"numShards":4}'
```

**Features:**
- **Embedded operation**: No separate database process; runs in-process
- **High throughput**: Single-shard baseline ~45k tasks/sec; 4 shards achieves ~83k tasks/sec (1.95× improvement)
- **Intra-process sharding** (Phase 8): Parallelizes write commits and compaction across N independent Pebble databases
- **Configurable durability**: `fsyncOnCommit=true` enables full durability at cost of throughput
- **Zero external dependencies**: Entire stack (server + storage) in one binary

**NumShards Configuration (Phase 8 Sharding):**

- `numShards: 0` or `1` (default) — Single-shard mode, traditional single database
- `numShards: N` (N > 1) — Opens N independent Pebble instances under `path/shard<i>/`, routes tasks by `hash(task_id) % N`
  - Each shard runs its own commit pipeline and compaction independently
  - Every operation (create, claim, result) is routed to shard by task ID, ensuring consistent affinity
  - Throughput scales nearly linearly with shard count (tested: 4 shards → 1.95×)

**Recommended Configurations:**

- **Development/Testing**: `numShards: 1`, `fsyncOnCommit: false` (fastest)
- **Single-node production**: `numShards: 4`, `fsyncOnCommit: false` (balanced throughput & latency)
- **Durability-critical**: `numShards: 4`, `fsyncOnCommit: true` (persistent on commit, ~20% throughput hit)

**Limitations:**
- **Single-node only**: Cluster mode (multi-node consensus) is not compatible with intra-process sharding in this release
- **No HA by design**: Embedded database loses availability if process restarts; use Redis plugin for high-availability requirements

**Use Cases:**
- Self-contained deployments (edge, embedded systems)
- Development and testing with full production parity
- Organizations with existing Pebble expertise
- Throughput-optimized single-node setups (replacing KVRocks)

### Memory Plugin (Testing Only)

The memory plugin provides in-memory storage for unit tests. **Not suitable for production.**

**Configuration:**

```yaml
persistenceProvider: memory
persistenceConfig: {}
```

**Environment Variables:**

```bash
PERSISTENCE_PROVIDER=memory
PERSISTENCE_CONFIG='{}'
```

**Features:**
- Zero external dependencies
- Fast test execution
- Simple implementation for understanding the plugin interface
- Thread-safe with mutex protection

## Architecture

### Plugin Interface

All persistence plugins implement the `PluginPersistence` interface:

```go
type PluginPersistence interface {
    TaskStorage() TaskStorage
    ResultStorage() ResultStorage
    SubscriptionStorage() SubscriptionStorage
    Health(ctx context.Context) error
    Close() error
}
```

### Storage Interfaces

**TaskStorage** - Task queue operations:
- Save: Persist a task to storage
- Get: Retrieve a task by ID
- Delete: Remove a task from storage
- EnqueueTask: Add tasks to queues based on command and priority
- ClaimTask: Atomically claim the next available task for processing
- UpdateLease: Extend or abandon a task lease
- AbandonLease: Release a task lease, making it available for other workers
- NackTask: Return a failed task to queue with retry delay and reason
- MoveDueDelayed: Move delayed tasks that are now ready to pending queues
- QueueLength: Get the count of pending tasks for a command
- QueueStats: Retrieve detailed statistics for a queue
- AdminQueues: Get administrative view of all queues
- CleanupExpired: Remove expired tasks before a specified time

**ResultStorage** - Task results:
- SaveResult: Store task execution results with outcome
- GetResult: Retrieve results by task ID
- UpdateTaskOnComplete: Update task status upon completion
- RemoveFromInprogAndClearLease: Remove task from in-progress and clear lease

**SubscriptionStorage** - Worker subscriptions:
- Register: Register worker webhooks for commands
- Unregister: Remove worker subscriptions for specific commands
- GetByCommand: Find subscriptions for specific commands
- GetByWorker: Retrieve all subscriptions for a worker
- RemoveExpired: Remove expired subscriptions before a specified time

## Backward Compatibility

### Default Behavior

If no `persistenceProvider` is configured, codeQ defaults to Redis with the following settings:

```yaml
persistenceProvider: redis
persistenceConfig:
  addr: <value from redisAddr>
  password: <value from redisPassword>
```

This ensures existing configurations work without modification.

### Migration Path

Existing deployments continue to work unchanged. To explicitly configure the persistence plugin:

1. Add `persistenceProvider: redis` to your config file
2. Move Redis connection settings to `persistenceConfig`
3. Test the configuration
4. (Optional) Migrate to alternative backends

## Developing Custom Plugins

### Step 1: Implement the Interface

Create a package that implements `PluginPersistence`:

```go
package myplugin

import (
    "github.com/osvaldoandrade/codeq/pkg/persistence"
)

type Plugin struct {
    // Your implementation
}

func NewPlugin(config persistence.PluginConfig) (persistence.PluginPersistence, error) {
    // Initialize your plugin
    return &Plugin{}, nil
}
```

### Step 2: Implement Storage Interfaces

Implement `TaskStorage`, `ResultStorage`, and `SubscriptionStorage`:

```go
func (p *Plugin) TaskStorage() persistence.TaskStorage {
    return &myTaskStorage{plugin: p}
}

// Implement all required methods for TaskStorage
type myTaskStorage struct {
    plugin *Plugin
}

func (s *myTaskStorage) EnqueueTask(ctx context.Context, task *domain.Task) error {
    // Your implementation
}
// ... implement other methods
```

### Step 3: Register the Plugin

Register your plugin in an `init()` function:

```go
func init() {
    persistence.RegisterProvider("myplugin", NewPlugin)
}
```

### Step 4: Import and Configure

Import your plugin in `cmd/server/main.go`:

```go
import (
    _ "github.com/osvaldoandrade/codeq/pkg/persistence/myplugin"
)
```

Configure it:

```yaml
persistenceProvider: myplugin
persistenceConfig:
  # Your plugin-specific configuration
```

## Testing

### Unit Tests with Memory Plugin

```go
import (
    _ "github.com/osvaldoandrade/codeq/pkg/persistence/memory"
    "github.com/osvaldoandrade/codeq/pkg/persistence"
)

func TestMyFeature(t *testing.T) {
    plugin, err := persistence.NewPersistence(
        persistence.ProviderConfig{
            Type: "memory",
            Config: []byte("{}"),
        },
        persistence.PluginConfig{
            Timezone: time.UTC,
        },
    )
    // Use plugin in tests
}
```

### Integration Tests

Use Redis plugin with miniredis for fast integration tests:

```go
import (
    "github.com/alicebob/miniredis/v2"
    _ "github.com/osvaldoandrade/codeq/pkg/persistence/redis"
)

func TestIntegration(t *testing.T) {
    mr, _ := miniredis.Run()
    defer mr.Close()
    
    plugin, _ := persistence.NewPersistence(
        persistence.ProviderConfig{
            Type: "redis",
            Config: []byte(fmt.Sprintf(`{"addr":"%s"}`, mr.Addr())),
        },
        persistence.PluginConfig{},
    )
    // Run integration tests
}
```

## Benefits

1. **Infrastructure Flexibility**: Use existing database infrastructure (Redis, PostgreSQL, etc.)
2. **Testing Simplification**: In-memory plugin eliminates Docker dependencies for unit tests
3. **Cloud Native**: Integrate with managed database services (DynamoDB, Cosmos DB, etc.)
4. **Extensibility**: Add new backends without modifying core code
5. **Backward Compatible**: Existing deployments work without changes

## Future Enhancements

Planned plugin implementations:

- **PostgreSQL Plugin**: Use PostgreSQL for persistence
- **DynamoDB Plugin**: AWS-native persistence
- **Cassandra Plugin**: Distributed database for scale
- **TiKV Plugin**: RAFT-backed consensus storage

See `docs/25-plugin-architecture-hld.md` for detailed design documentation.
