# Architecture Decision Records

This directory holds Architecture Decision Records (ADRs) for codeq.

An ADR captures a single architecturally significant decision: the
context, the choice that was made, the trade-offs accepted, and the
alternatives that were rejected. ADRs are immutable once accepted; a
later decision that supersedes them is recorded as a new ADR with the
older one marked `Superseded`.

## When to write an ADR

Write one when the change:

- alters the dependency direction between layers (domain / application /
  storage / transport);
- changes the public surface of `pkg/` (adds, renames, deletes);
- introduces or replaces a runtime dependency (database engine, queue,
  consensus library, auth provider);
- redefines a wire-level contract (HTTP route shape, gRPC service, event
  schema);
- changes a security boundary (authn, authz, tenant isolation);
- accepts a trade-off that future maintainers would otherwise reverse
  (e.g., choosing eventual over strong consistency, or in-memory state
  over persistence).

A one-line fix, a bug patch, or a pure refactor that preserves behavior
does not need an ADR.

## File naming

`NNNN-kebab-case-title.md` where `NNNN` is the next zero-padded sequence
number. Sequence is per-repo, not per-area.

## Template

Copy `template.md` to start a new ADR.

## Status lifecycle

```
Proposed → Accepted → (Superseded by NNNN | Deprecated)
```

- **Proposed**: open for discussion in a PR.
- **Accepted**: merged to `main`; the decision is in effect.
- **Superseded by NNNN**: a later ADR replaces this one. Keep the file;
  add the supersession line at the top.
- **Deprecated**: the decision no longer applies but no replacement was
  written. Rare.

## Index

| # | Title | Status |
|---|---|---|
| [0001](0001-target-architecture.md) | Target architecture (layered + hexagonal) | Accepted |
