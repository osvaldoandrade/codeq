# Absolute task expiry

`absoluteExpiry` is an opt-in topic policy for managed Logs batches. Ordinary
topics keep their previous retention behavior. Expiration is **not** inferred
from message content: consumers use the accepted Task `createdAt` / `expiresAt`.

## Configuration and activation

The server entrypoint is `./cmd/server`; `./cmd/codeq` is the CLI. Run a qualified
binary with `PERSISTENCE_PROVIDER=pebble` and a JSON `PERSISTENCE_CONFIG`, for
example `{"path":"<isolated-data-directory>","fsyncOnCommit":true}`. Never point
two processes at one Pebble directory. Raft peers each need their own directory,
the capable binary and `fsyncOnCommit=true`; the local setting alone cannot
attest peer configuration. Defaults remain false and no topic is opted in
automatically. The memory plugin does not accept absolute tasks.

An authorized topic owner calls the existing
`PUT /v1/codeq/admin/topics/cf-logs` with a policy such as:

```json
{
  "priorityTiers": [0],
  "maxAttempts": 100,
  "deadLetterTopicRef": "cf-logs-dlq",
  "retentionSeconds": 86400,
  "absoluteExpiry": true
}
```

On replicated installations, topic catalog protocol `v1` must already be enabled
on all peers under its existing rollout procedure. Non-durable activation is
422. Policy `visibilityFloor` is an optional RFC3339 timestamp: accepted tasks
created at or before it are hidden and retired. The stored floor only increases.
Changing retention preserves the old cutoff; increasing retention cannot
resurrect existing records.

## Task lifecycle

`POST /v1/codeq/tasks` and each item of `/tasks/batch` accept `expiresAt` as a
nonzero RFC3339Nano string. An absolute topic requires it. For a new task it must
be after creation and no later than creation plus the topic retention. A stable
named task ID retries the same command and payload, returning the original
`createdAt` and `expiresAt` even if the retry proposes a later deadline. Changed
content or a conflicting tenant returns 409. Missing/malformed/beyond-policy
deadlines return 400; an expired task identity returns 410 (`task_expired`).
Absolute-task HTTP and batch payloads preserve JSON numeric precision, including
integers above 2^53; legacy topics keep their previous numeric normalization.

Claims, heartbeat, result, retry, abandon and DLQ requeue never extend expiry.
Leases are capped at the effective deadline. Reads/listing/claims hide expired
tasks immediately; late writes fail closed. The bounded reaper removes payload,
result, lease and queue/TTL/idempotency/deduplication references in every state.
Its rotating scan is bounded to 512 identities per normal sweep, so physical
cleanup is asynchronous and its delay must be measured at installation load.
TTL/queue counts can reflect rows waiting for physical retirement.

Retirement keeps a no-payload tombstone for 48 hours after actual retirement.
Retries do not extend it. Once it expires the ID may be reused: this is not
eternal deduplication. The Logs gateway separately enforces the approved source
timestamp/replay boundary. Topic disable/delete returns 409 while tasks or
tombstones remain; unknown shard custody also fails closed. Legacy unreplicated
multi-node Cluster is not qualified for cross-node retirement.

`absoluteExpiryMaxIdentitiesPerShard` (env
`ABSOLUTE_EXPIRY_MAX_IDENTITIES_PER_SHARD`) defaults to 1,000,000 reserved
identities per local shard. It is an operational budget, not a product limit.
At capacity new absolute identities receive 429 with `Retry-After: 30`; ordinary
topics, existing valid retries and retirement are unaffected. Dimension it from
batch size, acceptance rate, retention, metadata and the 48-hour replay window.
Increasing it never authorizes additional infrastructure automatically.

Metrics (only the configured bounded shard label):

- `codeq_absolute_identity_usage`
- `codeq_absolute_identity_capacity`
- `codeq_absolute_identity_refused_total`
- `codeq_absolute_expired_purged_total`

## Local integration with synthetic JWT/JWKS

