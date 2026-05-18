# codeQ Documentation

codeQ is a reactive task queue with an embedded Pebble persistence engine
(CockroachDB's RocksDB-style LSM). It is written in Go and runs as a single
binary — HTTP/gRPC API, queueing logic, lease table, and on-disk LSM live
inside one process.

This index is organised by **user journey**. Pick the section that matches
what you're trying to do; each entry says what's actually in the file.

---

## 1. New to codeQ? Start here

- [`00-getting-started.md`](00-getting-started.md) — zero to a running task in
  ~10 minutes on a single node (download, start, enqueue, claim, complete).
- [`01-overview.md`](01-overview.md) — what codeQ is, in CS terms: queue
  semantics, persistence model, process layout.
- `43-tutorial-raft-cluster.md` — 3-node RAFT cluster walkthrough
  (*in flight*).
- `44-tutorial-go-sdk.md` — end-to-end Go SDK tutorial using
  `pkg/producerclient` and `pkg/workerclient` (*in flight*).

## 2. Choosing a deployment

- `41-deployment-modes.md` — decision guide: single-node vs RAFT vs
  multi-shard, with tradeoffs (*in flight*).
- `42-raft-failover-walkthrough.md` — what happens when a node dies: leader
  election, log replay, client behaviour (*in flight*).
- [`06-sharding.md`](06-sharding.md) — shard count sizing: intra-process
  sharding and how to pick a number.
- [`17-performance-tuning.md`](17-performance-tuning.md) — knobs: Pebble cache,
  fsync mode, batch sizes, GOMAXPROCS, lease intervals.

## 3. Architecture

- [`03-architecture.md`](03-architecture.md) — package graph, data flow, and
  process layout.
- [`07b-storage-pebble.md`](07b-storage-pebble.md) — Pebble LSM internals as
  used by codeQ: keyspaces, group commit, fsync policy, WAL.
- [`08b-pebble-sharding-internals.md`](08b-pebble-sharding-internals.md) —
  FNV-1a routing, per-shard atomic invariants, intra-process shard layout.
- [`40-raft-replication.md`](40-raft-replication.md) — multi-shard raft via
  `hashicorp/raft`: FSM, mux transport, single- and multi-shard modes,
  current limitations.
- [`19b-cluster-grpc-protocol.md`](19b-cluster-grpc-protocol.md) — inter-node
  gRPC protocol for the legacy consistent-hash cluster mode.
- [`27-persistence-plugin-system.md`](27-persistence-plugin-system.md) —
  persistence plugin interface (Pebble is the supported backend; memory
  provider is for tests).

## 4. Domain & semantics

- [`02-domain-model.md`](02-domain-model.md) — core entities: Task, Result,
  Subscription, and their relationships.
- [`05-queueing-model.md`](05-queueing-model.md) — FIFO ordering, priority,
  visibility timeout, delayed delivery.
- [`06b-lease-management.md`](06b-lease-management.md) — in-memory lease
  table: claim, extend, expire, reclaim.
- [`08-consistency.md`](08-consistency.md) — at-least-once delivery,
  idempotency, edge cases under failure.
- [`11-backoff.md`](11-backoff.md) — retry policies and backoff curves.
- [`39-multi-tenancy.md`](39-multi-tenancy.md) — tenant isolation in storage,
  auth, and worker claim paths.

## 5. APIs

- [`04-http-api.md`](04-http-api.md) — REST API on `:8080` (task lifecycle,
  results, admin endpoints).
- [`34-streaming-api-guide.md`](34-streaming-api-guide.md) — gRPC streaming
  API overview, protocol, throughput characteristics.
- [`35-producer-streaming-sdk.md`](35-producer-streaming-sdk.md) —
  `pkg/producerclient` against the producer stream on `:9092`.
- [`36-worker-streaming-sdk.md`](36-worker-streaming-sdk.md) —
  `pkg/workerclient` against the worker stream on `:9091`.
- [`15-cli-reference.md`](15-cli-reference.md) — `codeq-cli` commands and
  flags.
- [`18-package-reference.md`](18-package-reference.md) — public Go packages
  and their stability contracts.

## 6. Operations

- [`10-operations.md`](10-operations.md) — boot sequence, graceful shutdown,
  signal handling.
- [`14-configuration.md`](14-configuration.md) — full config YAML reference.
- [`28-troubleshooting.md`](28-troubleshooting.md) — common issues and
  diagnostic steps.
- [`29-operational-runbooks.md`](29-operational-runbooks.md) — incident
  response, maintenance, scaling, monitoring, data management.
- [`30-performance-baselines.md`](30-performance-baselines.md) — measured
  throughput history and regression benchmarks.
- [`33-staging-validation-runbook.md`](33-staging-validation-runbook.md) —
  staging performance validation procedure.

## 7. Security & observability

- [`09-security.md`](09-security.md) — authentication, JWKS, rate limiting.
- [`20-authentication-plugins.md`](20-authentication-plugins.md) — auth
  plugin interface and the identity-middleware migration appendix.
- [`37-observability.md`](37-observability.md) — Prometheus metrics and
  OpenTelemetry traces; what's exposed and where.
- [`12-webhooks.md`](12-webhooks.md) — result callback webhooks.
- [`38-result-storage-callbacks.md`](38-result-storage-callbacks.md) —
  result persistence and callback delivery semantics.

## 8. Testing

- [`19-testing.md`](19-testing.md) — test layout, what's covered, how to run
  the suite.
- [`26-load-testing.md`](26-load-testing.md) — k6 scenarios and load-test
  harness.

## 9. Examples & integrations

- [`13-examples.md`](13-examples.md) — short code recipes against the HTTP
  and gRPC APIs.
- [`integrations/go-integration.md`](integrations/go-integration.md) — Go
  integration guide for `pkg/producerclient` and `pkg/workerclient` with
  net/http, Gin, and Echo.
- [`../examples/custom-auth-plugin.md`](../examples/custom-auth-plugin.md) —
  worked example of an out-of-tree auth plugin.

Non-Go callers should use the [HTTP API](04-http-api.md) directly; no
language-specific SDKs ship with codeQ.

## 10. Legacy / reference

- [`05-cluster-architecture.md`](05-cluster-architecture.md) — Phase 5
  consistent-hash cluster mode, preserved for reference. For HA, use RAFT
  ([`40-raft-replication.md`](40-raft-replication.md)).
- [`22-local-development.md`](22-local-development.md) — Docker Compose
  development environment.
- [`21-developer-guide.md`](21-developer-guide.md) — contributor workflow:
  layout, tooling, conventions.
- [`25-plugin-architecture-hld.md`](25-plugin-architecture-hld.md) —
  high-level design for the persistence + auth plugin architecture.
- [`24-queue-sharding-hld.md`](24-queue-sharding-hld.md) — sharding RFC
  (intra-process Phase 8 sharding).
- [`32-shard-migration-guide.md`](32-shard-migration-guide.md) — shard
  rebalancing tooling.
- [`16-workflows.md`](16-workflows.md) — GitHub Actions CI/CD pipeline.

---

## Deployment assets (not docs, but useful)

- `deploy/docker-compose/` — Compose templates for local and single-node.
- `deploy/kubernetes/` — example Kubernetes manifests; use Helm for real
  installs.
- `deploy/config/codeq.example.yml` — annotated server config example.
- `helm/codeq/` — Helm chart with size profiles.

## Style

Contributors writing docs should follow [`_STYLE.md`](_STYLE.md).
