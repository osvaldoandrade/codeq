# Overview

codeQ is a task scheduling and completion service built on persistent queues in KVRocks. The service exposes **HTTP REST APIs** and **high-performance gRPC streaming APIs** for producers to create tasks, for workers to claim and complete tasks, and for operators to inspect and clean up queues. The system is reactive: workers pull tasks, and optionally receive webhook signals that work is available.

The design is inspired by Dyno Queues, which adds time-based and priority queues on top of Dynomite. Dynomite is a distributed data layer that exposes Redis semantics and provides multi-datacenter replication. codeQ applies the same motivation to KVRocks and uses Go for the implementation. The service favors availability and throughput over strict global ordering.

## APIs

- **HTTP REST API** (`docs/04-http-api.md`): Simple JSON-based API for task management, ideal for prototyping and low-frequency operations.
- **gRPC Streaming API** (`docs/34-streaming-api-guide.md`): Bidirectional streams for high-throughput producers and workers, achieving 2–3× better throughput than REST via pipelining and authentication amortization.

## Goals

- Provide a stable API for enqueue, claim, and completion.
- Persist state on disk via KVRocks.
- Support delayed retries, priority, and retries.
- Allow workers to pull by event type without a worker registry.
- Provide optional push notifications without assigning work.

## Non-goals

- Exactly-once processing.
- Global FIFO across all commands.
- Automatic worker discovery or scheduling.

## High-level data model

- Task: unit of work, identified by UUID.
- Message: queue element containing metadata used by the scheduler.
- Result: completion record stored independently of the task body.
- Command (event type): routing key for tasks.
- Shard: not implemented; queues are single-shard in the current service.
