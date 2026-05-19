# Authentication And Authorization

Authentication in codeQ answers a single question on every request: who is calling, and what are they allowed to do. The answer is a `*auth.Claims` value attached to the request context, and every subsequent decision the service makes — which tenant a task belongs to, whether a worker may claim it, whether a producer may publish for a given event type — reads from that one struct. There is no second lookup, no session table, no per-request database round trip; the claims are derived from a bearer token validated by a pluggable `auth.Validator` interface and then carried in-process through Gin's `*gin.Context`.

The design is deliberately conservative. Tokens are validated once at the HTTP edge, the validator decides what the token means (issuer, audience, scopes, event types), and downstream code only reads fields off `auth.Claims`. The validator itself is chosen at startup from a registry of named providers — `static`, `jwks`, or any custom provider compiled into the binary — and the same registry powers two parallel auth surfaces: one for producers (the side that publishes tasks) and one for workers (the side that claims them). Both pipelines share the same validator interface but enforce different shape requirements on the claims.

## The validator contract

The starting point is `pkg/auth/interface.go`. A `Validator` is anything with one method:

```go
type Validator interface {
    Validate(token string) (*Claims, error)
}
```

That is the entire surface. Claims, defined in the same file, carry the standard JWT triplet — subject, issuer, audience, issued-at, expires-at — alongside two codeQ-specific fields: `Scopes` (the set of operations this token is allowed to perform) and `EventTypes` (the set of task event types this token is allowed to publish or claim). The `Raw` field preserves the original claim map so downstream code can read provider-specific fields without forcing them into the typed shape. Tenant identity travels here; it is not a first-class field on `Claims` because not every deployment has multi-tenancy enabled.

A validator that returns claims with empty scopes or empty event types will be rejected upstream by the worker auth middleware (`internal/middleware/worker_auth.go:24-28`). The contract is "return what the token says, plain"; gating policy lives in middleware, not in the validator.

## The provider registry

Providers register themselves at package-init time into a process-wide map keyed by name. `pkg/auth/registry.go` exposes `RegisterProvider(name string, factory ValidatorFactory)` for the install side and `NewValidator(ProviderConfig) (Validator, error)` for the consume side. A `ProviderConfig` carries the provider name and an opaque `json.RawMessage` that the factory unmarshals on its own — each provider owns its config schema, so adding a new validator does not require changes to the configuration loader.

The two providers shipped with codeQ register in their own `init()` functions:

```go
// pkg/auth/jwks/validator.go:41-44
func init() {
    auth.RegisterProvider("jwks", NewValidatorFromJSON)
}

// pkg/auth/static/validator.go:83-85
func init() {
    auth.RegisterProvider("static", NewValidatorFromJSON)
}
```

Importing either package is enough to make it selectable; the application's `application_pebble.go` wires both providers by side-effect import. Custom validators follow the same pattern: implement `auth.Validator`, expose a `NewValidatorFromJSON` factory, register from `init()`, and the registry picks it up.

The factory takes `json.RawMessage` so the embedding YAML or JSON config can stay schema-agnostic. The provider unmarshals into its own struct, validates, and returns. If the provider is unknown the registry returns `unknown auth provider type: <name>` and the application refuses to start — a deliberate fail-closed default.

## JWKS validation

The JWKS validator at `pkg/auth/jwks/validator.go` is the production path. It downloads a JSON Web Key Set from a configured URL, caches the parsed RSA public keys for five minutes, and validates incoming tokens by `kid` lookup. The flow on each `Validate(tokenString)` call is straightforward: parse the JWT, read the `kid` from the header, fetch (or hit-cache) the matching RSA public key, verify the signature, then walk the claims and enforce the configured issuer and audience. Expiration is checked with a configurable clock skew tolerance — the default is zero, deployments with tight clocks usually leave it alone, deployments that share a token across a federated mesh may set a few seconds.

The validator is a thin layer over `github.com/golang-jwt/jwt/v5`; codeQ does not implement its own crypto. The key cache is guarded by `sync.RWMutex` and refreshed lazily — the first request after the cache TTL expires pays one HTTP round trip to the JWKS endpoint, subsequent requests within the window are pure in-memory work. There is no background refresh; if the JWKS endpoint goes down, cached keys keep working until the TTL elapses, then validation starts failing. This is a conservative choice — a misbehaving identity provider degrades gracefully, but rotation latency is bounded by the cache lifetime.

