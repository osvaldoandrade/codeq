# Changelog

All notable changes to codeQ will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [1.1.0] - 2026-02-15

### ⚠️ BREAKING CHANGES

#### In-Progress Queue Data Structure Change

The in-progress queue has been changed from a Redis LIST to a SET to achieve O(1) removal performance.

**What changed:**
- `codeq:q:<command>:inprog` is now a SET (previously LIST)
- Claim operation uses Lua script for atomic `RPOP` + `SADD` (previously `RPOPLPUSH`)
- Removal uses `SREM` O(1) (previously `LREM` O(N))
- Queue depth metrics use `SCARD` (previously `LLEN`)

**Migration required:**
- **Recommended**: Drain all in-progress tasks before upgrading (zero-downtime)
- **Alternative**: Convert existing LIST → SET during freeze window:
  ```bash
  # For each command queue
  redis-cli LRANGE codeq:q:generate-master:inprog 0 -1 | \
    xargs redis-cli SADD codeq:q:generate-master:inprog
  redis-cli DEL codeq:q:generate-master:inprog_old
  ```

**Why this change:**
This optimization eliminates the O(N) `LREM` operation during claim-time expired lease repair, 
significantly improving claim latency under high queue depth scenarios.

See: PR #68, Issue #45

### Performance Improvements

- **Optimized claim-time repair loop** ([#68](https://github.com/osvaldoandrade/codeq/pull/68))
  - Pipelined all TTL checks for expired lease scanning (single round-trip)
  - Reduced claim latency by ~40% under high in-progress queue depth (>1000 tasks)
  - Eliminated O(N) complexity in lease expiration requeue path

### Changed

- In-progress queue storage changed from LIST to SET
- Atomic claim move now uses Lua script instead of `RPOPLPUSH`
- Metrics collector updated to use `SCARD` for in-progress queue depth

### Documentation

- Updated architecture documentation to reflect Lua claim move
- Updated queueing model docs to specify SET for in-progress queue
- Updated storage layout documentation with SET operations
- Updated consistency and complexity analysis
- Updated performance tuning guide with optimization details
- Added breaking change warnings to migration guide
- Updated package reference with new operation names

---

## [1.0.0] - 2026-01-26

Initial release of codeQ.

### Added

- Persistent task queues backed by KVRocks/Redis
- Pull-based worker claims with lease management
- NACK with backoff and delayed queue support
- Dead-letter queue (DLQ) for max attempts exceeded
- Result storage with optional webhook callbacks
- Worker authentication via JWT (JWKS)
- Producer authentication via Tikti access tokens (JWKS)
- Plugin-based authentication system
- Priority queues (0-9 priority levels)
- Prometheus metrics endpoint
- CLI tool for task management
- Official SDKs for Java and Node.js/TypeScript
- Helm chart for Kubernetes deployment
- Comprehensive documentation suite

### Features

- **Queue Management**
  - Multiple named queues (commands)
  - Priority-based task scheduling
  - Delayed task visibility for retries
  - Automatic lease expiration repair
  
- **Worker Management**
  - Lease-based task ownership
  - Heartbeat extension support
  - NACK for explicit retry
  - Configurable retry policies (fixed, exponential, full jitter)

- **Observability**
  - Prometheus metrics
  - Queue depth gauges
  - Task lifecycle counters
  - Processing latency histograms
  - Custom Redis collector for queue stats

- **Webhooks**
  - Worker availability notifications
  - Result callbacks on task completion
  - Configurable delivery modes (fanout, group, hash)

- **SDKs**
  - Java SDK with Spring Boot, Quarkus, Micronaut support
  - Node.js/TypeScript SDK with Express, NestJS support
  - Task producer and worker abstractions
  - Automatic retry and error handling

[Unreleased]: https://github.com/osvaldoandrade/codeq/compare/v1.1.0...HEAD
[1.1.0]: https://github.com/osvaldoandrade/codeq/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/osvaldoandrade/codeq/releases/tag/v1.0.0
