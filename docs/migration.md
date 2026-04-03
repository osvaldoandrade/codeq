# Migration: legacy scheduler → codeQ

This document defines a safe migration path from the legacy scheduler/results storage to codeQ. codeQ uses a different key prefix (`codeq:`) and a slightly different queue layout (priority tiers, delayed/DLQ, results hash). There is no automatic migration in the service; operators must migrate keys or drain old queues before cutover.

## Compatibility notes

- API base path changes from `/v1/scheduler` to `/v1/codeq`.
- Worker auth moves from producer tokens to worker JWTs (JWKS validation). Producer auth uses Tikti access tokens (JWKS validation).
- Storage prefix changes from `legacy:` to `codeq:`.
- Pending queue keys now include priority tier: `pending:<priority>`.
- Result records are stored in `codeq:results` (new).

## ⚠️ Breaking Changes

### In-progress queue data structure change (v1.1.0+)

The in-progress queue changed from Redis LIST to SET for O(1) removal performance:
- **Old**: `codeq:q:<command>:inprog` was a LIST (RPOPLPUSH, LREM)
- **New**: `codeq:q:<command>:inprog` is a SET (RPOP + SADD via Lua, SREM)

**Migration impact**: Existing deployments with in-progress tasks **must drain or convert** before upgrading:
- **Recommended**: Drain all in-progress queues before upgrade (Option A below)
- **Alternative**: Manual conversion using `LRANGE` + `SADD` + `DEL` during freeze window

This change significantly improves claim-time repair performance by eliminating O(N) LREM operations.

**In-progress conversion example:**
```bash
# For each command queue, convert the in-progress LIST to SET:
COMMAND="generate_master"
redis-cli RENAME "codeq:q:${COMMAND}:inprog" "codeq:q:${COMMAND}:inprog_list"
for id in $(redis-cli LRANGE "codeq:q:${COMMAND}:inprog_list" 0 -1); do
  redis-cli SADD "codeq:q:${COMMAND}:inprog" "$id"
done
redis-cli DEL "codeq:q:${COMMAND}:inprog_list"
```

### DLQ data structure change

The DLQ changed from Redis LIST to SET for O(1) removal performance during admin cleanup:
- **Old**: `codeq:q:<command>:dlq` was a LIST (LPUSH, LLEN, LREM)
- **New**: `codeq:q:<command>:dlq` is a SET (SADD, SCARD, SREM)

**Migration impact**: Existing deployments with DLQ entries **must drain or convert** before upgrading:
- **Recommended**: Drain DLQ before upgrade (Option A below)
- **Alternative**: Manual conversion during freeze window:
  ```bash
  # For each command queue, convert the DLQ LIST to SET:
  COMMAND="generate_master"
  redis-cli RENAME "codeq:q:${COMMAND}:dlq" "codeq:q:${COMMAND}:dlq_list"
  for id in $(redis-cli LRANGE "codeq:q:${COMMAND}:dlq_list" 0 -1); do
    redis-cli SADD "codeq:q:${COMMAND}:dlq" "$id"
  done
  redis-cli DEL "codeq:q:${COMMAND}:dlq_list"
  ```

### Operation mapping

| Operation | Old (LIST) | New (SET) |
|---|---|---|
| Add to DLQ | `LPUSH` | `SADD` |
| DLQ depth | `LLEN` | `SCARD` |
| Remove from DLQ | `LREM` O(N) | `SREM` O(1) |
| Add to in-progress | `RPOPLPUSH` | `RPOP` + `SADD` (Lua) |
| Remove from in-progress | `LREM` O(N) | `SREM` O(1) |

> **Note**: The SET data structure also prevents duplicate task IDs in the DLQ and in-progress queues, which was a potential issue with the LIST-based approach.

## Key mapping

| Legacy key | New key | Notes |
|---|---|---|
| `legacy:tasks` | `codeq:tasks` | Task JSON is compatible; new fields default on use. |
| `legacy:tasks:ttl` | `codeq:tasks:ttl` | Retention index; score is epoch seconds. |
| `legacy:lease:<id>` | `codeq:lease:<id>` | Same format; prefix changes only. |
| `legacy:q:<command>:pending` | `codeq:q:<command>:pending:0` | Priority tiers default to `0`. |
| `legacy:q:<command>:inprog` | `codeq:q:<command>:inprog` | Convert list → set (store the same IDs via `SADD`). |
| _none_ | `codeq:q:<command>:delayed` | New; used for retries. |
| _none_ | `codeq:q:<command>:dlq` | New; used when `maxAttempts` exceeded. |
| _none_ | `codeq:results` | New results hash. |

## Migration options

### Option A: drain and cut over (recommended)

1. **Freeze producers** that write to the legacy scheduler.
2. **Let workers drain** the legacy queues until pending lists, in-progress sets, and DLQ are empty.
3. **Verify** all queues are drained:
   ```bash
   # Check pending queues
   redis-cli LLEN codeq:q:<command>:pending:0
   # Check in-progress (old LIST or new SET)
   redis-cli LLEN codeq:q:<command>:inprog    # returns 0 if drained or key is now a SET
   redis-cli SCARD codeq:q:<command>:inprog   # returns 0 if drained
   # Check DLQ (old LIST or new SET)
   redis-cli LLEN codeq:q:<command>:dlq       # returns 0 if drained or key is now a SET
   redis-cli SCARD codeq:q:<command>:dlq      # returns 0 if drained
   ```
