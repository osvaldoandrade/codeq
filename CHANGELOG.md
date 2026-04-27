# Changelog

All notable changes to codeQ will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Performance Regression Testing Framework**: Comprehensive benchmark suite for validating performance optimizations and detecting regressions
  - Sonic codec benchmarks (`internal/bench/sonic_bench_test.go`): Compares Sonic vs `encoding/json` across small, medium, and large payloads for both marshal and unmarshal operations
  - Bloom filter benchmarks (`internal/repository/bloom_bench_test.go`): Validates Add/MaybeHas throughput, memory footprint, and false positive rates (target: ≤1%)
  - GC pressure benchmarks (`internal/bench/gc_pressure_bench_test.go`): Tracks allocation counts, GC cycles, and pause times under sustained enqueue and claim/submit workloads
  - Staging validation runbook (`docs/33-staging-validation-runbook.md`): Step-by-step procedures for running benchmarks, acceptable performance ranges, and dashboard guidance
  - CI workflow updated to include Sonic, Bloom filter, and GC pressure benchmarks with `benchstat` comparison
- **Benchmark Regression Testing CI** (`.github/workflows/benchmark-regression.yml`): Automated Go benchmark regression detection on every PR and commit to main. Runs `BenchmarkHTTP_CreateClaimComplete` and `BenchmarkScheduler_CreateClaimComplete` with 10s iterations and 3 runs, archives results (90d retention, 1yr history), and posts detailed comparisons to workflow summaries. Helps catch performance regressions early before they impact production.
  - Automated baseline comparison and regression detection
  - Results archived for 90 days with 1-year history for trend analysis
  - Step summary output for quick PR review
  - Interpretation guide: `.github/copilot/instructions/07-benchmark-regression-ci.md`
  - Documentation: `docs/16-workflows.md` updated with setup and usage
- **Performance Baselines Documentation** (`docs/30-performance-baselines.md`): Baseline load test results from all six k6 scenarios and Go benchmarks, including throughput, latency percentiles, and regression testing guidance
- **Load Testing Results**: Ran all k6 scenarios (sustained throughput, burst load, many workers, prefill queue, mixed priorities, delayed tasks) with 0% error rates across the board

### Changed

- Bumped k6 image in `docker-compose.yml` from 0.49.0 to 0.55.0 to support nullish coalescing (`??`) syntax used in load test scripts

### Documentation

- Updated `docs/26-load-testing.md` with baseline performance summary table
- Updated `docs/17-performance-tuning.md` with load test insights and cross-references
- Added `docs/30-performance-baselines.md` to `docs/README.md` index

- **Queue Sharding Support**: Pluggable `ShardSupplier` interface and `StaticShardSupplier` implementation enable horizontal scaling across multiple KVRocks instances
  - `ShardSupplier` interface defined in `pkg/domain/shard.go` with `QueueShards` and `CurrentShard` methods
  - `StaticShardSupplier` in `internal/shard/static_supplier.go` provides config-driven shard routing with tenant override → command mapping → default shard precedence
  - Shard-aware queue key utilities in `internal/shard/key.go` with backward-compatible key format (`:s:<shardID>` segment omitted for default shard)
  - `ShardingConfig` added to `pkg/config/config.go` for YAML configuration
  - Validation prevents startup with misconfigured shard mappings
  - Documentation: `docs/06-sharding.md` and `docs/02-domain-model.md` updated
  - Design: `docs/24-queue-sharding-hld.md`

- **Persistence Plugin Architecture**: Pluggable persistence layer enables organizations to choose storage backends without core code changes
  - Plugin interface defined in `pkg/persistence/` with `PluginPersistence`, `TaskStorage`, `ResultStorage`, and `SubscriptionStorage` interfaces
  - Redis plugin wraps existing Redis/KVRocks implementation maintaining full backward compatibility
  - Memory plugin provides in-memory storage for unit tests (no external dependencies)
  - Configuration-driven plugin selection via `persistenceProvider` and `persistenceConfig`
  - Plugin registry pattern mirroring existing `pkg/auth` authentication plugins
  - Documentation: `docs/27-persistence-plugin-system.md`
  - Example configuration: `config.example.yml`
  - Future: PostgreSQL, DynamoDB, Cassandra plugins can be added without core changes

### ⚠️ BREAKING CHANGES

#### DLQ Data Structure Change

The DLQ queue has been changed from a Redis LIST to a SET to achieve O(1) removal performance during admin cleanup.

**What changed:**
- `codeq:q:<command>:dlq` is now a SET (previously LIST)
- DLQ enqueue uses `SADD` (previously `LPUSH`)
- DLQ depth uses `SCARD` (previously `LLEN`)
- DLQ cleanup uses `SREM` O(1) (previously `LREM` O(N))

**Upgrade path for existing deployments:**
1. **Option A (recommended)**: Drain all DLQ entries before upgrading, then deploy the new version.
2. **Option B**: Convert LIST keys to SET during a brief write freeze: `RENAME` old key → `LRANGE` → `SADD` → `DEL` backup.
3. **Rollback**: If needed, convert SET back to LIST via `SMEMBERS` → `LPUSH`.

