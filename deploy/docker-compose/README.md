# Docker Compose Deployments

This directory contains Compose files for running codeQ outside Kubernetes.

## Local Development

Use the source-mounted stack when developing codeQ itself:

```bash
docker compose \
  -f deploy/docker-compose/local-dev/compose.yaml \
  -f deploy/docker-compose/local-dev/compose.override.yaml \
  up -d
```

The local stack starts codeq from source with an embedded Pebble store,
seed data, and optional observability services.

## Single-Node Production

Use the production template for a small server deployment:

```bash
cp deploy/docker-compose/production/.env.example .env.codeq
docker compose --env-file .env.codeq \
  -f deploy/docker-compose/production/compose.yaml \
  up -d
```

Edit `.env.codeq` before production use; the example contains placeholder
issuer URLs and a placeholder webhook secret.

For multi-replica production, prefer the Helm chart in `helm/codeq`.
