# CLI

This repo ships a `codeq` CLI that targets the HTTP API.

It is designed for three workflows:

- producer operations (create tasks, query status)
- worker operations (claim, heartbeat, submit results)
- operator operations (inspect queues, run admin cleanup)

## Run

From this repository:

```bash
go run ./cmd/codeq --help
```

Or install a local binary:

```bash
go install ./cmd/codeq
```

## Config

The CLI stores configuration under:

- `~/.codeq/config.yaml`

It supports profiles so you can switch between clusters.

Initialize an empty config:

```bash
codeq init
```

## Tokens

The CLI can store:

- `producerToken` for `POST /tasks`
- `workerToken` (JWT) for worker endpoints

There is also an optional login helper (`codeq auth login`) that can fetch a producer token from an external IAM endpoint (template-driven).
For Tikti, the login helper fetches an `idToken` and then exchanges it for an `accessToken` via `/v1/accounts/token/exchange`.

## Examples

Create a task:

```bash
codeq task create --event render_video --priority 10 --payload '{"jobId":500}'
```

Start a local worker loop (pull):

```bash
codeq worker start --events render_video --concurrency 5
```

Inspect queue depth:

```bash
codeq queue inspect render_video
```
