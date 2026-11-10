# ADR 0014: Cache the JWKS key set, bound its staleness and answer 503 when it is unusable (CFP-070)

- **Status**: Proposed
- **Date**: 2026-11-08
- **Deciders**: Osvaldo Andrade (CFP-070 coordinator request after the
  2026-11-08 access-security review of the Logs runtime)
- **Related**: ADR 0002 (tenant claims after validation), ADR 0003 (binding
  tokens; `exp` with zero leeway, 360 s revocation bound), ADR 0010
  (`Retry-After` on bounded refusals), platform ADR-0022 C1.4/C1.5
- **Ships in**: the CodeQ release after v0.5.13
- **Revised**: 2026-11-08 after the independent security review (conditional
  GO): an idle validator no longer trusts a set older than 6 min without
  trying a refresh (F1); an unknown `kid` on a token issued after the fetch is
  503 instead of 401 (F2); redirects stay on the configured origin (F3);
  waits honour the request context (F4).
- **Revised**: 2026-11-10 after the second architecture, security and
  release reviews: an unknown `kid` earns 503 only for a token naming this
  validator's issuer and audience (security Low-1); the producer and worker
  validators of a process share one key-set cache per URL (P3-2); a gRPC
  `Unavailable` carries the retry delay as `google.rpc.RetryInfo` (P3-7);
  the rollout section states the current Conveste voter sizing (P3-8).
  Then, after the final security review: a token naming another issuer or
  audience is 401 before any key lookup, so no "could not check" path answers
  it 503 (L-1), and it starts, joins and waits for no refresh (I-1).

## Context

CodeQ is reachable from the internet at `/platform/codeq/v1/codeq` (for
Conveste, `https://conveste.codefoundry.cc`). Through v0.5.13
`pkg/auth/jwks/validator.go`:

1. **Unauthenticated amplification.** A token whose `kid` was not cached made
   the request fetch the JWKS from Tikti, every time, with no shared fetch and
   no rate limit. Anyone could drive unbounded fetches to Tikti, whose node
   already freezes for 10–60 s about every 10 minutes. When the 5-minute cache
   expired, every concurrent request refetched at once.
2. **Availability.** When that refetch failed (Tikti stall, 5 s timeout) the
   request got 401 even for a valid token signed by a key fetched minutes
   earlier. On 2026-11-08 at 04:08Z this made CodeQ reject the Logs API's
   publishes with 401 during a ~15 s Tikti stall; the publisher could not tell
   "your token is bad" from "I could not check".
3. **Hygiene.** The JWKS body was read without a size limit, RSA key sizes were
   not checked, `exp` was optional, and the 401 body echoed `err.Error()`
   (JWKS URL, dial errors, issuer values). Keys were cached per `kid` with one
   shared timestamp, so a key removed upstream stayed cached as long as some
   other fetch kept refreshing the timestamp.

## Decision

`pkg/auth/jwks` keeps one immutable, whole **key set** per key-set cache: the
usable keys of the last successful fetch and its fetch time.

- **One cache per URL per process.** A validator built with
  `jwks.NewValidator` has its own cache. The process builds its producer and
  worker validators (and the children of a `multi` validator) in one
  `auth.Scope` (`auth.NewScope`, `auth.NewScopedValidator`), and validators of
  that scope with the same JWKS URL and HTTP timeout share one cache: one key
  set, one refresh in flight, one 30 s attempt interval. Each validator keeps
  its own issuer and audience. The any-auth middleware and producer promotion
  try the worker and then the producer validator; with a shared cache such a
  request waits for at most one refresh (5 s by default), not one per
  validator, and Tikti sees half the requests. A token meant for the other
  validator's audience is refused by the first one before any key lookup, so
  it does not wait at all there. The key set is the same
  document whichever validator fetched it, so sharing it changes no trust
  decision.

- **Trust.** A token verifies only against a key present in a set that was
  fetched successfully. A well-formed `200` JWKS **replaces the whole set at
  once**, so keys it no longer lists stop verifying immediately, even when it
  lists no usable key (`no_usable_keys`, which then refuses every token). A
  transport error, timeout, non-200, body over 1 MiB, unreadable or trailing
  JSON, an absent or `null` `keys` member (RFC 7517 requires it; `{}` is not
  an empty set), or a duplicate `kid` (ambiguous) is a **failed refresh** and
  changes nothing.
