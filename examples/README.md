# codeq Examples

This directory hosts long-form examples that complement the per-feature
documentation under [`docs/`](../docs/).

## Contents

- [`custom-auth-plugin.md`](custom-auth-plugin.md) — walk-through for
  writing a custom authentication plugin against the
  [auth plugin system](../docs/20-authentication-plugins.md).

## Where the runnable examples went

Framework-specific integration examples for languages other than Go
were removed when codeq dropped non-Go SDKs. The supported client path
is now:

- The Go SDK in [`pkg/producerclient`](../pkg/producerclient) and
  [`pkg/workerclient`](../pkg/workerclient) for in-process Go callers.
- The HTTP API documented in
  [`docs/04-http-api.md`](../docs/04-http-api.md) for everything else
  (any language with an HTTP client).
- The gRPC streaming API in
  [`docs/34-streaming-api-guide.md`](../docs/34-streaming-api-guide.md)
  for high-throughput producers and workers.

For an end-to-end Go example, see
[`docs/integrations/go-integration.md`](../docs/integrations/go-integration.md).
