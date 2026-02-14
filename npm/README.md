# codeq (npm)

This package installs the `codeq` native CLI from GitHub Releases and exposes it on your PATH via npm.

## Install (npmjs)

```bash
npm i -g @osvaldoandrade/codeq
codeq --help
```

## Upgrade

```bash
npm i -g @osvaldoandrade/codeq@latest
```

or:

```bash
npm update -g @osvaldoandrade/codeq
```

## Overrides

- `CODEQ_GITHUB_REPO` (default `osvaldoandrade/codeq`): download binaries from another repo/fork.
- `CODEQ_RELEASE_TAG` (default `v<packageVersion>`): download from a specific tag.
- `CODEQ_GITHUB_TOKEN`: required when downloading binaries from a private repo release.
