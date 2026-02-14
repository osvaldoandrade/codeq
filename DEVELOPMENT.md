# Development Guide

This guide covers setting up your development environment, building, testing, and running codeQ locally.

## Prerequisites

- **Go 1.21+**: Check the version in `go.mod`
- **Git**: For cloning and version control
- **KVRocks or Redis**: For backend storage
- **curl**: For API testing
- **Docker** (optional): For running KVRocks in a container

## Initial Setup

### 1. Clone the Repository

````bash
git clone https://github.com/osvaldoandrade/codeq.git
cd codeq
````

### 2. Install Dependencies

````bash
go mod download
````

### 3. Start KVRocks

#### Option A: Using Docker

````bash
docker run -d \
  --name kvrocks-dev \
  -p 6666:6666 \
  apache/kvrocks:latest
````

#### Option B: Install Locally

Follow [KVRocks installation guide](https://kvrocks.apache.org/docs/getting-started) or use Redis:

````bash
# macOS with Homebrew
brew install redis
redis-server --port 6666
````

## Building

### Build the CLI

````bash
go build -o codeq ./cmd/codeq
./codeq --help
````

### Build for Multiple Platforms

````bash
# Linux AMD64
GOOS=linux GOARCH=amd64 go build -o codeq-linux-amd64 ./cmd/codeq

# macOS ARM64 (Apple Silicon)
GOOS=darwin GOARCH=arm64 go build -o codeq-darwin-arm64 ./cmd/codeq

# Windows AMD64
GOOS=windows GOARCH=amd64 go build -o codeq-windows-amd64.exe ./cmd/codeq
````

### Build with Optimizations (Release)

````bash
CGO_ENABLED=0 go build \
  -trimpath \
  -ldflags "-s -w" \
  -o codeq \
  ./cmd/codeq
````

## Testing

### Run All Tests

````bash
go test ./...
````

### Run Tests with Coverage

````bash
go test -v -cover ./...
````

### Run Specific Package Tests

````bash
# Test only the CLI
go test ./cmd/codeq/...

# Test middleware
go test ./internal/middleware/...

# Test repository layer
go test ./internal/repository/...
````

### Run Integration Tests

Integration tests require a running KVRocks/Redis instance:

````bash
# Start KVRocks on :6666 first, then:
export REDIS_ADDR=localhost:6666
go test -v ./pkg/app/...
````

### Run with Race Detection

````bash
go test -race ./...
````

## Running Locally

### Option 1: Run CLI Directly

The CLI can operate against a remote codeQ API or start a local worker:

````bash
# Configure CLI
./codeq init

# Create a task (requires API server)
./codeq task create \
  --event render_video \
  --priority 10 \
  --payload '{"jobId": 123}'

# Start a worker
./codeq worker start \
  --events render_video \
  --concurrency 5
````

### Option 2: Run API Server

To run the API server, you'll need the service wrapper from:
https://github.com/codecompany/codeq-service

That repository imports this module and wires up the HTTP server.

### Local Development Flow

A Python script is provided for end-to-end local testing:

````bash
# Ensure KVRocks is running on :6666
cd test
python3 local_flow.py
````

This script:
1. Generates a test JWT
2. Creates tasks
3. Claims tasks
4. Submits results

## Configuration

### Environment Variables

The CLI respects these environment variables:

````bash
export CODEQ_BASE_URL=http://localhost:8080
export CODEQ_PRODUCER_TOKEN=your-producer-token
export CODEQ_WORKER_TOKEN=your-worker-jwt
export CODEQ_ADMIN=true
````

For the full list of configuration options, see [docs/14-configuration.md](docs/14-configuration.md).

### Local Config File

The CLI stores configuration in `~/.codeq/config.yaml`:

````yaml
currentProfile: local
profiles:
  local:
    baseUrl: http://localhost:8080
    producerToken: producer-token
    workerToken: worker-jwt
    admin: true
````

## Code Organization

### Package Guidelines

- **`cmd/codeq/`**: CLI entry point, command definitions
- **`pkg/`**: Public, reusable packages
  - `app/`: Application initialization and HTTP routing
  - `config/`: Configuration loading and validation
  - `domain/`: Domain models (Task, Result, Subscription, etc.)
- **`internal/`**: Private implementation details
  - `backoff/`: Retry backoff algorithms
  - `controllers/`: HTTP request handlers
  - `middleware/`: Authentication, logging, request ID
  - `providers/`: External service adapters (Redis, uploaders)
  - `repository/`: Data access layer for KVRocks
  - `services/`: Business logic (scheduler, notifier, results)

### Adding a New Feature

1. **Define domain models** in `pkg/domain/` if needed
2. **Add repository methods** in `internal/repository/` for data access
3. **Implement business logic** in `internal/services/`
4. **Create controller** in `internal/controllers/` for HTTP handling
5. **Wire up routes** in `pkg/app/url_mappings.go`
6. **Add tests** for each layer
7. **Update documentation** in `docs/`

## Debugging

### Enable Debug Logging

When running the API server:

````bash
export LOG_LEVEL=debug
export LOG_FORMAT=text  # or json
````

### Inspect KVRocks State

````bash
# Connect to KVRocks
redis-cli -p 6666

# List all keys
KEYS *

# Inspect a task
GET task:some-uuid

# Check queue contents
ZRANGE queue:ready:GENERATE_MASTER 0 -1 WITHSCORES
ZRANGE queue:delayed:GENERATE_MASTER 0 -1 WITHSCORES
````

See [docs/07-storage-kvrocks.md](docs/07-storage-kvrocks.md) for the full key schema.

### Common Issues

See [TROUBLESHOOTING.md](TROUBLESHOOTING.md) for solutions to common problems.

## Linting and Formatting

### Format Code

````bash
go fmt ./...
````

### Vet Code

````bash
go vet ./...
````

### Optional: Install golangci-lint

For comprehensive linting:

````bash
# Install golangci-lint
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

# Run linter
golangci-lint run
````

## Helm Chart Development

### Install Chart Locally

````bash
# Start KVRocks
docker run -d --name kvrocks -p 6666:6666 apache/kvrocks:latest

# Install with Minikube or kind
helm install codeq ./helm/codeq \
  --set kvrocks.enabled=false \
  --set config.redisAddr=host.docker.internal:6666 \
  --set secrets.enabled=true \
  --set secrets.webhookHmacSecret=test-secret \
  --set config.workerJwksUrl=https://example.com/.well-known/jwks.json \
  --set config.workerIssuer=https://example.com
````

### Validate Helm Chart

````bash
# Lint the chart
helm lint ./helm/codeq

# Dry run to see generated manifests
helm install codeq ./helm/codeq --dry-run --debug
````

## Continuous Integration

GitHub Actions workflows are defined in `.github/workflows/`:

- **`release.yml`**: Builds binaries and publishes releases
- **`static.yml`**: Deploys documentation to GitHub Pages
- **Agentic workflows**: `update-docs.lock.yml`, `daily-qa.lock.yml`, etc.

## Performance Testing

### Smoke Test Script

A production smoke test is available:

````bash
cd scripts
./prod_smoke.sh
````

Set environment variables to target your deployment:

````bash
export CODEQ_BASE_URL=https://your-api.example.com
export CODEQ_EMAIL=your@email.com
export CODEQ_PASSWORD=yourpassword
./prod_smoke.sh
````

## Documentation

### Building Documentation Locally

Documentation is written in Markdown. To preview:

1. Use any Markdown viewer or editor
2. For GitHub Pages preview, use Jekyll locally:

````bash
cd wiki
# Follow Jekyll installation instructions
````

### Documentation Style

Follow these principles:

- **Diátaxis framework**: Separate tutorials, how-to guides, reference, and explanation
- **Active voice**: "Create a task" not "A task is created"
- **Progressive disclosure**: High-level concepts first, details second
- **Code examples**: Show, don't just tell

See the [docs/README.md](docs/README.md) for the documentation index.

## Next Steps

- Read the [architecture overview](docs/03-architecture.md)
- Explore the [HTTP API reference](docs/04-http-api.md)
- Review [queueing model](docs/05-queueing-model.md)
- Check out [examples](docs/13-examples.md)

## Getting Help

- Open an [issue](https://github.com/osvaldoandrade/codeq/issues) for bugs
- Start a [discussion](https://github.com/osvaldoandrade/codeq/discussions) for questions
- Review existing documentation in `docs/`
