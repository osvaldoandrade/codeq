# Configuration

## Core

- `port` (int): HTTP port, default 8080
  - Environment variable: `PORT`
- `timezone` (string): Timezone for task scheduling and logging, default `America/Sao_Paulo`
  - Environment variable: `TIMEZONE`
- `env` (string): Environment mode (`dev` or `prod`), affects validation strictness, default `dev`
  - Environment variable: `ENV`
  - In `dev` mode, missing auth config generates warnings instead of errors
- `logLevel` (string): Logging level (`debug`, `info`, `warn`, `error`), default `info`
  - Environment variable: `LOG_LEVEL`
- `logFormat` (string): Log output format (`json` or `text`), default `json`
  - Environment variable: `LOG_FORMAT`
  - Use `text` for local development, `json` for production structured logging

## Persistence Plugin

codeQ uses a pluggable persistence architecture allowing different storage backends. See `docs/26-persistence-plugin-system.md` for detailed information.

- `persistenceProvider` (string): Storage backend type, one of: `redis`, `memory`
  - Environment variable: `PERSISTENCE_PROVIDER`
  - Default: `redis` (for backward compatibility)
  - `redis`: Production-ready persistence using Redis or KVRocks
  - `memory`: In-memory storage for testing only (data lost on restart)
- `persistenceConfig` (JSON object): Provider-specific configuration
  - Environment variable: `PERSISTENCE_CONFIG` (JSON string)
  - Format varies by provider:
    - **Redis**: `{"addr":"localhost:6379","password":""}`
    - **Memory**: `{}`

### Backward Compatibility

If `persistenceProvider` is not configured, codeQ automatically defaults to Redis using legacy settings:

- `redisAddr` (string): KVRocks/Redis address, default `localhost:6379`
  - Environment variable: `REDIS_ADDR`
  - **Deprecated:** Use `persistenceProvider: redis` with `persistenceConfig` instead
- `redisPassword` (string, optional): KVRocks/Redis password
  - Environment variable: `REDIS_PASSWORD` or `KVROCKS_PASSWORD`
  - **Deprecated:** Use `persistenceProvider: redis` with `persistenceConfig` instead

### Example: Redis Plugin (Default)

````yaml
persistenceProvider: redis
persistenceConfig:
  addr: localhost:6379
  password: ""  # optional
````

### Example: Memory Plugin (Testing)

````yaml
persistenceProvider: memory
persistenceConfig: {}
````

## Tracing (OpenTelemetry)

Distributed tracing is optional and disabled by default. When enabled, codeQ exports spans via OTLP gRPC (compatible with Jaeger and Grafana Tempo).

- `tracingEnabled` (bool): Enable tracing, default `false`
  - Environment variable: `TRACING_ENABLED` (accepts: `true`, `1`, `yes`)
- `tracingServiceName` (string): Service name used in trace backend, default `codeq`
  - Environment variable: `TRACING_SERVICE_NAME` or `OTEL_SERVICE_NAME`
- `tracingOtlpEndpoint` (string): OTLP gRPC endpoint, default `localhost:4317`
  - Environment variable: `TRACING_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_ENDPOINT`
- `tracingOtlpInsecure` (bool): Use insecure (no-TLS) OTLP gRPC connection, default `false`
  - Environment variable: `TRACING_OTLP_INSECURE` or `OTEL_EXPORTER_OTLP_INSECURE`
  - Most local Jaeger/Tempo setups require `true`
- `tracingSampleRatio` (float): Trace sampling ratio (0 < ratio <= 1), default `1.0`
  - Environment variable: `TRACING_SAMPLE_RATIO`

## Scheduling

- `defaultLeaseSeconds` (int): Default task lease duration in seconds when not specified in claim request, default 300 (5 minutes)
  - Environment variable: `DEFAULT_LEASE_SECONDS`
  - Used by claim endpoint if `leaseSeconds` not provided in request body
- `requeueInspectLimit` (int): Maximum number of in-progress tasks to sample during claim-time lease repair, default 200
  - Environment variable: `REQUEUE_INSPECT_LIMIT`
  - Higher values increase repair thoroughness but add latency to claim operations
  - Uses `SRANDMEMBER` to randomly sample in-progress SET, then pipelines TTL checks
- `maxAttemptsDefault` (int): Default maximum retry attempts before moving task to DLQ, default 5
  - Environment variable: `MAX_ATTEMPTS_DEFAULT`
  - Can be overridden per-task via `maxAttempts` field in enqueue request

## Backoff

- `backoffPolicy` (string): Retry backoff algorithm, one of: `fixed`, `linear`, `exponential`, `exp_full_jitter`, `exp_equal_jitter`
  - Environment variable: `BACKOFF_POLICY`
  - Default: `exp_full_jitter`
  - See `docs/11-backoff.md` for algorithm details
- `backoffBaseSeconds` (int): Base delay for backoff calculation, default 5 seconds
  - Environment variable: `BACKOFF_BASE_SECONDS`
  - For exponential: initial delay = base × 2^(attempt-1)
- `backoffMaxSeconds` (int): Maximum backoff delay cap, default 900 seconds (15 minutes)
  - Environment variable: `BACKOFF_MAX_SECONDS`
  - Prevents unbounded exponential growth

## Producer auth

### Legacy JWKS Configuration (Deprecated)

The following fields are deprecated in favor of the new plugin-based authentication system (see `producerAuthProvider` below):

- `identityServiceUrl` (string): Authentication service base URL / issuer (example: `https://your-auth-server.com`)
  - Environment variable: `IDENTITY_SERVICE_URL`
  - Used to derive `identityJwksUrl` and `identityIssuer` if not explicitly set
  - Can be any OAuth2/OIDC compliant authentication service
