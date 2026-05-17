# codeq documentation style guide

This is the canonical style guide for codeq documentation. Every doc in
`docs/` and the root `README.md` must follow it. Subsequent waves of doc
revisions cite this file verbatim where relevant (especially the value
proposition and comparativos). If you change the value proposition here,
you must update the docs that quote it.

## 1. Value proposition

### One-line

> codeq is an embedded high-performance task queue: a single Go binary,
> Pebble for persistence, gRPC streaming for the wire — 83k tasks/s on
> one machine with zero external dependencies.

### One-paragraph

> codeq is a task queue written in Go that runs as a single process and
> stores everything in an embedded LSM (Pebble, the RocksDB-style engine
> from CockroachDB). The hot path is bidirectional gRPC streams from
> producers and workers; under the streams sits an intra-process shard
> table (up to N independent Pebble instances) and an in-memory lease
> table. The result on a 12-core Linux box is 83,420 tasks/s for the
> full create → claim → complete cycle, or 136,392 creates/s producer-
> only. No Redis, no broker, no coordinator required in the default
> mode. Cluster (consistent-hash + gRPC routing) and a legacy Redis
> backend are opt-in for HA and multi-node deployments.

### Comparativos (use verbatim or as a base)

| Dimension | codeq | Asynq | BullMQ | Celery | Kafka |
|---|---|---|---|---|---|
| External dependency | None (embedded Pebble) | Redis | Redis | Redis or RabbitMQ | ZooKeeper or KRaft cluster |
| Throughput (single node, full cycle) | 83k tasks/s | ~10k tasks/s | ~5k tasks/s | ~3k tasks/s | 100k+ msgs/s, but no task semantics |
| Language affinity | Go (HTTP + gRPC; SDKs for Java, Node, Python, Go) | Go | Node only | Python only | Polyglot |
| Durability | Pebble batch + group-commit; optional fsync | Redis AOF / RDB | Redis AOF / RDB | Broker-dependent | Replicated log |
| Multi-tenant native | Yes (JWT tenantId namespacing built in) | No | No | No | No |
| Learning curve | One binary, HTTP + curl in minutes | Moderate (Redis ops) | Moderate (Node + Redis) | High (broker + result backend) | High (broker, consumer groups, schema registry) |

When citing this table in a doc, link back here:

