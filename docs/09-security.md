# Security and authentication

## Producer auth

Producers use `Authorization: Bearer <idToken>` validated against the Identity service. The service calls `POST {identityServiceUrl}/v1/accounts/lookup?key={identityServiceApiKey}` with the id token. A non-200 response or empty user list returns `401`.

## Worker auth

Workers use JWTs validated against `workerJwksUrl`. Required claims:

- `iss`: issuer identifier
- `aud`: includes `codeq-worker`
- `sub`: worker identifier
- `exp`, `iat`, `jti`
- `eventTypes`: list of allowed event types
- `scope`: space-delimited permissions

Required scopes:

- `codeq:claim` for `/tasks/claim`
- `codeq:heartbeat` for `/tasks/:id/heartbeat`
- `codeq:abandon` for `/tasks/:id/abandon`
- `codeq:nack` for `/tasks/:id/nack`
- `codeq:result` for `/tasks/:id/result`
- `codeq:subscribe` for `/workers/subscriptions`

The worker ID is derived from `sub`. Request bodies do not provide `workerId`.

### Worker identity semantics

`sub` is the ownership identity stored in task records and leases. Two supported patterns:

- **Instance identity (recommended)**: each worker instance uses a unique `sub`. This prevents cross-instance interference and yields strict ownership.
- **Pool identity**: a worker pool shares a `sub`. Any instance may heartbeat or complete tasks claimed by another instance. This is acceptable when the pool is homogeneous.

Optional claim:

- `workerGroup`: used only for webhook grouping and routing. It does not grant access on its own.

When `workerGroup` is present, webhook subscriptions must use the same group id.

## Authorization rules

- Claim: requested `commands` must be a subset of token `eventTypes`.
- Heartbeat/abandon/nack/result: token `sub` must match `task.workerId`.
- Missing required scope returns `403`.
- Admin: require `role=ADMIN` or a separate admin issuer.

## Webhook security

Webhook registration requires a worker token. codeQ signs webhook notifications with an HMAC derived from the worker token or a configured shared secret. Workers must validate the signature and timestamp.
