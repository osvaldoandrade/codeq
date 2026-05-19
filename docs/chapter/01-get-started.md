---
title: Get started
---

codeQ is an embedded task queue server. It runs as a single Go binary, stores every task, queue index, and result in [Pebble](https://github.com/cockroachdb/pebble) (an LSM-tree storage engine derived from RocksDB), and exposes three wire protocols out of the same process: an HTTP REST API on `:8080`, a producer gRPC bidirectional stream on `:9092`, and a worker gRPC bidirectional stream on `:9091`. There is no broker to deploy beside it, no Redis to provision, no coordinator to keep alive. The default mode is one process, one Pebble directory, at-least-once delivery via in-memory leases rebuilt from the on-disk `KeyInprog` index at startup.

The rest of this chapter walks the shortest possible end-to-end path: install the binary, start the server, create a task, claim and complete it. By the end you have a single node sustaining the create-claim-complete cycle from your laptop, with the Pebble commit pipeline doing the durability work behind the scenes. Replication, cluster topology, and the streaming SDKs come later — there is an optional Raft-replicated mode covered in chapter 4 for deployments that need multi-node HA, but everything below uses the embedded single-node path that ships by default.

## Installation

The shell installer is the default path. The command below downloads a pre-built binary from the matching GitHub Release (`CODEQ_INSTALL_MODE=auto`) and falls back to `go build` from a fresh clone if the download is unavailable for your platform. It needs `curl`, and it needs `go` only when the binary fetch fails.

```bash
curl -fsSL https://raw.githubusercontent.com/osvaldoandrade/codeq/main/install.sh | sh
```

The npm package wraps the same binary. Installing it globally pulls the platform-matched release artifact through a postinstall hook and drops the `codeq` executable on your `$PATH`. The wrapper exists so that Node-first teams can pin the queue server version in `package.json` alongside their application; the runtime is identical to the shell-installed binary.

```bash
npm install -g @osvaldoandrade/codeq
```

The container image is the most direct path for server deployments. It bundles the `codeq` binary on a minimal base image and is published to GHCR for every release. Pair it with the bundled Raft cluster compose template for a three-node setup, or run it standalone with a bind-mounted data volume.

```bash
docker pull ghcr.io/osvaldoandrade/codeq-service:latest
```

If you have Go on your host and want to run codeQ standalone, the shell installer is the simplest. If you are deploying to a server, the Docker image is the most direct.

## Running the server

A single-node server starts with `codeq serve`. The process opens the HTTP listener on `:8080`, the producer gRPC stream on `:9092`, and the worker gRPC stream on `:9091`. The persistence backend is the embedded Pebble store under `./data/` relative to the working directory — created on first start, recovered on subsequent starts by replaying the WAL into the memtable. No external service is contacted; everything the server needs to accept produces and lease tasks lives in that directory.

The defaults are deliberately tuned for the laptop case: one shard, no fsync on commit (the group-commit coalescer in `internal/repository/pebble/db.go:71-82` still batches concurrent writers into a single Pebble batch), and the static auth provider configured with the bearer token `dev-token`. The configuration surface is defined in `pkg/config/config.go`; the same file documents every override and the YAML keys the loader recognises.

```bash
codeq serve
```

A successful start prints, in order, the Pebble open log line with the data directory, the HTTP listener binding `:8080`, and the gRPC stream listeners binding `:9091` and `:9092`. If the data directory does not exist, codeQ creates it; if it does, the LSM is recovered from the existing SSTables and WAL before the listeners come up. Shutdown is `SIGINT` or `SIGTERM` — the server drains in-flight HTTP and gRPC requests, flushes the Pebble memtable, and exits.

## Producing a task

A task in codeQ is the tuple `(command, payload, priority)` plus a small set of optional fields (idempotency key, max attempts, delay, webhook URL). The command is the routing key workers subscribe to. The payload is opaque bytes the server stores and hands back verbatim on claim. The priority is a non-negative integer; higher values are dispatched first within the same command. The server assigns the task ID, persists the task body and the `KeyReady` index entry in a single Pebble batch, and only acknowledges the produce after that batch reaches the commit pipeline. The implication is that a successful HTTP `200` or a successful `CreateAck` on the producer stream means the task is durable on the local Pebble store; a crash between accept and ack will not lose data.

Over HTTP, a produce is a `POST` to `/v1/codeq/tasks` with a JSON body. The bearer token authenticates the producer; the `command` field selects the routing key; the `payload` field is opaque application data; `priority` is optional and defaults to zero.

```bash
curl -X POST http://localhost:8080/v1/codeq/tasks \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"command":"PROCESS_ORDER","payload":{"orderId":"42"},"priority":5}'
```

The HTTP path is the simplest way to get a task into the system from any language. The Go SDK is the same path with type-safe arguments and one less round trip — it speaks the producer gRPC bidirectional stream on `:9092`, pipelining many `CreateTask` events through one connection and matching server acks by sequence number. The minimal program looks like this.

```go
import "github.com/osvaldoandrade/codeq/pkg/producerclient"

c, _ := producerclient.New(producerclient.Config{
    Addr:  "localhost:9092",
    Token: "dev-token",
})
defer c.Close()

s, _ := c.Connect(ctx)
defer s.Close()

id, _ := s.Produce(ctx, producerclient.CreateRequest{
    Command:  "PROCESS_ORDER",
    Payload:  []byte(`{"orderId":"42"}`),
    Priority: 5,
})
_ = id
```

The streaming path is what underpins the measured throughput figure on the reference box: 76,639 tasks/s sustained for the full create-claim-complete cycle, single-node Pebble, gRPC streams on both ends (`internal/bench/profile_full_cycle_test.go`). The same code runs against a Raft-replicated cluster without source changes — only the dial address differs.

## Consuming a task

A worker pulls tasks by holding open a bidirectional gRPC stream on `:9091`. The handshake announces the worker's identity (from the bearer token's JWT subject), the list of commands it can process, and the lease duration it wants on each task. After the handshake the server begins dispatching tasks the worker is allowed to claim; the worker calls its `Handler` for each one and replies with a `Result`. The lease is bounded — if the worker does not return a result or refresh the lease before it expires, the reaper returns the task to `KeyReady` with an incremented attempt counter, giving at-least-once delivery.

