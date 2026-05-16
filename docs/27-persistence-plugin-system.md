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

### Pebble Plugin (Embedded Local Storage)

The Pebble plugin provides embedded persistence using CockroachDB's Pebble key-value store. Ideal for single-node deployments, edge computing, and high-throughput scenarios where external dependencies must be minimized.

**Configuration:**

```yaml
persistenceProvider: pebble
persistenceConfig:
  path: ./codeq-pebble
  fsyncOnCommit: false
```

**Environment Variables:**

```bash
PERSISTENCE_PROVIDER=pebble
PERSISTENCE_CONFIG='{"path":"./codeq-pebble","fsyncOnCommit":false}'
```

**Configuration Options:**

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `path` | string | `./codeq-pebble` | Directory where Pebble stores database files. Created automatically if missing. |
| `fsyncOnCommit` | boolean | `false` | Force fsync on every commit for durability-first deployments. Reduces throughput ~30% but ensures zero data loss on process crash. |

**Performance Characteristics:**

- **Throughput**: Optimized for 10k–50k req/s per instance with batch group commit
- **Latency**: p95 latency < 10ms under normal load (no network hops)
- **Memory**: Minimal external memory requirements; memory usage scales with cache size
- **Scaling**: Single-node only (for multi-node deployments with Pebble, use cluster mode with hash-based routing via gRPC)

**Use Cases:**

- **Single-node deployments**: No external database dependency
- **High throughput**: Local storage eliminates network latency
- **Edge computing**: Embedded database for on-premise installations
- **Development & testing**: Faster iteration than Redis-based setups
- **Cluster deployments**: Each node maintains local Pebble; cluster routes requests via gRPC

**Features:**

- Embedded persistence with zero external dependencies
- Atomic batch writes with group commit optimization
- Bloom filters for fast negative lookups
- Configurable fsync for durability vs. performance tradeoff
- Full feature parity with Redis backend (queues, leases, results, webhooks)
- Distributed clustering support (each node has local Pebble + cluster mode)

**Migration from Redis to Pebble:**

1. Stop the running codeQ instance
2. Update configuration: set `persistenceProvider: pebble`
3. Restart codeQ — data migrates automatically on first run
4. Verify queue stats with `/admin/queues` endpoint

Data from Redis is *not* automatically copied to Pebble. For data migration, export results from Redis and re-enqueue or use a migration utility.

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
