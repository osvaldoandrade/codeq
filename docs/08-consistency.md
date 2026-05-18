# Consistency, failures, and clocks

## Consistency model

Each Redis command is linearizable, but workflows span multiple keys without a multi-key transaction. This yields at-least-once delivery and eventual convergence. codeQ favors availability and throughput over global ordering.

**Exception:** Result finalization uses atomic `MULTI/EXEC` (TxPipeline) to prevent task resurrection. See [Performance Tuning § Result Submission Race Condition](17-performance-tuning.md#15-result-submission-race-condition-atomic-finalization) for details.

## Delivery semantics

- At-least-once delivery: duplicate processing is possible.
- No exactly-once guarantee.
- FIFO only within a command and priority tier.

## Failure windows

- If a crash occurs after the atomic claim move (Lua `RPOP` + `SADD`) and before `SETEX`, the task is stuck in in-progress without a lease. The requeue logic detects missing/expired leases and moves it back to ready.
- If a crash occurs after result persistence but before atomic finalization (`MULTI/EXEC`), the service will retry the finalization on the next heartbeat or manual recovery. The result is authoritative and the list is repaired atomically.

## Clocks

Lease expiry is enforced by Pebble's in-memory lease table plus the reaper sweep (`internal/repository/pebble/reaper.go::sweepLeases`); the persisted `LeaseUntil` field on the task body is authoritative across restarts. The service-clock `leaseUntil` returned to workers is advisory and may drift slightly under clock skew. Timestamps in records are not guaranteed to be globally monotonic.

## Complexity evidence

Let C be the number of commands, L the scan limit, and N the list length.

- Enqueue: O(1)
- Claim: O(C * L) to scan and requeue + O(1) per successful pop
- Requeue: O(L) to scan leases (pipelined) + O(1) removal per expired task (`SREM`)
- Cleanup: O(k * m) where k is tasks deleted and m is the number of lists touched

These bounds follow Redis list, set, and sorted set complexity.
