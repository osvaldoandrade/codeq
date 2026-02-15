# Security and authentication

## Producer auth

Producers must call the API using an **access token (RS256)** validated via JWKS:

- Request header: `Authorization: Bearer <accessToken>`
- Services validate access tokens locally using JWKS (`identityJwksUrl`), not via lookup.
- The default JWKS plugin supports standard JWT tokens with RSA signatures.
- For integration with OAuth2/OIDC providers, clients typically:

  - Obtain an ID token via login
  - Exchange it for an access token from your auth provider
  - Use the access token to call CodeQ APIs

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

### Dev fallback (producer as worker)

When `allowProducerAsWorker=true`, codeQ accepts a producer token for worker endpoints and maps it to a synthetic worker identity with wildcard `eventTypes` and full worker scopes. This is intended for local/dev environments only.

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

## Authentication Plugins

CodeQ uses a plugin-based authentication system. The default implementation validates JWT tokens using JWKS (JSON Web Key Sets), but you can implement custom authentication plugins for different auth systems.

See [Authentication Plugins](21-authentication-plugins.md) for details on:
- Using the default JWKS plugin
- Creating custom authentication plugins
- Plugin interface reference
- Migration guide
