# codeQ Helm Chart

This chart deploys the codeQ API and, optionally, a single-node KVRocks instance.
For production, use a size profile and override identity, worker auth, secrets,
and Redis/KVRocks settings for your environment.

## Quick install

```bash
helm upgrade --install codeq ./helm/codeq \
  -f ./helm/codeq/values-small.yaml \
  --namespace codeq --create-namespace \
  --set secrets.enabled=true \
  --set secrets.webhookHmacSecret=YOUR_SECRET \
  --set config.identityServiceUrl=https://api.storifly.ai \
  --set config.workerJwksUrl=https://your-jwks \
  --set config.workerIssuer=https://issuer
```

By default, `values-small.yaml` deploys embedded KVRocks. For medium and large
profiles, use an external KVRocks/Redis-compatible service.

## Size profiles

| Profile | File | Intent |
| --- | --- | --- |
| `dev` | `values-dev.yaml` | Local or ephemeral namespaces with static dev tokens |
| `small` | `values-small.yaml` | Small production server, embedded KVRocks |
| `medium` | `values-medium.yaml` | Multi-replica production, external KVRocks recommended |
| `large` | `values-large.yaml` | Higher traffic production, external KVRocks required |

Example:

```bash
helm upgrade --install codeq ./helm/codeq \
  -f ./helm/codeq/values-medium.yaml \
  --namespace codeq --create-namespace \
  --set kvrocks.enabled=false \
  --set config.redisAddr=kvrocks.prod.svc.cluster.local:6666 \
  --set secrets.enabled=true \
  --set secrets.webhookHmacSecret=YOUR_SECRET \
  --set config.identityServiceUrl=https://issuer.example.com \
  --set config.workerJwksUrl=https://issuer.example.com/.well-known/jwks.json \
  --set config.workerIssuer=https://issuer.example.com
```

## Disable embedded KVRocks

```bash
helm upgrade --install codeq ./helm/codeq \
  --set kvrocks.enabled=false \
  --set config.redisAddr=your-kvrocks:6666
```

## Console wizard

The CLI can generate a Docker or Helm installation bundle:

```bash
codeq install
```

For a non-interactive Kubernetes plan:

```bash
codeq install \
  --target kubernetes \
  --size medium \
  --namespace codeq \
  --redis-addr kvrocks.prod.svc.cluster.local:6666 \
  --identity-service-url https://issuer.example.com \
  --worker-jwks-url https://issuer.example.com/.well-known/jwks.json \
  --worker-issuer https://issuer.example.com
```

## Values

Key values:

- `image.repository`, `image.tag`: codeQ image
- `config.redisAddr`: external KVRocks address (ignored when `kvrocks.enabled=true`)
- `config.identityServiceUrl`: Tikti base URL / issuer (used to derive `identityJwksUrl` by default)
- `config.workerJwksUrl`, `config.workerIssuer`: required for worker JWT validation
- `secrets.enabled`: enable Secret for `webhookHmacSecret` (and legacy `identityServiceApiKey`)
- `kvrocks.enabled`: deploy embedded KVRocks (single node)

See `values.yaml` for the complete list.
