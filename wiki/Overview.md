# Overview

codeQ is a queueing and completion service for event-driven systems.

A producer enqueues a task under a `command` (event type). A worker claims work by asking for one or more commands it is willing to execute. Claiming creates a lease and records ownership (`workerId`). The worker then heartbeats the lease while it is executing, and eventually submits a terminal result (`COMPLETED` or `FAILED`).

The system is reactive and pull-first: workers pull tasks. Push exists only to reduce latency and idle polling. When enabled, codeQ sends advisory webhook notifications that work is available, but it never assigns tasks through webhooks.

## What codeQ Optimizes For

codeQ is designed to be operationally small and predictable:

- The API surface is intentionally narrow: enqueue, claim, heartbeat, abandon, result, nack.
- Scheduling metadata is persisted in KVRocks with simple Redis primitives (lists, hashes, sorted sets, TTL keys).
- The hot-path operations are O(1) for enqueue and for a successful claim.

## Goals and Non-Goals

Goals:

- Stable HTTP APIs for enqueue, claim, and completion.
- Persistence on SSD via KVRocks.
- Delayed retries, priority tiers, and a dead-letter policy (`maxAttempts`).
- Workers can pull by event type without a worker registry.
- Optional push signals and result callbacks to reduce GET polling.

Non-goals:

- Exactly-once processing.
- Global FIFO across commands.
- Automatic worker discovery or active task assignment.

## Core Entities

- **Task**: the unit of work (UUID), with status and ownership.
- **Message**: the scheduling view of a task (command, priority, payload as string).
- **Result**: terminal completion record stored independently of task body.
- **Command**: routing key (event type) used by both producers and workers.

## Delivery Semantics

codeQ provides at-least-once delivery.

A lease is authoritative by KVRocks TTL. If a worker crashes, loses the lease, or explicitly nacks, the task is eligible to be retried. As a result, duplicate processing is possible and consumers must make task handlers idempotent when side effects matter.

If you need push behavior, use [Webhooks](Webhooks) as a latency optimization, not as a correctness mechanism.
