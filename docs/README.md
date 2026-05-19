# codeQ Documentation

The codeQ docs are organised as a book and a reference manual. The book is
six chapters under [`chapter/`](chapter/), meant to be read in order by a
first-time reader: install, concepts, the Sous function runtime, the
streaming and storage layer, observability, and performance. The reference
manual below is the existing collection of focused documents — install
details, decision guides, deep dives, API references, runbooks — kept for
when you need a specific page rather than a full read-through.

---

## The codeQ book (read in order)

1. [Get Started](chapter/01-get-started.md) — install, run a server, produce + consume your first task
2. [Concepts and Architecture](chapter/02-concepts-and-architecture.md) — the domain model, queue semantics, sharding, leases, deployment modes
3. [Sous Functions](chapter/03-sous-functions.md) — running serverless functions on top of codeQ via Sous (github.com/osvaldoandrade/sous)
4. [CodeQ IO](chapter/04-codeq-io.md) — gRPC streams, REST, the persistence engine, raft replication
5. [Observability](chapter/05-observability.md) — tracing, metrics, profiling, structured logs
6. [Performance](chapter/06-performance.md) — measured throughput, the cost of HA, scaling decisions

## Quick paths

- Install codeQ → [Get Started](chapter/01-get-started.md)
- Understand the architecture → [Concepts and Architecture](chapter/02-concepts-and-architecture.md)
- Tune throughput → [Performance](chapter/06-performance.md)
- Set up HA → [CodeQ IO § Replication](chapter/04-codeq-io.md) and the [RAFT failover walkthrough](42-raft-failover-walkthrough.md)
- Add observability → [Observability](chapter/05-observability.md)

---

## Reference manual

### Getting started details

- [`00-getting-started.md`](00-getting-started.md) — zero-to-running on a single node in ten minutes
- [`13-examples.md`](13-examples.md) — short HTTP and gRPC recipes
- [`43-tutorial-raft-cluster.md`](43-tutorial-raft-cluster.md) — three-node RAFT cluster walkthrough
- [`44-tutorial-go-sdk.md`](44-tutorial-go-sdk.md) — end-to-end Go SDK tutorial

### Decision guides

- [`41-deployment-modes.md`](41-deployment-modes.md) — single-node vs RAFT vs multi-shard tradeoffs
- [`42-raft-failover-walkthrough.md`](42-raft-failover-walkthrough.md) — what happens when a node dies
- [`06-sharding.md`](06-sharding.md) — intra-process shard count sizing
- [`17-performance-tuning.md`](17-performance-tuning.md) — Pebble cache, fsync, batch sizes, lease knobs

### Architecture deep dives

- [`01-overview.md`](01-overview.md) — what codeQ is, in CS terms
- [`03-architecture.md`](03-architecture.md) — package graph, data flow, process layout
- [`07b-storage-pebble.md`](07b-storage-pebble.md) — Pebble LSM internals as used by codeQ
- [`08b-pebble-sharding-internals.md`](08b-pebble-sharding-internals.md) — FNV-1a routing and per-shard invariants
- [`40-raft-replication.md`](40-raft-replication.md) — multi-shard raft via hashicorp/raft
- [`19b-cluster-grpc-protocol.md`](19b-cluster-grpc-protocol.md) — inter-node gRPC protocol for cluster mode
- [`27-persistence-plugin-system.md`](27-persistence-plugin-system.md) — persistence plugin interface (Pebble is supported)

### Domain and semantics

- [`02-domain-model.md`](02-domain-model.md) — Task, Result, Subscription entities
- [`05-queueing-model.md`](05-queueing-model.md) — FIFO, priority, visibility, delayed delivery
- [`06b-lease-management.md`](06b-lease-management.md) — claim, extend, expire, reclaim
- [`08-consistency.md`](08-consistency.md) — at-least-once delivery and idempotency under failure
- [`11-backoff.md`](11-backoff.md) — retry policies and backoff curves
- [`39-multi-tenancy.md`](39-multi-tenancy.md) — tenant isolation across storage, auth, claim paths

### APIs

- [`04-http-api.md`](04-http-api.md) — REST API on port 8080
- [`34-streaming-api-guide.md`](34-streaming-api-guide.md) — gRPC streaming protocol and throughput
- [`35-producer-streaming-sdk.md`](35-producer-streaming-sdk.md) — pkg/producerclient against the producer stream
- [`36-worker-streaming-sdk.md`](36-worker-streaming-sdk.md) — pkg/workerclient against the worker stream
- [`15-cli-reference.md`](15-cli-reference.md) — codeq-cli commands and flags
- [`18-package-reference.md`](18-package-reference.md) — public Go packages and stability contracts

### Operations

- [`10-operations.md`](10-operations.md) — boot, shutdown, signals
- [`14-configuration.md`](14-configuration.md) — full config YAML reference
- [`28-troubleshooting.md`](28-troubleshooting.md) — common issues and diagnostic steps
- [`29-operational-runbooks.md`](29-operational-runbooks.md) — incident response and maintenance
- [`30-performance-baselines.md`](30-performance-baselines.md) — measured throughput history
- [`33-staging-validation-runbook.md`](33-staging-validation-runbook.md) — staging validation procedure

### Security and observability

- [`09-security.md`](09-security.md) — authentication, JWKS, rate limiting
- [`20-authentication-plugins.md`](20-authentication-plugins.md) — auth plugin interface
- [`37-observability.md`](37-observability.md) — Prometheus metrics and OpenTelemetry traces
- [`12-webhooks.md`](12-webhooks.md) — result callback webhooks
- [`38-result-storage-callbacks.md`](38-result-storage-callbacks.md) — result persistence and callback semantics

### Testing

- [`19-testing.md`](19-testing.md) — test layout, coverage, how to run the suite
- [`26-load-testing.md`](26-load-testing.md) — k6 scenarios and load-test harness

### Integrations

- [`integrations/go-integration.md`](integrations/go-integration.md) — Go integration with net/http, Gin, Echo

### Legacy / reference

- [`05-cluster-architecture.md`](05-cluster-architecture.md) — Phase 5 consistent-hash cluster, preserved for reference
- [`22-local-development.md`](22-local-development.md) — Docker Compose development environment
- [`21-developer-guide.md`](21-developer-guide.md) — contributor workflow, tooling, conventions
- [`25-plugin-architecture-hld.md`](25-plugin-architecture-hld.md) — plugin architecture high-level design
- [`24-queue-sharding-hld.md`](24-queue-sharding-hld.md) — intra-process sharding RFC
- [`32-shard-migration-guide.md`](32-shard-migration-guide.md) — shard rebalancing tooling
- [`16-workflows.md`](16-workflows.md) — GitHub Actions CI/CD pipeline

---

## Deployment assets

- `deploy/docker-compose/` — Compose templates for local and single-node
- `deploy/kubernetes/` — example Kubernetes manifests; use Helm for real installs
- `deploy/config/codeq.example.yml` — annotated server config example
- `helm/codeq/` — Helm chart with size profiles

## Style

Contributors writing docs should follow [`_STYLE.md`](_STYLE.md).
