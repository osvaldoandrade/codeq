# @osvaldoandrade/codeq (npm)

This package installs the `codeq` native binary from GitHub Releases and
puts it on your PATH via npm. The npm wrapper is the recommended entry
point for users who already have a Node.js toolchain — the heavy lifting
(server runtime, gRPC streams, Pebble persistence) all happens in the
single Go binary it downloads.

The version field in `package.json` is `0.0.0-development`; the actual
released version comes from the GitHub Releases tag. The postinstall
script downloads `codeq-<os>-<arch>` from
`https://github.com/osvaldoandrade/codeq/releases/download/v<version>/`
and writes it to `npm/native/codeq`.

## What you get

The `codeq` CLI is a single Go binary that:

- **Runs the server** (`codeq server` / `codeq run`) — HTTP + gRPC on
  embedded Pebble, no external broker required. See
  [_STYLE.md § Value proposition](../docs/_STYLE.md#1-value-proposition).
- **Scaffolds deployments** (`codeq install --target docker|kubernetes`)
  — generates a Docker Compose stack with the pre-built `codeq` image or
  a Helm-ready Kubernetes bundle.
- **Drives the API** (`codeq tasks ...`, `codeq queues ...`,
  `codeq cluster ...`) — administrative operations against a running
  server.

Full command surface: [docs/15-cli-reference.md](../docs/15-cli-reference.md).

## Install

```bash
npm install -g @osvaldoandrade/codeq
codeq --help
```

Node.js 18 or newer is required (`engines.node: ">=18"`).

## Quick start (Docker Compose stack)

```bash
codeq install --target docker --size dev --no-prompt
cd codeq-install
docker compose up -d
```

This generates a `codeq-install/` directory with a Compose stack that
points at a pre-built `osvaldoandrade/codeq` image. Editing the Go
source in this repo does **not** change that image — you need to
rebuild and republish, or point Compose at a locally-built tag. The
generated stack is the fastest path from `npm i -g` to a running
server.

For Kubernetes:

```bash
codeq install --target kubernetes --size medium --no-prompt
```

## Upgrade

```bash
npm install -g @osvaldoandrade/codeq@latest
```

or:

```bash
npm update -g @osvaldoandrade/codeq
```

## Environment overrides

- `CODEQ_GITHUB_REPO` (default `osvaldoandrade/codeq`) — download
  binaries from another repo or fork.
- `CODEQ_RELEASE_TAG` (default `v<packageVersion>`) — pin to a specific
  tag instead of the version in `package.json`.
- `CODEQ_GITHUB_TOKEN` — required for private-repo releases; also picks
  up `GITHUB_TOKEN` if set.

## Troubleshooting

If your platform is not in the supported matrix
(`linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`,
`windows/amd64`), postinstall exits non-zero and prints a source-install
fallback:

```bash
curl -fsSL https://raw.githubusercontent.com/osvaldoandrade/codeq/main/install.sh | sh
```

## See also

- [Root README](../README.md) — project overview and quickstart.
- [Getting started](../docs/00-getting-started.md) — first task, end to end.
- [CLI reference](../docs/15-cli-reference.md) — every subcommand and flag.
- [_STYLE.md § Value proposition](../docs/_STYLE.md#1-value-proposition) — what codeq is in one paragraph.
