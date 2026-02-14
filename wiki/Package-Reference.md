# Package Reference

This document provides detailed information about the codeQ codebase structure and package responsibilities.

## Repository Layout

```
codeq/
├── cmd/codeq/          # CLI entrypoint
├── pkg/                # Public API packages
│   ├── app/            # Application bootstrap
│   ├── config/         # Configuration
│   └── domain/         # Domain entities
├── internal/           # Private implementation
│   ├── backoff/        # Retry logic
│   ├── controllers/    # HTTP handlers
│   ├── middleware/     # Auth & middleware
│   ├── providers/      # External services
│   ├── repository/     # Data access
│   └── services/       # Business logic
├── helm/codeq/         # Kubernetes Helm chart
├── docs/               # Documentation
├── test/               # Integration tests
└── wiki/               # GitHub Pages content
```

## Public Packages (`pkg/`)

### `pkg/app`

**Purpose**: Application initialization and HTTP server setup

**Key files**:
- `application.go`: Main `Application` struct with `Start()` method
- `url_mappings.go`: HTTP route definitions (Gin router)
- `integration_test.go`: End-to-end integration tests

**Usage**: Imported by service runtime (see `github.com/codecompany/codeq-service`)

### `pkg/config`

**Purpose**: Configuration loading, validation, and defaults

**Key files**:
- `config.go`: `Config` struct with YAML unmarshaling and environment variable overrides

**Features**:
- Loads from YAML file
- Overrides via environment variables (e.g., `PORT`, `REDIS_ADDR`)
- Auto-computed defaults (e.g., `identityJwksUrl` from `identityServiceUrl`)
- Validation warnings for missing required fields

### `pkg/domain`

**Purpose**: Core domain types (entities, value objects)

**Key files**:
- `task.go`: `Task`, `Command` types
- `result.go`: `Result`, `Artifact`, `SubmitResultRequest` types
- `subscription.go`: `Subscription` type (webhook subscriptions)
- `queue_stats.go`: `QueueStats` type (admin metrics)

## Internal Packages (`internal/`)

### `internal/controllers`

**Purpose**: HTTP request handlers (Gin framework)

Controllers handle all HTTP endpoints for task lifecycle, webhook subscriptions, and admin operations.

### `internal/middleware`

**Purpose**: Request preprocessing (auth, logging, correlation IDs)

Provides authentication (producer and worker tokens), authorization (event type scopes), and request processing.

### `internal/services`

**Purpose**: Business logic layer (orchestration, state machines, webhooks)

Core services include:
- **scheduler_service**: Task creation, claiming, NACK, repair
- **results_service**: Result storage and validation
- **result_callback_service**: Task-level webhook delivery
- **subscription_service**: Worker availability subscriptions
- **notifier_service**: Worker availability notifications

### `internal/repository`

**Purpose**: Data access layer (Redis operations, queue semantics)

Repositories handle all Redis interactions for tasks, results, and subscriptions.

### `internal/providers`

**Purpose**: External service integrations

Provides Redis client initialization and artifact storage.

### `internal/backoff`

**Purpose**: Retry delay computation

Implements backoff policies: fixed, linear, exponential, and jitter variants.

## CLI Package (`cmd/codeq`)

Command-line interface for local development and testing. Includes commands for auth, task management, worker operations, and queue inspection.

## Further Reading

- [Architecture](Architecture)
- [Domain Model](Domain-Model)
- [HTTP API](HTTP-API)
- [Configuration](Configuration)
