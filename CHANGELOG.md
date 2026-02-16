# Changelog

All notable changes to codeQ will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### ⚠️ BREAKING CHANGES

#### DLQ Data Structure Change

The DLQ queue has been changed from a Redis LIST to a SET to achieve O(1) removal performance during admin cleanup.

**What changed:**
- `codeq:q:<command>:dlq` is now a SET (previously LIST)
- DLQ enqueue uses `SADD` (previously `LPUSH`)
- DLQ depth uses `SCARD` (previously `LLEN`)
- DLQ cleanup uses `SREM` O(1) (previously `LREM` O(N))

**Migration required:**
- **Recommended**: Drain DLQ before upgrading
- **Alternative**: Convert existing LIST → SET during freeze window (rename list to a temp key, then `SADD` members)

### Performance Improvements

- **Idempotency Bloom filter optimization**: Added in-process rotating Bloom filter to eliminate negative Redis lookups on idempotent enqueue fast-path. Achieves 20-30% enqueue latency reduction for workloads with fresh idempotency keys. ([#105](https://github.com/osvaldoandrade/codeq/pull/105))
  - 1M capacity, 1% false positive rate, 30-minute rotation
  - ~2.4 MB memory footprint per API server instance
  - Thread-safe with atomic operations, zero configuration required
- **Faster admin cleanup**: tasks now track an optional `lastKnownLocation` to avoid unnecessary O(N) list scans during `CleanupExpired`.
- **Optimized MoveDueDelayed batching**: Eliminated redundant task JSON reads and batch all updates in single pipeline. Reduces O(3M) round-trips to O(M) for M due tasks, achieving 50-70% latency reduction for delayed→pending migrations. ([#96](https://github.com/osvaldoandrade/codeq/pull/96))

### Added

- **Redis-backed rate limiting**: Optional token bucket rate limiter for API endpoints ([#102](https://github.com/osvaldoandrade/codeq/pull/102))
  - Per-bearer-token rate limiting with configurable `requestsPerMinute` and `burstSize`
  - Separate limits for producer, worker, webhook, and admin scopes
  - Fail-open strategy: allows requests when Redis is unavailable
  - HTTP 429 responses with `Retry-After` header when limits exceeded
  - New metric: `codeq_rate_limit_hits_total` counter for monitoring rejections
  - Disabled by default; see [Configuration](docs/14-configuration.md) and [Operations](docs/10-operations.md#rate-limiting) for setup

### Documentation

- **Framework example READMEs** ([#135](https://github.com/osvaldoandrade/codeq/pull/135))
  - Added comprehensive README for NestJS example (`examples/nodejs/nestjs/README.md`)
    - Complete quick start guide with prerequisites and setup instructions
    - Architecture diagram showing producer and worker patterns
    - Detailed API endpoint documentation with curl examples
    - Worker implementation guide with heartbeat management
    - Production best practices including error handling and monitoring
    - Troubleshooting section with common issues and solutions
  - Added comprehensive README for Spring Boot example (`examples/java/springboot/README.md`)
    - Complete quick start guide with Maven and application setup
    - Architecture diagram and component overview
    - REST API documentation with request/response examples
    - Worker implementation with `@Scheduled` annotation patterns
    - Configuration management and externalized properties
    - Production deployment guide including health checks and Docker
    - Monitoring with Spring Actuator integration
    - Comprehensive troubleshooting section
  - Enhanced framework integration documentation in `examples/README.md`
    - Improved navigation with clear structure for Java and Node.js examples
    - Quick start commands for each framework
    - Cross-references to integration guides and SDK documentation

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

### Added

- **Multi-tenant queue isolation** ([#66](https://github.com/osvaldoandrade/codeq/pull/66))
  - Complete tenant isolation at the queue level for multi-tenant deployments
  - Automatic tenant ID extraction from JWT claims (`tenantId`, `tenant_id`, `organizationId`, `organization_id`)
  - Tenant-specific queue namespacing for pending, in-progress, delayed, and dead-letter queues
  - Prevents cross-tenant task visibility and resource contention
  - Backward compatible with single-tenant deployments (empty tenant ID)
  - No performance impact: queue operations remain O(1) or O(log n)

### Performance Improvements

- **Optimized claim-time repair loop** ([#68](https://github.com/osvaldoandrade/codeq/pull/68))
  - Pipelined all TTL checks for expired lease scanning (single round-trip)
  - Reduced claim latency by ~40% under high in-progress queue depth (>1000 tasks)
  - Eliminated O(N) complexity in lease expiration requeue path

### Changed

- In-progress queue storage changed from LIST to SET
- Atomic claim move now uses Lua script instead of `RPOPLPUSH`
- Metrics collector updated to use `SCARD` for in-progress queue depth
- Task struct now includes `tenantId` field for multi-tenant isolation
- Queue keys include tenant ID segment: `codeq:q:{command}:{tenantID}:{type}`

### Documentation

- **Tenant isolation** ([#77](https://github.com/osvaldoandrade/codeq/pull/77))
  - Added multi-tenant architecture section to architecture docs
  - Documented tenant ID extraction logic in security docs
  - Updated domain model with tenantId field explanation
  - Updated HTTP API docs with automatic tenant scoping
  - Updated queueing model with tenant isolation section
  - Updated storage layout with tenant-specific queue key patterns
- **Performance optimizations**
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
