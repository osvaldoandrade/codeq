# Persistence Plugin Migration Guide

This guide documents how to migrate from direct Redis configuration to the persistence plugin system, and how to validate backward compatibility.

## Overview

codeQ v1.x introduced a plugin-based persistence layer that abstracts storage operations behind a unified interface. The Redis plugin wraps the existing repository implementation, ensuring **zero data-format changes** — existing Redis/KVRocks data remains fully compatible.

## Who Needs to Migrate?

**You do NOT need to change anything if:**
- You are happy with Redis/KVRocks and your existing configuration
- codeQ defaults to the Redis plugin when no `persistenceProvider` is set

**You should migrate if:**
- You want to explicitly declare the persistence backend in configuration
- You plan to switch to an alternative backend (Memory for testing, or future plugins)
- You want to use the plugin configuration format for consistency

## Configuration Migration

### Before (Implicit Redis)

Previously, Redis connection settings were provided directly:

```yaml
# config.yaml (legacy format)
redisAddr: localhost:6379
redisPassword: ""
```

Or via environment variables:

```bash
REDIS_ADDR=localhost:6379
REDIS_PASSWORD=""
```

### After (Explicit Plugin Configuration)

The new format uses `persistenceProvider` and `persistenceConfig`:

```yaml
# config.yaml (plugin format)
persistenceProvider: redis
persistenceConfig:
  addr: localhost:6379
  password: ""        # optional
```

Or via environment variables:

```bash
PERSISTENCE_PROVIDER=redis
PERSISTENCE_CONFIG='{"addr":"localhost:6379","password":""}'
```

### Step-by-Step Migration

1. **Verify current configuration works**: Ensure your existing deployment is healthy before migrating.

2. **Add plugin configuration**: Add `persistenceProvider` and `persistenceConfig` to your config file alongside existing settings.

3. **Test the configuration**: Deploy to a staging environment and verify:
   - Health endpoint returns OK
   - Tasks can be enqueued and claimed
   - Results are saved and retrievable
   - Existing data is accessible

4. **Remove legacy settings** (optional): Once verified, you may remove the old `redisAddr`/`redisPassword` settings.

## Data Format Compatibility

The Redis plugin uses the **exact same key formats** as the direct repository implementation:

| Data Type | Key Format | Example |
|-----------|-----------|---------|
| Tasks HASH | `codeq:tasks` | Field: `<task-uuid>`, Value: JSON |
| Results HASH | `codeq:results` | Field: `<task-uuid>`, Value: JSON |
| TTL Index | `codeq:tasks:ttl` | ZSET with score = expiry timestamp |
| Pending Queue | `codeq:q:<cmd>:pending:<priority>` | `codeq:q:generate_master:pending:5` |
| In-Progress | `codeq:q:<cmd>:inprog` | SET of task IDs |
| Delayed Queue | `codeq:q:<cmd>:delayed` | ZSET with score = visible-at timestamp |
| DLQ | `codeq:q:<cmd>:dlq` | SET of task IDs |
| Lease Key | `codeq:lease:<task-id>` | STRING with expiry |
| Subscriptions | `codeq:subs:<sub-id>` | HASH of subscription data |

**Key point**: Commands in Redis keys are always **lowercase** (e.g., `generate_master`) even though the domain constants are uppercase (`GENERATE_MASTER`).

### Tenant and Shard Keys

When using tenant isolation or sharding, keys include additional segments:

```
codeq:q:<cmd>:<tenant>:pending:<priority>        # with tenant
codeq:q:<cmd>:s:<shard>:pending:<priority>        # with shard
codeq:q:<cmd>:<tenant>:s:<shard>:pending:<priority>  # both
```

## Rollback Instructions

If you need to revert to the legacy configuration:

1. Remove `persistenceProvider` and `persistenceConfig` from your config
2. Restore `redisAddr` and `redisPassword` settings
3. Restart codeQ

**No data migration is needed** — the underlying Redis data is identical regardless of which configuration format is used.

## Plugin Configuration Reference

### Redis Plugin

```yaml
persistenceProvider: redis
persistenceConfig:
  addr: "localhost:6379"   # Redis/KVRocks address (host:port)
  password: ""             # Redis password (optional)
```

### Memory Plugin (Testing Only)

```yaml
persistenceProvider: memory
persistenceConfig: {}
```

> **Warning**: The memory plugin stores data in-process memory. All data is lost on restart. Use only for testing.

## Verifying Backward Compatibility

The persistence plugin includes a shared contract test suite that verifies behavioral consistency across all backends. To run the tests:

```bash
# Run all persistence plugin tests
go test ./pkg/persistence/... -count=1 -v

# Run only Redis plugin backward compatibility tests
go test ./pkg/persistence/redis/... -count=1 -v

# Run the shared contract tests for all plugins
go test ./pkg/persistence/redis/... ./pkg/persistence/memory/... -run Contract -v
```

### What the Contract Tests Verify

| Test Area | What's Verified |
|-----------|----------------|
| Health | Backend connectivity check |
| EnqueueAndClaim | Task enqueue → claim lifecycle preserves command and payload |
| GetNotFound | Proper error for missing tasks |
| ClaimTaskEmptyQueue | Graceful handling of empty queues |
| QueueLength | Accurate pending task count after enqueue |
| QueueStats | Queue statistics reflect enqueued tasks |
| MoveDueDelayed | Delayed task promotion runs without error |
| AdminQueues | Administrative queue view returns data |
| SaveAndGetResult | Result persistence and retrieval |
| GetResultNotFound | Proper error for missing results |
| UpdateTaskOnComplete | Task status update on completion |
| RemoveFromInprogAndClearLease | Lease cleanup on completion |
| RegisterAndGetByCommand | Subscription registration and query |
| RemoveExpired | Expired subscription cleanup |

### Data Format Tests (Redis-Specific)

The `TestRedisPluginDataFormatCompatibility` test verifies:
- Tasks are stored in `codeq:tasks` HASH
- TTL tracking uses `codeq:tasks:ttl` ZSET
- Pending queues use `codeq:q:<cmd>:pending:<priority>` LIST format
- All key formats match the direct repository implementation

## Developing Custom Plugins

To create a new persistence plugin that passes the contract tests:

1. Implement the `PluginPersistence` interface (see `pkg/persistence/interface.go`)
2. Register via `persistence.RegisterProvider("yourplugin", factory)` in `init()`
3. Create a test file that runs the contract suite:

```go
package yourplugin

import (
    "testing"
    "github.com/osvaldoandrade/codeq/pkg/persistence/persistencetest"
)

func TestYourPluginContractTests(t *testing.T) {
    plugin := createYourPlugin(t)
    defer plugin.Close()
    persistencetest.RunPluginContractTests(t, plugin)
}
```

4. All contract tests must pass before a plugin is considered production-ready.

## References

- Plugin interface: `pkg/persistence/interface.go`
- Plugin registry: `pkg/persistence/registry.go`
- Redis plugin: `pkg/persistence/redis/plugin.go`
- Memory plugin: `pkg/persistence/memory/plugin.go`
- Contract tests: `pkg/persistence/persistencetest/contract.go`
- Plugin system overview: `docs/27-persistence-plugin-system.md`
- Plugin architecture HLD: `docs/25-plugin-architecture-hld.md`