See [`docs/migration.md`](docs/migration.md) for detailed step-by-step procedures, rollback plan, and verification checklist.

> **Note**: The in-progress queue underwent the same LIST → SET change in v1.1.0. Both queues now use SETs. The migration guide covers both conversions.

### Performance Improvements

- **Idempotency Bloom filter optimization**: Added in-process rotating Bloom filter to eliminate negative Redis lookups on idempotent enqueue fast-path. Achieves 20-30% enqueue latency reduction for workloads with fresh idempotency keys. ([#105](https://github.com/osvaldoandrade/codeq/pull/105))
  - 1M capacity, 1% false positive rate, 30-minute rotation
  - ~2.4 MB memory footprint per API server instance
  - Thread-safe with atomic operations, zero configuration required
- **JSON Serialization with Bytedance Sonic**: Replaced Go's standard `encoding/json` with Bytedance Sonic codec in hot paths for 2-3x faster JSON operations and 40-50% fewer allocations. ([#315](https://github.com/osvaldoandrade/codeq/pull/315))
  - Applied to: task repository (enqueue/claim), result repository (submit/get), subscription repository, notifier and callback services
  - Expected improvements: 5-10% enqueue p50 latency reduction, 10-15% claim p50 latency reduction
  - Reduces garbage collection pressure by 10-20%, improving tail latencies (p99, p99.9)
  - No configuration needed; used transparently with graceful fallback handling
- **Redis batch pipelining optimization**: Consolidated multiple Redis round-trips into single pipeline batches in hot paths for dramatic latency reduction. ([#367](https://github.com/osvaldoandrade/codeq/pull/367))
  - `AdminQueues`: 80+ RTTs → 1-2 RTT (90% reduction) for queue depth queries
  - `QueueStats`: 10-13 RTTs → 1 RTT (92% reduction) for queue statistics
  - `PendingLength`: 10 RTTs → 1 RTT (90% reduction) for pending queue counts
  - `SaveResult` / `UpdateTaskOnComplete` / `RemoveFromInprogAndClearLease`: 2 RTTs → 1 RTT (50% reduction) for result operations
  - Expected improvements: 3-5x throughput increase in admin operations, 50-90% latency reduction
- **Enqueue path pipelining**: Optimized task enqueue operation to pipeline all three Redis operations (HSet task, ZAdd TTL, LPush/ZAdd queue) into a single request. ([#389](https://github.com/osvaldoandrade/codeq/pull/389))
  - Latency reduction: 3 RTTs → 1 RTT (67% reduction)
  - Throughput gain: 3× improvement under network latency (5-10ms RTT)
  - Production impact: 60-70% enqueue latency reduction observed
  - Fully transparent to API consumers; no breaking changes
  - See `.github/copilot/instructions/08-enqueue-optimization.md` for implementation details
- **Claim finalization TTL pipelining**: Optimized task claim operation to batch lease, state update, and TTL bump into a single pipeline. ([#408](https://github.com/osvaldoandrade/codeq/pull/408))
  - Latency reduction: 3 RTTs → 1 RTT (50% reduction)
  - Throughput gain: 3× improvement under network latency (5-10ms RTT)
  - Production impact: 15-20% overall claim latency improvement under high worker concurrency
  - Enables faster task claim throughput for worker pools with 50+ concurrent claimers
  - Fully transparent to workers; no API or configuration changes
  - Reference: `internal/repository/task_repository.go:711-732` (Claim function finalization)
- **Subscription ListActive pipelining**: Optimized webhook subscription listing to batch all subscription data fetches into a single Redis pipeline. ([#411](https://github.com/osvaldoandrade/codeq/pull/411))
  - Latency reduction: N+1 RTTs → 2 RTTs (99% reduction for N≥50)
  - Real-world example: 50 subscriptions at 5ms latency: 255ms → 10ms (96% improvement)
  - Throughput gain: 25-50× improvement under realistic network conditions (5-10ms RTT)
  - Production impact: Especially significant for systems with 100+ subscriptions per command
  - Batch cleanup of expired subscriptions in separate pipeline
  - Fully transparent to webhook consumers; no API or configuration changes
  - Reference: `internal/repository/subscription_repository.go:120-179` (ListActive function implementation)
  - Documentation: `docs/17-performance-tuning.md` Section 9: Subscription Operations (ListActive)
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

- **Python SDK**: Official Python client for CodeQ with async and sync variants ([#338](https://github.com/osvaldoandrade/codeq/pull/338))
  - `CodeQClient` (async) and `SyncCodeQClient` (sync) for producer and worker operations
  - Full type hints with PEP 561 marker for IDE support and static type checking
  - httpx-based HTTP client with connection pooling and automatic retries (exponential backoff via tenacity)
  - Support for all API operations: task creation, claiming, result submission, webhooks, admin operations
  - Published as `codeq-client` package on PyPI
  - Comprehensive integration guide at `docs/integrations/python-integration.md` with FastAPI, Django, and Flask examples
  - 65 unit tests with >95% coverage

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
