# Sous Functions Concepts

This page is the conceptual map between Sous and codeQ. It explains, term by term, how a function invocation in Sous becomes a task in codeQ, how the worker side translates that task back into a function call, how the lease lifecycle interacts with isolate failures, and what the wire model looks like end to end. The level of detail is the same as the rest of the [Concepts](Concepts-Overview) section; the reader should come away knowing which codeQ field carries which Sous-level value and what every result kind means for the function's caller.

Before going further it is worth restating the project description from the [Sous repository](https://github.com/osvaldoandrade/sous): "SOUS is a serverless execution layer for agent automation, deploying functions without compilation. The platform ensures runtime parity between local and cluster, executing functions in secure isolates." Every concept on this page lines up with one of the four claims in that sentence — serverless execution, no compilation, runtime parity, secure isolates — and the rest of this section ([Overview](Sous-Functions-Overview), [Get Started](Sous-Functions-Get-Started), [Configure Workers](Sous-Functions-Configure-Workers)) builds on the same vocabulary.

## A function invocation is a task

The starting point is the smallest possible statement: a function invocation is a codeQ task. The Sous control plane accepts a request to run a function — over HTTP, over gRPC, however it chooses to expose itself to its own clients — and translates that request into a `CreateTask` event on the codeQ producer stream. From that point forward the codeQ task is the only durable record of the invocation. If the Sous control plane crashes after the `CreateTask` was acknowledged, the invocation is still in flight; if it crashes before the acknowledgement, the invocation never happened and the caller will retry. The acknowledgement boundary is the same as for any other producer, described in detail on the [Producer Stream](IO-Producer-Stream) page.

The task that Sous creates is not special. It uses the same `Task` struct described on [Tasks and Results](Concepts-Tasks-And-Results), with the same lifecycle states (PENDING → IN_PROGRESS → COMPLETED or FAILED), the same lease semantics, and the same retry-and-dead-letter rules. Nothing about it tells codeQ that there is a function on the other end. The only signal that a Sous worker uses to recognise the task is its `Command` field, which by convention carries the registered function name.

## The wire model

This is the field-by-field mapping. The left column is the Sous-level concept; the right column is where that concept lives in the codeQ task. The mapping is a Sous convention — codeQ does not enforce it — but every Sous deployment follows it, and the rest of this page assumes it.

| Sous concept | codeQ task field | Notes |
|---|---|---|
| Function name | `Command` | A short identifier registered with the control plane, e.g. `reconcile` or `summarize-document`. |
| Serialized arguments | `Payload` | An opaque byte slice. Sous chooses the encoding; codeQ does not parse it. |
| Function output | `ResultRecord.Body` | JSON-encoded by the worker SDK; readable by the producer via `GetResult`. |
| Failure detail | `ResultRecord.Error` | Human-readable string. The task transitions to FAILED. |
| Caller tenant | `Task.TenantId` | Derived from the JWT used to authenticate the producer stream. |
| Caller webhook | `Task.Webhook` | Optional. If set, codeQ POSTs the result here on completion. |
| Retry budget | `Task.MaxAttempts` | Sous chooses the default per function; the control plane writes the value. |
| Scheduling delay | `Task.RunAt` / `DelaySeconds` | Used for delayed invocations and for backoff after Nack. |

The convention is deliberately spare. The `Command` is short enough to read in logs without truncation; the `Payload` is opaque so that Sous can change its argument encoding without coordinating with codeQ; the result is a JSON map so that producers can read it back through `GetResult` without an SDK for the function's native encoding. Everything else — tracing context, function metadata, isolate hints — rides in the task's metadata channel, which is treated as opaque by codeQ and meaningful by Sous.

## How a Sous worker translates a task back

On the worker side the wire model runs in reverse. A Sous worker, which is an ordinary codeQ worker built on `pkg/workerclient/client.go`, opens a bidirectional stream to `:9091`, presents a JWT in the `Hello` event, and issues `Ready` to claim work. The stream is identical in shape to any other worker stream and the API is the one described on [Configure Workers](Sous-Functions-Configure-Workers).

When the stream delivers a task — or a `TaskBatch` for workers configured with `BatchSize` greater than one — the worker reads three things from it. The `Command` field tells the worker which function to invoke. The `Payload` field becomes the function's argument tuple after Sous's decoding step. The `LeaseUntil` field tells the worker how long it has before the lease expires and the task is fair game for another claim. The worker then hydrates the named function into an isolate, calls it with the decoded arguments, and observes the outcome.

The outcome maps back to one of four [`Result`](https://github.com/osvaldoandrade/codeq/blob/main/pkg/workerclient/result.go) constructors. A function that runs to completion produces `workerclient.Completed(body)`, where `body` is the function's return value encoded as a JSON map. A function that fails with an error the runtime classifies as terminal produces `workerclient.Failed(err)`. A function that fails transiently and asks Sous to retry produces `workerclient.Nack(delaySeconds, reason)`, which returns the task to the queue with a backoff. A worker that is shutting down cleanly and cannot finish the task produces `workerclient.Abandon()`, which releases the lease so another worker can claim immediately. Those four verbs are the only language a Sous worker uses to report function outcomes, and they are the same four verbs every other codeQ worker uses.

## Secure isolates as worker execution environments

The phrase "secure isolates" describes the unit in which a Sous worker actually runs a function. The Sous documentation is the authoritative reference for what an isolate is; from codeQ's point of view it is enough to know that an isolate is a process boundary with a restricted syscall surface and a resource cap on CPU, memory, and wall-clock time. Three properties of that boundary matter to the codeQ integration.

The first is that the isolate can die without taking the worker down. If a function hits an OOM cap, an executor-thread deadlock, or an explicit `kill -9` from a watchdog, only the isolate is gone — the Sous worker process is still running, still attached to its codeQ stream, and still able to claim more work. When the worker observes the death, it usually reports `Nack` so the task can be retried, or `Failed` if the failure is deterministic. Either way the codeQ lease is released and the task is back in a defined state.

The second is that the isolate's resource cap is enforced by the kernel, not by codeQ. codeQ has no notion of CPU quota or memory limit per task; it has a lease and a heartbeat. If a function exceeds its cap, the isolate is torn down and the worker decides what to report. If the worker stops heartbeating because the host is overloaded, the reaper expires the lease and the task returns to the queue. The two safety nets are independent and stack on top of each other.

The third is that an isolate carries no state between invocations. A function that mutates a global variable on the first run has a fresh global variable on the second. The implication for codeQ is that a Sous task is by construction idempotent at the isolate level: a retry runs the function in a fresh environment, with no leftover state from the failed attempt. The function author is still responsible for external side-effects — writes to a database, calls to a third-party API — but the runtime substrate is clean.

## Runtime parity

"Runtime parity between local and cluster" is the second half of the secure-isolate claim. The semantics on a developer laptop are the same as on a production node: the same syscall filter, the same resource cap, the same input and output contract. A function that runs to completion locally will run to completion on the cluster, and a function that exceeds its memory cap locally will exceed it in production. The Sous runtime is responsible for that guarantee.

What codeQ contributes to runtime parity is that the same task model serves both worlds. A developer running a single Sous worker against a single embedded codeQ binary on their laptop sees the same task lifecycle that an operator running ten Sous worker replicas against a five-node codeQ cluster sees. The wire surface is identical, the result kinds are identical, the lease semantics are identical. There is no separate "local mode" of codeQ that swaps in a different scheduler. The local run is the production run, scaled down.

That property has a practical consequence for testing. A function whose retry policy depends on the lease expiring can be tested locally by waiting out a short lease; the same code path executes in production with whatever lease the operator configures. A function that depends on the task being delivered exactly once until lease expiry can be tested by killing the worker mid-run; the local task will come back through the same reaper that runs in production. Neither test requires a Sous-specific mode; both rely on properties that codeQ provides unconditionally.

## At-least-once delivery and the Sous contract

Tasks in codeQ are delivered at least once. The lease prevents two workers from holding the same task at the same time, but it does not prevent the same task from being executed twice on different workers if a worker takes a task, runs the function, and then dies before reporting back. When that happens the lease expires, the reaper returns the task to PENDING, another worker claims it, and the function runs a second time. The producer sees one `COMPLETED` record corresponding to whichever worker succeeded in reporting; the function itself ran twice.

Sous inherits that property. The Sous-level contract for a function is therefore that it must tolerate being called more than once with the same arguments. The function author can either make the function deterministically idempotent — same arguments produce the same external effect — or use an idempotency key on the producer side so that codeQ collapses duplicate invocations to a single task. The idempotency mechanism is described on [Tasks and Results](Concepts-Tasks-And-Results) and is a one-line opt-in on the producer side.

The lease lifecycle is the place where at-least-once concretely shows up. A Sous worker that claims a task gets a lease of `LeaseSeconds` (server default if zero, configurable on the worker client via `Config.LeaseSeconds` in [`pkg/workerclient/client.go`](https://github.com/osvaldoandrade/codeq/blob/main/pkg/workerclient/client.go)). If the function runs to completion within the lease, the worker submits a result and the lease is released. If the function is still running when the lease is about to expire, the worker is expected to issue a heartbeat to extend it. If the worker cannot heartbeat — because the host is overloaded, the isolate is stuck, or the network has partitioned — the lease expires and another worker can claim. The function may run twice. Sous workers, like all codeQ workers, must therefore size their lease budget to the expected execution time of the function, not the median.

## Latency profile

The latency of a Sous function call is the sum of several components and the design point is the medium-grained range. The first component is the producer round trip: the time between the Sous control plane sending `CreateTask` and receiving the `CreateAck`. With the gRPC producer stream this is a single network round trip plus a Pebble batch commit, typically in the low single-digit milliseconds on local-disk storage.

The second component is queue wait time: the time the task spends in PENDING before a worker claims it. Under steady state with capacity to spare this is at most the worker's idle backoff, around 50 milliseconds with default settings; under load this can grow arbitrarily. The third is claim and stream delivery: the gRPC stream send from server to worker, typically sub-millisecond on a local network. The fourth is isolate startup, which is the dominant fixed cost in cold-start cases and is a Sous concern. The fifth is the function's own execution time. The sixth is the result send and the producer's `GetResult` poll, which adds another network round trip plus a Pebble read.

The profile fits work that takes at least tens of milliseconds. For sub-millisecond RPC the overheads dominate and a different layer — direct gRPC, an in-process call, a shared-memory invocation — is the right answer. For work that takes hundreds of milliseconds or more, the inherited durability, retry, lease, and dead-letter behaviour is worth the fixed cost. Sous is documented for this range, and codeQ's performance work documented on [Performance Overview](Performance-Overview) is calibrated for the same range.

## What the producer sees when a function completes

After a function runs to completion, the producer can read the result. The Sous control plane is the most common reader because it is the layer that returns function output to whoever invoked the function in the first place. The read happens via `GetResult(taskID)`, which is described on [Tasks and Results](Concepts-Tasks-And-Results) and is the same RPC any application would use.

The `ResultRecord` returned by `GetResult` contains the `Status` ("COMPLETED" or "FAILED"), the `Body` (the JSON-encoded function return value, present on COMPLETED), the `Error` (the failure string, present on FAILED), and timestamps. Sous's control plane interprets the `Body` as the function's return value, decodes it according to the function's declared return type, and hands it back to its caller. None of that decoding is visible to codeQ; the `ResultRecord` is opaque to anyone who is not the matching Sous control plane. A second consumer of codeQ on the same instance can read its own results in its own way without ever seeing a Sous payload.

## Walking the full path

To make the model concrete, here is one invocation traced end to end. A Sous client calls the Sous control plane with `invoke(name=summarize, args={url:"…"})`. The control plane validates the call, derives a tenant from its JWT, and sends a `CreateTask` on the codeQ producer stream with `Command="summarize"`, `Payload` set to the encoded `args` map, `MaxAttempts=3`, and a Sous-chosen idempotency key. codeQ persists the task in Pebble through the group-commit coalescer described on [IO Group Commit Coalescer](IO-Group-Commit-Coalescer), assigns a task ID, and returns `CreateAck` with that ID. The control plane stores the ID and returns "accepted" to its client.

A Sous worker, configured for `Commands=["summarize"]` on its `Ready` events, claims the task. The server attaches a 60-second lease, removes the task from the pending list for that command, places it in the in-progress index, and streams a `Task` event back to the worker. The worker decodes the payload, hydrates the `summarize` function into a fresh isolate, calls it with the decoded arguments, and observes the return value `{"summary":"…"}`. The worker constructs `workerclient.Completed(map[string]any{"summary":"…"})` and the SDK marshals the body, sends a `Result` event with `Status="COMPLETED"`, and moves to the next `Ready`. codeQ writes the `ResultRecord`, marks the task COMPLETED, drops the lease, and removes the in-progress entry.

When the Sous control plane next polls `GetResult` for that task ID, it reads back the `ResultRecord`, decodes the `Body` as the function's return type, and hands the summary back to whoever invoked the function. The entire trace fits on one page because every step is one operation, and every operation is either a Sous concern or a codeQ concern with no ambiguity in between. That clarity is the reason the design works.

## Where to read next

[Get Started](Sous-Functions-Get-Started) walks through the operational topology — how to bring up a Sous control plane and a Sous worker pool against an existing codeQ instance. [Configure Workers](Sous-Functions-Configure-Workers) covers the worker pool's tunables in depth. The [IO Overview](IO-Overview) section is the reference for the gRPC wire surface, and the rest of the [Concepts](Concepts-Overview) section explains the underlying codeQ behaviour that Sous inherits.
