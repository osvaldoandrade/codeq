# Storage layout (KVRocks)

KVRocks implements the Redis protocol and persists to disk. codeQ uses lists, hashes, sorted sets, sets, and TTL keys. Each operation is atomic at the command level.

## Keyspace

All keys are prefixed with `codeq:`.

- `codeq:tasks` (hash): field = task ID, value = task JSON.
- `codeq:results` (hash): field = task ID, value = result JSON.
- `codeq:tasks:ttl` (ZSET): member = task ID, score = retention cutoff epoch seconds.
- `codeq:q:<command>:<tenantID>:pending:<priority>` (list)
- `codeq:q:<command>:<tenantID>:inprog` (set)
- `codeq:q:<command>:<tenantID>:delayed` (ZSET)
- `codeq:q:<command>:<tenantID>:dlq` (set)
- `codeq:lease:<id>` (string)
- `codeq:idempo:<key>` (string)
- `codeq:subs:<event>` (ZSET): webhook subscriptions with TTL score

### Queue Key Patterns

Queue keys include the tenant ID to provide complete isolation between tenants:

- **With tenant ID**: `codeq:q:{command}:{tenantID}:pending:{priority}`
- **Without tenant ID** (legacy/backward compatibility): `codeq:q:{command}:pending:{priority}`

The tenant ID segment is omitted for backward compatibility when `tenantID` is empty. This allows existing single-tenant deployments to continue working without migration.

## Command usage

- Hash: `HSET`, `HGET`, `HDEL`
- Lists: `LPUSH`, `RPOP`, `LLEN`, `LREM` (pending)
- Sets: `SADD`, `SREM`, `SCARD`, `SRANDMEMBER` (in-progress tracking + DLQ)
- ZSET: `ZADD`, `ZRANGEBYSCORE`, `ZREM`
- Keys: `SETEX`, `TTL`, `EXPIRE`, `DEL`
- Lua: `EVALSHA` (atomic claim move: `RPOP` + `SADD`); `EVAL` (fallback for kvrocks)

### Lua scripts

codeQ uses Lua scripts for atomicity in critical paths:

1. **Claim move script** (`claimMoveScript` in `task_repository.go`): Atomically moves a task from the pending queue to in-progress state using `RPOP` and `SADD`.
2. **Rate limiter script** (`tokenBucketScript` in `token_bucket.go`): Token bucket algorithm for rate limiting across distributed workers.

**Script preloading (kvrocks compatibility):**

codeQ preloads Lua scripts at startup via `PreloadScripts()` calls in `application.go`. This is necessary because kvrocks (a Redis fork) returns `NOSCRIPT` errors in a different format than standard Redis. The preload ensures that subsequent `EVALSHA` calls always hit the cached script.

If preloading fails or a script is evicted, codeQ automatically falls back to `EVAL` (loading the full script on demand). This fallback is transparent to callers and maintains correctness.

**Implementation detail:** When a kvrocks instance returns a `NOSCRIPT` error, codeQ detects it by checking for the string `"NOSCRIPT"` in the error message. It then retries using `EVAL` instead of `EVALSHA`.

## Retention

Tasks are retained for 24 hours and removed by admin cleanup. Cleanup removes task records, results, leases, and queue entries. Retention does not use Redis key TTL for task records to avoid accidental deletions.
