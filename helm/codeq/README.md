# codeQ Helm Chart

This chart deploys codeQ. Persistence is Pebble, configured as the
persistence provider. The chart does not deploy a separate database.

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

## Size profiles

The `codeq install` command emits a values file for `dev`, `small`, `medium`,
or `large`. Every profile persists on Pebble.

Example:

```bash
helm upgrade --install codeq ./helm/codeq \
  -f ./helm/codeq/values-medium.yaml \
  --namespace codeq --create-namespace \
  --set persistence.pebble.enabled=true \
  --set secrets.enabled=true \
  --set secrets.webhookHmacSecret=YOUR_SECRET \
  --set config.identityServiceUrl=https://issuer.example.com \
  --set config.workerJwksUrl=https://issuer.example.com/.well-known/jwks.json \
  --set config.workerIssuer=https://issuer.example.com
```

## Persistence

CodeQ persists on Pebble. The chart mounts `persistence.pebble.path` from a PVC when `persistence.pebble.enabled` is true.

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
  --identity-service-url https://issuer.example.com \
  --worker-jwks-url https://issuer.example.com/.well-known/jwks.json \
  --worker-issuer https://issuer.example.com
```

## Values

Key values:

- `image.repository`, `image.tag`: codeQ image
- `persistence.pebble.path`: Pebble data directory inside the pod
- `config.identityServiceUrl`: Tikti base URL / issuer (used to derive `identityJwksUrl` by default)
- `config.workerJwksUrl`, `config.workerIssuer`: required for worker JWT validation
- `secrets.enabled`: enable Secret for `webhookHmacSecret` (and legacy `identityServiceApiKey`)
- `persistence.pebble.enabled`: persist the Pebble directory on a PVC

See `values.yaml` for the complete list.
