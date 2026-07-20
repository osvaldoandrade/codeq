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
| Elevation of privilege | subject fallback overrides a bad alias | fallback only when every supported alias is absent | blank/non-string tests |

Tenant resolution occurs only after the configured validator accepts the token.
It does not weaken route scopes, worker event types, admin scope, idempotency, or
storage-level tenant prefixes. A global administrator remains bound to the
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