> See [_STYLE.md § Comparativos](./_STYLE.md#comparativos-use-verbatim-or-as-a-base).

## 2. Audience profile

Primary reader: **backend engineer evaluating alternatives to Redis-backed
task queues**. They are typically deciding between Asynq (Go + Redis),
BullMQ (Node + Redis), Celery (Python + broker), Sidekiq (Ruby + Redis),
Temporal (workflow engine), and Kafka (event log used as a queue).

What they care about, ranked:

1. **Throughput** — concrete numbers, harness names, and the box they
   were measured on. No "blazing fast", no "10x". Cite the benchmark.
2. **Operational cost** — how many processes, how many config knobs, how
   long until they can `curl` a task.
3. **Language affinity** — Go server, but with first-class SDKs for the
   reader's stack.
4. **Durability and at-least-once** — what survives a crash, and how
   they prove it.
5. **Multi-tenant** — whether they can run one queue server for many
   customers without writing isolation themselves.
6. **HA and scaling story** — single-node first, but with a credible
   path to multi-node.

Write for someone who already understands queues. Skip onboarding to
basic concepts like "what is a task". Do define codeq-specific terms
(shard, lease table, ring, scatter-gather) the first time they appear.

## 3. Voice and tone

### Numbers first, narrative second

Bad:

> codeq is blazing fast and can handle massive workloads.

Good:

> codeq sustains 83,420 tasks/s for a full create → claim → complete
> cycle on a 12-core Linux box, 4 Pebble shards, 32 producer slots at
> batch=8, 128 worker slots, 20s measurement window
> (`internal/bench/profile_full_cycle_test.go::TestProfile_FullCycle`,
> `PHASE8_SHARDS=4 PHASE6_BATCH=32 PHASE6_PROD_BATCH=8`).

Every throughput claim cites:

- The exact test name in `internal/bench/`.
- The env vars used to parameterize it.
- The hardware class (cores, OS).
- The measurement window.

### Honest about limitations

Always name the trade-offs. Examples to use as templates:

- "HA is only available via the Redis backend or by running a cluster of
  Pebble nodes. A single Pebble process loses availability while it
  restarts; recovery is fast (in-memory lease table rebuilt from the
  `KeyInprog` scan at Open) but not instantaneous."
- "Cluster mode and intra-process sharding (`numShards > 1`) are
  mutually exclusive — startup panics if both are enabled. Pick one:
  multi-node across machines, or multi-shard inside one process."
- "Delivery is at-least-once, not exactly-once. A worker that crashes
  between completion and ACK will see its task re-delivered after the
  lease expires."

### No marketing fluff

Banned words: blazing, massive, lightning, seamless, robust,
cutting-edge, world-class, next-gen, revolutionary.

Replace adjectives with measurements. If you cannot measure it, do not
claim it.

### Opinionated

Tell the reader what to pick:

- "For single-node deployments use Pebble with `numShards: 4`."
- "Do not enable `fsyncOnCommit` unless you have measured the latency
  impact in your workload — it costs ~20% throughput in our harness."
- "Use Redis only if you need multi-node HA today and cannot deploy the
  cluster mode."

## 4. Markdown conventions

- **Headings**: ATX style (`#`), single `#` per file (the title), then
  `##` for top-level sections, `###` for subsections. Do not skip
  levels.
- **Code blocks**: always tag the language.
  - `bash` for shell, `go` for Go, `yaml` for config, `json` for JSON,
    `protobuf` for `.proto`, `mermaid` for diagrams.
  - Untagged blocks are forbidden — they break syntax highlighting on
    GitHub.
- **Admonitions** (GitHub-flavored):

  ```markdown
  > **Note**: a normal aside.
  > **Warning**: a footgun. Always for irreversible operations or
  > silent data loss.
  > **Performance**: a tuning hint with a number.
  ```

- **Tables**: pipe-aligned, header row separator `---`. Always have a
  header row. Cells with multi-word values stay on one line.
- **Inline code**: backticks for file paths, env vars, config keys,
  function names, and protocol constants.
- **Lists**: `-` for unordered, `1.` for ordered. Two spaces of
  indentation for nested items.
- **Links**: prefer descriptive text over "click here". Relative links
  for in-repo references (see §6).

## 5. Mermaid conventions

All diagrams must render on the GitHub default theme in both light and
dark mode. **Never set inline colors or styles** — let the renderer
pick.

### Flow diagrams

Use `graph TB` for top-to-bottom layered architecture, `graph LR` for
left-to-right pipelines. Always use `subgraph` to delimit logical
layers.

```mermaid
graph TB
  subgraph Producers
    P1[Producer SDK]
  end
  subgraph Server[codeq server]
    HTTP[HTTP API]
    GRPC[gRPC stream]
  end
  subgraph Storage
    PEB[Pebble shards]
  end
  P1 --> GRPC
  GRPC --> PEB
  HTTP --> PEB
```

Aim for 8 to 12 nodes max in a single diagram. If it grows past that,
split it.

### Sequence diagrams

Use `sequenceDiagram` for flows over time. Participant names start
capitalized.

```mermaid
sequenceDiagram
  participant Producer
  participant Server
  participant Pebble
  Producer->>Server: Produce(task)
  Server->>Pebble: BatchCommit
  Pebble-->>Server: ack
  Server-->>Producer: TaskID
```

### State machines

Use `stateDiagram-v2` for task lifecycle. Transition labels describe
the **trigger action**, not the resulting state.

```mermaid
stateDiagram-v2
  [*] --> PENDING: Produce
  PENDING --> IN_PROGRESS: Claim
  IN_PROGRESS --> COMPLETED: SubmitResult(ok)
  IN_PROGRESS --> PENDING: Nack(retry)
  IN_PROGRESS --> DLQ: maxAttempts exceeded
  COMPLETED --> [*]
  DLQ --> [*]
```

### No inline colors

Bad:

```text
style P1 fill:#f9f,stroke:#333
```

Good: nothing — let the theme decide.

## 6. Cross-link policy

Every doc ends with a `## See also` section enumerating related docs.
Example:

```markdown
## See also

- [Architecture](./03-architecture.md)
- [Storage layout (Pebble)](./07b-storage-pebble.md)
- [Performance tuning](./17-performance-tuning.md)
```

Path conventions:

- **Inside `docs/`** (doc-to-doc): use relative paths starting with
  `./`, e.g. `[Overview](./01-overview.md)`.
- **From root `README.md`** (or other root-level files): use paths
  rooted at `docs/`, e.g. `[Overview](docs/01-overview.md)`.
- **To source files**: use repo-rooted paths, e.g.
  `[application.go](../pkg/app/application.go)` from inside `docs/`, or
  `pkg/app/application.go` from `README.md`.
- **Anchors**: lowercase, hyphen-separated, match GitHub's slug
  algorithm.

When you cite the value prop, link to this file:

```markdown
See [_STYLE.md § Value proposition](./_STYLE.md#1-value-proposition).
```

## 7. Numbers must come from measurement

Every throughput, latency, or memory claim in any doc must be traceable
to a test in `internal/bench/`. The format is:

> N tasks/s (`internal/bench/<file>.go::<TestName>`, env: `KEY=VAL`,
> hardware: 12-core Linux).

Catalog of canonical benchmarks (use these names, do not invent new
ones):

| Workload | Harness | Result on reference box |
|---|---|---|
| Full cycle, 4 shards, batched | `internal/bench/profile_full_cycle_test.go::TestProfile_FullCycle` (`PHASE8_SHARDS=4 PHASE6_BATCH=32 PHASE6_PROD_BATCH=8`) | 83,420 tasks/s |
| Producer-only, batched stream | `internal/bench/producer_stream_vs_rest_test.go::TestProducerThroughput_StreamBatchPath` | 136,392 creates/s |
| Worker-only, batched stream | `internal/bench/worker_stream_saturation_test.go::TestSaturation_StreamPath` (c=4, `PHASE6_BATCH=32`) | 23,518 tasks/s |
| Shard sweep | `internal/bench/profile_full_cycle_test.go::TestProfile_FullCycle` (`PHASE8_SHARDS=1,2,4,6,8`) | 42k / 65k / 83k / 68k / 67k |

Reference box: 12-core Linux (kernel 5.15, WSL2-compatible), Go 1.25.0,
local Pebble, loopback gRPC, no fsync.

If you need a number that is not in this table, run the bench, add the
row here, then cite it in your doc. Do not estimate.

## See also

- [README](../README.md) — applies this style guide at the entry point.
- [Overview](./01-overview.md) — the canonical statement of what codeq
  is.
- [Architecture](./03-architecture.md) — package-level breakdown.
- [Performance baselines](./30-performance-baselines.md) — raw bench
  output and historical numbers.
