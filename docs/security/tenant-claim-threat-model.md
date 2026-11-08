# Tenant claim threat model

Scope: validated token to tenant-scoped HTTP/gRPC queue access. Protected assets
are task payloads, results, subscriptions, QueueTopic policies, rate limits, and
physical tenant prefixes. Trust changes at token producer to validator,
validator to claim resolver, and resolved tenant to storage/provider keys.

## STRIDE and abuse cases

| Threat | Abuse case | Control | Evidence |
| --- | --- | --- | --- |
| Spoofing | attacker signs with another key, issuer, or audience | configured JWKS signature, issuer, audience, and expiry validation | JWKS validator tests |
| Tampering | token contains `tid=payments` and a legacy alias for another tenant | all supplied aliases must agree exactly after trimming | conflict/property/fuzz tests |
| Repudiation | transport resolves the same token differently | one resolver for HTTP, producer gRPC, and worker gRPC | compile and compatibility matrix |
| Information disclosure | malformed tenant escapes a key prefix or selects an empty/global scope | DNS-label validation; missing/malformed values fail closed before handlers | unsafe/missing claim tests |
| Denial of service | oversized or unexpected claim types trigger parser failure | bounded JWT parsing plus type checks; resolver never reflects raw values | fuzz tests |
| Denial of service | unauthenticated tokens with random `kid`s, or many requests at cache expiry, drive JWKS fetches to the identity provider | whole-set cache; one shared refresh at a time; at most one refresh attempt per 30 s per validator for any cause; unknown `kid`s negatively cached until the next permitted attempt; refresh-ahead in the background; JWKS body ≤ 1 MiB, token ≤ 64 KiB; JWKS redirects ≤ 3 and only within the configured origin (scheme, host, port); an unverified `iat` only turns an unknown-`kid` 401 into 503 and never starts a fetch (ADR 0014) | `TestConcurrentUnknownKidsFetchAtMostOncePerInterval`, `TestColdStartFailureIs503WithoutLeakingDetails`, `TestUnknownKidIssuedAfterFetchIsRetryable`, `TestJWKSRedirectsStayOnTheConfiguredOrigin` |
| Spoofing (residual) | a signing key removed upstream keeps verifying after its removal | only keys from a successfully fetched set are trusted; a well-formed refresh replaces the whole set at once, even when empty. Identity provider answering: the key verifies for < 6 min after the last fetch that listed it, busy or idle (a set ≥ 6 min old is used only after a refresh attempt failed). Identity provider unreachable: at most 15 min after that fetch, then 503 until a refresh succeeds. RSA ≥ 2048 bits, RS256/384/512 only, `exp` required with zero leeway | `TestIdleValidatorRefusesRemovedKeyPastGrace`, `TestRemovedKeyRefusedAtEveryAgePastGrace`, `TestPastGraceStaleSetOnlyAfterFailedRefresh`, `TestRotationAcceptsNewKidAndDropsRemovedKid`, `TestAuthoritativeEmptySetRevokesEveryKey`, `TestStallKeepsLastGoodSetWithinBoundThen503`; outage window is an accepted residual, ADR 0014 |
| Information disclosure | a 401 body or gRPC status echoes validator errors (JWKS URL, dial errors, issuer values) | fixed bodies: `invalid token` (401) or `authentication temporarily unavailable` (503 + `Retry-After`); gRPC `Unauthenticated` / `Unavailable` with fixed messages; refresh logs and metrics carry only fixed outcomes and ages | `TestAuthFailuresUseFixedBodiesAndDistinctStatuses`, `authstatus` tests |
| Elevation of privilege | subject fallback overrides a bad alias | fallback only when every supported alias is absent | blank/non-string tests |
| Information disclosure | any token reads another tenant's task or result by ID | task reads require `task.tenantId` equal to the resolved tenant; foreign and missing tasks answer identically | binding-scope e2e and controller tests |
| Elevation of privilege | a binding-scoped workload token reaches admin, another topic, webhooks, subscriptions, gRPC streams or worker promotion | ADR 0003 claim contract and route allow-list | binding-scope unit, middleware and e2e tests |
| Information disclosure | any token reuses another tenant's `idempotencyKey` and receives that tenant's task (payload, `tenantId`) | replay only when `task.tenantId` equals the caller's tenant; otherwise 409 `idempotency_conflict` with no task or ID; Publish binding keys namespaced `tid\x00eventType\x00key`; NUL refused in client keys; binding replay also requires the token's command | Pebble repository tests, controller tests, idempotency e2e on the production router |
| Information disclosure (residual) | a non-binding token learns from the 409 that a raw legacy key exists in another tenant | one bit only (never the task, ID or tenant); binding keys are namespaced and cannot hit another tenant; tenant-namespacing legacy keys needs a dual-read migration | accepted residual, ADR 0003 |
| Information disclosure | a follower forwards the bearer token to a host chosen by a request or a stale/forged leader hint (SSRF) | target must equal a configured `RAFT_PEER_HTTP_ADDRS` value other than self; malformed peers excluded; no environment proxy; no redirects followed | leaderforward unit tests (unconfigured, trailing slash, self, empty, malformed, redirect) |
| Spoofing / elevation of privilege | client sends `X-CodeQ-Forwarded` to skip checks on the leader | the header carries no authority; the leader re-runs authentication, the binding allow-list and controller rules; on a follower it only yields 503 | three-voter test: forged header with bad token is 401, cross-tenant heartbeat is `not-owner` |
| Denial of service | forwarding loop or amplification between voters | one hop only: a forwarded request is never forwarded again (503 `leader_unavailable`, `Retry-After: 1`); per-request timeout 10 s (claims `waitSeconds` + 10 s) | loop tests on single, batch and claim routes |
| Tampering (duplication) | a follower answers a forward that reached the leader and then timed out or broke with a retryable 503, and the client's retry duplicates an already-applied create | only failures before the transport accepted the whole request are 503 + `Retry-After`; after that, 504 `leader_forward_timeout` / 502 `leader_forward_interrupted` without `Retry-After`; the transport never re-sends a written non-idempotent request; retries use a tenant-bound `idempotencyKey` | leaderforward unit tests (sent-then-timeout, sent-then-reset, stalled mid-body stays 503, exactly one leader call), three-voter leader-loss test |
| Denial of service | large bodies buffered for replay | at most 16 MiB buffered; larger bodies that must be forwarded get 413; unauthenticated requests are refused before the gate | body-limit unit and cluster tests |
| Information disclosure | forwarding logs or metrics leak credentials | one bounded log line per decision without `Authorization`, body or token; metric labels are route templates and fixed results | log-redaction unit test |
| Information disclosure (residual) | the bearer token crosses one more in-cluster HTTP hop (follower to leader) | same trust zone and transport as the existing edge to `codeq:8080` hop; configured peers only | accepted residual, platform ADR-0022 C1.5 |

Tenant resolution occurs only after the configured validator accepts the token.
It does not weaken route scopes, worker event types, admin scope, or
storage-level tenant prefixes; idempotent replay is bound to the resolved
tenant (ADR 0003). A global administrator remains bound to the
resolved tenant unless a separately reviewed global route says otherwise.

## Token-producer verification

- JWKS producer tokens expose their raw `jwt.MapClaims` to the resolver after
  issuer, audience, signature, and expiry checks.
- Static tokens expose only operator-configured raw claims and use the same
  resolver; they are not a production identity substitute.
- Producer-as-worker copies the already validated producer raw claims and then
  re-runs the same resolver at the worker boundary.
- HTTP and both gRPC handshakes reject invalid tenant claims before scheduling,
  claiming, results, subscriptions, or topic administration can execute.

## Review record

The owner waived independent security and peer-review gates and accepted the
documented T4 risk. That waiver is not an independent review or sign-off. No
GCP, Kubernetes, deployment, workload, or production validation is part of this
evidence.
