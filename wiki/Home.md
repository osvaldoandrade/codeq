# codeQ Wiki

codeQ is a reactive scheduling and completion service built on persistent queues in KVRocks. Producers enqueue tasks under a `command` (event type). Workers pull tasks by command, claim ownership via a lease, and then either complete, fail, or nack the task for a delayed retry.

The product intent is simple: keep the queue semantics small and explicit, so workers can be dumb and horizontal scaling is safe. codeQ does not try to "assign work" to workers. Even when push is enabled, codeQ only sends advisory signals that work is available; the worker must still claim through the pull API.

## Quick Start Path

1. [Get Started](Get-Started)
2. [Overview](Overview)
3. [Architecture](Architecture)
4. [HTTP API](HTTP-API)
5. [Webhooks](Webhooks)

## Documentation Map

### Start Here

- [Get Started](Get-Started)
- [Overview](Overview)
- [Architecture](Architecture)

### Core Concepts

- [Domain Model](Domain-Model)
- [Queueing Model](Queueing-Model)
- [Storage (KVRocks)](Storage-KVRocks)
- [Consistency](Consistency)
- [Retry and Backoff](Retry-and-Backoff)
- [Webhooks](Webhooks)
- [Security](Security)

### Interfaces

- [HTTP API](HTTP-API)
- [CLI](CLI)
- [Configuration](Configuration)

### Operations

- [Operations](Operations)
- [Migration](Migration)
- [Sharding](Sharding)
- [Examples](Examples)

### Use Cases

- [Use Cases](Use-Cases)
