# Storage layout (KVRocks)

KVRocks implements the Redis protocol and persists to disk. codeQ uses lists, hashes, sorted sets, sets, and TTL keys. Each operation is atomic at the command level.

## Keyspace

All keys are prefixed with `codeq:`.

- `codeq:tasks` (hash): field = task ID, value = task JSON.
- `codeq:results` (hash): field = task ID, value = result JSON.
- `codeq:tasks:ttl` (ZSET): member = task ID, score = retention cutoff epoch seconds.
- `codeq:q:<command>:pending:<priority>` (list)
- `codeq:q:<command>:inprog` (set)
- `codeq:q:<command>:delayed` (ZSET)
- `codeq:q:<command>:dlq` (list)
- `codeq:lease:<id>` (string)
- `codeq:idempo:<key>` (string)
- `codeq:subs:<event>` (ZSET): webhook subscriptions with TTL score

## Command usage

- Hash: `HSET`, `HGET`, `HDEL`
- Lists: `LPUSH`, `RPOP`, `LLEN`, `LREM` (pending + dlq)
- Sets: `SADD`, `SREM`, `SCARD`, `SRANDMEMBER` (in-progress tracking)
- ZSET: `ZADD`, `ZRANGEBYSCORE`, `ZREM`
- Keys: `SETEX`, `TTL`, `EXPIRE`, `DEL`
- Lua: `EVAL` (atomic claim move: `RPOP` + `SADD`)

## Retention

Tasks are retained for 24 hours and removed by admin cleanup. Cleanup removes task records, results, leases, and queue entries. Retention does not use Redis key TTL for task records to avoid accidental deletions.
