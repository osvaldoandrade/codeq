# ADR 0002: Resolve one canonical tenant claim

## Status

Accepted by the owner under the documented T4 review waiver on 2026-07-20.
Independent security and peer-review approvals were not performed and are not
implied by this status.

## Context

Authenticated HTTP, producer gRPC, and worker gRPC paths previously interpreted
tenant claims in three different functions. HTTP preferred `tid`, the streams
did not recognize it, and every path silently selected one alias when a token
contained conflicting values. A correctly signed token could therefore map to
different physical queue prefixes depending on transport.

The JWKS validator already requires the configured issuer and audience and
validates signature and expiry before tenant resolution. The static validator
is a local/operator compatibility producer. Producer-as-worker preserves the
validated producer raw claims. Tenant resolution must apply one rule after all
of those producers validate a token.

## Decision

`tid` is the canonical tenant claim. During the compatibility window, codeQ
also accepts `tenantId`, `tenant_id`, `organizationId`, and `organization_id`.
Every supplied claim must be a string, trim to the same lowercase DNS-label
value, and match `^[a-z][a-z0-9-]{1,62}$`. A blank, non-string, unsafe, or
conflicting claim fails closed before queue access.

Subject fallback remains supported only for single-tenant tokens that contain
none of the supported tenant claim names. The subject must satisfy the same
tenant format. The fallback never overrides a present but invalid tenant claim.

The rule lives in `internal/authclaims` and is consumed by HTTP producer/worker
middleware and both gRPC stream handshakes. Error responses do not echo claim
values.

## Compatibility matrix

| Producer shape | Result |
| --- | --- |
| Tikti/workload token with `tid` | accepted; canonical |
| Firebase-era token with `tenantId` | accepted during compatibility window |
| legacy token with `tenant_id` | accepted during compatibility window |
| organization token with `organizationId` or `organization_id` | accepted during compatibility window |
| static local token with one supported raw claim | accepted; same resolver |
| producer token used as worker | accepted only after producer validation; same resolver |
| multiple aliases with one value | accepted |
| aliases with different values | rejected |
| no aliases and DNS-label `sub` | accepted as single-tenant fallback |
| present blank/non-string/unsafe alias | rejected; no fallback |

## Deprecation

New token producers must emit `tid` immediately. Legacy aliases remain readable
through 2027-01-31. Beginning 2026-10-31, operators should measure alias use at
the identity provider rather than logging raw claims in codeQ. Removal requires
a compatibility report showing 30 days with no legacy issuance, a major-release
notice, and a separate ADR. Subject fallback follows the same removal gate.

## Verification and residual risk

Compatibility-table tests, alias-order properties, fuzzing, and mutation
testing exercise the resolver. Middleware and both stream servers compile
against the same function; JWT validator tests separately cover issuer,
audience, signature, expiry, and claim decoding. The threat model records the
cross-tenant abuse cases.

Independent security validation was waived by the owner. The remaining risk is
an undocumented external producer using a non-DNS tenant or relying on a
conflicting token. The change intentionally rejects both rather than selecting
an ambiguous tenant. It authorizes no release or production action.
