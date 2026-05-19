# Get Started: Overview

This section is for readers who have never run codeQ before. The goal is concrete: by the end you have a codeQ server running, a task created, a worker that claimed the task, a result fetched, and a reasonable idea of which of the four supported topologies — local binary, single Docker container, three-node raft cluster, or Kubernetes — you actually want to keep. The pages that follow are written to be read in order on a first pass and as standalone references afterward.

codeQ is a single binary. The same binary can run as a standalone task queue server on a developer laptop, as a member of a three-node raft cluster that survives one node failure, or as a Kubernetes Deployment behind a Service. The choice of topology does not change the API; it changes how many copies of your data exist and what happens when a process dies. The pages in this section walk through each topology in order of complexity. You can stop at the first one that matches your situation, and you should — there is no reward for reading past the point where the system fits the problem.

## Who this section is for

If you are evaluating codeQ, the right entry point is [Run Locally](Get-Started-Run-Locally). You install once, you start the server, you submit a task, and you see the queue model behave. The detour through Docker, compose, and Kubernetes is unnecessary for evaluation — the local binary is the same code path the cluster runs. The clustered topologies are operational concerns; they do not change anything about how the API behaves or what the data model looks like.

If you are integrating codeQ into a Go service, read [Run Locally](Get-Started-Run-Locally) for the install and quickstart, then jump to [Sous Functions Get Started](Sous-Functions-Get-Started) for the typed Go SDK. The SDK opens one bidirectional gRPC stream per process and multiplexes every produce, every claim, every heartbeat, and every result through it. You do not need to know the wire protocol to use it. You do need to know that there is one, because that shape is what makes the SDK fast and what keeps long-poll claims cheap. Reading the [Worker Stream](IO-Worker-Stream) page is useful but not required to ship.

If you are an operator preparing to run codeQ in a real environment, the natural order is [Run In Docker](Get-Started-Run-In-Docker) to understand the image and the volume layout, then [Run With Docker Compose](Get-Started-Run-With-Docker-Compose) to see the three-node raft topology, then [Run In Kubernetes](Get-Started-Run-In-Kubernetes) for a Helm-based install. The compose template is the canonical reference for how nodes talk to each other; the Helm chart is the same topology with Kubernetes-native plumbing. There is no separate "production guide" because there is no separate production binary. What runs on your laptop is what runs in your data center.

## What you will know after reading this section

Five concrete things, and they map onto the five pages.

You will be able to install codeQ. Three install paths exist. The first is `curl -fsSL .../install.sh | sh`, which downloads a pre-built binary from the latest GitHub Release. The second is `npm install -g @osvaldoandrade/codeq`, which wraps the same binary as an npm package and runs the postinstall download script. The third is `go install github.com/osvaldoandrade/codeq/cmd/codeq@latest`, which builds the CLI from source against your local Go toolchain. The first two install both the `codeq` CLI and the matching server binary; the Go-install path gives you the CLI alone. The server binary itself lives at `cmd/server/main.go` and is what the Docker image runs.

You will know which ports matter. codeQ exposes HTTP on `:8080`. When the worker stream is enabled it listens on `:9091`, configured by `WORKER_STREAM_ADDR`. When the producer stream is enabled it listens on `:9092`, configured by `PRODUCER_STREAM_ADDR`. In a cluster, every node opens a gRPC control plane on `:9090`, configured by `CLUSTER_GRPC_ADDR`. In a raft cluster with the multiplexed transport enabled, every node opens a single raft TCP listener on `:7000`, configured by `RAFT_BIND_ADDR`. These are the only ports you will see in any compose or Helm file in this repository, and they are the only ports your firewall needs to know about.

You will know how to submit, claim, and complete a task. Three HTTP calls: `POST /v1/codeq/tasks` to enqueue, `POST /v1/codeq/tasks/claim` to claim with a lease, `POST /v1/codeq/tasks/{id}/result` to finalize. Every page in this section walks through that triple at least once. The shape of those requests does not change between topologies; what changes is whether the server you hit is the only copy of the data, one of N replicas, or a Pod behind a Kubernetes Service. The Go SDK calls translate one-to-one onto those HTTP calls, so the wire is the same shape on both surfaces.

You will know what persistence looks like. Pebble — an embedded LSM tree, the same engine CockroachDB uses internally — lives at `/var/lib/codeq/pebble` inside the container, or wherever `persistenceConfig.path` points on a local run. Every task, every queue entry, every result is one or more key-value writes to that store. A single-node codeQ is durable to that disk; a raft cluster is durable to the majority of the disks behind its FSM. You will see the volume mounts spelled out in the compose and Helm pages, and you will understand why a raft commit only acknowledges after two of three FSMs have applied the write.

You will know which deployment mode fits your problem. The conceptual answer lives in [Deployment Modes](Concepts-Deployment-Modes), but the practical answer falls out of this section. A laptop install is for development. A single Docker container is for development, demo, and small environments where one machine failing is acceptable downtime. A three-node raft cluster is the answer for high availability. Kubernetes wraps either single-node or cluster topology with horizontal scaling and managed restarts. None of these modes are mutually exclusive over time — a system that started on a laptop migrates to a single container, then to a cluster, without rewriting any application code, because the API is invariant under topology.

## How the pages connect

The four runtime pages are independently readable. Each starts from scratch and assumes nothing about the previous one. They do, however, share a common Quickstart payload: a `PROCESS_ORDER` task that a curl-driven producer creates and a curl-driven worker claims and completes. The point is to make the same workload observable across topologies, so you can see exactly which behaviors change when persistence moves from a local directory to a Docker volume to a Kubernetes PersistentVolumeClaim, and which behaviors do not change at all.

