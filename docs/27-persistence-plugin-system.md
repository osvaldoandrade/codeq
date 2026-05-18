# Persistence Plugin System

## Overview

codeq's persistence layer is built around an embedded Pebble store
(CockroachDB's RocksDB-style LSM). The pluggable interface in
`pkg/persistence/` exists so future backends can be added without
touching the service layer — but Pebble is the only supported and
benchmarked path. Treat anything else in this document as architectural
reference for extension, not deployment options.

## Pebble (default, embedded)

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
- **Cluster + sharding mutually exclusive**: `numShards > 1` is not compatible with `cluster.enabled=true` in this release. The startup path rejects the combination.
- **HA**: a single-process Pebble loses availability across restarts. Multi-node HA is the cluster path (consistent-hash ring over N Pebble nodes), not a shared database.

**Use cases:**
- Self-contained deployments (edge, embedded systems)
- Development and testing with full production parity
- Throughput-optimized single-node production

### Memory (testing only)

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

Pebble integration tests use `t.TempDir()` and don't need a separate
process or container:

```go
import (
    "testing"
    "time"

    _ "github.com/osvaldoandrade/codeq/pkg/persistence/memory"
    "github.com/osvaldoandrade/codeq/pkg/persistence"
)

func TestIntegration(t *testing.T) {
    plugin, _ := persistence.NewPersistence(
        persistence.ProviderConfig{
            Type:   "pebble",
            Config: []byte(`{"path":"` + t.TempDir() + `","fsyncOnCommit":false,"numShards":1}`),
        },
        persistence.PluginConfig{Timezone: time.UTC},
    )
    _ = plugin
    // Run integration tests
}
```

## Why a plugin interface at all

Pebble is the only supported path today. The interface exists to keep
the storage layer swappable for two scenarios:

1. **In-process testing**: the memory provider lets unit tests skip disk
   I/O entirely.
2. **Future backends**: anything that wants to replace Pebble (a
   replicated LSM, a cloud-managed KV store) plugs in here without
   touching `internal/services` or the HTTP/gRPC surfaces.

There are no concrete plans to ship a non-Pebble production backend.

See `docs/25-plugin-architecture-hld.md` for detailed design documentation.

## See also

- [Configuration](./14-configuration.md) — Persistence provider configuration
- [Storage Layout: Pebble](./07b-storage-pebble.md) — Pebble keyspace and on-disk format
- [Plugin Architecture HLD](./25-plugin-architecture-hld.md) — Plugin design patterns
- [Performance Tuning](./17-performance-tuning.md) — Storage backend optimization
- [Troubleshooting](./28-troubleshooting.md) — Storage issues and diagnostics
