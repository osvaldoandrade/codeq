# codeq

> Embedded task queue server. Pebble (LSM tree) for durable storage;
> optional Raft consensus for replicated HA; optional intra-process
> sharding for multi-core write parallelism. Accessed via HTTP API,
> Go gRPC streaming clients, or `codeq-cli`.

[![Go Reference](https://pkg.go.dev/badge/github.com/osvaldoandrade/codeq.svg)](https://pkg.go.dev/github.com/osvaldoandrade/codeq)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Issues](https://img.shields.io/github/issues/osvaldoandrade/codeq.svg)](https://github.com/osvaldoandrade/codeq/issues)

## What is codeq?

codeq is a single Go binary that exposes task-queue semantics
(create, claim, lease, heartbeat, complete, retry, DLQ) on top of
[Pebble](https://github.com/cockroachdb/pebble), the LSM-tree key/value
engine used by CockroachDB. Storage, lease table, scheduler, HTTP API
and gRPC streams all live in the same process and share one disk
directory. There is no external broker, no Redis, no ZooKeeper.

Three deployment shapes, all from the same binary:

| Mode          | Topology                          | Durability                       | Failure model                               |
|---------------|-----------------------------------|----------------------------------|---------------------------------------------|
| Single node   | 1 process, 1 Pebble DB            | Pebble WAL + group commit        | Process death loses unflushed batch         |
| Multi-shard   | 1 process, N Pebble DBs           | Per-shard WAL, FNV-1a routing    | Same as single node, N× write parallelism   |
| Raft cluster  | 3 (or 5) processes, replicated FSM| WAL + replicated log + snapshots | Tolerates `f = (N-1)/2` node failures        |

The three modes are mutually exclusive at config time; the check lives
at `pkg/config/config.go:662-683`.

## Architecture

```
                  HTTP :8080                    Pebble (LSM tree)
   ┌────────────┐ ─────────────────▶ ┌───────────────────┐ ────────▶ ./data/
   │ codeq-cli  │                    │   codeq  server   │
   ├────────────┤ gRPC :9092 stream  │  ┌─────────────┐  │
   │ Go SDK     │ ─────────────────▶ │  │ lease table │  │
   │ producer-  │                    │  │  (in-mem)   │  │
   │  client    │                    │  └─────────────┘  │
   ├────────────┤ gRPC :9091 stream  │                   │  Raft :7000 (mux)
   │ Go SDK     │ ─────────────────▶ │   ┌─── FSM ───┐   │ ─────────────┐
   │ worker-    │                    │   │ Apply →   │   │  AppendEntries
   │  client    │                    │   │ Pebble    │   │  (consensus)
   └────────────┘                    │   └───────────┘   │ ◀────────────┤
                                     └───────────────────┘              ▼
                                                                  ┌───────────┐
                                                                  │ peer node │
                                                                  └───────────┘
```

Ports and protocols at a glance:

- `:8080` — HTTP/JSON API (Gin), used by `codeq-cli`, curl, dashboards.
- `:9092` — Producer gRPC bidirectional stream (`internal/producer/server.go`).
- `:9091` — Worker gRPC bidirectional stream (`internal/worker/server.go`).
- `:7000` — Raft transport, multiplexed across shards by a 4-byte
  big-endian group ID prefix (`internal/raft/mux_transport.go:15-52`).

Every write goes through the in-memory lease table, then is committed
to Pebble. When Raft is enabled, the write is first proposed to the
replicated log; once a majority quorum acks, the FSM applies it to
Pebble (`internal/raft/fsm.go:43-62`, `SetRepr → Commit(NoSync)`).

### Sharding

When `sharding.numShards > 1`, the repository routes each task to
shard `FNV-1a-64(taskID) % N`
(`internal/repository/pebble/sharded_task_repository.go:61-65`). Each
shard owns its own Pebble DB, its own WAL, and its own group-commit
coalescer (`maxMergeBatch = 64`,
`internal/repository/pebble/db.go:71-82, 341-401`). This buys
write parallelism on multi-core hardware without crossing the network.

### Group commit

Both the Pebble layer and the Raft FSM batch concurrent writes before
fsync/apply:

- Pebble coalescer merges up to **64** in-flight batches per commit
  (`internal/repository/pebble/db.go:71-82`, `commitChanBuf = 1024`).
- Raft Apply coalescer merges up to **128** committed entries per FSM
  call (`internal/raft/db.go`, `raftMergeBatch = 128`).

Trade-off: higher batch sizes increase tail latency for the first
caller in a batch but raise steady-state throughput.

## Deployment modes and measured throughput

All numbers are from in-tree benchmarks. Re-run them with
`go test -run TestProfile_FullCycle ./internal/bench/...` etc. Loopback
network, Go 1.25, `fsyncOnCommit=false` unless noted.

| Mode                            | Throughput (full cycle: create + claim + complete) | Bench source                                                         |
|---------------------------------|----------------------------------------------------|----------------------------------------------------------------------|
| Single node, 4 Pebble shards    | **~83k tasks/s** (gRPC stream)                     | `internal/bench/profile_full_cycle_test.go`                          |
| 3-node Raft cluster, HTTP       | **~3.9k cycles/s** (1-shard and 4-shard, smart routing) | `pkg/app/raft_smart_routing_bench_test.go`                      |
| 3-node Raft, full-cycle baseline | reported alongside single-node REST in same harness | `pkg/app/raft_bench_test.go`                                        |

The order-of-magnitude gap between single-node and Raft is the cost
of consensus: every write costs one round-trip across `:7000` plus a
majority disk fsync on followers. Pick Raft when you need fault
tolerance (f = 1 with N = 3); pick single-node with shards when you
need higher throughput on one box.

## When to use codeq

- You want claim/lease/retry/DLQ semantics, not a raw log.
- You want one binary, one disk directory, no broker.
- You need either single-node throughput **or** a small replicated
  cluster — not both at once.
- You speak Go (or are happy talking HTTP from any language).

## When not to use codeq

| If you need…                              | Pick…                                      |
|-------------------------------------------|--------------------------------------------|
| Pub/sub at Kafka scale, retained log      | Kafka                                      |
| At-least-once delivery with cloud queueing| SQS                                        |
| A Python-native task framework            | Celery                                     |
| A Redis-backed Go task queue              | Asynq (note: needs Redis)                  |
| Cross-DC replication, geo-aware routing   | Build on Kafka or use a managed system     |

codeq matches Asynq's API surface but stores tasks in an embedded LSM
instead of Redis. Trade-off: the storage layer is local to the process
(no shared broker), so durability and HA come from Pebble + Raft, not
from a separately-managed data store.

## Quick start

```bash
git clone https://github.com/osvaldoandrade/codeq
cd codeq
docker compose \
  -f deploy/docker-compose/local-dev/compose.yaml \
  -f deploy/docker-compose/local-dev/compose.override.yaml \
  up -d
```

Server is now on `http://localhost:8080` with Pebble at `./data`.

Create a task over HTTP:

```bash
curl -X POST http://localhost:8080/v1/codeq/tasks \
  -H 'Authorization: Bearer <producer-token>' \
  -H 'Content-Type: application/json' \
  -d '{"command":"GENERATE_MASTER","payload":{"jobId":"j-123"},"priority":3}'
```

Claim a task:

```bash
curl -X POST http://localhost:8080/v1/codeq/tasks/claim \
  -H 'Authorization: Bearer <worker-token>' \
  -H 'Content-Type: application/json' \
  -d '{"commands":["GENERATE_MASTER"],"leaseSeconds":120,"waitSeconds":10}'
```

Submit a result:

```bash
curl -X POST http://localhost:8080/v1/codeq/tasks/<id>/result \
  -H 'Authorization: Bearer <worker-token>' \
  -H 'Content-Type: application/json' \
  -d '{"status":"COMPLETED","result":{"ok":true}}'
```

For high-throughput producers and workers, prefer the gRPC streaming
API (long-lived bidirectional stream, amortized auth, pipelined acks):
see [docs/34-streaming-api-guide.md](docs/34-streaming-api-guide.md).

## Go SDK

The Go SDK lives inside the main module — there is no separate
package to install:

- `pkg/producerclient` — create tasks (single + batched, streaming on `:9092`).
- `pkg/workerclient` — claim, heartbeat, complete (streaming on `:9091`).

```bash
go get github.com/osvaldoandrade/codeq
```

Minimal producer:

```go
import (
    "context"
    "github.com/osvaldoandrade/codeq/pkg/producerclient"
)

cli, _ := producerclient.New(producerclient.Config{
    Addr:  "localhost:9092",
    Token: producerToken,
})
defer cli.Close()

sess, _ := cli.Connect(context.Background())
defer sess.Close()

taskID, _ := sess.Produce(ctx, producerclient.CreateRequest{
    Command:  "GENERATE_MASTER",
    Payload:  []byte(`{"jobId":"j-123"}`),
    Priority: 3,
})
```

Callers outside Go talk to the HTTP API on `:8080`
([docs/04-http-api.md](docs/04-http-api.md)).

## Comparison

| Property                         | codeq                          | Asynq      | BullMQ     | Celery       | Kafka                    |
|----------------------------------|--------------------------------|------------|------------|--------------|--------------------------|
| Storage                          | Pebble (LSM), embedded         | Redis      | Redis      | Redis/Rabbit | Replicated log           |
| External dependency              | **None**                       | Redis      | Redis      | Broker + RB  | KRaft / ZooKeeper        |
| Task semantics (claim/lease/DLQ) | Yes                            | Yes        | Yes        | Yes          | No (log only)            |
| HA model                         | Raft consensus (f=1 with N=3)  | Redis repl | Redis repl | Broker-dep   | ISR + leader election    |
| Client surface                   | Go (HTTP + gRPC, Go SDK only)  | Go only    | Node only  | Python only  | Polyglot                 |
| Single-node throughput (full cycle) | ~83k tasks/s (4 shards, gRPC) | not measured here | not measured here | not measured here | n/a (no task semantics) |

Only the codeq number is from an in-tree benchmark
(`internal/bench/profile_full_cycle_test.go`). The other rows describe
storage and topology, not throughput — measure each candidate on your
own workload before deciding.

## Repo layout

```text
cmd/                  CLI entrypoints (codeq, codeq-cli, server)
internal/             unexported packages
  bench/              throughput + latency benchmarks (source of truth for perf claims)
  cluster/            consistent-hash ring + gRPC router (kept for reference; use raft for HA)
  controllers/        HTTP handlers (Gin)
  middleware/         auth, tracing, rate-limit, tenant extraction
  producer/           producer gRPC stream server (:9092)
  raft/               replicated log, FSM, mux transport (:7000)
  repository/         Pebble persistence + sharded repository
  services/           scheduler, results, callbacks, subscriptions
  worker/             worker gRPC stream server (:9091)
pkg/                  public packages
  app/                application bootstrap (single, sharded, raft)
  auth/               JWT + tenant scoping
  config/             config parsing, mode mutual-exclusion checks
  domain/             task model
  producerclient/     Go producer SDK
  workerclient/       Go worker SDK
deploy/               docker-compose and Kubernetes config
docs/                 specifications, runbooks, performance baselines
examples/             example applications and integration patterns
helm/codeq/           Helm chart and size profiles
npm/                  npm distribution wrapper for codeq-cli
```

Cluster mode (consistent-hash ring + gRPC routing) is preserved for
reference. For new HA deployments, use Raft replication —
see [docs/40-raft-replication.md](docs/40-raft-replication.md).

## Install the CLI

macOS, Linux, or Windows via Git Bash:

```bash
curl -fsSL https://raw.githubusercontent.com/osvaldoandrade/codeq/main/install.sh | sh
```

Or via npm (the npm package is a thin wrapper that downloads the Go binary):

```bash
npm i -g @osvaldoandrade/codeq
codeq --help
```

Generate a Docker or Kubernetes install bundle (Pebble-backed, no
external broker):

```bash
codeq install
```

See [docs/15-cli-reference.md](docs/15-cli-reference.md) for the full
CLI surface.

## Where next

- [Getting started](docs/00-getting-started.md) — first task end to end.
- [Overview](docs/01-overview.md) — goals, non-goals, fit.
- [Architecture](docs/03-architecture.md) — package layout, request flows.
- [HTTP API reference](docs/04-http-api.md).
- [Streaming API guide](docs/34-streaming-api-guide.md) — gRPC producer and worker streams.
- [Performance baselines](docs/30-performance-baselines.md) — raw bench output and per-release history.
- [Performance tuning](docs/17-performance-tuning.md) — shard counts, batch sizes, fsync trade-offs.
- [Raft replication](docs/40-raft-replication.md) — leader lease, snapshots, quorum behavior.
- [Operational runbooks](docs/29-operational-runbooks.md) — on-call procedures.
- [Style guide](docs/_STYLE.md) — voice, numbers, diagrams, links.

## Contributing

Issues and PRs are welcome. Before opening a PR, read
[CONTRIBUTING.md](CONTRIBUTING.md) and [docs/_STYLE.md](docs/_STYLE.md).

## License

MIT. See [LICENSE](LICENSE).
