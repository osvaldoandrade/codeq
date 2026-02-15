# codeQ

Reactive scheduling and completion system backed by persistent queues in KVRocks.

This repository contains the core runtime, HTTP API wiring, and a Helm chart for small clusters.
The production service wrapper lives at:
https://github.com/codecompany/codeq-service

## Links

- GitHub: https://github.com/osvaldoandrade/codeq
- Issues: https://github.com/osvaldoandrade/codeq/issues
- Specs index: `docs/README.md`

## Why codeQ

codeQ provides:

- Persistent queues on KVRocks (Redis protocol).
- Pull-based worker claims with leases.
- NACK + backoff + delayed queues.
- DLQ for tasks that exceed `maxAttempts`.
- Result storage and optional callbacks (webhooks).
- Worker auth via JWT (JWKS), producer auth via Tikti access tokens (JWKS).
- **Official SDKs** for Java and Node.js/TypeScript with framework integrations.

## Get started

### Install CLI (macOS/Linux/Windows via Git Bash)

```bash
curl -fsSL https://raw.githubusercontent.com/osvaldoandrade/codeq/main/install.sh | sh
```

Requires `git` and `go`.

### Install CLI via npm (npmjs)

```bash
npm i -g @osvaldoandrade/codeq
codeq --help
```

Upgrade:

```bash
npm i -g @osvaldoandrade/codeq@latest
```

### Use SDKs for Java and Node.js

Integrate codeQ into your microservices with official SDKs:

**Java** (Spring Boot, Quarkus, Micronaut):
```xml
<dependency>
    <groupId>io.codeq</groupId>
    <artifactId>codeq-sdk-java</artifactId>
    <version>1.0.0</version>
</dependency>
```

**Node.js/TypeScript** (Express, NestJS):
```bash
npm install @codeq/sdk
```

📚 **SDK Documentation**:
- [SDK Overview & Quick Start](sdks/README.md)
- [Java Integration Guide](docs/integrations/21-java-integration.md)
- [Node.js Integration Guide](docs/integrations/22-nodejs-integration.md)
- [Example Applications](examples/)

### 1) Helm (small cluster)

The chart in this repo deploys codeQ and, by default, a single-node KVRocks instance.

```bash
git clone https://github.com/osvaldoandrade/codeq
cd codeq

helm install codeq ./helm/codeq \
  --set secrets.enabled=true \
  --set secrets.webhookHmacSecret=YOUR_SECRET \
  --set config.identityServiceUrl=https://your-auth-server.com \
  --set config.workerJwksUrl=https://your-jwks \
  --set config.workerIssuer=https://issuer
```

Disable embedded KVRocks and point to your own:

```bash
helm install codeq ./helm/codeq \
  --set kvrocks.enabled=false \
  --set config.redisAddr=your-kvrocks:6666
```

### 2) Service runtime

The API server and Dockerfile are in the service repo:
https://github.com/codecompany/codeq-service

That repo consumes this module and exposes the HTTP API.

## Quick API flow

Create a task (producer token):

```bash
curl -X POST http://localhost:8080/v1/codeq/tasks \
  -H 'Authorization: Bearer <producer-token>' \
  -H 'Content-Type: application/json' \
  -d '{"command":"GENERATE_MASTER","payload":{"jobId":"j-123"},"priority":3}'
```

Claim a task (worker JWT):

```bash
curl -X POST http://localhost:8080/v1/codeq/tasks/claim \
  -H 'Authorization: Bearer <worker-token>' \
  -H 'Content-Type: application/json' \
  -d '{"commands":["GENERATE_MASTER"],"leaseSeconds":120,"waitSeconds":10}'
```

Submit result:

```bash
curl -X POST http://localhost:8080/v1/codeq/tasks/<id>/result \
  -H 'Authorization: Bearer <worker-token>' \
  -H 'Content-Type: application/json' \
  -d '{"status":"COMPLETED","result":{"ok":true}}'
```

## Specs and docs

Start here: `docs/README.md`

Key references:

- **Getting Started Tutorial**: `docs/00-getting-started.md` - **Start here for your first experience with codeQ**
- **Overview**: `docs/01-overview.md` - System goals and design principles
- **HTTP API**: `docs/04-http-api.md` - Complete API reference
- **CLI Reference**: `docs/15-cli-reference.md` - CLI command documentation
- **SDK Integration**: `sdks/README.md` - Official Java and Node.js SDKs
  - [Java Integration](docs/integrations/21-java-integration.md) - Spring Boot, Quarkus, Micronaut
  - [Node.js Integration](docs/integrations/22-nodejs-integration.md) - Express, NestJS, React
- **Examples**: `examples/` - Working examples with Java and Node.js frameworks
- **Developer Guide**: `docs/22-developer-guide.md` - Contributing and internal architecture
- **Queue model**: `docs/05-queueing-model.md` - Queue semantics
- **Storage layout**: `docs/07-storage-kvrocks.md` - KVRocks data structures
- **Backoff and retries**: `docs/11-backoff.md` - Retry logic
- **Webhooks**: `docs/12-webhooks.md` - Push notifications
- **Configuration**: `docs/14-configuration.md` - Config reference
- **Performance**: `docs/17-performance-tuning.md` - Optimization guide
- **Workflows**: `docs/16-workflows.md` - GitHub Actions automation
- **Migration**: `docs/migration.md` - Upgrade guide
- **Contributing**: `CONTRIBUTING.md` - Contribution guidelines

## Repo layout

- `pkg/`: public packages (`app`, `config`, `domain`)
- `internal/`: controllers, middleware, services, repositories, providers
- `helm/codeq`: Helm chart
- `docs/`: full specification

## License

MIT. See `LICENSE`.
