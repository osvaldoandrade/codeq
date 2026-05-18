# codeq SDKs

codeq is accessed over gRPC. The official Go SDK lives inside the main
module:

- [`pkg/producerclient`](../pkg/producerclient) — task creation
  (single + batched, streaming).
- [`pkg/workerclient`](../pkg/workerclient) — claim, heartbeat,
  result submission (streaming).

Install:

```bash
go get github.com/osvaldoandrade/codeq
```

See the root [README](../README.md#go-sdk) for a minimal example and
[`docs/34-streaming-api-guide.md`](../docs/34-streaming-api-guide.md)
for the full protocol. Non-Go callers should use the HTTP API
documented in [`docs/04-http-api.md`](../docs/04-http-api.md).
