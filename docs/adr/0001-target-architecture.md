# ADR 0001: Target architecture (layered + hexagonal)

- **Status**: Accepted
- **Date**: 2026-06-28
- **Tracks**: epic #645

## Context

codeq grew organically. Today the layout couples public API, internal
services, persistence, transport, and wiring in ways that block
independent change:

- `pkg/app/Application` is a god-object exposing 11 internal services as
  a public surface.
- `pkg/config/Config` is a 689-line struct mixing HTTP, gRPC, Redis,
  Pebble, Raft, auth, sharding, and observability.
- `pkg/persistence` ships Redis and in-memory implementations, leaking
  infrastructure into the public API.
- Core task algorithms (`Claim`, `Enqueue`, `MoveDueDelayed`) are
  duplicated across three places (Redis repo, Pebble repo, cluster
  router) with no shared contract.
- `internal/services/scheduler_service.go` is 541 lines / 12 fields and
  owns task creation, claiming, retry, webhook coordination, and admin.
- There is no enforced rule that domain logic stays free of HTTP or DB
  imports.

Without a stable target, the planned refactor (epics #645–#656) cannot
converge.

## Decision

codeq adopts a **layered architecture with hexagonal boundaries at
storage and transport**. The layout is:

```
cmd/                       thin entrypoints (≤300 LOC each)
  server/                  composition root for the service
  codeq/                   composition root for the CLI

pkg/                       public, stable surface — contracts only
  types/                   DTOs (Task, ResultRecord, QueueStats, ...)
  plugin/                  storage and extension interfaces
  auth/                    Validator interface + Claims
  producerclient/          gRPC + HTTP SDK
  workerclient/            gRPC + HTTP SDK

internal/                  private implementation
  core/                    pure domain: model, policy, errors
  application/             use-cases (TaskCreator, TaskClaimer, ...)
  storage/
    adapter/redis/         Redis adapter
    adapter/pebble/        Pebble adapter
  cluster/                 routing, ring, bloom, raft group coordination
  replication/             Raft FSM, log/snapshot stores, mux transport
  server/
    http/<resource>/       HTTP handlers grouped by resource
    grpc/<service>/        gRPC stream servers
    middleware/            request id, auth, tenant, tracing, ...
  config/                  loader (flag > env > yaml > default)
  bootstrap/server/        wiring helpers invoked by cmd/server
  cli/                     CLI commands, profile loader, output formatting
  observability/
    metrics/               Prometheus registry + collectors
    tracing/               OpenTelemetry setup
  testutil/wait/           polling helpers for tests
```

**Dependency direction is strict and one-way**:

```
cmd/ ──▶ internal/bootstrap ──▶ internal/{server,application,storage,cluster}
                                          │
                                          ▼
                                  internal/core ──▶ pkg/types
                                                    pkg/plugin
                                                    pkg/auth
```

- `internal/core` imports only stdlib and `pkg/{types,plugin,auth}`.
- `internal/application` imports `internal/core` and consumer-side
  interfaces it defines for storage and transport collaborators.
- `internal/storage/adapter/*` and `internal/server/*` implement
  interfaces declared by their consumers.
- `pkg/` never imports `internal/`.

**Interfaces are defined on the consumer side**: the package that calls
a collaborator owns the interface; the package that implements it
satisfies that interface structurally.

**Errors are typed structs** in `internal/core/errors`. Callers inspect
them with `errors.As`. String matching on error messages is forbidden.

**Composition happens in `cmd/`**. There is no DI framework. Wiring is
visible, debuggable, and `cmd/server/main.go` is the only file that
knows the full dependency graph.

**The rules are enforced**, not aspirational: `depguard` in
`.golangci.yml` (PR #713) blocks imports that violate the layering. The
coverage floor in `.testcoverage.yml` (PR #714) enforces a 95% floor on
`internal/core` and 80–85% on storage and transport.

## Consequences

What this guarantees:

- Domain logic is portable. A change to `net/http` or to the database
  engine cannot reach `internal/core`.
- The public API (`pkg/`) is small and stable. Internal restructuring
  no longer breaks SDK consumers.
- New contributors can navigate by layer: where does *X* live? Answer:
  the layer that owns *X*'s responsibility.
- Refactors land as small PRs because the seams are explicit.

What this costs:

- More files, more package declarations, more interface definitions.
- A breaking change to `pkg/` (removing `pkg/app`, slimming
  `pkg/persistence`) — mitigated by a one-release deprecation window
  via strangler-fig aliases (epic #656).
- `cmd/server/main.go` becomes the central wiring file. It must stay
  readable; bootstrap helpers absorb the bulk.

New obligations:

- Every change consults the layer rules before adding an import.
- Every public API change in `pkg/` lands with a deprecation window and
  an ADR.
- Every domain decision (state machine, invariant, error category)
  lives in `internal/core`, not in services or handlers.

## Alternatives considered

- **Keep the current layout, refactor in place**. Rejected: there is no
  rule to anchor refactors against. Drift would continue.
- **Pure DDD with bounded contexts and a context map**. Rejected for
  this scope: codeq is one bounded context (task scheduling). The
  ceremony of context maps does not pay off.
- **Clean architecture with `usecase/`, `port/`, `adapter/` folders
  exactly as in Uncle Bob's book**. Rejected: the layered + hexagonal
  hybrid above conveys the same constraints with fewer levels of
  indirection and matches the reference implementation that informed
  this work.
- **A DI framework (`wire`, `fx`)**. Rejected: constructor injection in
  `main()` is sufficient and keeps the dependency graph greppable.

## References

- `.golangci.yml` (PR #713) — depguard rules that enforce the layering.
- `.testcoverage.yml` (PR #714) — per-layer coverage floors.
- Epics #645 (foundations), #646 (`internal/core`), #647 (reshape
  `pkg/`), #648 (storage adapters), #649 (server), #650 (application),
  #651 (composition root), #656 (rollout).
