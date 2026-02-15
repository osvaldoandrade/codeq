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

- `identityServiceUrl` (string): Authentication service base URL / issuer (example: `https://your-auth-server.com`)
  - Used to derive `identityJwksUrl` and `identityIssuer` if not explicitly set
  - Can be any OAuth2/OIDC compliant authentication service
- `identityJwksUrl` (string): JWKS URL for producer access tokens (default: `{identityServiceUrl}/v1/.well-known/jwks.json`)
- `identityIssuer` (string): expected `iss` for producer access tokens (default: `{identityServiceUrl}`)
- `identityAudience` (string, optional): expected `aud` for producer access tokens
- `identityServiceApiKey` (string, optional): API key for integration with legacy auth services

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

## Rate limiting

Rate limiting is optional and disabled by default. When enabled, CodeQ uses a Redis-backed token bucket algorithm to enforce per-token rate limits.

Rate limit configuration is per scope (producer, worker, webhook, admin). Each scope has two parameters:

- `requestsPerMinute` (int): Maximum sustained request rate per minute, default 0 (disabled)
- `burstSize` (int): Maximum burst capacity (tokens in bucket), default 0 (disabled)

Both values must be greater than zero to enable rate limiting for a scope.

**Note:** The `webhook` scope is currently reserved for future use and is not yet implemented in the codebase. Only `producer`, `worker`, and `admin` scopes are actively enforced.

### YAML configuration

````yaml
rateLimit:
  producer:
    requestsPerMinute: 1000  # 1000 requests/min sustained
    burstSize: 100           # Allow bursts up to 100 requests
  worker:
    requestsPerMinute: 600
    burstSize: 50
  webhook:
    requestsPerMinute: 600
    burstSize: 100
  admin:
    requestsPerMinute: 30
    burstSize: 5
````

### Behavior

- **Token bucket algorithm**: Tokens refill at `requestsPerMinute / 60` per second, up to `burstSize` capacity
- **Per-bearer-token**: Rate limits are scoped per individual bearer token (SHA256-hashed for privacy)
- **Fail-open**: If Redis is unreachable, rate limiting is bypassed to prevent outages
- **429 responses**: When rate limit is exceeded, API returns `429 Too Many Requests` with `Retry-After` header (in seconds)
- **TTL management**: Token bucket state expires automatically after ~2 refill cycles to bound memory usage

### Metrics

Rate limit rejections are tracked by the `codeq_rate_limit_hits_total` counter with labels:
- `scope`: `producer`, `worker`, or `admin` (webhook scope reserved for future use)
- `operation`: `create_task`, `claim`, or `cleanup`

### Example: setting producer limits

For a producer creating up to 1000 tasks/minute with occasional bursts:

````yaml
rateLimit:
  producer:
    requestsPerMinute: 1000
    burstSize: 100
````

This allows:
- Sustained rate of ~16.7 requests/second
- Bursts up to 100 requests before throttling
- Tokens refill at ~16.7/second

### Disabling rate limiting

Omit the `rateLimit` section or set both values to 0:

````yaml
rateLimit:
  producer:
    requestsPerMinute: 0
    burstSize: 0
````
