# Retry and backoff

## Retry triggers

A retry occurs when:

- a lease expires before completion
- the worker calls an explicit `nack` endpoint

The task `attempts` counter increments on each claim.

## Backoff policies

codeQ supports the following policies per event type or task:

- fixed: `delay = fixedSeconds`
- linear: `delay = baseSeconds * attempts`
- exponential: `delay = min(maxSeconds, baseSeconds * 2^attempts)`
- exponential with full jitter: `delay = rand(0, min(maxSeconds, baseSeconds * 2^attempts))`
- exponential with equal jitter: `delay = min(maxSeconds, baseSeconds * 2^attempts)/2 + rand(0, min(maxSeconds, baseSeconds * 2^attempts)/2)`

Default policy:

- baseSeconds = 5
- maxSeconds = 900
- policy = exponential with full jitter

## Dead-letter policy

When `attempts >= maxAttempts`, the task is moved to the DLQ and marked `FAILED` with `error=MAX_ATTEMPTS`. This prevents infinite retries.

## NACK semantics

`POST /v1/codeq/tasks/:id/nack` triggers a retry. The server:

1. Verifies ownership and `IN_PROGRESS` status.
2. Increments `attempts`.
3. If `attempts >= maxAttempts`, moves the task to DLQ and marks `FAILED`.
4. Otherwise computes `delaySeconds`:
   - If the request includes `delaySeconds`, use `min(delaySeconds, backoffMaxSeconds)`.
   - Else compute delay using the configured backoff policy and cap at `backoffMaxSeconds`.
5. Moves the task to the delayed queue with `visibleAt = now + delaySeconds`.
6. Clears the lease and removes the task from in-progress.

Lease expiry uses the same retry logic with a computed delay and `retryReason=LEASE_EXPIRED`.