Scopes from a JWKS-issued token are read from the `scope` claim as a space-separated string, in the OAuth2 convention. Event types come from a JSON array under the `eventTypes` claim — codeQ extends standard JWT claims here because there is no standard way to express "this worker may claim tasks of type X". The extension is conservative; it lives in the claim set, not in a sidecar.

## Static validation

`pkg/auth/static/validator.go` is the development and small-deployment path. It compares the incoming bearer token to a single configured value as a constant-time string match and returns a fixed `*auth.Claims` payload if they match. The config carries the subject, optional email, scopes, event types, and a `raw` map that drops straight onto `claims.Raw`. There is no signature verification, no expiry, no key rotation — the token IS the credential, and the operator is responsible for rotating it manually when needed.

This is the validator the docker-compose layouts in `deploy/docker-compose/raft-cluster/` and `deploy/docker-compose/cluster/` use out of the box. It removes the need to stand up an identity provider for local experimentation, and it makes the auth contract observable in plain text — the static config IS the policy.

The static config accepts either a JSON object or a bare string. A bare string is treated as the token value with defaults filled in (subject="static", empty scopes, empty event types) — handy for the smallest possible smoke test, but unusable for worker auth because workers require non-empty scopes and event types.

## Per-token scopes

Every worker-side endpoint sits behind `RequireWorkerScope(scope)` from `internal/middleware/worker_scope.go`. The middleware reads the claims off the context, checks `claims.HasScope(scope)`, and returns `403 missing scope` on miss. The scope vocabulary is short and operation-shaped:

`codeq:claim` is required to call `POST /v1/codeq/tasks/claim`. `codeq:heartbeat` covers `POST /v1/codeq/tasks/:id/heartbeat`. `codeq:abandon` is for `POST /v1/codeq/tasks/:id/abandon`, `codeq:nack` for `POST /v1/codeq/tasks/:id/nack`, `codeq:result` for `POST /v1/codeq/tasks/:id/result`, and `codeq:subscribe` for the long-poll endpoints. A worker token that only carries `codeq:claim` can pull work but cannot submit results — useful for split-credential setups where the claim side and the result side are different services with different blast radii.

Scopes are a flat string list on the claims, not a hierarchy; there is no implicit "claim implies heartbeat", and a worker that intends to do a full claim/heartbeat/result cycle needs all three scopes explicitly. The flatness is deliberate. Hierarchical scope languages tend to grow ambiguity at scale, and codeQ's vocabulary is small enough that explicit enumeration is cheaper to reason about than rule resolution.

Producers, by contrast, do not gate per-endpoint by scope. Producer auth in `internal/middleware/auth.go` only verifies that the token validates and stamps the subject on the request. Authorization for producers happens at the tenant level — see below — not at the operation level. A producer that holds a valid token may publish any task into the tenant their token resolves to; if the deployment needs stricter producer policy, the recommendation is to issue per-event-type tokens at the identity provider and rely on the `eventTypes` claim plus the `AllowProducerAsWorker` bridge (see `worker_auth.go:31-49`).

## Tenant flow into request context

Tenant identity is one of the two values that makes the whole system multi-tenant; the other is the per-task `tenantId` field on the domain model. The middleware that connects the two is `internal/middleware/tenant.go::extractTenantID`. It looks at `claims.Raw` for a `tenantId` key (camelCase first, then snake_case, then `organizationId` / `organization_id` for SSO-issued tokens), trims whitespace, and falls back to `claims.Subject` if no explicit tenant field was set. The resolved value gets stamped onto the Gin context with `c.Set("tenantID", tenantID)`, and downstream controllers read it via `middleware.GetTenantID(c)`.

The fallback to subject matters. In single-tenant deployments the operator issues one token, leaves `tenantId` out of the claims entirely, and lets the subject serve as the tenant key. In multi-tenant deployments — typically backed by JWKS — every token carries an explicit `tenantId` claim and the subject is the worker or producer identity within that tenant. Both shapes flow through the same middleware without per-deployment branching.

