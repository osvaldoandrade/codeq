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

## Load Testing

codeQ includes a comprehensive load testing framework with k6 scenarios. Run load tests against your local environment:

````bash
# Start codeQ with dependencies
docker compose up -d

# Run a load test scenario
docker compose --profile loadtest run --rm k6 run /scripts/01_sustained_throughput.js
````

**Available scenarios:**
- `01_sustained_throughput.js` - 500 tasks/sec for 5 minutes (configurable via env vars)
- `02_burst_10k_10s.js` - Burst of 10,000 tasks in 10 seconds
- `03_many_workers.js` - 100+ concurrent worker instances
- `04_prefill_queue.js` - Fill queue with 100K+ pending tasks
- `05_mixed_priorities.js` - Mixed priority distribution
- `06_delayed_tasks.js` - Delayed task scheduling

**Customize scenarios with environment variables:**

````bash
# High throughput test
RATE=1000 DURATION=10m WORKER_VUS=300 \
  docker compose --profile loadtest run --rm k6 run /scripts/01_sustained_throughput.js

# Quick burst test
RATE=2000 BURST_DURATION=5s DRAIN_DURATION=2m WORKER_VUS=400 \
  docker compose --profile loadtest run --rm k6 run /scripts/02_burst_10k_10s.js
````

**Monitor performance:**
- View k6 output for latency percentiles and throughput
- Access Prometheus metrics at `http://localhost:9090`
- View Grafana dashboards at `http://localhost:3000`
- Use `/v1/codeq/admin/queues/:command` to monitor queue depth

For comprehensive load testing documentation, see:
- [`docs/26-load-testing.md`](26-load-testing.md) - Complete load testing guide
- [`loadtest/README.md`](../loadtest/README.md) - Scenario documentation and usage

