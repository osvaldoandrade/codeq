# SDK Integration Overview

This document provides a high-level overview of all official codeQ client SDKs,
their capabilities, and guidance on choosing the right SDK for your environment.

## Available SDKs

| SDK | Language | Package | Status |
|-----|----------|---------|--------|
| [Java SDK](sdks/java/core/) | Java 17+ | `io.codeq:codeq-sdk` | ✅ Production |
| [Node.js SDK](sdks/nodejs/) | Node.js 18+ / TypeScript | `@codeq/sdk` | ✅ Production |
| [Python SDK](sdks/python/) | Python 3.10+ | `codeq-client` | ✅ Production |
| [Go SDK](sdks/go/) | Go 1.22+ | `github.com/osvaldoandrade/codeq/sdks/go` | ✅ Production |

## Feature Matrix

| Feature | Java | Node.js | Python | Go |
|---------|------|---------|--------|----|
| Create task | ✅ | ✅ | ✅ | ✅ |
| Batch create | — | ✅ | ✅ | ✅ |
| Claim task | ✅ | ✅ | ✅ | ✅ |
| Batch claim | — | ✅ | ✅ | ✅ |
| Submit result | ✅ | ✅ | ✅ | ✅ |
| Batch submit | — | ✅ | ✅ | ✅ |
| Heartbeat | ✅ | ✅ | ✅ | ✅ |
| Abandon | ✅ | ✅ | ✅ | ✅ |
| Nack | ✅ | ✅ | ✅ | ✅ |
| Subscriptions | — | ✅ | ✅ | ✅ |
| Get task | — | ✅ | ✅ | ✅ |
| Get result | — | ✅ | ✅ | ✅ |
| Wait for result | — | ✅ | ✅ | ✅ |
| Admin: list queues | — | ✅ | ✅ | ✅ |
| Admin: queue stats | — | ✅ | ✅ | ✅ |
| Admin: cleanup | — | ✅ | ✅ | ✅ |
| Async support | — | Promise | async/await + sync | context.Context |
| Auto retry | — | ✅ (axios-retry) | ✅ (tenacity) | ✅ (built-in) |
| Type safety | ✅ | ✅ (TypeScript) | ✅ (PEP 561) | ✅ (structs) |
| Zero dependencies | — | — | — | ✅ |

## Choosing an SDK

### Java SDK
Best for enterprise Java applications using **Spring Boot**, **Quarkus**, or
**Micronaut**. Uses OkHttp for HTTP and Jackson for JSON serialization.

→ [Java Integration Guide](docs/integrations/java-integration.md)

### Node.js / TypeScript SDK
Best for **Express**, **NestJS**, or **React** applications. Provides full
TypeScript types and supports both CommonJS and ES Module builds.

→ [Node.js Integration Guide](docs/integrations/nodejs-integration.md)

### Python SDK
Best for **FastAPI**, **Django**, or **Flask** applications and ML/AI pipelines.
Offers both an async client (httpx) and a synchronous wrapper.

→ [Python Integration Guide](docs/integrations/python-integration.md)

### Go SDK
Best for **cloud-native** Go services. Uses only the standard library with
zero external dependencies. Supports context-based cancellation and timeouts.

→ [Go Integration Guide](docs/integrations/go-integration.md)

## Common Patterns

### Producer Pattern

Create tasks from your application or API layer.

```
┌──────────────┐        ┌─────────────┐
│  Your App    │──SDK──▶│  codeQ      │
│  (Producer)  │        │  Server     │
└──────────────┘        └─────────────┘
```

### Worker Pattern

Claim and process tasks in background services.

```
┌─────────────┐        ┌──────────────┐
│  codeQ      │──SDK──▶│  Your Worker │
│  Server     │        │  (Consumer)  │
└─────────────┘        └──────────────┘
```

### Hybrid Pattern

A single service can act as both producer and worker.

```
┌──────────────────────┐
│  Your Service        │
│  ┌────────────────┐  │        ┌─────────────┐
│  │ Producer Token │──┼──SDK──▶│  codeQ      │
│  │ Worker Token   │◀─┼──SDK──│  Server     │
│  └────────────────┘  │        └─────────────┘
└──────────────────────┘
```

## Authentication

All SDKs authenticate via **JWT Bearer tokens**. Three token scopes control
access:

| Token | Scope | Operations |
|-------|-------|------------|
| Producer | `producer` | Create tasks, get task status |
| Worker | `worker` | Claim, heartbeat, submit results, nack, abandon |
| Admin | `admin` | Queue stats, cleanup, administrative operations |

Tokens are passed in the `Authorization: Bearer <token>` header on every
request.

## Configuration

All SDKs support configuration through environment variables:

| Variable | Description |
|----------|-------------|
| `CODEQ_BASE_URL` | Base URL of the codeQ server |
| `CODEQ_PRODUCER_TOKEN` | JWT for producer operations |
| `CODEQ_WORKER_TOKEN` | JWT for worker operations |
| `CODEQ_ADMIN_TOKEN` | JWT for admin operations |

## SDK Development Guidelines

Each SDK follows these standards:

- **HTTP client** for the codeQ REST API
- **Type definitions** for all request and response models
- **Authentication helpers** with JWT Bearer tokens
- **Producer API** — create and batch-create tasks
- **Worker API** — claim, heartbeat, submit, abandon, nack
- **Admin API** — queue stats, cleanup
- **Retry with backoff** — configurable exponential back-off for transient errors
- **Comprehensive tests** — unit tests with mocked HTTP
- **README** — quick-start examples and API reference
- **Package registry** — published to the language ecosystem (Maven, npm, PyPI, Go modules)

## Related Documentation

- [SDK Quick Start](sdks/README.md)
- [HTTP API Reference](docs/04-http-api.md)
- [Authentication & Security](docs/09-security.md)
- [Package Reference](docs/18-package-reference.md)
