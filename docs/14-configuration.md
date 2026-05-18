# Configuration

The canonical file-based example lives at `deploy/config/codeq.example.yml`.
Docker Compose and Helm examples live under `deploy/docker-compose` and
`helm/codeq`.

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

## Persistence

codeq runs on an embedded Pebble store. The pluggable persistence
interface still lives in `pkg/persistence/` so future backends can be
added, but Pebble is the only supported and benchmarked path. See
`docs/27-persistence-plugin-system.md` for the interface contract and
`docs/07b-storage-pebble.md` for the Pebble keyspace layout.

- `persistenceProvider` (string): Storage backend, default `pebble`
  - Environment variable: `PERSISTENCE_PROVIDER`
  - `pebble`: embedded CockroachDB LSM, intra-process sharding, no
    external dependencies. **Use this.**
  - `memory`: in-memory only, data lost on restart. **Testing only.**
- `persistenceConfig` (JSON object): provider-specific configuration
  - Environment variable: `PERSISTENCE_CONFIG` (JSON string)
  - Pebble: `{"path":"./codeq-pebble","fsyncOnCommit":false,"numShards":4}`
  - Memory: `{}`

### Example: Pebble (default, embedded, single-node)

**Development (fast, non-durable):**

````yaml
persistenceProvider: pebble
persistenceConfig:
  path: ./codeq-pebble
  fsyncOnCommit: false
  numShards: 1
````

**Production (4-shard, high throughput):**

````yaml
persistenceProvider: pebble
persistenceConfig:
  path: /var/lib/codeq/pebble
  fsyncOnCommit: false
  numShards: 4
````

**Durability-critical (4-shard, durable):**

````yaml
persistenceProvider: pebble
persistenceConfig:
  path: /var/lib/codeq/pebble
  fsyncOnCommit: true
  numShards: 4
````

**Pebble configuration details:**

- `path` (string, required): Directory where Pebble databases are stored. Will be created if it doesn't exist. For `numShards > 1`, subdirectories `shard0/`, `shard1/`, etc. are created automatically.
- `fsyncOnCommit` (bool, optional, default: false):
  - `false`: Writes are buffered; faster (~83k tasks/sec), data lost on process crash
  - `true`: Writes are synced to disk; slower (~67k tasks/sec), durable across process restarts
- `numShards` (int, optional, default: 1): Number of independent Pebble instances for intra-process sharding (Phase 8)
  - `1`: Single database, traditional behavior
  - `2–4`: Recommended range; parallelizes commit pipelines and compaction
  - `8+`: Diminishing returns; scheduling overhead may exceed parallelism gains
  - Best practice: Set to CPU core count (e.g., 4 cores → `numShards: 4`)

**Pebble notes:**

- **Single-machine only**: Pebble uses an exclusive lock on the data directory. Only one codeQ process can run at a time per `path`.
- **Data persistence**: Data persists in `{path}` across restarts. This directory must exist and be writable.

**Use cases:**

- **`numShards: 1, fsyncOnCommit: false`**: Development, testing, evaluation
- **`numShards: 4, fsyncOnCommit: false`**: Single-node production, throughput-optimized (45k–83k tasks/sec)
- **`numShards: 4, fsyncOnCommit: true`**: Single-node production, durability-critical (~67k tasks/sec)

See `docs/30-performance-baselines.md` (section "Phase 8: Pebble Intra-Process Sharding") for detailed performance characteristics and benchmarks. See `docs/07b-storage-pebble.md` for storage layout and additional details.

### Example: Memory (testing only)

````yaml
persistenceProvider: memory
persistenceConfig: {}
````

## Raft replication (opt-in HA)

