# Sous Functions Configure Workers

A Sous worker pool is the side of the integration that actually runs functions. Each replica of the pool is an ordinary codeQ worker built on the Go SDK in [`pkg/workerclient/client.go`](https://github.com/osvaldoandrade/codeq/blob/main/pkg/workerclient/client.go). The replica opens a bidirectional stream to codeQ's worker port `:9091`, presents a JWT in the `Hello` handshake, registers the function commands it knows how to run, and from then on issues `Ready` events to pull tasks and `Result` events to report outcomes. This page covers how Sous wires that wire-level behaviour to its pool: how many slots per replica, how long a lease should be, when to enable batched claims, and what each tuning knob actually does on the codeQ side.

The detailed Sous-side configuration — which YAML key holds which value, what the Sous defaults are, how the worker pool autoscales — lives in the [Sous repository](https://github.com/osvaldoandrade/sous). This page documents the codeQ-side surface that those Sous knobs ultimately drive. If you are tuning a Sous deployment in production you will end up reading both pages, because the right value for `Concurrency` depends on how Sous schedules isolates and the right value for `LeaseSeconds` depends on how long the slowest function in your registry takes to run.

## The slot model

A codeQ worker client is built around a slot model. Each slot is one independent loop of "Ready → receive Task(s) → run handler → send Result(s)". The number of slots is controlled by `Config.Concurrency` on the worker client; that number is the upper bound on how many tasks one replica can have in flight at once. A Sous worker pool replica typically maps one slot to one isolate-execution lane: when the slot receives a task, the worker hydrates the named function into an isolate, runs it, and reports back. While the isolate is running, the slot is busy and does not issue another `Ready` until the function completes (or the handler returns a `Nack`/`Abandon`).

```mermaid
flowchart LR
    subgraph Replica[Sous worker pool replica]
      H[Stream handshake<br/>Hello -> HelloAck]
      direction LR
      S1[Slot 1]
      S2[Slot 2]
      S3[Slot N]
    end

    H -.->|JWT, WorkerID, Commands| CQ[codeQ worker stream :9091]
    S1 -->|Ready{LeaseSeconds, Count}| CQ
    S2 -->|Ready{LeaseSeconds, Count}| CQ
    S3 -->|Ready{LeaseSeconds, Count}| CQ
    CQ -->|Task / TaskBatch| S1
    CQ -->|Task / TaskBatch| S2
    CQ -->|Task / TaskBatch| S3
    S1 -->|Result / ResultBatch| CQ
    S2 -->|Result / ResultBatch| CQ
    S3 -->|Result / ResultBatch| CQ
```

The slots share one gRPC stream and one writer goroutine. The SDK serialises outbound events through that writer to amortise lock contention; the Phase 6 throughput work documented in the package comment of `pkg/workerclient/client.go` found per-slot `Send` calls fighting for a single send mutex under high concurrency, and the writer goroutine eliminates that path. From the operator's point of view the implication is that adding more slots on one replica does not multiply the gRPC overhead linearly — the framing cost is shared.

## The four worker-client knobs that matter

The public API on `pkg/workerclient` exposes a handful of fields on `Config`; for a Sous deployment, four of them are the load-bearing knobs.

`Concurrency` is the number of slots per replica. Each slot can hold one task at a time. If a function takes on average 200 milliseconds and a replica has 10 slots, the replica's steady-state throughput is at most 50 invocations per second per replica — modulo the codeQ-side claim cost, the network, and the per-task isolate startup. Sous chooses a default per workload profile; the codeQ side does not care about the exact value as long as it is consistent with the lease budget and the function's median execution time.

`LeaseSeconds` is sent on each `Ready` and tells codeQ how long the worker expects to hold the task. The server clamps the value to its own ceiling; zero means "use the server default". A Sous worker should pick a `LeaseSeconds` that comfortably exceeds the median execution time of the longest function in its registry — typically 30 to 120 seconds — and pair it with heartbeats for long-running invocations. If `LeaseSeconds` is too short, slow functions trigger lease expiry and end up running twice; if it is too long, a crashed worker takes a long time to release its tasks back to the queue, and the throughput floor goes down.

`BatchSize` controls how many tasks each `Ready` requests and how many results coalesce into one `ResultBatch`. The default of zero or one keeps the legacy single-task path: one `Ready` returns one `Task`, and each completion is sent as one `Result`. Setting `BatchSize` greater than one enables the batched path: each `Ready` requests up to N tasks via a `TaskBatch`, and the slot sends one `ResultBatch` with up to N entries when the tasks finish. The batch path is described in detail in the SDK source comments and is the route to amortising gRPC framing and Pebble commit cost over many tasks. It is most useful when the per-task work is short relative to the framing cost; for Sous functions that take hundreds of milliseconds the batched path is rarely a win, because the framing is already small relative to the work.

`Commands` restricts what the worker will pull. A Sous worker pool replica that knows how to run `summarize` and `reconcile` registers `Commands=["summarize","reconcile"]` and codeQ will only deliver tasks whose `command` field matches one of those values. If `Commands` is empty, codeQ uses whatever the JWT's `eventTypes` claim allows. A registry mismatch — a function the control plane is publishing under a name the workers do not register — manifests as a steady-state queue depth on that command with no claims; that is the standard symptom to look for during a deploy.

## Sizing concurrency to the cluster

The interaction between worker concurrency and codeQ throughput is governed by the claim path. A `Ready` event is cheap on the server side: it walks the per-command FIFO list, attaches a lease, and returns a `Task` (or a `TaskBatch`). On a Pebble-backed single node the per-claim cost is dominated by the durable index write — the in-progress index has to record the assignment before the claim is acknowledged. The [Performance Single Node Throughput](Performance-Single-Node-Throughput) page documents the steady-state claim rate for various configurations; the right concurrency for a Sous pool is the one that keeps the replica's slots saturated without exceeding codeQ's serving capacity.

A useful rule of thumb is to start with a concurrency that equals the function's median execution time divided by the target per-replica claim interval. If the function takes 100 milliseconds on average and you want each replica to handle 100 invocations per second, you need roughly 10 slots. If the per-function variance is high — some invocations take 50 ms, others take 2 seconds — start lower and watch the lease-expiry counter on the codeQ side; if it stays at zero, the lease budget is healthy and you can push concurrency higher. If it starts climbing, the workers are not heartbeating fast enough for the slowest functions, and either the lease budget or the heartbeat cadence needs adjustment.

## Lease budgets and heartbeats

The lease is the codeQ-side guarantee that a task is owned by exactly one worker at a time. When a Sous worker claims a task, it gets `LeaseSeconds` of exclusive ownership; if it does not finish in time and does not heartbeat, the reaper expires the lease and the task is up for re-claim. Heartbeats extend the lease without finishing the task, and the SDK provides them as part of the slot loop.

For Sous, the lease budget is the upper bound on how long an isolate can run before either finishing or heartbeating. The Sous runtime is responsible for issuing heartbeats from inside long-running isolates and for tearing down isolates that exceed the function's declared timeout. The codeQ side only sees the result of those policies: either a `Result` arrives in time, or the lease expires and the task goes back to the queue. The right `LeaseSeconds` is therefore the longest expected function execution time, plus a margin for isolate startup, plus a safety factor for clock skew between worker and server.

In practice this is in the 30 to 120 second range for most Sous deployments. Going much shorter causes spurious retries for functions that are merely slow; going much longer makes recovery from a crashed worker take longer than necessary. The trade-off is the same as for any worker pool on codeQ and is described in [Concepts Leases And Ownership](Concepts-Leases-And-Ownership).

## When to enable batched claims

The batched-claim path (`BatchSize` greater than one) is a throughput optimisation, not a correctness change. With `BatchSize=8`, each `Ready` requests up to 8 tasks and the server replies with a `TaskBatch` containing whatever is available, up to that limit. The slot runs each task in turn and accumulates results, then sends one `ResultBatch` instead of 8 individual `Result` events. The amortisation is over gRPC framing and Pebble commit cost: a single `ResultBatch` is one server-side batch commit instead of N.

For Sous, the batched path is rarely useful. The reason is that Sous tasks are dominated by isolate startup plus function execution, both of which are far more expensive than the gRPC framing being amortised. Batching helps when the per-task work is short — single-digit milliseconds — and the framing cost is a meaningful fraction of total time. For functions in the tens-of-milliseconds-and-up range, batching adds latency without saving meaningful CPU. The recommendation is to leave `BatchSize` at its default for most Sous workloads and revisit only if profiling shows the writer goroutine or the result-send path is a meaningful contributor to total latency.

There is a second cost worth flagging. A batched claim ties N tasks to one slot for the duration of the batch. If one task is much slower than the others, the slot cannot pick up new work until all N finish. For functions with high variance in execution time, a non-batched path with `BatchSize=1` distributes work more evenly across slots and gives better tail latency. Most Sous deployments fit this profile.

## What codeQ sees per replica

Once the wiring is in place, codeQ tracks each worker pool replica by the `WorkerID` it presents in `Hello`. The `WorkerID` does two things. It is the lease owner recorded against every task the replica claims; if the replica disappears, the reaper finds its tasks by scanning the in-progress index for that ID. It is also the unit of accounting in the operator surface — per-worker claim rate, per-worker active lease count, per-worker error rate. A Sous worker pool that uses a stable `WorkerID` per replica (typically the pod name or a UUID generated at start) gives the operator a clear per-replica view in metrics and logs.

The other thing codeQ tracks is the rate of `Ready` events per replica. A healthy worker pool sends `Ready` at a roughly steady rate — close to the per-slot throughput multiplied by the number of slots, minus whatever time slots are blocked on isolates. A replica whose `Ready` rate drops to zero is either deadlocked or has all its slots stuck on long-running isolates; either way the operator can see the symptom without any Sous-specific instrumentation.

## The handshake and the JWT

Every replica starts with one `Hello` event carrying a JWT and an optional `WorkerID`. codeQ validates the JWT, accepts or rejects the handshake, and returns a `HelloAck` with the resolved `TenantId` and `WorkerId`. From that point the stream is authenticated for the lifetime of the connection. The JWT typically encodes the worker's `eventTypes` claim, which constrains which `Commands` the replica is allowed to register. The Sous control plane is responsible for issuing tokens whose claims match the function registry the worker is configured to serve; misalignment shows up at handshake time as an explicit error code, not as a silent failure.

The `Hello`/`HelloAck` exchange is described in detail on [IO Worker Stream](IO-Worker-Stream); for Sous it is the same protocol every other worker uses.

## Where to go next

If you are tuning concurrency and lease in real numbers, [Performance Tuning Knobs](Performance-Tuning-Knobs) and [Performance Single Node Throughput](Performance-Single-Node-Throughput) have measurements that calibrate the trade-offs. If you are deploying a Sous worker pool against a Raft-replicated codeQ cluster, [Concepts Cluster Level Failover](Concepts-Cluster-Level-Failover) explains how worker connections survive a leader change. If you are about to register your first function and want to know how it shows up on the wire, [Deploy](Sous-Functions-Deploy) walks through the end-to-end registration flow.

The Sous repository at [github.com/osvaldoandrade/sous](https://github.com/osvaldoandrade/sous) is where the worker-pool binary lives, where its YAML configuration is documented, and where Sous-specific concerns like isolate caching and function registry distribution are explained. This page only covers the codeQ-side surface those Sous knobs operate against.
