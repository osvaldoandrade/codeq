# Consistency, failures, and clocks

## Consistency model

Each Redis command is linearizable, but workflows span multiple keys without a multi-key transaction. This yields at-least-once delivery and eventual convergence. codeQ favors availability and throughput over global ordering.

## Delivery semantics

- At-least-once delivery: duplicate processing is possible.
- No exactly-once guarantee.
- FIFO only within a command and priority tier.

## Failure windows

- If a crash occurs after the atomic claim move (Lua `RPOP` + `SADD`) and before `SETEX`, the task is stuck in in-progress without a lease. The requeue logic detects missing/expired leases and moves it back to ready.
- If a crash occurs after result persistence but before list cleanup, the result is authoritative and the list is repaired later.

## Clocks

Lease TTL is enforced by KVRocks and is authoritative. `leaseUntil` is advisory and derived from the service clock. Multi-instance deployments must tolerate clock skew. Timestamps in records are not guaranteed to be globally monotonic.

## Complexity evidence

Let C be the number of commands, L the scan limit, and N the list length.

- Enqueue: O(1)
- Claim: O(C * L) to scan and requeue + O(1) per successful pop
- Requeue: O(L) to scan leases (pipelined) + O(1) removal per expired task (`SREM`)
- Cleanup: O(k * m) where k is tasks deleted and m is the number of lists touched

These bounds follow Redis list, set, and sorted set complexity.
