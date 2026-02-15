# Migration: legacy scheduler → codeQ

This document defines a safe migration path from the legacy scheduler/results storage to codeQ. codeQ uses a different key prefix (`codeq:`) and a slightly different queue layout (priority tiers, delayed/DLQ, results hash). There is no automatic migration in the service; operators must migrate keys or drain old queues before cutover.

## Compatibility notes

- API base path changes from `/v1/scheduler` to `/v1/codeq`.
- Worker auth moves from producer tokens to worker JWTs (JWKS validation). Producer auth uses Tikti access tokens (JWKS validation).
- Storage prefix changes from `legacy:` to `codeq:`.
- Pending queue keys now include priority tier: `pending:<priority>`.
- Result records are stored in `codeq:results` (new).

## ⚠️ Breaking Changes

**In-progress queue data structure change (v1.1.0+)**:

The in-progress queue changed from Redis LIST to SET for O(1) removal performance:
- **Old**: `codeq:q:<command>:inprog` was a LIST (RPOPLPUSH, LREM)
- **New**: `codeq:q:<command>:inprog` is a SET (RPOP + SADD via Lua, SREM)

**Migration impact**: Existing deployments with in-progress tasks **must drain or convert** before upgrading:
- **Recommended**: Drain all in-progress queues before upgrade (Option A below)
- **Alternative**: Manual conversion using `LRANGE` + `SADD` + `DEL` during freeze window

This change significantly improves claim-time repair performance by eliminating O(N) LREM operations.

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
2. **Let workers drain** the legacy queues until pending and in-progress queues are empty.
3. **Deploy codeQ** and point producers/workers to `/v1/codeq`.
4. **Verify** new tasks are stored under `codeq:`.

This approach avoids key renames and is lowest risk but requires a write freeze window.

### Option B: in-place key migration

Use this when you cannot wait for a full drain. It requires a brief write freeze to keep keys consistent during the rename/copy.

1. **Freeze producers/workers** hitting the legacy scheduler.
2. **Copy or rename keys** according to the mapping table. For pending lists, rename to `pending:0`. For `inprog`, copy IDs from the legacy list into the new set using `SADD` (a list cannot be renamed into a set).
3. **Start codeQ** with the new prefix.
4. **Unfreeze clients** and verify queue lengths.

Recommended approach is `DUMP/RESTORE` or `RENAMENX` per key category in a controlled script. The operation is **O(N)** over the number of keys and list items.

## Rollback plan

If codeQ fails after cutover:

1. Freeze clients.
2. Restore the legacy keyspace from backup (or keep a preserved copy).
3. Point clients back to the legacy scheduler.

## Verification checklist

- `codeq:tasks` hash contains task JSON records.
- `codeq:q:<command>:pending:<priority>` lists match expected counts.
- `codeq:lease:<id>` keys exist for in-progress tasks.
- `GET /v1/codeq/admin/queues` returns non-zero counts when expected.
- Sample task can be claimed, nacked, re-claimed, and completed.

## Timing and complexity

- Rename/copy operations are linear in the number of keys and list entries.
- Large queues should be migrated during a low-traffic window.
- Backup/restore time is bounded by KVRocks disk throughput and dataset size.
