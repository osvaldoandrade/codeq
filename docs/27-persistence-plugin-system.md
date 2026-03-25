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
- EnqueueTask: Add tasks to queues
- ClaimTask: Atomically claim tasks for processing
- UpdateLease: Extend task leases
- AbandonLease: Release tasks back to queue
- NackTask: Return failed tasks with retry delay

**ResultStorage** - Task results:
- SaveResult: Store task execution results
- GetResult: Retrieve results by task ID
- UpdateTaskOnComplete: Update task status

**SubscriptionStorage** - Worker subscriptions:
- Register: Register worker webhooks
- Unregister: Remove worker subscriptions
- GetByCommand: Find subscriptions for specific commands

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