4. **Deploy codeQ** and point producers/workers to `/v1/codeq`.
5. **Verify** new tasks are stored under `codeq:` using the verification checklist below.

This approach avoids key renames and is lowest risk but requires a write freeze window.

### Option B: in-place key migration

Use this when you cannot wait for a full drain. It requires a brief write freeze to keep keys consistent during the rename/copy.

1. **Freeze producers/workers** hitting the legacy scheduler.
2. **Back up the keyspace** before making any changes:
   ```bash
   redis-cli BGSAVE
   ```
3. **Copy or rename keys** according to the mapping table:
   - For pending lists: `RENAME` to `pending:0`.
   - For `inprog` and `dlq` (LIST → SET conversion):
     ```bash
     # Convert in-progress LIST to SET
     COMMAND="generate_master"
     redis-cli RENAME "codeq:q:${COMMAND}:inprog" "codeq:q:${COMMAND}:inprog_list"
     for id in $(redis-cli LRANGE "codeq:q:${COMMAND}:inprog_list" 0 -1); do
       redis-cli SADD "codeq:q:${COMMAND}:inprog" "$id"
     done
     redis-cli DEL "codeq:q:${COMMAND}:inprog_list"

     # Convert DLQ LIST to SET
     redis-cli RENAME "codeq:q:${COMMAND}:dlq" "codeq:q:${COMMAND}:dlq_list"
     for id in $(redis-cli LRANGE "codeq:q:${COMMAND}:dlq_list" 0 -1); do
       redis-cli SADD "codeq:q:${COMMAND}:dlq" "$id"
     done
     redis-cli DEL "codeq:q:${COMMAND}:dlq_list"
     ```
4. **Verify** converted key types:
   ```bash
   redis-cli TYPE "codeq:q:${COMMAND}:inprog"  # should return "set"
   redis-cli TYPE "codeq:q:${COMMAND}:dlq"      # should return "set"
   ```
5. **Start codeQ** with the new prefix.
6. **Unfreeze clients** and verify queue lengths using the verification checklist below.

The operation is **O(N)** over the number of keys and list items. For large DLQs (>10,000 entries), consider batching the `LRANGE` + `SADD` in chunks of 1,000 to avoid blocking Redis.

## Rollback plan

If codeQ fails after cutover:

1. **Freeze clients** immediately.
2. **Convert SET keys back to LIST** (if Option B was used):
   ```bash
   COMMAND="generate_master"
   # Rollback DLQ: SET → LIST
   for id in $(redis-cli SMEMBERS "codeq:q:${COMMAND}:dlq"); do
     redis-cli LPUSH "codeq:q:${COMMAND}:dlq_rollback" "$id"
   done
   redis-cli DEL "codeq:q:${COMMAND}:dlq"
   redis-cli RENAME "codeq:q:${COMMAND}:dlq_rollback" "codeq:q:${COMMAND}:dlq"

   # Rollback in-progress: SET → LIST
   for id in $(redis-cli SMEMBERS "codeq:q:${COMMAND}:inprog"); do
     redis-cli LPUSH "codeq:q:${COMMAND}:inprog_rollback" "$id"
   done
   redis-cli DEL "codeq:q:${COMMAND}:inprog"
   redis-cli RENAME "codeq:q:${COMMAND}:inprog_rollback" "codeq:q:${COMMAND}:inprog"
   ```
3. Or **restore** the legacy keyspace from the backup taken before migration.
4. **Point clients** back to the legacy scheduler.
5. **Verify** that the legacy system is processing tasks correctly.

## Verification checklist

After migration, verify each item before unblocking traffic:

- [ ] **Task storage**: `redis-cli HLEN codeq:tasks` returns expected number of task records.
- [ ] **Pending queues**: `redis-cli LLEN codeq:q:<command>:pending:<priority>` matches expected counts.
- [ ] **In-progress type**: `redis-cli TYPE codeq:q:<command>:inprog` returns `set` (not `list`).
- [ ] **DLQ type**: `redis-cli TYPE codeq:q:<command>:dlq` returns `set` (not `list`).
- [ ] **DLQ count**: `redis-cli SCARD codeq:q:<command>:dlq` matches pre-migration `LLEN` count.
- [ ] **Lease keys**: `redis-cli EXISTS codeq:lease:<task-id>` returns `1` for any in-progress tasks.
- [ ] **Admin API**: `GET /v1/codeq/admin/queues` returns non-zero counts when expected.
- [ ] **End-to-end**: Sample task can be created, claimed, nacked, re-claimed, and completed.
- [ ] **No legacy keys**: `redis-cli KEYS legacy:*` returns empty (if migrating from legacy prefix).

## Timing and complexity

- Rename/copy operations are linear in the number of keys and list entries.
- Large queues should be migrated during a low-traffic window.
- Backup/restore time is bounded by KVRocks disk throughput and dataset size.
- **Estimated migration time**: ~1 minute per 100,000 DLQ/in-progress entries on a typical Redis instance.
