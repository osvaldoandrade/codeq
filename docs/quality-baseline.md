# Quality regression baseline

Measured on 2026-07-20 at `3d91502` with Go 1.26.5. This file is an
inventory and ratchet starting point, not a claim that the existing debt is
acceptable. Issue #654 owns incremental coverage growth.

## Lint

The full golangci-lint v2.12.2 audit reports 1,264 findings:

| Category | Count | Disposition |
| --- | ---: | --- |
| goconst | 503 | refactor debt; fix by touched package |
| revive | 305 | exported API/comment debt; fix by touched package |
| gci/gofumpt/misspell | 114 | mechanical fix |
| gocognit/gocyclo/cyclop/nestif/funlen | 110 | complexity debt; refactor with behavior tests |
| errorlint/errcheck/nilerr | 70 | correctness debt; fix, never suppress broadly |
| staticcheck/unused/unparam/ineffassign/unconvert | 65 | correctness/dead-code debt; fix by package |
| dupl/gocritic/copyloopvar/prealloc/bodyclose/exhaustive/depguard | 37 | mixed correctness/design debt; inspect individually |
| golangci gosec analyzer | 60 | duplicate security surface; canonical disposition is the standalone Gosec inventory below |

The required lint job evaluates only issues introduced after the PR base SHA.
The complete audit remains warn-only and visible. This blocks new debt without
pretending that 1,264 findings disappeared.

The required race suite runs with `-short`; benchmark/profile/load scenarios
remain isolated in the benchmark workflow so they cannot exhaust ephemeral
ports or distort functional coverage. The Bloom snapshot now uses atomic-only
reads, closing the race exposed by the promoted gate.

## Coverage

The maintained full-suite profile is 48.8% (3,897 of 7,991 statements).
`pkg/producerclient` is 72.2% and `pkg/workerclient` is 77.9%. The required
floor starts at 48% total. #654 introduces package/file floors only as their
dedicated suites stabilize; this avoids claiming per-package enforcement that
the current aggregate profile cannot express reliably.

## Gosec

Gosec v2.28.0 at medium confidence and severity reports 71 findings:

| Rule | Count | Disposition |
| --- | ---: | --- |
| G115 integer conversion | 54 | bound is established by modulo, protobuf/domain limits, timestamps, or length checks; add narrowly scoped rationale or fix |
| G108 optional pprof | 1 | keep opt-in listener separate and loopback by default; scoped suppression |
| G404 non-cryptographic random | 3 | scheduling jitter only, never identity/token/key material; scoped suppression |
| G301 directory permission | 6 | fix to owner/group or owner-only permissions |
| G304 operator-selected file | 5 | validate containment where data is written; document same-user CLI/config reads |
| G204 subprocess arguments | 1 | fixed executable vectors produced by the installer; scoped suppression |
| G306 file permission | 1 | fix secret-bearing output to owner-only permissions |

That initial inventory is now reduced to zero unsuppressed medium/high
findings. The remaining accepted cases are source-local suppressions whose
rule IDs and rationales are validated by the required job.

The required security job rejects every unsuppressed medium/high-severity
finding, tracks suppressions, and rejects suppression comments without both a
rule ID and rationale. Govulncheck is required independently.

## Gate policy

- A suppression names exactly one rule and states the established bound or why
  the operation is non-security-sensitive.
- Generated protobuf files and test-only benchmark code do not define the
  production baseline.
- The base SHA, tool version, and full-audit counts are updated only in a PR
  that includes the generated reports and explains every count change.
- No green delta gate may be described as a clean repository-wide audit.
- The separate legacy coverage subset is required at its measured 62.1% floor;
  the canonical full-suite gate remains the 48% total policy.

This evidence authorizes no release, deployment, GCP, Kubernetes, workload, or
production access.
