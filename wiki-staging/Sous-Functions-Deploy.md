# Sous Functions Deploy

This page describes what happens when a Sous function is deployed and how the deployment shows up on the codeQ side. The Sous-side mechanics — how a function is bundled, how the control plane stores its source or its compiled form, how the worker pool distributes the artefact across replicas — live in the [Sous repository](https://github.com/osvaldoandrade/sous). The page here is short by design: deployment is largely a Sous concern, and codeQ only participates at two well-defined moments.

## What "deploy" means in this section

In Sous, deploying a function means registering it with the control plane so that future invocations can dispatch to it. The Sous CLI `cs fn init <name>` scaffolds the function, `cs fn test --path <dir>` runs it locally against an isolate, and a subsequent push or register step makes the function available to the running control plane. After that point, an invocation request with the function's name resolves to a runnable artefact and the control plane is allowed to enqueue tasks for it. The exact sequence, the file layout of a function, and the artefact format are documented in the Sous repository and are not duplicated here.

What this page covers is the codeQ side of that flow. codeQ does not see "deploy" as a distinct event — it does not track function registrations, and it cannot tell you whether a particular function exists. What it sees is the tasks that start to arrive once Sous decides the function is deployable, and the workers that start to register the function's name in their `Commands` list. The two are the operational signals that the deploy worked from codeQ's seat.

## What codeQ sees when a function is registered

A function registration on the Sous side does not directly produce any codeQ traffic. The control plane learns about the new function, the worker pool either fetches the artefact or is informed where to find it, and the codeQ instance keeps doing what it was doing. The signals that matter to a codeQ operator only appear once the function is actually exercised.

The first signal is on the worker side. When a Sous worker pool replica is updated to include the new function in its registry, its next `Hello` (after a restart or a configuration reload) registers an updated `Commands` list. The `Ready` events that follow include the new command name. From codeQ's seat, this looks like a normal handshake plus an expanded set of commands that the worker is willing to claim. The change is observable in the worker-stream logs and in the per-command active-worker metric.

The second signal is on the producer side. When the Sous control plane decides to invoke the function — either because an end client called it or because the control plane is testing the registration — it emits a `CreateTask` event with `Command` set to the function's name. The task lands in the queue keyed by `(tenant, command)`, the same key that any other producer's tasks would use. If a worker is registered for that command, it claims the task within an idle backoff (default 50 milliseconds). If no worker is registered, the task sits in PENDING until a worker shows up — exactly the same behaviour codeQ has for any other unclaimed command.

That symmetry is the point. A Sous function's deployment status, from the codeQ side, is just "is there a queue depth on this command name, and is there a worker claiming from it". Both numbers are visible without any Sous-specific instrumentation.

## The task command name convention

Sous chooses the function name as the task `command`. The convention is documented on [Concepts](Sous-Functions-Concepts) and is the only piece of Sous-specific knowledge that creeps into the task model. codeQ does not require the command to be a function name — many other producers use commands like `email.send` or `payment.charge` — but every Sous deployment uses the function's registered name verbatim. The implication for operators is that the `command` field in codeQ logs, metrics, and admin queries is human-readable and corresponds one-to-one with what a Sous developer sees in their function registry.

A practical consequence is that you can use codeQ's per-command queue depth metrics to track the health of individual functions. If `summarize` has a queue depth of zero and `reconcile` has a queue depth of two thousand, you know one function is keeping up and the other is not. If `reconcile` has a queue depth of two thousand and no workers are claiming from it, the deploy has not finished propagating to the worker pool. The codeQ operator surface, documented in [Observability Overview](Observability-Overview), is enough to draw these conclusions without involving the Sous control plane's own metrics.

## The deploy sequence end to end

A typical Sous function deploy walks through the steps in the following order. The Sous repository is the canonical reference for each step; the description here is just enough to show where codeQ enters.

A developer authors the function locally and runs `cs fn test --path <dir>` to exercise it in an isolate. This step is entirely local — Sous does not talk to codeQ, and no task is created. The output is confidence that the function runs to completion in the same isolate runtime that production will use.

The developer registers the function with the Sous control plane through whatever push mechanism the Sous repository documents (CLI, gateway, file drop). The control plane records the registration in its own store and propagates the artefact to the worker pool, either by pushing the bundle, by exposing it for pull, or by an out-of-band distribution mechanism. None of this involves codeQ.

The worker pool reloads its function registry. The Sous mechanism for this is a Sous concern — a config reload, a restart, a hot-swap — and is out of scope here. The codeQ-side effect is that the next `Hello`/`Ready` cycle from the affected workers includes the new command in their `Commands` list. From here on, codeQ is willing to deliver tasks for the function whenever they arrive.

An invocation arrives at the Sous control plane. The control plane translates it into a `CreateTask` event, sends it on the producer stream, and codeQ persists it through the Pebble batch commit path. A worker claims it, the function runs in an isolate, the worker reports a result, and the control plane reads the result back to its caller. The walkthrough on [Concepts](Sous-Functions-Concepts) covers every wire event in this flow.

## Rolling a deploy back

There is no "rollback" verb in codeQ. A function rollback is whatever Sous chooses to do — typically reverting the registry to an earlier artefact and reloading the worker pool. From codeQ's side, the in-flight tasks are unaffected by the registry change: any task that was already in IN_PROGRESS continues to be owned by whichever worker claimed it under the previous version. The lease will either complete normally or expire and be re-claimed under the new version. If the function's interface changed between versions, the worker that claims under the new version may see arguments it cannot decode and report `Failed`; that is a Sous concern and is described in the Sous documentation under function versioning.

The implication for operators is that during a rollback, the per-command queue depth and the lease-expiry counter on codeQ are good signals to watch. A clean rollback shows the queue draining at the usual rate; a botched rollback — where the new artefact is incompatible with in-flight task payloads — shows a spike in `FAILED` results on the affected command. Either way the diagnostic surface is codeQ's standard one.

## What deploy does not affect

Two things are deliberately untouched by a Sous function deploy. The first is the codeQ persistence engine: tasks already in PENDING or IN_PROGRESS remain in Pebble exactly as they were, and the persistence layer's batch commit guarantees are unchanged. The second is the codeQ authorization model: a function deploy does not change which JWTs can produce tasks for that command or which workers can claim them. Authorization is governed by the JWT claims described on [Concepts Authentication And Authorization](Concepts-Authentication-And-Authorization), and Sous's control plane uses the same claim model when it issues tokens to its own workers.

The deploy is therefore a Sous-only event from codeQ's side. It is observable through downstream effects — new `Commands` registrations on `Hello`, new task arrivals on `CreateTask`, completions on the matching `Result` events — but it does not require any coordination with codeQ, any reconfiguration of codeQ, or any operational action on the codeQ cluster.

## Where to go next

If you are about to write a function and want the contract it must satisfy, [Develop](Sous-Functions-Develop) covers the lease-aware, side-effect-aware, deterministic shape a Sous function should aim for. If you want to know what a deployed function looks like in operational tooling, [Configure Workers](Sous-Functions-Configure-Workers) describes the per-replica accounting and the `WorkerID` model that pins claims to replicas. The Sous repository at [github.com/osvaldoandrade/sous](https://github.com/osvaldoandrade/sous) is the canonical reference for the deploy CLI, the artefact format, the registry semantics, and the function-versioning rules.
