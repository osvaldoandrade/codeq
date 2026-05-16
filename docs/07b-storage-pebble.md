# Storage layout (Pebble)

Pebble is an embedded key-value store (CockroachDB's LSM engine) that runs inside the codeQ process. Unlike Redis/KVRocks, it requires no external dependencies and is ideal for local development, testing, and small deployments.

## Keyspace

Pebble uses a hierarchical key structure with byte-prefixes. All keys follow the pattern:

- `codeq/{command}/{tenantID}/...` (hierarchical prefix organization)

Data structures stored:

- **Tasks hash**: Task metadata keyed by task ID
- **Results hash**: Task results keyed by task ID
- **Pending queues**: Priority-ordered lists per (command, tenantID, priority)
- **In-progress set**: Task IDs currently claimed for execution
- **Delayed queue**: ZSET for delayed task delivery with scores as epoch timestamps
- **Dead Letter Queue (DLQ)**: Set of task IDs exceeding `maxAttempts`
- **Leases**: Ephemeral records tracking worker ownership (key: `codeq/lease/{leaseID}`)
- **Idempotency cache**: Deduplication records (key: `codeq/idempo/{idempotencyKey}`)
- **Subscriptions**: Webhook registrations with TTL scores (key: `codeq/subs/{event}`)

### Key Organization

Pebble keys use a flat namespace with hierarchical prefixes optimized for LSM-tree range scans:

- **Pending queue**: `codeq/q/{command}/{tenantID}/pending/{priority}/{seq}/{id}`
  - `{seq}`: 8-byte big-endian sequence number (enables FIFO ordering within priority)
  - `{id}`: Task ID (unique per task)
- **In-progress tracking**: `codeq/q/{command}/{tenantID}/inprog/{id}`
- **Delayed tasks**: `codeq/q/{command}/{tenantID}/delayed/{id}` (sorted by timestamp score)
- **DLQ**: `codeq/q/{command}/{tenantID}/dlq/{id}`
- **Task data**: `codeq/tasks/{id}` (JSON-encoded task metadata)
- **Result data**: `codeq/results/{id}` (JSON-encoded result)
- **TTL tracking**: `codeq/tasks/ttl/{id}` (retention epoch seconds)
- **Leases**: `codeq/lease/{id}`
- **Idempotency**: `codeq/idempo/{key}`
- **Subscriptions**: `codeq/subs/{event}/{id}`

## Multi-tenancy

Like Redis storage, Pebble enforces tenant isolation via tenant ID inclusion in all queue keys. When `tenantID` is empty (legacy mode), it is omitted from keys for backward compatibility.

## Sequence Numbers

Pebble uses process-wide sequence counters (atomic 64-bit integers) to assign unique ordering to pending queue entries within a priority bucket. This ensures FIFO ordering even after restart.

On startup, Pebble:

1. Scans all pending queue keys
2. Extracts sequence numbers from keys
3. Recovers the high-water mark (max sequence seen)
4. Sets internal counter to max+1

This recovery is linear in the pending queue size; for extremely large queues (millions of entries), consider chunking.

## Atomicity

Pebble commits are atomic at the batch level. A batch can contain multiple puts, deletes, or range operations that commit as a single unit. codeQ uses batches for:

- Task finalization (save result + remove from in-progress + delete lease + update TTL)
- Lease repair (multiple TTL checks and requeue operations)
- Bulk operations (batch enqueue, batch result submission)

## Bloom Filters and Caching

Pebble's LSM-tree is tuned for codeQ's workload:

- **Block cache**: 256 MiB (configurable) for hot data
- **Bloom filters**: 10 bits per key (~1% false positive rate) on all levels to speed up negative lookups
- **Compression**: Snappy (default) on lower levels

## Retention and Cleanup

Tasks are retained for 24 hours (configurable). The cleanup process:

1. Scans TTL index for expired entries
2. Removes task records, results, leases, and queue entries
3. Compacts the LSM tree to reclaim space

Cleanup runs periodically and can be triggered manually via admin API.

## Single-writer Property

Pebble's embedded design means only one process may hold the database lock at a time. This is enforced at the OS level (flock on the database directory). For distributed deployments, use Redis/KVRocks instead.

## Performance Characteristics

- **Point lookups**: O(1) amortized, cached or single-level read
- **Range scans**: O(log N) for initial seek + O(K) for K results
- **Writes**: O(1) amortized with write batching
- **Compaction**: Background, ~10-15% slowdown during heavy merging

## Comparison with Redis/KVRocks

| Feature | Pebble | Redis/KVRocks |
|---------|--------|---------------|
| External dependency | No (embedded) | Yes (server) |
| Multi-process access | No (exclusive lock) | Yes (shared) |
| Distribution | Single machine only | Cluster capable |
| Deployment simplicity | High (zero setup) | Medium (standalone service) |
| Memory footprint | ~300 MiB (configurable) | Variable (depends on data) |
| Persistence | Automatic to disk | Configurable RDB/AOF |
| Replication | Manual (backup only) | Built-in cluster support |

## When to Use Pebble

- **Local development**: Zero setup, instant startup
- **Testing**: Isolated databases, cleanup on process exit
- **Single-machine production**: Small deployments, high availability via external failover
- **Prototype**: Quick validation before Redis scale-out

## When to Use Redis/KVRocks

- **Distributed systems**: Multiple codeQ instances sharing one queue
- **High availability**: Cluster replication and failover
- **Large scale**: Horizontal scaling, sharding support
- **Existing infrastructure**: Leverage existing Redis/KVRocks clusters
