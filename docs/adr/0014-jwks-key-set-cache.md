# ADR 0014: Cache the JWKS key set, bound its staleness and answer 503 when it is unusable (CFP-070)

- **Status**: Proposed
- **Date**: 2026-11-08
- **Deciders**: Osvaldo Andrade (CFP-070 coordinator request after the
  2026-11-08 access-security review of the Logs runtime)
- **Related**: ADR 0002 (tenant claims after validation), ADR 0003 (binding
  tokens; `exp` with zero leeway, 360 s revocation bound), ADR 0010
  (`Retry-After` on bounded refusals), platform ADR-0022 C1.4/C1.5
- **Ships in**: the CodeQ release after v0.5.13

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

`pkg/auth/jwks` keeps one immutable, whole **key set** per validator: the
usable keys of the last successful fetch and its fetch time.

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
- **Refresh.** At most one refresh runs at a time; concurrent callers that need
  it wait for that one (singleflight, bounded by the HTTP timeout, default 5 s,
  capped at 10 s). A new attempt of **any** cause — cold start, unknown `kid`,
  retry after failure, refresh-ahead — starts only when **30 s**
  (`minRefreshInterval`) have passed since the previous attempt started. A set
  older than **5 min** (`refreshInterval`, Tikti's `Cache-Control: max-age=300`)
  is refreshed in the background by the next request, which does not wait and
  is answered from the current set.
- **Negative cache.** Every `kid` absent from the current set is refused (401)
  without a fetch until the next attempt is permitted. A per-`kid` table would
  add nothing: random `kid`s defeat it, and the global interval already bounds
  fetches to one per 30 s per validator.
- **Bounded staleness.** While refreshes fail, the last good set keeps
  verifying for at most **15 min after its fetch** (`maxKeySetAge`).
- **Outcomes.** A token that was evaluated and refused wraps
  `auth.ErrInvalidToken` → **401** with the fixed body `invalid token` (gRPC
  `Unauthenticated`, `invalid token`): bad signature, unknown `kid` after a
  successful refresh, wrong issuer/audience, expired, missing `exp`,
  disallowed algorithm, malformed. A token that **could not be evaluated** —
  no set yet, the set is older than the bound, or an unknown `kid` while the
  latest refresh failed — returns `*auth.UnavailableError` → **503** with
  `Retry-After` (seconds until the next permitted attempt, 1–30; the HTTP
  timeout while one is in flight) and the fixed body `authentication
  temporarily unavailable` (gRPC `Unavailable`). Middleware that tries
  several validators (worker, producer promotion, any-auth, `multi`) answers
  503 when any of them was unavailable and none accepted. Only the fixed
  sentinels `missing Authorization header`, `invalid Authorization format`,
  `missing eventTypes` and `missing scope` keep their own 401 text.
- **Observability** (fixed label values, no token data, no URL, no error
  text): `codeq_jwks_refresh_total{outcome=success|no_usable_keys|
  fetch_timeout|fetch_error|http_status|too_large|invalid_document|
  duplicate_kid}`, `codeq_jwks_refresh_skipped_total{trigger=unknown_kid|
  refresh_ahead}`, `codeq_jwks_key_set_decisions_total{state=fresh|stale|
  unavailable}`, histogram `codeq_jwks_key_set_age_seconds`. Logs: one `WARN
  jwks refresh failed` per failed attempt (outcome, key-set age, bound,
  whether a usable set remains), `INFO jwks key set updated` only on first
  load or a `kid` change (counts only), `WARN jwks key set has no usable
  keys`.

New code: `pkg/auth/errors.go` (`ErrInvalidToken`, `ErrUnavailable`,
`UnavailableError`, `RetryAfter`, `PreferUnavailable`),
`pkg/auth/jwks/{validator,metrics}.go`, `internal/authstatus` (gRPC mapping),
and the HTTP mapping in `internal/middleware/auth.go`.

## Why 15 minutes, and why it is safe

- **Long enough.** With the 5-minute refresh-ahead and 30 s retries, a set is
  at most ~5.5 min old when an outage starts. A single Tikti stall (10–60 s)
  never reaches the bound; a 15-minute bound rides through roughly 9–10
  minutes of *continuous* Tikti unavailability (for example a Tikti Pod
  reschedule) with no client-visible effect for already-issued tokens.
- **Short enough.** It is three times the freshness Tikti itself advertises
  (`max-age=300`) and the same order as the token lifetimes involved (Logs
  binding tokens ≤ 300 s). Past it, CodeQ stops trusting keys it cannot
  re-confirm and answers 503 rather than accept.
- **Fail-closed is preserved.** The bound never admits a key that was not in a
  successfully fetched set. Staleness only lengthens how long a key **removed
  upstream** keeps working, and only while CodeQ cannot reach Tikti: as soon
  as one refresh succeeds the removal takes effect at once. Under normal
  operation removal takes effect within ~5.5 min, which is better than before
  (removed keys could stay cached indefinitely). Attackers cannot extend the
  window — unknown-`kid` traffic never replaces or ages a good set, and a
  failed or ambiguous refresh never shrinks it. Signature, issuer, audience,
  `exp` and tenant checks are unchanged or stricter; 503 is never an
  acceptance.
- **Residual.** To revoke a compromised Tikti key during a Tikti/CodeQ network
  partition faster than 15 min, restart the CodeQ voters (the cache is
  in-memory; a cold validator answers 503 until it fetches the new set).

## Consequences

- Tikti receives at most one JWKS request per 30 s per validator (two
  validators — producer and worker — per voter) under any traffic, plus the
  5-minute refresh-ahead; previously unbounded.
- Valid tokens keep working through Tikti stalls; validation never waits for
  Tikti while a usable set exists and the token's `kid` is known.
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
  validator is one small map. No new dependency, configuration key or Pod
  resource.

## Rollout and rollback

- Image-only change. Releasing it rolls the three CodeQ voters on Code and on
  Conveste (Conveste voters now request 125m / 384Mi per Conveste ADR-0071
  Amendment 90; this change adds no measurable CPU or memory). No schema,
  storage, Raft, CRD or chart change; mixed-version voters during the roll are
  compatible because validation is local to each voter.
- Verify after rollout: `codeq_jwks_refresh_total{outcome="success"}` rises
  about once per 5 min per validator; `codeq_jwks_key_set_decisions_total
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

- `pkg/auth/jwks/cache_test.go`, `internal/middleware/auth_failure_test.go`,
  `internal/authstatus/status_test.go`, `pkg/auth/errors_test.go`.
- `docs/security/tenant-claim-threat-model.md` (JWKS amplification, stale-key
  residual, error disclosure rows).
- Tikti `internal/workloadidentity/jwks_verifier.go` (same 1 MiB, 2048-bit and
  30 s unknown-`kid` limits for its own workload-identity verifier).
