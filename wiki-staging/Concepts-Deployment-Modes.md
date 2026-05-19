# Deployment Modes

codeQ supports four practical deployment topologies. Each one trades a different combination of throughput, fault tolerance, operational cost, and complexity. The choice is upfront and largely structural — there is no smooth migration between modes — so picking the right one matters. This page is the decision guide: what each mode looks like, what it costs, what it buys, and which mode fits which workload. Numbers cited here come from the benchmark suite under `internal/bench/` and `pkg/app/raft_*_bench_test.go`; full breakdowns live in [Performance Overview](Performance-Overview).

The four modes are: single-node Pebble, single-node Pebble with intra-process multi-shard, three-node raft replication, and three-node raft replication with per-shard raft groups. The non-recommended mode — two-node raft — is called out explicitly because the configuration validator does not reject it and operators sometimes pick it for cost reasons.

## Single-node Pebble

This is the simplest topology. One process, one Pebble store, one HTTP listener on `:8080`, one optional gRPC stream listener on `:9092` for the producer and worker streams. There is no replication, no consensus, no leader election. The process is the cluster.

Throughput sits at the engine's saturation point. The full-cycle benchmark at `internal/bench/profile_full_cycle_test.go` measures **76,639 tasks per second** on a single-node gRPC path — create, claim, complete cycle, end to end, on a developer laptop. This is the highest throughput any codeQ topology delivers because there is no replication tax, no network round-trip per write, no quorum wait. Every write hits the local engine's group commit coalescer, which collapses 64 concurrent submitters into one Pebble commit (`internal/repository/pebble/db.go:117-122`), and the engine's commit pipeline does the rest.

Fault tolerance is f=0. A process crash takes the queue offline until the process restarts. A disk corruption takes the queue offline permanently unless the operator has external backups. The blast radius is the box. For workloads where the cost of a few minutes of downtime is acceptable — internal pipelines, development environments, batch processing where the producer can retry from external state — this is the right mode. For workloads where any outage produces customer-visible failures it is not.

The configuration is minimal: `PERSISTENCE_PROVIDER=pebble`, `PERSISTENCE_CONFIG='{"path":"/var/lib/codeq/pebble"}'`, every other knob defaulted. The docker-compose layout at `deploy/docker-compose/local-dev/` is exactly this mode. The footprint is a single container plus a single volume.

This mode is the default. New deployments start here and only move to a different mode after they have measured something the single node cannot deliver — either throughput beyond 76k tasks/s, or a fault-tolerance requirement that forbids any single-point-of-failure.

## Single-node Pebble with multi-shard

The single-node mode has one bottleneck under sustained load: the Pebble commit pipeline. Even with the group commit coalescer, one commit pipeline can only run as fast as the underlying device and the engine's internal lock. The multi-shard mode adds N independent commit pipelines by opening N independent Pebble stores within the same process, keyed by `numShards` in the persistence config.

The wiring lives in `pkg/app/application_pebble.go:141-176`. The application opens `numShards` Pebble instances at `Path/shard0/`, `Path/shard1/`, and so on. Each instance has its own coalescer goroutine, its own compaction, its own block cache. Writes are hash-routed by task UUID through a `ShardedTaskRepository` that fans out to the owning shard. Reads go to the same shard. Cross-shard transactions do not exist — every operation targets a single shard, which is enforced by the routing layer.

The throughput gain scales sub-linearly. At two shards the measured throughput is roughly 1.5× the single-shard rate. At four shards it is roughly 3×. Beyond four shards the gains drop sharply: the bottleneck shifts from the engine to the Go runtime's goroutine scheduler and the single HTTP/gRPC listener that fans out to every shard. The sweet spot in practice is four shards on a box with eight or more cores; below that, the producer and worker goroutines do not generate enough concurrent commit pressure to saturate the parallel pipelines.

