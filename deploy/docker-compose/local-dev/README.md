# Local Development Compose

This stack runs codeQ from the repository source with hot reload.

```bash
docker compose \
  -f deploy/docker-compose/local-dev/compose.yaml \
  -f deploy/docker-compose/local-dev/compose.override.yaml \
  up -d
```

Optional observability:

```bash
docker compose \
  -f deploy/docker-compose/local-dev/compose.yaml \
  -f deploy/docker-compose/local-dev/compose.override.yaml \
  --profile obs up -d
```

Load testing:

```bash
docker compose \
  -f deploy/docker-compose/local-dev/compose.yaml \
  -f deploy/docker-compose/local-dev/compose.override.yaml \
  --profile loadtest run --rm k6 run /scripts/01_sustained_throughput.js
```
