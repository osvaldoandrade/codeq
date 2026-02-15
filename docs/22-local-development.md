# Local Development (docker-compose)

This guide sets up a full local environment for codeQ development using Docker Compose:

- KVRocks (Redis-compatible)
- codeQ API server (runs from source, with hot reload)
- Optional: Prometheus + Grafana + Jaeger (`--profile obs`)

## Quick Start

1. Create your local env file (optional):

````bash
cp .env.example .env
````

2. Start everything:

````bash
docker compose up -d
````

3. Verify:

````bash
curl -sSf http://localhost:8080/metrics | head
````

4. Watch logs:

````bash
docker compose logs -f codeq
````

5. Stop:

````bash
docker compose down
````

## Hot Reload

`docker-compose.override.yml` runs the API server via `air` using `.air.toml`.
Any change under `./internal`, `./pkg`, `./cmd`, etc should trigger a rebuild/restart.

## Seeded Example Tasks

The `seed` service creates a few tasks on startup (idempotent via `idempotencyKey`).

Check queue stats (uses the static dev token by default):

````bash
TOKEN="${CODEQ_PRODUCER_TOKEN:-dev-token}"
curl -sS -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/v1/codeq/admin/queues | jq .
````

## Run Tests Inside the Container

````bash
docker compose exec codeq go test ./...
````

## Optional Observability Stack (Prometheus + Grafana + Jaeger)

Start with the `obs` profile:

````bash
docker compose --profile obs up -d
````

Then open:

- Prometheus: `http://localhost:9090`
- Grafana: `http://localhost:3000` (default user/pass: `admin` / `admin`)
- Jaeger UI: `http://localhost:16686`

Grafana auto-provisions:

- Prometheus datasource (`http://prometheus:9090`)
- Dashboard JSON from `docs/grafana/codeq-dashboard.json`

