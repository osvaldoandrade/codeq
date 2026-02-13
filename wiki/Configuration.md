# Configuration

## Core

- `port` (int): HTTP port, default 8080
- `redisAddr` (string): KVRocks/Redis address, default `localhost:6379`
- `redisPassword` (string, optional): KVRocks/Redis password
- `timezone` (string): default `America/Sao_Paulo`
- `env` (string): default `dev`
- `logLevel` (string): default `info`
- `logFormat` (string): `json|text`, default `json`

## Scheduling

- `defaultLeaseSeconds` (int): default 300
- `requeueInspectLimit` (int): default 200
- `maxAttemptsDefault` (int): default 5

## Backoff

- `backoffPolicy` (string): `fixed|linear|exponential|exp_full_jitter|exp_equal_jitter`
- `backoffBaseSeconds` (int): default 5
- `backoffMaxSeconds` (int): default 900

## Producer auth

- `identityServiceUrl` (string): Tikti base URL / issuer (example: `https://api.storifly.ai`)
- `identityJwksUrl` (string): JWKS URL for producer access tokens (default: `{identityServiceUrl}/v1/.well-known/jwks.json`)
- `identityIssuer` (string): expected `iss` for producer access tokens (default: `{identityServiceUrl}`)
- `identityAudience` (string, optional): expected `aud` for producer access tokens
- `identityServiceApiKey` (string, legacy): API key used by clients when calling `/v1/accounts/token/exchange?key=...` (not required for server-side JWT validation)

## Worker auth

- `workerJwksUrl` (string)
- `workerAudience` (string): default `codeq-worker`
- `workerIssuer` (string)
- `allowedClockSkewSeconds` (int): default 60
- `allowProducerAsWorker` (bool): when true, producer tokens can access worker endpoints and are mapped to a wildcard worker identity (dev only)

## Webhooks

- `webhookHmacSecret` (string)
- `subscriptionMinIntervalSeconds` (int): default 5
- `subscriptionCleanupIntervalSeconds` (int): default 60
- `resultWebhookMaxAttempts` (int): default 5
- `resultWebhookBaseBackoffSeconds` (int): default 2
- `resultWebhookMaxBackoffSeconds` (int): default 60

## Artifact storage

- `localArtifactsDir` (string): default `/tmp/codeq-artifacts`