When you have done the local run, the Docker run will feel like the same exercise inside a container. When you have done the Docker run, the compose run will feel like the same exercise across three containers. That is intentional. The state machine, the storage layout, the route handlers, and the SDK calls are all identical across topologies. The only thing that changes is what surrounds the binary.

The reading order is also the complexity order. [Run Locally](Get-Started-Run-Locally) introduces auth tokens, Pebble configuration, the task lifecycle, and the Go SDK in roughly that sequence. [Run In Docker](Get-Started-Run-In-Docker) reintroduces the same concepts inside a container and adds named volumes. [Run With Docker Compose](Get-Started-Run-With-Docker-Compose) adds raft consensus, peer DNS, the leader-redirect, and the leader-kill failover test. [Run In Kubernetes](Get-Started-Run-In-Kubernetes) wraps it all in a Helm chart and adds Services, ConfigMaps, Secrets, and PVCs. By the end of the fourth page you have seen every primitive codeQ uses in any deployment.

## A word on conventions

The wiki uses `path/to/file.go:line` to cite source. Every assertion that involves a default value, a port, an environment variable, or a tuning knob is traceable to either the configuration file at `pkg/config/config.go`, the application boot path at `pkg/app/application.go`, or the relevant compose template under `deploy/docker-compose/`. If you see a number quoted without a citation, treat it as a typo and open an issue.

The wiki uses Mermaid for diagrams. Ports are labeled in the diagrams the same way they are labeled in the compose files: `:8080`, `:9091`, `:9092`, `:9090`, `:7000`. Reading the diagrams from left to right gives you the request flow; reading them top to bottom gives you the call stack. The arrow style hints at the transport: a thick arrow is a synchronous HTTP or gRPC call, a thin arrow is a one-way notification, and a dashed arrow is internal state movement.

Code snippets are runnable as written when copy-pasted into a Unix shell. Where a snippet uses a placeholder, the placeholder is enclosed in `<angle-brackets>` so it is obvious you need to substitute. The standard placeholder for the auth token is `dev-token`, which matches the static auth blocks in every compose file in the repository.

## What this section does not cover

Anything beyond "first task submitted and completed end to end" is somewhere else. For the conceptual model — what is a task, what is a lease, what is a queue, what does a raft commit mean — read the [Concepts](Concepts-Overview) section. For the wire protocol of the producer and worker streams, read [Producer Stream](IO-Producer-Stream) and [Worker Stream](IO-Worker-Stream). For metrics, dashboards, and how to debug a stuck task, read [Observability Overview](Observability-Overview). For "how fast does it go and how much does raft cost", read [Performance Overview](Performance-Overview).

If you finish this section and want a typed Go API rather than raw HTTP, the next click is [Sous Functions Get Started](Sous-Functions-Get-Started). If you are still deciding which topology you want, read [Deployment Modes](Concepts-Deployment-Modes) first — it explains the tradeoffs in detail so you do not have to read all four runtime pages before deciding. If you want the architecture from the top down before doing any practical work, [Architecture Overview](Concepts-Architecture-Overview) is the entry point.

## Prerequisites

The pages in this section assume some baseline. The exact baseline depends on which runtime page you intend to follow.

For [Run Locally](Get-Started-Run-Locally), you need a Unix-like environment (Linux, macOS, or WSL2 on Windows), `curl`, `jq` for parsing JSON output, and either a pre-existing `codeq` binary path or Go 1.25+ for `go install`. The install script handles fetching a release binary if you do not have Go installed. The page does not assume any prior codeQ knowledge.

For [Run In Docker](Get-Started-Run-In-Docker), you need a working Docker daemon. The image is `ghcr.io/osvaldoandrade/codeq-service:latest`, built from the repository's top-level `Dockerfile`. Pulling the image is the only network step. The page does not assume any prior Docker expertise beyond `docker run`.

For [Run With Docker Compose](Get-Started-Run-With-Docker-Compose), you need `docker compose` v2 (the plugin, not the legacy `docker-compose` binary) and approximately 1 GB of disk for the three named volumes used by node-a, node-b, and node-c. The page assumes you have read [Run In Docker](Get-Started-Run-In-Docker), or are willing to skim it for the image and environment-variable conventions.

For [Run In Kubernetes](Get-Started-Run-In-Kubernetes), you need a Kubernetes cluster you can `kubectl` against, Helm 3, and the ability to create a PersistentVolumeClaim. A local `kind` or `minikube` cluster is sufficient for learning; the chart at `helm/codeq/` is identical for production. The page does not assume Kubernetes expertise but does assume basic kubectl familiarity — you should know what a Deployment, a Service, and a PVC are.

## A common thread

Across all four runtime pages the same architectural truth holds: one process, one HTTP listener, one Pebble store, optional gRPC streams for higher-throughput clients, and optional raft when one disk is not enough. The shape of the system does not grow when you scale it; the shape of the surrounding orchestration does. That is the design promise. By the end of this section you will have seen that promise hold under four different operational pressures, and you will be ready to decide which topology to commit to.

## Where this leads

The cleanest path through codeQ for someone learning it for the first time is to install via `curl install.sh | sh`, run `codeq` once to confirm the CLI is on `$PATH`, follow [Run Locally](Get-Started-Run-Locally) end to end, and then read [Concepts Overview](Concepts-Overview). At that point you have submitted a task, you have seen it complete, and you have read why the system is shaped the way it is. Every other section becomes a reference you consult on demand. From there, the next chapters expand into the architecture, the IO surfaces, and the operational concerns; you can read them in any order, because each section is its own self-contained narrative.
