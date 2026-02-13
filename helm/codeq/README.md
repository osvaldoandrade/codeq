# codeQ Helm Chart

This chart deploys the codeQ API and (optionally) a single-node KVRocks instance for small clusters.

## Quick install

```bash
helm install codeq ./helm/codeq \
  --set secrets.enabled=true \
  --set secrets.webhookHmacSecret=YOUR_SECRET \
  --set config.identityServiceUrl=https://api.storifly.ai \
  --set config.workerJwksUrl=https://your-jwks \
  --set config.workerIssuer=https://issuer
```

By default, `kvrocks.enabled=true` and codeQ will point to the embedded KVRocks Service.

## Disable embedded KVRocks

```bash
helm install codeq ./helm/codeq \
  --set kvrocks.enabled=false \
  --set config.redisAddr=your-kvrocks:6666
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
