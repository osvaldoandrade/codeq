# Use Case: Admin Cleanup

This flow removes expired tasks and associated records from KVRocks.

codeQ retains tasks for a bounded window (default 24h) and deletes them via an explicit admin operation.

## Preconditions

- Caller is authorized as admin.
- A cutoff timestamp is chosen (typically "now - retention").

## Main flow

1. Operator calls `POST /v1/codeq/admin/tasks/cleanup`.
2. codeQ scans the retention index (`codeq:tasks:ttl`) for tasks whose retention cutoff is <= the requested cutoff.
3. For each selected task (bounded by `limit`), codeQ removes:
   - task record
   - result record
   - lease key
   - idempotency mapping (if present)
   - queue entries (pending, in-progress, delayed, DLQ)
4. The endpoint returns a summary of deletions.

## Sequence diagram

```mermaid
sequenceDiagram
  participant O as Operator
  participant Q as codeQ
  participant K as KVRocks

  O->>Q: POST /admin/tasks/cleanup
  Q->>K: ZRANGEBYSCORE tasks:ttl (<= cutoff)
  loop for each task up to limit
    Q->>K: HDEL tasks + HDEL results
    Q->>K: DEL lease + DEL idempo
    Q->>K: LREM pending/inprog + ZREM delayed + LREM dlq
    Q->>K: ZREM tasks:ttl
  end
  Q-->>O: 200 OK (summary)
```
