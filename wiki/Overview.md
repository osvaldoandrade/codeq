# Overview

codeQ is a queueing and completion service for event-driven systems that need one thing above all: work must survive.

If you have producers emitting events like `render_video`, `generate_master`, or `send_email`, and you have a fleet of workers that comes and goes with deploys, crashes, and autoscaling, you want a small set of semantics you can build systems on: enqueue work, claim ownership, keep a lease alive while you execute, and write down a terminal outcome.

That is the story codeQ implements.

## The Story: From Event to Result

A producer creates a task under a `command` (an event type). The payload is stored as a JSON string, because the queue should not care about business objects, only about durable transport and scheduling metadata.

Workers do not register, and codeQ does not schedule them. A worker simply asks: "do you have any tasks for commands I am allowed to execute?" When a claim succeeds, codeQ records ownership (`workerId`) and creates a lease with a TTL enforced by KVRocks. While the worker is executing, it may heartbeat the lease. When it finishes, it submits a terminal result: `COMPLETED` with an application-specific result object, or `FAILED` with a string error.

Terminal means terminal. If you want to retry, you do not complete and then retry; you `nack` the task before it becomes terminal. NACK transitions the task back into the delayed queue and applies a backoff policy. If the task exceeds `maxAttempts`, codeQ moves it to DLQ and records `MAX_ATTEMPTS` as the terminal failure reason.

If a worker disappears mid-flight, the lease expires. codeQ intentionally does not depend on a background scanner to detect that. Instead, it performs bounded repair during claim operations: when workers claim, codeQ opportunistically scans a limited window of in-progress tasks and requeues the ones whose leases are missing or expired. This keeps the system operationally small while still converging to "stuck tasks become claimable again."

## Push Exists, But It Never Assigns

codeQ is pull-first. Push is an optimization for latency and idle polling, not a correctness mechanism.

There are two push paths: worker availability notifications (advisory webhook signals that a command has work available, where the worker must still claim and the notification does not change ownership) and result callbacks (task-level webhooks that fire on terminal completion so producers can avoid polling `GET /tasks/:id/result`).

Both are intentionally best-effort and should be treated as at-least-once delivery. Consumers must deduplicate by `taskId` when it matters. See [Webhooks](Webhooks).

## What You Can Rely On (Invariants)

codeQ is designed so you can reason about it without reading the implementation. Delivery is at-least-once, which means duplicate processing is possible and task handlers must be idempotent when side effects matter. The lease TTL in KVRocks is authoritative (derived timestamps in task records are advisory). Ownership is explicit and bound to the worker token `sub`, so a different `sub` cannot heartbeat or complete a task it does not own. And queues are durable: task records, results, and scheduling metadata persist on disk through KVRocks.

These invariants are the foundation for the rest of the spec: [Queueing Model](Queueing-Model), [Consistency](Consistency), and [Retry and Backoff](Retry-and-Backoff).

## How This Stays Fast

Most operations map to a small number of Redis/KVRocks primitives (lists, hashes, sorted sets, TTL keys). Enqueue and successful claim are constant-time. The only scanning is bounded (for example by `requeueInspectLimit`) and happens during claim so the system converges without requiring a separate repair service. See [Storage (KVRocks)](Storage-KVRocks).

## Security in One Sentence

Producers authenticate via Tikti access tokens (JWKS) and workers authenticate via JWT (JWKS). The worker token carries two independent allowlists: `eventTypes` (which commands it may claim) and `scope` (which endpoints it may call). See [Security](Security).

## Reading Path

If you want to understand codeQ end-to-end, read [Architecture](Architecture) then [HTTP API](HTTP-API), followed by [Queueing Model](Queueing-Model), [Webhooks](Webhooks), and [Security](Security).
