# Shard Migration Guide

This guide covers how to migrate tasks between shards in a codeQ multi-shard deployment using the built-in `migrate-shards` CLI command.

## Overview

When you reconfigure command-to-shard mappings (e.g., moving `GENERATE_MASTER` from the default shard to a dedicated `compute-shard`), existing tasks remain on the original shard. The `migrate-shards` command moves those tasks — including their data, ordering, and metadata — to the new shard so that workers begin processing them from the correct backend.

## Prerequisites

- codeQ CLI (`codeq`) built from source or installed
- A codeQ server configuration file (`config.yaml`) with sharding enabled and backends defined
- Network access to both source and destination Redis/KVRocks instances
- Low traffic on the command being migrated (recommended)

## Configuration

The migration tool reads the same `config.yaml` used by the codeQ server. Ensure your sharding section defines both the source and destination backends:

```yaml
# config.yaml
sharding:
  enabled: true
  defaultShard: "default"

  commandMappings:
    GENERATE_MASTER: "compute-shard"   # New target shard

  backends:
    default:
      address: "kvrocks-primary:6379"
      password: "${REDIS_PRIMARY_PASSWORD}"
      db: 0
      poolSize: 20

    compute-shard:
      address: "kvrocks-compute:6379"
      password: "${REDIS_COMPUTE_PASSWORD}"
      db: 0
      poolSize: 30
```

## Step-by-Step Migration

### Step 1: Preview with Dry-Run

Always start with a dry-run to see what would be migrated:

```bash
codeq migrate-shards \
    --config config.yaml \
    --command GENERATE_MASTER \
    --from-shard default \
    --to-shard compute-shard \
    --dry-run
```

Expected output:

```
Checking shard health...
  ✓ default
  ✓ compute-shard
[DRY-RUN] No changes will be made

Migration: default → compute-shard (command: GENERATE_MASTER)

Results
  Pending:     Would migrate 1523 tasks
  Delayed:     Would migrate 47 tasks
  In-Progress: Would migrate 12 tasks
  DLQ:         Would migrate 3 tasks
  Total: Would migrate 1585 tasks in 0s
```

Review the counts. This tells you exactly how many tasks exist on the source shard for this command.

### Step 2: Execute Migration

When ready, run without `--dry-run` and add `--verify` for post-migration validation:

```bash
codeq migrate-shards \
    --config config.yaml \
    --command GENERATE_MASTER \
    --from-shard default \
    --to-shard compute-shard \
    --verify
```

The command shows a progress bar during migration and prints results:

```
Checking shard health...
  ✓ default
  ✓ compute-shard

Migration: default → compute-shard (command: GENERATE_MASTER)

Results
  Pending:     Migrated 1523 tasks
  Delayed:     Migrated 47 tasks
  In-Progress: Migrated 12 tasks
  DLQ:         Migrated 3 tasks
  Total: Migrated 1585 tasks in 2.3s

Verifying migration...
  Source remaining:  pending=0 delayed=0 inprog=0 dlq=0
  Dest counts:       pending=1523 delayed=47 inprog=12 dlq=3
  Shard default: healthy
  Shard compute-shard: healthy

✓ Migration verified successfully
```

### Step 3: Tenant-Specific Migration (Optional)

To migrate tasks for a specific tenant:

```bash
codeq migrate-shards \
    --config config.yaml \
    --command GENERATE_MASTER \
    --tenant tenant-premium-abc \
    --from-shard default \
    --to-shard premium-shard \
    --verify
```

## CLI Reference

```
codeq migrate-shards [flags]

Flags:
  --config string       Path to codeQ server configuration file (required)
  --command string      Command to migrate (required)
  --from-shard string   Source shard identifier (required)
  --to-shard string     Destination shard identifier (required)
  --tenant string       Tenant ID (optional, for tenant-specific migration)
  --batch-size int      Number of tasks per batch (default 1000)
  --dry-run             Preview migration without making changes
  --verify              Run post-migration verification
```

## What Gets Migrated

The migration moves tasks across **all queue types**:

| Queue Type | Data Structure | What Happens |
|-----------|---------------|-------------|
| Pending (priorities 0-9) | LIST | Task IDs moved preserving FIFO order |
| Delayed | Sorted Set | Task IDs moved with delay scores preserved |
| In-Progress | SET | Task IDs moved (active tasks continue) |
| DLQ | SET | Dead-letter task IDs moved |

For each task ID moved, the tool also copies:
- **Task data**: The full JSON task record from `codeq:tasks` hash
- **TTL index**: The retention expiry score from `codeq:tasks:ttl`