For an isolated process test, build `go build -o <test-binary> ./cmd/server`.
Start a test-owned JWKS HTTP server with a disposable RSA key (publish only its
public JWK with `kid`, `n`, `e`). Set `PORT`, `LOCAL_ARTIFACTS_DIR`,
`PERSISTENCE_PROVIDER=pebble`, the JSON `PERSISTENCE_CONFIG` above,
`IDENTITY_JWKS_URL`, `WORKER_JWKS_URL`, `IDENTITY_ISSUER`, `WORKER_ISSUER` and
`WORKER_AUDIENCE=codeq-worker` for the test fixture. Explicit auth provider JSON:

```json
{
  "jwksUrl": "<test-jwks-url>",
  "issuer": "<test-issuer>",
  "audience": "codeq-producer",
  "httpTimeout": 5000000000
}
```

Use `PRODUCER_AUTH_PROVIDER=jwks` / `PRODUCER_AUTH_CONFIG` for this object and
`WORKER_AUTH_PROVIDER=jwks` / `WORKER_AUTH_CONFIG` for the same object with
audience `codeq-worker`. Keep generated private keys/tokens inside the fixture;
do not print them or pass them as command-line arguments. A test-only `dev`
configuration avoids unrelated production webhook configuration. Production
deployment retains its existing authority and transport configuration.

Sign short-lived RS256 tokens with the JWKS `kid`. Canonical binding claims are:

```json
{
  "iss": "<test-issuer>",
  "aud": "codeq-producer",
  "sub": "codefoundry:workload:<cluster>:<namespace>:<service>:<pod-uid>",
  "tid": "conveste",
  "iat": 0,
  "exp": 0,
  "scope": "codeq:publish",
  "eventTypes": ["cf-logs"],
  "codeq_binding": {
    "uid": "<ObsConfig-UID>",
    "generation": 1,
    "policy": "Publish",
    "topicId": "conveste.cf-logs"
  }
}
```

Replace `iat`/`exp` with current integer seconds, lifetime at most 300 seconds.
Do not add `role`, `tenant_epoch`, `topic_controller`, legacy tenant aliases or
extra keys inside `codeq_binding`. A Subscribe token uses the same tenant/topic
and ObsConfig UID/generation, its worker subject, audience `codeq-worker`, policy
`Subscribe`, and exactly these scopes:
`codeq:abandon codeq:claim codeq:heartbeat codeq:nack codeq:result`.

Provision the topic with a separate authorized test admin token (`codeq:admin`,
producer audience, canonical `tid`, no binding marker), then use header-auth HTTP:

1. Publish: `POST /v1/codeq/tasks`, command `cf-logs`, stable taskId, JSON payload,
   maxAttempts 100 and explicit expiresAt. Expect 202 with accepted Task metadata.
2. Claim: `POST /v1/codeq/tasks/claim`,
   `{"commands":["cf-logs"],"leaseSeconds":60}`. Expect Task metadata unchanged.
3. Heartbeat: `POST /v1/codeq/tasks/<id>/heartbeat`, `{"extendSeconds":60}` using
   the same worker subject that claimed it.
4. Complete: `POST /v1/codeq/tasks/<id>/result`,
   `{"status":"COMPLETED","result":{"accepted":1}}` using that worker.

Binding tokens are HTTP-only; producer/worker gRPC streams intentionally reject
them under ADR-0022. CodeQ validates the carrier's shape, signature, tenant,
policy and ownership; the platform issuer owns ObsConfig provenance. The real
RSA/JWKS HTTP regression is
`TestAbsoluteExpiryStrictBindingJWKSHTTPWorkerLifecycle`; real Raft/failure and
restart tests complement it. Cross-process Logs gateway/worker and storage
acceptance belongs to the platform integration test, not this fixture alone.

## Rollback

Stop new collection before draining. Keep capable readers, durable writers,
fsync and cleanup until active tasks and tombstones drain. Then disable/delete
the topic, followed by a compatible binary rollback. Never erase task indexes,
disable fsync early or replace capable peers with expiry-unaware writers while
absolute data remains. A failed fsync cannot be acknowledged as durable; a
same-ID retry must establish a new durable fence before returning the old receipt.