`cfg.Raft` enables [hashicorp/raft](https://github.com/hashicorp/raft)
replication on top of the embedded Pebble store. Each Pebble shard
becomes its own raft group; the leader applies writes locally and
replicates them via the consensus log to followers' FSMs. Reads stay
local on every node.

Full architecture, failover, and operational details in
[`docs/40-raft-replication.md`](40-raft-replication.md).

Config fields under `raft`:

- `enabled` (bool, default false): enable raft.
- `selfId` (string, required when enabled): stable raft server ID.
  Must be unique per node and appear in `peers`.
- `bindAddr` (string, required when enabled): base TCP address for
  this node's raft transports, e.g. `127.0.0.1:7000`. Multi-shard
  uses `bindAddr+shardIdx` per group.
- `bootstrap` (bool): set true on exactly ONE node when forming a new
  cluster. Ignored on subsequent restarts (raft preserves state).
- `peers` (map[string]string): peer ID → base bind address for every
  node including self.
- `heartbeatMS` / `electionMS` / `leaderLeaseMS` / `commitMS` (int,
  optional): raft timing knobs; defaults match hashicorp/raft defaults
  (1000 / 1000 / 500 / 50).
- `applyTimeoutSeconds` (int, optional, default 10): per-write
  raft.Apply timeout.

**Mutual exclusion**: raft is rejected at startup if `cluster.enabled`
or `sharding.enabled` is also set, or if `persistenceProvider != pebble`.

### Example: 3-node raft cluster

````yaml
persistenceProvider: pebble
persistenceConfig:
  path: /var/lib/codeq/pebble
  numShards: 4

raft:
  enabled: true
  selfId: node-1            # different per node
  bindAddr: 127.0.0.1:7000  # different per node
  bootstrap: true           # ONLY on node-1 during initial cluster formation
  peers:
    node-1: 127.0.0.1:7000
    node-2: 127.0.0.2:7000
    node-3: 127.0.0.3:7000
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

### Worker Streaming gRPC

- `workerStreamAddr` (string, optional): Enables the bidirectional worker gRPC stream on the provided listen address, for example `:9091`
  - Environment variable: `WORKER_STREAM_ADDR`
  - Empty by default; REST worker endpoints remain available either way
  - Uses the same worker auth, scopes, event type authorization, tenant resolution, and `allowProducerAsWorker` behavior as the REST worker path

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

Rate limiting is optional and disabled by default. Two implementations:

- **Pure-Pebble path** (default): an in-process token-bucket limiter
  enforces the configured limits **per-node**. In a cluster of N nodes,
  the effective ceiling per (scope, subject) is N × `requestsPerMinute`.
  No external coordinator; state is in-process memory, GC'd after 10
  minutes of idle per bucket.
- **Redis-backed path** (legacy): Lua-script token bucket with shared
  state across all codeq instances connected to the same Redis. Used
  automatically when `persistenceProvider != pebble`.

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
- **Fail-open**: If the limiter backend is unreachable, rate limiting is bypassed to prevent outages
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

## Clustering

Clustering enables horizontal scaling across multiple codeQ nodes using a consistent-hash ring for task ownership. When disabled, codeQ runs as a single-node instance (default behavior, no configuration needed).

### Configuration

````yaml
cluster:
  enabled: true
  
  # Node definitions in the cluster
  nodes:
    - id: "node-1"
      grpc_addr: "codeq-1.internal:50051"
    - id: "node-2"
      grpc_addr: "codeq-2.internal:50051"
    - id: "node-3"
      grpc_addr: "codeq-3.internal:50051"
  
  # Optional: Bloom filter for negative lookup optimization
  bloom:
    enabled: true
    capacity: 100000          # max items per bloom
    false_positive_rate: 0.01 # 1% FP rate
  
  # Optional: gRPC client configuration
  grpc:
    timeout_seconds: 10
    # tls_enabled: true      # mTLS configuration (future)
````

### Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enable cluster mode (default: single-node) |
| `nodes[*].id` | string | required | Stable node identifier (appears in logs, metrics) |
| `nodes[*].grpc_addr` | string | required | Host:port for internal gRPC server |
| `bloom.enabled` | bool | `true` | Enable Bloom filter caching for negative lookups |
| `bloom.capacity` | int | `100000` | Maximum number of items per Bloom filter |
| `bloom.false_positive_rate` | float | `0.01` | Target false positive rate (0.0 - 1.0) |
| `grpc.timeout_seconds` | int | `10` | RPC timeout for inter-node calls |

### Deployment Notes

1. **Static membership**: All nodes must know all other nodes at startup. Changes require rolling restart.
2. **Local storage**: Each node runs an embedded Pebble shard. Tasks lost on node crash (no replication).
3. **REST API unchanged**: Producers and workers still use HTTP; gRPC is internal to the cluster.
4. **Load distribution**: Virtual nodes (256 per real node) ensure smooth load balancing (~5% std dev for 3-16 nodes).
5. **No automatic recovery**: Operator must provision replacement nodes with same ID.

### Monitoring

Cluster health can be monitored via metrics:

- `codeq_cluster_nodes_total`: Number of nodes in ring
- `codeq_cluster_local_hash_load`: Proportion of hash space owned by this node
- `codeq_cluster_bloom_stale`: Age of cached peer blooms (in seconds)

### Related

See [05-cluster-architecture.md](05-cluster-architecture.md) for detailed clustering concepts and request routing patterns.