- `identityJwksUrl` (string): JWKS URL for producer access tokens (default: `{identityServiceUrl}/v1/.well-known/jwks.json`)
  - Environment variable: `IDENTITY_JWKS_URL`
- `identityIssuer` (string): expected `iss` claim for producer access tokens (default: `{identityServiceUrl}`)
  - Environment variable: `IDENTITY_ISSUER`
- `identityAudience` (string, optional): expected `aud` claim for producer access tokens
  - Environment variable: `IDENTITY_AUDIENCE`
- `identityServiceApiKey` (string, optional): API key for integration with legacy identity services
  - Environment variable: `IDENTITY_SERVICE_API_KEY`
  - Used by deprecated identity middleware

### Plugin-Based Authentication (Recommended)

- `producerAuthProvider` (string): Authentication provider type for producer endpoints, one of: `jwks`, `static`
  - Environment variable: `PRODUCER_AUTH_PROVIDER`
  - Default: `jwks` if `identityJwksUrl` is set
  - See `docs/20-authentication-plugins.md` for provider details
- `producerAuthConfig` (JSON string or object): Provider-specific configuration
  - Environment variable: `PRODUCER_AUTH_CONFIG` (JSON string)
  - Format varies by provider (see authentication plugins documentation)

## Worker auth

### Legacy JWKS Configuration (Deprecated)

The following fields are deprecated in favor of the new plugin-based authentication system (see `workerAuthProvider` below):

- `workerJwksUrl` (string): JWKS URL for worker JWT validation
  - Environment variable: `WORKER_JWKS_URL`
- `workerAudience` (string): Expected `aud` claim in worker JWTs, default `codeq-worker`
  - Environment variable: `WORKER_AUDIENCE`
- `workerIssuer` (string): Expected `iss` claim in worker JWTs
  - Environment variable: `WORKER_ISSUER`
- `allowedClockSkewSeconds` (int): JWT timestamp tolerance in seconds, default 60
  - Environment variable: `ALLOWED_CLOCK_SKEW_SECONDS`
  - Allows for clock drift between issuer and codeQ server
- `allowProducerAsWorker` (bool): Development flag to allow producer tokens on worker endpoints, default `false`
  - Environment variable: `ALLOW_PRODUCER_AS_WORKER` (accepts: `true`, `1`, `yes`)
  - When `true`, producer tokens can claim tasks and are mapped to a wildcard worker identity (`*`)
  - **WARNING:** Use only in development; violates security boundaries in production

### Plugin-Based Authentication (Recommended)

- `workerAuthProvider` (string): Authentication provider type for worker endpoints, one of: `jwks`, `static`
  - Environment variable: `WORKER_AUTH_PROVIDER`
  - Default: `jwks` if `workerJwksUrl` is set
  - See `docs/20-authentication-plugins.md` for provider details
- `workerAuthConfig` (JSON string or object): Provider-specific configuration
  - Environment variable: `WORKER_AUTH_CONFIG` (JSON string)
  - Format varies by provider (see authentication plugins documentation)

## Webhooks

- `webhookHmacSecret` (string): Secret key for HMAC-SHA256 webhook signature generation
  - Environment variable: `WEBHOOK_HMAC_SECRET`
  - Required for webhook security; signatures sent in `X-CodeQ-Signature` header
  - Recommended: Generate with `openssl rand -hex 32`
- `subscriptionMinIntervalSeconds` (int): Minimum interval between worker availability notifications per subscription, default 5 seconds
  - Environment variable: `SUBSCRIPTION_MIN_INTERVAL_SECONDS`
  - Prevents webhook spam; enforced at subscription creation and heartbeat
- `subscriptionCleanupIntervalSeconds` (int): Background job interval for expired subscription removal, default 60 seconds
  - Environment variable: `SUBSCRIPTION_CLEANUP_INTERVAL_SECONDS`
- `resultWebhookMaxAttempts` (int): Maximum delivery attempts for task result webhooks, default 5
  - Environment variable: `RESULT_WEBHOOK_MAX_ATTEMPTS`
  - After max attempts, webhook delivery is abandoned (not retried)
- `resultWebhookBaseBackoffSeconds` (int): Base delay for result webhook retry backoff, default 2 seconds
  - Environment variable: `RESULT_WEBHOOK_BASE_BACKOFF_SECONDS`
- `resultWebhookMaxBackoffSeconds` (int): Maximum backoff delay for result webhook retries, default 60 seconds
  - Environment variable: `RESULT_WEBHOOK_MAX_BACKOFF_SECONDS`

## Artifact storage

- `localArtifactsDir` (string): Local filesystem directory for storing task result artifacts, default `/tmp/codeq-artifacts`
  - Environment variable: `LOCAL_ARTIFACTS_DIR`
  - Directory must be writable by codeQ process
  - **Note:** Artifacts are stored locally and not replicated across replicas
  - For production multi-replica deployments, use external object storage (S3, GCS, etc.)

## Rate limiting

Rate limiting is optional and disabled by default. When enabled, CodeQ uses a Redis-backed token bucket algorithm to enforce per-token rate limits.

Rate limit configuration is per scope (producer, worker, webhook, admin). Each scope has two parameters:

- `requestsPerMinute` (int): Maximum sustained request rate per minute, default 0 (disabled)
- `burstSize` (int): Maximum burst capacity (tokens in bucket), default 0 (disabled)

Both values must be greater than zero to enable rate limiting for a scope.

**Note:** The `webhook` scope is **not currently implemented** in the codebase. Only `producer`, `worker`, and `admin` scopes are actively enforced. The webhook rate limiting configuration is reserved for future use.

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
