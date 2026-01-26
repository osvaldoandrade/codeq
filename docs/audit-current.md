# Audit: Current Scheduler Service (pre-codeQ)

This document captures the current behavior of the repository before the codeQ refactor.

## Routes

Base path: `/v1/scheduler`

- `POST /v1/scheduler/tasks`
  - Auth: `Authorization: Bearer <idToken>`
  - Body: `command`, `payload`, `priority?`, `webhook?`
  - Response: `202` with task JSON

- `POST /v1/scheduler/tasks/claim`
  - Auth: `Authorization: Bearer <idToken>`
  - Body: `workerId`, `commands?`, `leaseSeconds?`
  - Response: `200` with task JSON or `204` if empty

- `POST /v1/scheduler/tasks/:id/heartbeat`
  - Auth: `Authorization: Bearer <idToken>`
  - Body: `workerId`, `extendSeconds?`
  - Response: `200` or `403` if not owner

- `POST /v1/scheduler/tasks/:id/abandon`
  - Auth: `Authorization: Bearer <idToken>`
  - Body: `workerId`
  - Response: `200` or `403` if not owner

- `GET /v1/scheduler/tasks/:id`
  - Auth: `Authorization: Bearer <idToken>`
  - Response: `200` with task JSON or `404`

- `GET /v1/scheduler/admin/queues`
  - Auth: `Authorization: Bearer <idToken>` + `X-Role: ADMIN`
  - Response: `200` with queue lengths

- `POST /v1/scheduler/admin/tasks/cleanup`
  - Auth: `Authorization: Bearer <idToken>` + `X-Role: ADMIN`
  - Body: `limit?`, `before?` (RFC3339)
  - Response: `200` with `{deleted, before, limit}`

Health:

- `GET /healthz` (no auth)

## Auth behavior

- All `/v1/scheduler` routes use `AuthMiddleware`.
- Identity lookup: `POST {IdentityServiceURL}/v1/accounts/lookup?key={IdentityServiceApiKey}` with JSON `{idToken}`.
- Role is derived from request header `X-Role`, default `USER` if missing.
- Admin endpoints require `userRole == ADMIN`.

## Storage (Redis/KVRocks)

Key prefix: `legacy:`

- `legacy:tasks` (hash): field = task id, value = task JSON string
- `legacy:tasks:ttl` (ZSET): member = task id, score = expireAt epoch seconds
- `legacy:lease:<id>` (string): value = workerId, TTL = lease
- `legacy:q:<command>:pending` (list)
- `legacy:q:<command>:inprog` (list)

Commands (event types):

- `GENERATE_MASTER`
- `GENERATE_CREATIVE`

Retention:

- `taskRetention = 24h` (hard-coded)
- `bumpTTL` called on create/claim/heartbeat/abandon/requeue

Queue behavior:

- Create: `LPUSH` pending list
- Claim: `RPOPLPUSH` pending -> inprog, `SETEX` lease
- Requeue expired: `LRANGE` inprog, `TTL` lease <= 0 -> `LREM` inprog + `LPUSH` pending
- Cleanup: `ZRANGEBYSCORE` on ttl index, then delete hash/zset/lease and `LREM` from lists

## Task model

Fields used in storage:

- `id`, `command`, `payload` (JSON string), `priority`, `webhook`, `status`, `workerId`, `leaseUntil`, `createdAt`, `updatedAt`
- Status constants defined: `PENDING`, `IN_PROGRESS`, `COMPLETED`, `FAILED`
- Only `PENDING` and `IN_PROGRESS` used by scheduler flows

## Config

Config struct fields:

- `port`, `redisAddr`, `identityServiceUrl`, `identityServiceApiKey`
- `timezone`, `logLevel`, `logFormat`, `env`
- `defaultLeaseSeconds` (default 300)
- `requeueInspectLimit` (default 200)

Env overrides:

- `PORT`, `REDIS_ADDR`, `IDENTITY_SERVICE_URL`, `IDENTITY_SERVICE_API_KEY`

Default values:

- `port=8080`, `redisAddr=localhost:6379`, `timezone=America/Sao_Paulo`, `logLevel=info`, `logFormat=json`, `env=dev`

## Tests

- `test_scheduler_service.py` covers:
  - `/healthz`
  - create task (with and without auth)
  - claim
  - get task
  - heartbeat
  - abandon
  - admin queues (requires `X-Role: ADMIN`)
- Uses Identity `signInWithPassword` to obtain `idToken`
- Does not cover admin cleanup endpoint

## External services

- Identity service used for token validation (`/v1/accounts/lookup`)
- No worker-specific auth or JWKS validation in current code

## Naming Decisions (codeQ)

- Service name: `codeQ`
- Keyspace prefix: `codeq:`
- API base path: `/v1/codeq`
- GitHub pages repo link: `TBD` (confirm canonical repo URL)
