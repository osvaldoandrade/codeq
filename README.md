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

## Get started

### Install CLI (macOS/Linux/Windows via Git Bash)

```bash
curl -fsSL https://raw.githubusercontent.com/osvaldoandrade/codeq/main/install.sh | sh
```

Requires `git` and `go`.

### 1) Helm (small cluster)

The chart in this repo deploys codeQ and, by default, a single-node KVRocks instance.

```bash
git clone https://github.com/osvaldoandrade/codeq
cd codeq

helm install codeq ./helm/codeq \
  --set secrets.enabled=true \
  --set secrets.webhookHmacSecret=YOUR_SECRET \
  --set config.identityServiceUrl=https://api.storifly.ai \
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

- HTTP API: `docs/04-http-api.md`
- Queue model: `docs/05-queueing-model.md`
- Storage layout: `docs/07-storage-kvrocks.md`
- Backoff and retries: `docs/11-backoff.md`
- Webhooks: `docs/12-webhooks.md`
- Configuration: `docs/14-configuration.md`
- Migration: `docs/migration.md`

## Repo layout

- `pkg/`: public packages (`app`, `config`, `domain`)
- `internal/`: controllers, middleware, services, repositories, providers
- `helm/codeq`: Helm chart
- `docs/`: full specification

## License

MIT. See `LICENSE`.
