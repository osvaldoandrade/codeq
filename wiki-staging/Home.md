# codeQ

codeQ is a task queue server written in Go. It accepts tasks over an HTTP API or a pair of bidirectional gRPC streams, persists them on an embedded LSM tree (Pebble), and hands them out to workers under a lease-based at-least-once delivery contract. A single binary serves producers, workers, and operators; the same binary forms a three-node raft cluster when high availability matters.

The design is opinionated. There is one storage engine (Pebble), one consensus protocol (raft via `hashicorp/raft`), and one transport for inter-node traffic (gRPC over a multiplexed TCP listener). Tasks are addressed by a server-assigned UUID. Queue ordering is per-command, priority + FIFO inside a priority. Workers pull, never poll a database directly. Most everything happens in one process; the open ports are `:8080` (HTTP), `:9091` (worker gRPC stream), `:9092` (producer gRPC stream), `:9090` (cluster control plane), and `:7000` (raft mux). Everything else — metrics, profiling, tracing — rides those listeners or piggybacks on the HTTP one.

This wiki documents the system from three angles. The first is operational: how to run codeQ on a laptop, in Docker, with `docker compose`, or on Kubernetes. The second is conceptual: what a task is, what a lease means, what consensus buys you, what a shard is, why the cluster topology looks the way it does. The third is reference: every endpoint, every metric, every tuning knob.

## Read by section

The wiki is organized into six sections. Each section has an overview page that explains what the section covers and who it is for, followed by topical pages that go deep on one idea at a time. Start at the overview if you are new to codeQ; jump to a topical page if you already know what you want.

The [Get Started](Get-Started-Overview) section walks through installing codeQ, running it on a laptop, running it in Docker, standing up a three-node raft cluster, and deploying to Kubernetes. Start here if you have never seen codeQ before.

The [Concepts and Architecture](Concepts-Overview) section explains tasks, queues, sharding, leases, multi-tenancy, authentication, the persistence engine, consensus, cluster failover, and deployment modes. Start here if you want to understand how codeQ works before deciding whether to use it.

The [Sous Functions](Sous-Functions-Overview) section documents the worker framework that turns a Go function into a codeQ consumer. Read this if you want to ship business logic rather than wire up gRPC stubs by hand.

The [CodeQ IO](IO-Overview) section is the wire-protocol reference: the producer stream, the worker stream, the REST API, the persistence engine, the group-commit coalescer, raft replication, and the mux transport. Read this when you need to know exactly what bytes move where.

The [Observability](Observability-Overview) section covers tracing, metrics, profiling, and logging. This is the chapter you want open in another tab when you are debugging a production incident.

The [Performance](Performance-Overview) section reports measured throughput, the cost of high availability, multi-shard scaling, tuning knobs, and the bench harness itself. Every number is traced back to the source file that produced it.

## Quick paths

If you came here with a specific intent, the table below points to a single page that gets you moving.

| Intent | Start here |
| --- | --- |
| I want to install codeQ on my laptop | [Run Locally](Get-Started-Run-Locally) |
| I want to run it in one Docker container | [Run In Docker](Get-Started-Run-In-Docker) |
| I want a three-node HA cluster | [Run With Docker Compose](Get-Started-Run-With-Docker-Compose) |
| I want to deploy on Kubernetes | [Run In Kubernetes](Get-Started-Run-In-Kubernetes) |
| I want to understand the architecture | [Architecture Overview](Concepts-Architecture-Overview) |
| I want to wire a Go producer or worker | [Sous Functions Get Started](Sous-Functions-Get-Started) |
| I want to read the HTTP API | [REST API](IO-REST-API) |
| I want to see throughput numbers | [Single-Node Throughput](Performance-Single-Node-Throughput) |
| I want to tune for my workload | [Tuning Knobs](Performance-Tuning-Knobs) |
| Production broke and I need help | [Observability Overview](Observability-Overview) |

## A note on prose

The pages in this wiki are written long-form on purpose. codeQ is not a configuration file; understanding what it does, and what it does not do, requires reading a few paragraphs. Where a table or bullet list is the right shape for the information, the page uses one. Where the right shape is two paragraphs explaining a tradeoff, the page uses two paragraphs. If you only want the command, every page has the command. If you want the why, the why is there too.

## Source code and issues

The codeQ source lives at [github.com/osvaldoandrade/codeq](https://github.com/osvaldoandrade/codeq). Issues, discussions, and pull requests are accepted on GitHub. The wiki is generated from `wiki-staging/` in the main repository — edits go via PR there.