The cost is operational complexity. The disk layout has N subdirectories under the configured path. The metrics endpoint exposes per-shard commit latencies. A backup script must capture all N subdirectories atomically (typically via filesystem snapshot, because Pebble's directory is locked exclusively). A restart needs every shard to come up successfully — one shard's corruption brings down the whole process.

When to pick this mode: the single-node baseline cannot keep up with sustained load, and the deployment can tolerate the f=0 fault model. Typical scenarios are high-throughput internal pipelines where horizontal scaling via more boxes is harder than vertical scaling via more shards on one box. The fault-tolerance story is unchanged from the single-node case — a crash still takes the queue offline — so this mode does not solve availability problems.

The configuration is `PERSISTENCE_CONFIG='{"path":"/var/lib/codeq/pebble","numShards":4}'`. Everything else defaults.

## Three-node RAFT

Three-node raft is the smallest configuration that delivers actual fault tolerance. Three processes, three Pebble stores, one raft group spanning all three. Every write goes through the leader's `raft.Apply`, which replicates the write to both followers, waits for a majority ack (the leader plus one follower), and applies the write through the FSM on every replica. The cluster tolerates one node failure: a leader death triggers an election, the surviving two nodes form a majority, one of them becomes the new leader, and the cluster keeps serving. Two simultaneous failures stall the cluster — the remaining node knows it has lost quorum and refuses writes.

The throughput is materially lower than single-node. The HTTP smart-routing benchmark (`pkg/app/raft_smart_routing_bench_test.go`) measures roughly **3,949 / 3,883 cycles per second** end-to-end on a three-node loopback cluster. The gRPC path (`pkg/app/raft_grpc_bench_test.go`) measures **9-10k cycles/s**. The gap from single-node's 76k is the cost of replication: every write pays an AppendEntries round-trip to followers and a quorum wait before the commit returns. The Apply coalescer at the raft layer (`raftMergeBatch=128`, `internal/raft/db.go:142-149`) recovers most of the throughput loss by amortising the per-Apply overhead across many concurrent submitters, which is what unlocks the 10k figure on the gRPC path.

Fault tolerance is f=1 with N=3. The failover budget on a healthy LAN is roughly one to three seconds end to end (see [Cluster Level Failover](Concepts-Cluster-Level-Failover)). HTTP clients learn about new leaders via 307 Temporary Redirect; gRPC clients via `ErrNotLeader` and a reconnect through the connection pool. Idempotency across the transition is preserved through the producer-side `Idempotency-Key` and the ghost index — a retry that crosses leader boundaries still produces the same task UUID and the same result, no duplicates.

The configuration is the docker-compose layout at `deploy/docker-compose/raft-cluster/compose.yaml`, summarised: three services running the same image, `RAFT_ENABLED=true` on all three, `RAFT_BOOTSTRAP=true` on only the first, `RAFT_PEERS=node-a=node-a:7000,node-b=node-b:7000,node-c=node-c:7000`, `RAFT_PEER_HTTP_ADDRS` set so the 307 redirect can produce a usable URL. Each node has its own Pebble volume; the raft log lives inside that Pebble (under separate key prefixes), so backups capture both the application state and the raft state in one filesystem snapshot.

When to pick this mode: the deployment cannot tolerate a single-node outage, and the workload fits within the ~10k cycles/s envelope. Typical scenarios are customer-facing pipelines, payment workflows, event sourcing for systems of record where any outage is visible. This is the default recommended mode for production deployments that need fault tolerance.

## Three-node RAFT with multi-shard

The single-shard raft path has one bottleneck: one raft log on the leader. Every write serialises through that log, and at sustained ~10k cycles/s the log append plus AE fanout become the dominant cost. The multi-shard raft mode partitions the keyspace across N raft groups, each with its own log and its own leader. Different keys land in different shards; different shards make progress in parallel.

The wiring is the M2 path in `pkg/app/application_pebble.go:195-297`. Each Pebble shard gets its own raft group, opened via `raftpkg.OpenWithPebble`, with its own bind address and its own peer set. The transport layer is shared across shards via the mux acceptor (`internal/raft/mux_transport.go`): every node opens a single TCP listener on `:7000` and demuxes inbound raft connections by a 4-byte BigEndian group ID prefix. The wire shape is detailed in [Mux Transport](IO-Mux-Transport); the practical consequence is that adding more shards does not require reserving more TCP ports on the host.

The leadership view is per-shard. Node A might be the leader of shard 0 and a follower of shard 1, while node B leads shard 1 and follows shard 0, and node C follows both. A client write for a task whose UUID hashes to shard 0 must reach node A; a write for shard 1 must reach node B. The smart client (or the 307 redirect chain) routes the write to the correct leader per shard. The reaper on each node is gated per-shard: it sweeps shard 0 only when this node leads shard 0, and shard 1 only when this node leads shard 1 (`pkg/app/application_pebble.go:433-441`).

Throughput gains in this mode have not been characterised in the bench harness with the same rigour as the single-shard cases. The conceptual envelope is "single-shard raft throughput times sub-linear-shard-factor" — adding shards lifts the ceiling but the smart client and the fanout cost reduce the multiplier below linear. The practical recommendation is to start with the three-node single-shard mode and only move to multi-shard raft after measuring the single-shard ceiling.

The operational cost is high. Backups must capture all shards on all nodes atomically. Leadership monitoring goes from one /v1/codeq/raft/status entry to N entries. A network partition can leave the cluster in a state where some shards have a leader on the majority side and others on the minority — the minority-side shards stop accepting writes while the majority-side shards continue, which produces a partial-availability profile that is harder to reason about than the all-or-nothing single-shard case.

When to pick this mode: three-node single-shard raft has been measured to be the bottleneck, the workload fits cleanly into independent shards (most queue workloads do — task IDs are independent), and the operations team has the maturity to run a multi-shard cluster. For most deployments this mode is overkill; the single-shard raft mode at ~10k cycles/s is enough.

## The mode comparison at a glance

The numbers below are measured throughput in cycles per second (create + claim + complete) under sustained load on a developer-class loopback. Production numbers depend on disk, network, and workload shape; treat these as relative magnitudes, not absolute predictions.

Single-node Pebble (no shards): 76,639 tasks/s (`internal/bench/profile_full_cycle_test.go`). f=0, no replication. Smallest operational footprint. Default for development and internal pipelines.

Single-node Pebble + 4 shards: roughly 3× single-shard, sub-linear scaling. f=0, no replication. Modest operational complexity. Use when the engine is the bottleneck and HA is not required.

Three-node RAFT (1 shard, HTTP): 3,949 cycles/s (`pkg/app/raft_smart_routing_bench_test.go`). f=1 with N=3. Failover budget 1-3s. HTTP clients use 307 redirect.

Three-node RAFT (1 shard, gRPC): 9-10k cycles/s (`pkg/app/raft_grpc_bench_test.go`). Same f=1 fault model. gRPC clients use connection pool and `ErrNotLeader` for retries. Default production recommendation.

Three-node RAFT + N shards: lifts the single-shard ceiling sub-linearly. Same fault model. Highest operational complexity. Reserve for cases where the single-shard mode is measured to be the bottleneck.

## What NOT to choose: two-node raft

The configuration validator does not reject N=2 (`pkg/config/config.go:662-683`). It cannot distinguish "operator wants two nodes for cost reasons" from "operator made a typo". Operators occasionally pick two anyway, reasoning that two nodes give some redundancy. They do not.

The majority of two is two. With two nodes, a single failure leaves one node, which is a minority of the cluster. The surviving node knows it has lost quorum and refuses writes. The two-node configuration tolerates zero failures. It is strictly worse than a one-node configuration because the operator now has two boxes to maintain, two disks to back up, two processes to monitor, and the same f=0 fault model.

A two-node configuration only makes sense as a transient state during a cluster expansion or contraction — for example, adding a third node to an existing three-node cluster temporarily looks like four nodes, and removing a node from three leaves two. Steady-state operation in N=2 is a misconfiguration. The recommendation is unequivocal: pick one or three, never two. Pick five only if the read patterns specifically benefit from more local replicas, which queue workloads rarely do.

## Compose-time mutual exclusivity

Several configuration knobs are mutually exclusive. `raft.enabled` and `cluster.enabled` cannot both be true (`pkg/config/config.go:662-683`) — they solve different problems with incompatible assumptions. `raft.enabled` and the legacy `sharding.enabled` (per-tenant backend selector for external Redis shards, deprecated for Pebble-only deployments) cannot both be true. `raft.enabled` requires `PersistenceProvider=pebble`; raft does not support any other engine.

The intra-process `numShards` (in the persistence config) does compose with raft, and that combination produces the multi-shard raft mode above. It does not compose with the gRPC cluster router. The matrix of valid combinations:

- single-node Pebble: `raft.enabled=false`, `cluster.enabled=false`, `numShards=1` (or unset). Valid.
- single-node Pebble + shards: `raft.enabled=false`, `cluster.enabled=false`, `numShards=N>1`. Valid.
- gRPC cluster: `raft.enabled=false`, `cluster.enabled=true`, `numShards=1`. Valid. This is the cross-node hash-routing path with no replication; it is documented separately and is not the recommended fault-tolerant mode.
- three-node raft: `raft.enabled=true`, `cluster.enabled=false`, `numShards=1`. Valid.
- three-node raft + shards: `raft.enabled=true`, `cluster.enabled=false`, `numShards=N>1`. Valid.

Anything not on this list is rejected at startup. The application logs the rejection reason and exits non-zero.

## Operational considerations across modes

Backups. Pebble holds an exclusive directory lock; live `cp -r` is unsafe. The supported pattern is filesystem snapshot (LVM, ZFS, EBS snapshot, etc.) which captures the directory atomically without coordinating with the running process. In multi-shard or multi-node modes the snapshot must capture every shard or every node consistently; the easiest way is to snapshot every volume at the same moment from the storage layer.

Restarts. The single-node modes restart instantly — the engine replays its WAL into a fresh memtable, recovers the in-memory lease table from `KeyInprog` rows, recovers the seq counter from the pending range, and is ready. The raft modes have a slightly longer restart because raft replays its log (and possibly installs a snapshot from the leader if the node is far behind). Typical restart latency on a healthy cluster is a few seconds.

Upgrades. The single-node modes upgrade by stopping and starting the process. The raft modes upgrade by rolling: stop one follower, upgrade it, start it, wait for it to rejoin and catch up, repeat for the next follower, and finally upgrade the leader. The leader step triggers an election, which costs the failover budget once per cluster upgrade. The application's binary format guarantees compatibility across patch versions within a minor version; minor-version transitions require checking the release notes for log-format and snapshot-format compatibility.

Observability. All modes expose Prometheus metrics on `:9091/metrics` and OTel traces on the configured exporter. The raft modes additionally expose `/v1/codeq/raft/status` (`internal/controllers/raft_status_controller.go`) which is the canonical source for "where is the leader". The metrics for raft latency and Apply queue depth are essential for tuning the failover and throughput parameters; see [Performance Tuning Knobs](Performance-Tuning-Knobs).

## Decision recipe

Start with single-node Pebble. Measure under the actual workload. If throughput is below 50k tasks/s and the workload tolerates an outage window measured in minutes, stay there. If throughput is the bottleneck and the workload still tolerates outages, add `numShards=4`. If outages are not acceptable, move to three-node raft and accept the throughput tax. If three-node raft is the bottleneck for throughput reasons (not latency reasons — multi-shard does not help tail latency), add `numShards=N` to the raft configuration.

The decision is not symmetric. Going from single-node to three-node raft is a one-way migration: the raft log is initialised at bootstrap time and cannot be retrofitted onto an existing Pebble store with consistent state. Plan for the mode you need before you start ingesting production data, not after.

## See also

- [Architecture Overview](Concepts-Architecture-Overview) for the full wiring picture across modes
- [Cluster Level Failover](Concepts-Cluster-Level-Failover) for what happens in the three-node modes when a leader dies
- [Persistence Engine](Concepts-Persistence-Engine) for what the engine layer does under each mode
- [Performance Single Node Throughput](Performance-Single-Node-Throughput) for the 76k breakdown
- [Performance Cost Of HA](Performance-Cost-Of-HA) for the raft throughput tax
- [Performance Multi Shard Scaling](Performance-Multi-Shard-Scaling) for the shard scaling curve