On the worker side the same extraction runs, then a second decision happens at task claim time. The Pebble repository's claim path inspects the task's stored `tenantId` and refuses to lease a task whose tenant differs from the claiming worker's tenant context. The check lives in the repository layer, not in middleware, because the policy is "a worker only sees tasks in its own tenant" — that is enforceable in storage by scoping the iteration range, and codeQ does exactly that. Middleware sets the tenant; the repository enforces the scope; the controller never has to negotiate. The split is the conventional shape for namespace-based isolation: identity at the edge, isolation at the storage layer.

## Custom validator interface

The pluggable design exists so deployments that need provider-specific behavior — mutual-TLS with client cert claims, signed Kubernetes service-account tokens, HMAC-signed shared secrets with rotation — can add their own validator without forking codeQ. The recipe is small. Create a package, implement `auth.Validator` (one method), expose a `NewValidatorFromJSON(json.RawMessage) (auth.Validator, error)` factory, and call `auth.RegisterProvider("yourname", NewValidatorFromJSON)` from an `init()` function. Import the package once anywhere in the main binary so the `init()` runs.

The factory pattern keeps the application's config loader agnostic. The `producer.auth.provider` and `worker.auth.provider` keys in the YAML pick the registered name; the corresponding `producer.auth.config` and `worker.auth.config` blobs are the opaque JSON the factory will unmarshal. Because the registry is per-process, two different deployments of the same binary can pick different providers from config alone — no rebuild.

The pluggability also covers the test path. The Pebble repository tests in `pkg/auth/static/validator_test.go` exercise the registry round-trip end-to-end: register, configure, validate, assert claim shape. Custom validators get the same harness for free.

## Composition with the rest of the request pipeline

The full edge order for a worker request is auth → tenant → scope → rate limit → controller. `worker_auth.go` validates the bearer token and stamps the claims onto the context. `tenant.go` reads the claims and stamps the tenant. `worker_scope.go` checks the per-endpoint scope. The rate limiter (covered in [Persistence Engine](Concepts-Persistence-Engine) for the token-bucket implementation, and in [REST API](IO-REST-API) for the per-route configuration) reads the tenant to scope the bucket key. By the time the controller runs, identity, tenancy, and authorization are all decided and immutable.

Producers run a shorter chain: auth → tenant → rate limit → controller. There is no per-endpoint scope check; the producer either holds a valid token or it does not. The rate limiter still keys on tenant so a noisy producer cannot starve other tenants' submission paths.

The `AllowProducerAsWorker` flag in `pkg/config/config.go` is a deliberate bridge: when set, a request that arrives at a worker endpoint with only producer credentials is upgraded to a synthetic worker claim with the full default scope set and wildcard event types. This is the "dev mode" path — a single token serves both roles — and it is intentionally explicit so production deployments leave it false and force separate credentials.

## Boundaries

What auth does not do: there is no built-in user management, no token issuance, no refresh flow, no MFA. codeQ is a relying party. The expectation is that an upstream identity provider (Auth0, Okta, Keycloak, an internal OIDC service, or an operator running a JWT-signer script for static deployments) issues tokens and codeQ validates them. The static validator exists precisely to keep development friction near zero while still exercising the same code path that production JWKS validation runs through; the validation pipeline does not branch on provider identity once `Validate()` returns.

What auth also does not do: it does not enforce per-task ownership. A worker with `codeq:result` can submit a result for any task it has been leased — the lease, not the auth claim, is the authority. Lease ownership is checked at the repository layer (see [Leases And Ownership](Concepts-Leases-And-Ownership)), and the auth claim is the necessary-but-not-sufficient precondition. A worker that holds a valid scope but no lease on the target task gets a clean rejection from the repository.

The whole subsystem is a single package, two validator implementations, one registry, and four middleware functions. The smallness is the point.

## See also

- [Multi-Tenancy](Concepts-Multi-Tenancy) for how the tenant ID flows through the repository and reaper layers
- [Leases And Ownership](Concepts-Leases-And-Ownership) for the second authorization gate at task-claim time
- [REST API](IO-REST-API) for the per-endpoint scope assignments
- [Configuration](14-configuration) for the YAML keys that pick the provider and supply its config