- **Usable keys.** `kty=RSA`, non-empty `kid`, `use` absent or `sig`, `alg`
  absent or one of RS256/RS384/RS512 (and equal to the token's `alg` when
  present), modulus 2048–8192 bits, odd exponent ≥ 3 that fits 32 bits.
  Other entries are skipped.
- **Tokens.** At most 64 KiB; `alg` RS256/RS384/RS512 only (the family the
  validator always accepted; `none`, HMAC, ECDSA, EdDSA and PSS are refused
  before any key lookup or fetch); `kid` required; issuer and audience exact;
  `exp` **required**; `exp`/`nbf` with **zero leeway** as before
  (`clockSkew` is still accepted in the config and still does not extend
  them, preserving ADR 0003's bound).
- **Another issuer or audience first.** Before any key lookup the validator
  reads the token's **unverified** `iss` and `aud`. Unless `iss` equals its
  issuer and `aud` contains its audience, the token is refused (401,
  `invalid issuer` / `invalid audience` internally): a token for another
  issuer or audience is 401 at once, because it could never be accepted
  whatever key signed it. It therefore never gets a 503 on any path below (no
  set yet, a set past the bound, a failed refresh, a known `kid` past the
  grace window with nothing to fall back on, an unknown `kid`), and it never
  starts, joins or waits for a refresh, so foreign tokens cannot use the
  refresh slot. This check only ever refuses: acceptance still requires the
  verified signature and the parser's exact issuer and audience checks. It
  records no key-set decision (no lookup happened).
- **Refresh.** At most one refresh runs at a time; concurrent callers that need
  it wait for that one (singleflight). A wait ends at the HTTP timeout
  (default 5 s, capped at 10 s) or when the caller's request context ends,
  whichever is first; an abandoned wait answers 503 and never accepts, and the
  shared refresh continues for the other waiters. A new attempt of **any**
  cause — cold start, unknown `kid`, retry after failure, refresh-ahead —
  starts only when **30 s** (`minRefreshInterval`) have passed since the
  previous attempt started. Redirects are followed at most 3 times and only
  within the configured origin (scheme, host and port); any other redirect
  fails the refresh (`fetch_error`).
- **Freshness.** By key-set age (time since its successful fetch), for a
  token whose `kid` is in the set:
  - **< 5 min** (`refreshInterval`, Tikti's `Cache-Control: max-age=300`):
    verified from the set.
  - **5–6 min** (`refreshInterval` + `refreshGrace` of 60 s): verified from the
    set; the request starts a background refresh and does not wait.
  - **≥ 6 min:** the request waits for a refresh (it starts one, or joins the
    one in flight) and is decided on its result: after a successful refresh
    only the new set counts, so a `kid` it no longer lists is refused (401). The
    old set is used only when that refresh failed, or when an attempt made
    within the last 30 s failed (then the request does not wait), and only up
    to the staleness bound below. This is what keeps an idle validator — e.g.
    the worker validator, used only on gRPC `Hello` — from trusting a set Tikti
    could have replaced.
- **Negative cache.** A `kid` absent from the current set gets no fetch until
  the next attempt is permitted. A per-`kid` table would add nothing: random
  `kid`s defeat it, and the global interval already bounds fetches to one per
  30 s per cache. Its answer is 401, except for a token (which, as above,
  names this validator's issuer and audience) whose **unverified** `iat` is
  no earlier than the set's fetch
  time minus 5 s and no later than now plus 5 s: such a token may be signed
  by a key Tikti published after the fetch (Tikti publishes only its current
  key and does not pre-publish the next, `repos/tikti/internal/utils/jwks.go`),
  so it gets **503 + `Retry-After`** (until the next permitted attempt,
  ≤ 30 s) and the retry is decided by that refresh. A token for another
  issuer or audience is 401 at once (above) and cannot be used to force
  503s. The unverified `iat` only chooses between 401 and 503: it is read
  after the refresh-or-skip decision, so it never starts a fetch and never
  contributes to an acceptance. To earn a 503 instead of a 401 a forged token must name this
  validator's issuer and audience and carry a current `iat`, and once a later
  refresh succeeds the same token is 401.
- **Bounded staleness.** While refreshes fail, the last good set keeps
  verifying for at most **15 min after its fetch** (`maxKeySetAge`). Exact
  bounds for a key Tikti removed:
  - **Tikti answering** (every refresh attempt succeeds): the key verifies
    only with a set younger than 6 min, so for **less than 6 min after the
    last fetch that still listed it** — whether the validator is busy or idle.
    (v0.5.13: about 5 min, but only by refetching on the request path.)
  - **Tikti unreachable or failing**: until the first successful refresh, at
    most **15 min after that fetch**; from 15 min on every token is 503 until
    a refresh succeeds. As soon as one refresh succeeds the removal applies.
  - **Latency cost**: past 6 min, a request that starts or joins a refresh
    waits for it, at most the HTTP timeout (5 s default) or its own deadline;
    during an outage this happens to the requests that coincide with the one
    permitted attempt per 30 s. Below 6 min, or with a known-failed attempt in
    the last 30 s, a known `kid` never waits.
- **Outcomes.** A token that was evaluated and refused wraps
  `auth.ErrInvalidToken` → **401** with the fixed body `invalid token` (gRPC
  `Unauthenticated`, `invalid token`): another issuer or audience (before
  any key lookup), bad signature, unknown `kid` after a successful refresh
  (except the `iat` case above),
  expired, missing `exp`, disallowed algorithm, malformed. A token that
  **could not be evaluated** and names this validator's issuer and
  audience — no set yet, the set is older than the bound, an
  unknown `kid` while the latest refresh failed, an unknown `kid` possibly
  newer than the set, or a wait abandoned because the request context ended —
  returns `*auth.UnavailableError` → **503** with
  `Retry-After` (seconds until the next permitted attempt, 1–30; the HTTP
  timeout while one is in flight) and the fixed body `authentication
  temporarily unavailable`. On the gRPC streams it is `Unavailable` with the
  same fixed message and a `google.rpc.RetryInfo` status detail whose
  `retry_delay` is the `Retry-After` value in whole seconds (the standard
  gRPC retry hint, sent in the `grpc-status-details-bin` trailer; clients that
  ignore details are unaffected). Middleware that tries
  several validators (worker, producer promotion, any-auth, `multi`) answers
  503 when any of them was unavailable and none accepted. Only the fixed
  sentinels `missing Authorization header`, `invalid Authorization format`,
  `missing eventTypes` and `missing scope` keep their own 401 text.
- **Observability** (fixed label values, no token data, no URL, no error
  text): `codeq_jwks_refresh_total{outcome=success|no_usable_keys|
  fetch_timeout|fetch_error|http_status|too_large|invalid_document|
  duplicate_kid}`, `codeq_jwks_refresh_skipped_total{trigger=unknown_kid|
  refresh_ahead}`, `codeq_jwks_key_set_decisions_total{state=fresh|stale|
  unavailable|kid_not_yet_known|canceled}` (`kid_not_yet_known` is the
  issuer/audience/`iat` 503 above and is attacker-reachable for anyone who
  knows the issuer and audience, so it is kept out of `unavailable`;
  `canceled` is an abandoned wait), histogram
  `codeq_jwks_key_set_age_seconds`. Logs: one `WARN
  jwks refresh failed` per failed attempt (outcome, key-set age, bound,
  whether a usable set remains), `INFO jwks key set updated` only on first
  load or a `kid` change (counts only), `WARN jwks key set has no usable
  keys`.

New code: `pkg/auth/errors.go` (`ErrInvalidToken`, `ErrUnavailable`,
`UnavailableError`, `RetryAfter`, `PreferUnavailable`), `pkg/auth/interface.go`
(additive `ContextValidator` and `ValidateContext(ctx, v, token)`; validators
without it are called as before), `pkg/auth/scope.go` (additive `Scope`,
`NewScope`, `RegisterScopedProvider`, `NewScopedValidator`; providers without
a scoped factory are built as by `NewValidator`),
`pkg/auth/jwks/{validator,observer}.go`, `internal/authstatus` (gRPC mapping
and `RetryInfo`), and the HTTP mapping in
`internal/middleware/auth.go`. HTTP middleware passes the request context and
the producer and worker gRPC streams pass the stream context; `multi` passes
it to its children.

## Why 15 minutes, and why it is safe

- **Long enough.** With refreshes starting from 5 min and 30 s retries, a busy
  validator's set is at most ~5.5 min old when an outage starts (an idle one
  is refreshed by its first request past 6 min). A single Tikti stall (10–60
  s) never reaches the bound; a 15-minute bound rides through roughly 9–10
  minutes of *continuous* Tikti unavailability (for example a Tikti Pod
  reschedule) with no client-visible refusal for already-issued tokens (only
  the bounded waits above).
- **Short enough.** It is three times the freshness Tikti itself advertises
  (`max-age=300`) and the same order as the token lifetimes involved (Logs
  binding tokens ≤ 300 s). Past it, CodeQ stops trusting keys it cannot
  re-confirm and answers 503 rather than accept.
- **Fail-closed is preserved.** The bound never admits a key that was not in a
  successfully fetched set. Staleness only lengthens how long a key **removed
  upstream** keeps working beyond the normal < 6 min, and only while CodeQ
  cannot reach Tikti: as soon as one refresh succeeds the removal takes effect
  at once. Under normal operation removal takes effect less than 6 min after
  the last fetch that listed the key, busy or idle (v0.5.13: ~5 min, but keys
  shared one timestamp, so a removed key could stay cached as long as other
  fetches kept it fresh). Attackers cannot extend the window — unknown-`kid`
  traffic never replaces or ages a good set, a failed or ambiguous refresh
  never shrinks it, and forged `iss`/`aud`/`iat` claims change only 401 into
  503. Signature,
  issuer, audience, `exp` and tenant checks are unchanged or stricter; 503 is
  never an acceptance.
- **Residual.** To revoke a compromised Tikti key during a Tikti/CodeQ network
  partition faster than 15 min, restart the CodeQ voters (the cache is
  in-memory; a cold validator answers 503 until it fetches the new set).

## Consequences

- Tikti receives at most one JWKS request per 30 s per key-set URL per voter
  under any traffic (the producer and worker validators share one cache;
  configurations naming different URLs or HTTP timeouts get one cache each),
  plus the 5-minute refresh-ahead; previously unbounded.
- Valid tokens keep working through Tikti stalls. Validation of a known `kid`
  never waits while the set is younger than 6 min; past that it waits at most
  the HTTP timeout (or its own deadline) for one refresh, and during an outage
  only requests that coincide with the one attempt per 30 s wait.
- **Key rotation.** Tikti publishes only its current key, so tokens signed by
  a new key can reach a validator that fetched before the rotation. If no
  attempt was made in the last 30 s, the request refreshes and is accepted. If
  one was, it gets 503 + `Retry-After` ≤ 30 s (its `iat` postdates the fetch)
  and the retry is accepted; only a Tikti clock more than 5 s behind CodeQ can
  still turn this into a 401 for up to 30 s. Clients must retry 503 (the Logs
  runtime collectors do). No rotation procedure is needed beyond that.
- Clients see a new **503 + `Retry-After`** on authentication. CodeQ already
  answers 503 + `Retry-After` for leader forwarding (`leader_unavailable`).
  The Logs runtime (`code-admin-logs-runtime` `internal/httpapi/server.go`
  at `d79048f`) maps a CodeQ 401 to `PERMISSION_DENIED` 401, which its
  collectors do not retry, and every other dependency failure, 503 included,
  to `LOGS_UNAVAILABLE` 503, which they retry; so the 04:08Z class of failure
  becomes retryable instead of a rejection, and is normally absorbed by the
  cached set. Other clients must treat 503 as retryable.
- Tokens without `exp` are now refused. Every RS256 token signer in Tikti
  (`internal/services`: user, workload identity, binding, topic, managed
  Logs, AI and Trino tokens) sets `exp`; static (non-JWKS) providers are
  unaffected.
- Validators start one short-lived goroutine per refresh attempt; memory per
  key-set cache is one small map. No new configuration key or Pod resource;
  `google.golang.org/genproto/googleapis/rpc`, already in the module graph
  through gRPC, becomes a direct dependency for `RetryInfo`.

## Rollout and rollback

- Image-only change. Releasing it rolls the three CodeQ voters on Code and on
  Conveste. Conveste voters run at the owner-approved sizing of 2026-11-06
  (Conveste ADR-0071): requests 500m CPU / 512Mi, limits 1000m CPU / 512Mi;
  this change adds no measurable CPU or memory and changes no request or
  limit. No schema,
  storage, Raft, CRD or chart change; mixed-version voters during the roll are
  compatible because validation is local to each voter.
- Verify after rollout: `codeq_jwks_refresh_total{outcome="success"}` rises
  about once per 5 min per key-set cache; `codeq_jwks_key_set_decisions_total
  {state="unavailable"}` stays 0 outside Tikti outages; Logs publishes show no
  401 during Tikti stalls.
- Rollback: redeploy the previous CodeQ image (v0.5.13). No state to migrate
  or clean up; the previous behaviour (per-request fetches, 401 on fetch
  failure, optional `exp`) returns with it.

## Alternatives considered

- **Per-`kid` negative cache only.** Random `kid`s bypass it; the global
  minimum interval is the actual bound.
- **Background refresher goroutine per validator.** `auth.Validator` has no
  lifecycle (`Close`), so it would leak in tests and embedders; request-driven
  refresh-ahead gives the same latency property without it.
- **Package-global cache registry keyed by URL.** It would share caches
  implicitly, but validators have no `Close`, so it would keep caches (and
  their state) alive across embedders and tests in one binary; the process
  builds its validators in one explicit `auth.Scope` instead.
- **Unbounded stale use.** Would keep trusting removed keys indefinitely during
  a partition; rejected.
- **Keep the old set when a well-formed JWKS has no usable keys.** More
  available if Tikti publishes a broken set, but it trusts keys the issuer no
  longer publishes; rejected (fail closed).
- **401 on unavailability (status quo).** Indistinguishable from a bad token
  and caused the 04:08Z Logs rejections; rejected.
- **`golang.org/x/sync/singleflight`.** A new module dependency for a
  ten-line, single-key in-flight call.

## References

- `pkg/auth/jwks/cache_test.go`, `pkg/auth/jwks/freshness_test.go` (review
  regressions: `TestIdleValidatorRefusesRemovedKeyPastGrace`,
  `TestPastGraceStaleSetOnlyAfterFailedRefresh`,
  `TestUnknownKidIssuedAfterFetchIsRetryable`,
  `TestUnknownKidForeignIssuerOrAudienceIsRefused`,
  `TestWaiterHonorsRequestContext`,
  `TestJWKSRedirectsStayOnTheConfiguredOrigin`), `pkg/auth/jwks/foreign_test.go`
  (`TestForeignTokenIsRefusedOnEveryUnavailablePath`,
  `TestForeignTokenStartsNoRefresh`), `pkg/auth/jwks/shared_test.go`
  (`TestScopedValidatorsShareOneKeySetCachePerURL`,
  `TestSharedCacheWaitsForOneRefreshAcrossValidators`),
  `pkg/auth/multi/validator_test.go` (`TestScopedMultiSharesChildKeySetCache`),
  `internal/middleware/auth_failure_test.go`,
  `internal/authstatus/status_test.go` (`TestStreamUnavailableCarriesRetryInfo`),
  `pkg/auth/errors_test.go`, `pkg/auth/interface_test.go`,
  `pkg/auth/scope_test.go`.
- `docs/security/tenant-claim-threat-model.md` (JWKS amplification, stale-key
  residual, error disclosure rows).
- Tikti `internal/workloadidentity/jwks_verifier.go` (same 1 MiB, 2048-bit and
  30 s unknown-`kid` limits for its own workload-identity verifier).
