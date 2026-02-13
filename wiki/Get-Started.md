# Get Started

This repository contains the core codeQ module (domain, queueing logic, HTTP handlers, and CLI). Most deployments run the production wrapper service (`codeq-service`) and use this repo as a module dependency.

If your goal is to evaluate codeQ quickly, the Helm chart in this repo is the fastest path.

## Deploy (Helm)

The chart deploys codeQ and, by default, a single-node KVRocks instance.

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

Bring your own KVRocks:

```bash
helm install codeq ./helm/codeq \
  --set kvrocks.enabled=false \
  --set config.redisAddr=your-kvrocks:6666
```

## Run the CLI

The CLI is in `cmd/codeq`:

```bash
go run ./cmd/codeq --help
```

Initialize a local CLI config:

```bash
go run ./cmd/codeq init
```

## Quick End-to-End Flow (HTTP)

The minimal functional flow is:

1. Producer creates a task.
2. Worker claims the task.
3. Worker submits a terminal result.

### 1) Create a task

```bash
curl -X POST http://localhost:8080/v1/codeq/tasks \
  -H 'Authorization: Bearer <producer-token>' \
  -H 'Content-Type: application/json' \
  -d '{"command":"GENERATE_MASTER","payload":{"jobId":"j-123"},"priority":3}'
```

### 2) Claim a task

In production, this requires a worker JWT with `codeq:claim` scope and an `eventTypes` allowlist.

In local/dev environments, you can enable the fallback that maps the producer token to a synthetic worker identity:

- `allowProducerAsWorker=true`

Then you can claim using the same bearer token:

```bash
curl -X POST http://localhost:8080/v1/codeq/tasks/claim \
  -H 'Authorization: Bearer <producer-token>' \
  -H 'Content-Type: application/json' \
  -d '{"commands":["GENERATE_MASTER"],"leaseSeconds":120,"waitSeconds":10}'
```

### 3) Submit a result

```bash
curl -X POST http://localhost:8080/v1/codeq/tasks/<id>/result \
  -H 'Authorization: Bearer <worker-token>' \
  -H 'Content-Type: application/json' \
  -d '{"status":"COMPLETED","result":{"ok":true}}'
```

If you set a task-level `webhook` during creation, codeQ will post a signed callback when the task reaches `COMPLETED` or `FAILED`. See [Webhooks](Webhooks).