The Go SDK wraps that loop in `workerclient.Run`. You construct a `Client` with the dial address, the bearer token, the commands you want, and a concurrency count (the number of in-flight tasks the client maintains in parallel). `Run` blocks until the context is cancelled or the stream errors. Each handler invocation gets one task and returns one of four results.

```go
import "github.com/osvaldoandrade/codeq/pkg/workerclient"

w, _ := workerclient.New(workerclient.Config{
    Addr:        "localhost:9091",
    Token:       "dev-token",
    Commands:    []string{"PROCESS_ORDER"},
    Concurrency: 4,
})
defer w.Close()

_ = w.Run(ctx, func(ctx context.Context, t workerclient.Task) workerclient.Result {
    // your work
    return workerclient.Completed(map[string]any{"ok": true})
})
```

The four `Result` constructors map to distinct server-side state transitions. `Completed(body)` writes the result body to `KeyResult`, deletes the in-progress index entry, releases the lease, and triggers any registered webhook — the task is terminal. `Failed(err)` marks the task as permanently failed with the supplied error string; the task is moved to the dead-letter queue without retry. `Nack(delaySeconds, reason)` returns the task to the queue after the requested delay, increments the attempt counter, and is the right answer for transient failures the worker expects to recover from. `Abandon()` releases the lease without bumping the attempt counter; the task goes back to the ready queue immediately and the next worker picks it up — useful when the current worker has decided it should not be the one to process this task, but the work itself is fine.

The handler contract is concurrent-safe: `Concurrency: 4` means up to four handler goroutines run in parallel against the same Pebble store. The handler must therefore be safe to invoke from many goroutines at once. The `ctx` passed to the handler is cancelled when the lease is about to expire, when the stream is closing, or when the parent context is cancelled — handlers that do long I/O should respect it.

## Where to go next

Chapter 2 (Concepts and architecture) is the next step if you want to understand how codeQ stores and replicates. It walks the on-disk layout (the LSM keyspace, the `KeyReady` and `KeyInprog` indices, the result and DLQ ranges), the in-memory lease table, and the optional Raft FSM that turns a single-node Pebble store into a replicated state machine. The single-node behaviour you saw above is the foundation every other deployment mode builds on.

Chapter 4 (codeQ IO) is the chapter to read if you want to dig into the gRPC streaming API directly. It documents the producer and worker stream protocols at the protobuf level — the handshake, the `CreateTask`/`CreateTaskBatch`/`CreateAck` shape on the producer side, the `Hello`/`Ready`/`Task`/`Result` shape on the worker side, and the backpressure semantics that bound memory under load. The Go SDK shown above is one wrapper around those messages; any language with a gRPC stub generator can speak the same wire.

Chapter 3 (Sous functions) is the chapter for deploying serverless functions on top of codeQ — short-lived handlers that scale on task arrival rather than running as long-lived worker processes. The runtime sits on the same worker stream and the same lease semantics, so everything you learned about `Completed`, `Failed`, `Nack`, and `Abandon` still applies.

For deeper, recipe-style walkthroughs the reference manual covers the same ground with more operational detail. The detailed quickstart at [docs/00-getting-started.md](../00-getting-started.md) goes through every config knob the example above accepts as default. The three-node Raft cluster bootstrap is at [docs/43-tutorial-raft-cluster.md](../43-tutorial-raft-cluster.md). The full producer-plus-worker Go tutorial — including idempotency keys, batched produce, and graceful shutdown — is at [docs/44-tutorial-go-sdk.md](../44-tutorial-go-sdk.md).