## Batch Processing

The `--batch-size` flag controls how many tasks are moved per atomic batch. The default of 1000 balances throughput and memory:

- **Smaller batches** (100-500): Less memory pressure, safer for constrained environments
- **Larger batches** (2000-5000): Faster migration, higher peak memory usage

```bash
# Smaller batches for constrained environments
codeq migrate-shards --config config.yaml \
    --command SEND_EMAIL --from-shard default --to-shard notification \
    --batch-size 200
```

## Rollback Procedure

If a migration needs to be reversed, run the migration in the opposite direction:

```bash
# Rollback: move tasks back to original shard
codeq migrate-shards \
    --config config.yaml \
    --command GENERATE_MASTER \
    --from-shard compute-shard \
    --to-shard default \
    --verify
```

Also update the `commandMappings` in your server configuration to route new tasks back to the original shard, then restart the server.

### Partial Migration Recovery

If a migration fails partway through (e.g., network error), some tasks may already be on the destination while still present on the source. Pending queue migration is **not idempotent** — re-running could insert duplicate task IDs into the destination list. To recover safely:

1. **Run a dry-run** to see remaining tasks on the source:
   ```bash
   codeq migrate-shards --config config.yaml \
       --command GENERATE_MASTER --from-shard default \
       --to-shard compute-shard --dry-run
   ```

2. **Rollback first, then re-run** — move everything back to the source, then perform a clean migration:
   ```bash
   # Move any tasks already on the destination back to source
   codeq migrate-shards --config config.yaml \
       --command GENERATE_MASTER --from-shard compute-shard \
       --to-shard default --verify

   # Re-run the full migration
   codeq migrate-shards --config config.yaml \
       --command GENERATE_MASTER --from-shard default \
       --to-shard compute-shard --verify
   ```

3. **Or complete forward** — re-run the migration to move remaining tasks. Be aware that a small number of tasks may appear on both shards if the previous run was interrupted between writing to the destination and removing from the source. Use `--verify` to confirm counts:
   ```bash
   codeq migrate-shards --config config.yaml \
       --command GENERATE_MASTER --from-shard default \
       --to-shard compute-shard --verify
   ```

> **Note:** Delayed queues (sorted sets) and DLQ (sets) are safe from duplicates since their underlying data structures deduplicate members. Only pending queues (lists) are susceptible to duplicates after an interrupted migration.

## Performance Expectations

| Metric | Typical Range | Notes |
|--------|--------------|-------|
| Throughput | 5,000-20,000 tasks/sec | Depends on network latency and Redis instance performance |
| Latency per batch | 1-5ms | Dominated by Redis round-trip time |
| Memory overhead | ~1KB per task in batch | Batch size × average task JSON size |
| Network traffic | ~1KB per task | Task data copied between shards |

### Recommendations

- **Schedule during low traffic**: Migration reads from pending queues, which temporarily reduces available tasks for workers on the source shard
- **Monitor queue depths**: Watch Prometheus metrics during migration to ensure workers aren't starved
- **Use smaller batch sizes for large tasks**: If task payloads are large (>10KB), reduce batch size to limit memory pressure

## Verification Details

The `--verify` flag checks:

1. **Source emptiness**: All queue keys for the migrated command should have zero remaining tasks
2. **Destination counts**: The destination shard should have the migrated task counts
3. **Shard health**: All configured shards respond to PING

A successful verification shows `✓ Migration verified successfully`. If verification fails, review the printed counts to identify which queue type has discrepancies.

## Troubleshooting

### "sharding is not enabled or no backends configured"

Ensure your config file has `sharding.enabled: true` and at least one backend defined.

### "source shard not found in config backends"

The `--from-shard` value must match a key in `sharding.backends`.

### "one or more shards are unhealthy"

The tool performs a health check before migration. Ensure all Redis instances are running and reachable.

### Migration is slow

- Increase `--batch-size` (e.g., `--batch-size 5000`)
- Ensure network latency between the CLI and Redis is low
- Check Redis instance load (CPU, memory, connections)

### Tasks appear duplicated after interrupted migration

Re-run the migration. The tool processes remaining tasks on the source. Use `--verify` to confirm correct distribution afterward.

## Related Documentation

- [Queue Sharding HLD](24-queue-sharding-hld.md) — Architecture and design for multi-shard deployments
- [Configuration Reference](14-configuration.md) — Full configuration options including sharding
- [Operational Runbooks](29-operational-runbooks.md) — Standard operational procedures
- [Troubleshooting](28-troubleshooting.md) — General troubleshooting guide
