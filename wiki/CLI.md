# CLI

This repo ships a `codeq` CLI that targets the HTTP API.

> **📚 For complete CLI documentation, see [docs/15-cli-reference.md](../docs/15-cli-reference.md)**

It is designed for three workflows:

- producer operations (create tasks, query status)
- worker operations (claim, heartbeat, submit results)
- operator operations (inspect queues, run admin cleanup)

## Quick Start

### Install

Via npm:

````bash
npm i -g @osvaldoandrade/codeq
codeq --help
````

Via install script:

````bash
curl -fsSL https://raw.githubusercontent.com/osvaldoandrade/codeq/main/install.sh | sh
````

### Initialize

````bash
codeq init
````

### Configuration

The CLI stores configuration under:

- `~/.codeq/config.yaml`

It supports profiles so you can switch between clusters.

## Common Commands

### Producer

Create a task:

````bash
codeq task create --event render_video --priority 10 --payload '{"jobId":500}'
````

Check task status:

````bash
codeq task get <task-id>
````

Get result:

````bash
codeq task result <task-id>
````

### Worker

Start a local worker loop:

````bash
codeq worker start --events render_video --concurrency 5
````

### Operator

Inspect queue depth:

````bash
codeq queue inspect render_video
````

## Authentication

The CLI can store:

- `producerToken` for `POST /tasks`
- `workerToken` (JWT) for worker endpoints

### Via Configuration

````bash
codeq auth set --producer <token>
codeq auth set --worker <token>
````

### Via IAM Login

There is an optional login helper (`codeq auth login`) that can fetch a producer token from an external IAM endpoint (template-driven).

For Tikti, the login helper fetches an `idToken` and then exchanges it for an `accessToken` via `/v1/accounts/token/exchange`.

````bash
codeq auth login
````

### Via Environment Variables

````bash
export CODEQ_PRODUCER_TOKEN=your-token
export CODEQ_WORKER_TOKEN=your-jwt
codeq task create --event TEST
````

## Complete Reference

For detailed documentation including all commands, options, and examples, see:

**[docs/15-cli-reference.md](../docs/15-cli-reference.md)**
