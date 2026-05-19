# The Cost of HA

Turning on three-node RAFT replication drops codeQ's full-cycle throughput from roughly seventy-six thousand tasks per second to roughly nine or ten thousand. That is an order of magnitude, and it is the price of fault tolerance. This page explains every component of that drop, what part of it the Apply coalescer recovers, what part is intrinsic to the consensus protocol, and what part is a property of the bench environment rather than the system itself. The published numbers come from `pkg/app/raft_grpc_bench_test.go::TestRaftBench_GRPC` and `pkg/app/raft_smart_routing_bench_test.go::TestRaftBench_SmartRouting`. Both run a three-node cluster on a single WSL2 host bound to loopback. Both are reproducible.

## The numbers

The gRPC stream path against three-node RAFT measures nine to ten thousand cycles per second at steady state. The exact reading from a representative run was 9,400 cycles per second on a single shard and 10,200 cycles per second on four shards — both inside the noise range of the bench, so the cleaner statement is "nine to ten thousand." Under optimal cache and CPU conditions, peaks have been observed near twenty thousand cycles per second, but those peaks do not survive multiple runs. The steady-state floor is what this page treats as the published number.

The HTTP REST path against the same three-node RAFT cluster measures 3,949 cycles per second on a single shard and 3,883 cycles per second on four shards (`pkg/app/raft_smart_routing_bench_test.go`). The four-shard number does not improve over the single-shard number because the bottleneck is not in storage — it is in the client's `http.Transport` idle-pool mutex. The HTTP bench is preserved as the smart-routing reference (it exercises the 307-redirect path that REST clients use to find the leader), not as a representative throughput.

Comparing single-node Pebble at seventy-six thousand cycles per second to three-node RAFT at ten thousand: a factor of roughly seven point five. That factor is the answer to "what does HA cost?" — on this hardware, in this environment.

## Where the factor comes from

Each component of the gap is identifiable, and the relative weight of each is what determines the order-of-magnitude shape.

The first component is the consensus round-trip. Every write that lands at the leader gets logged locally and dispatched to the followers via `AppendEntries`. The leader must hear acknowledgement from a majority — two of three nodes including itself — before it can commit. That is one network round-trip per write. On loopback the round-trip is a kernel TCP loopback path (no NIC, no DMA, no serialization wire delay) which is on the order of tens of microseconds. On a real LAN it would be hundreds of microseconds. On a WAN it would be milliseconds. The bench measures the loopback case, so the consensus cost is the lower bound, not an upper bound.

The second component is the FSM apply. Once the leader's `AppendEntries` has been quorum-acknowledged, the entry commits and every replica applies it to its own Pebble FSM. That is three Pebble commits per logical operation instead of one. Each replica goes through its own `commitPipeline`, its own coalescer, its own WAL append, its own memtable insert. The Apply coalescer at `internal/raft/db.go:125-149` (constant `raftMergeBatch = 128`, `db.go:149`) amortizes the per-Replicate fixed cost across N concurrent submitters — the leader merges up to one hundred twenty-eight outstanding `Replicate` calls into a single raft submission, the followers apply the merged log entries in one FSM trip, and the result is that three Pebble commits per operation become three Pebble commits per merged group of operations.

The third component is loopback CPU contention. Three codeq processes on the same twelve-core host each run the full server stack: HTTP listener, gRPC stream listeners, the RAFT transport listener, the Pebble compaction workers, the lease reaper, and a fan-out of bench client goroutines hammering all three. The scheduler is dividing twelve cores across three processes plus the bench goroutines, and the cache footprint of three independent Pebble block caches plus three RAFT log stores plus three sets of goroutine stacks exceeds the L3 of the test box. The CPU profile of the bench shows kernel time substantially higher than it does for single-node, which is the loopback-and-context-switch tax.

The fourth component is disk contention. All three Pebble directories live on the same physical disk under the WSL2 file system overlay. Three concurrent commit pipelines hitting the same disk serialize at the device queue. On a real cluster each node would have its own disk; on this bench they share one, and the shared device latency is part of the published number.

The Apply coalescer mitigates the first two components — it cuts the per-Replicate overhead by a factor of roughly thirty to fifty percent depending on the bench load. It cannot help the third or fourth, because those are properties of the host, not of codeQ. The published "nine to ten thousand cycles per second" includes the coalescer; without it, the same bench measures correspondingly lower.

## Why the Apply coalescer exists

The original RAFT implementation routed every `Replicate` call through `raft.Apply` directly. Each apply submitted one log entry, waited for quorum, and the FSM applied that one entry. At any given moment, only one entry was in flight at the leader's apply pipeline, so the leader's throughput was bounded by one apply per round-trip.

Profile data at the time showed the same shape as the original Pebble coalescer problem: fixed cost per operation, no amortization across concurrent operations, mutex profile dominated by the apply pipeline's serial section. The fix mirrored the Pebble solution. `internal/raft/db.go:594` describes the mechanism: concurrent `Replicate` calls queue on an `applyCh` channel, the apply loop pulls them off and merges up to `raftMergeBatch=128` into a single concatenated batch, and the merged batch goes through one `raft.Apply` call. The followers see one log entry whose payload is a concatenation of many sub-batches, and their FSM unpacks and applies them all in one Pebble commit.

The effect on the bench: the same number of round-trips per second moves more tasks per round-trip. On a thirty-percent recovery the bench moves from about seven thousand cycles per second baseline to ten thousand with the coalescer; on a fifty-percent recovery it moves from about six-and-a-half thousand to ten thousand. The exact recovery depends on offered load: very low concurrency does not fill the merge window, very high concurrency hits the merge cap. The bench at one hundred twenty-eight concurrent workers across three nodes hits the merge window comfortably.

The trade-off is tail latency. A `Replicate` caller that arrives just after the loop has dispatched a merge waits up to one full merge cycle before its batch goes. With a merge cap of one hundred twenty-eight and a steady-state merge rate of several hundred per second, that wait is single-digit milliseconds at peak — within the same order of magnitude as the raft round-trip itself. The tail latency added by the coalescer is small compared to the consensus latency it amortizes against.

## What the bench environment distorts

The reader looking at "nine to ten thousand cycles per second" and trying to extrapolate to a production deployment should know exactly what the loopback bench is and is not.

Loopback is faster than a real network in latency. A TCP round-trip on the loopback interface bypasses the NIC, the driver's interrupt handling, the cable, the switch, and the receiving side's IRQ. It is mostly memory copies plus a kernel context switch. A real one-gigabit LAN round-trip adds tens to a hundred microseconds. A real ten-gigabit LAN adds less. A WAN adds milliseconds. So the raft consensus round-trip is shorter on loopback than it would be on any real link.

Loopback is slower than a real network in CPU. The single-host bench has three codeq processes plus a bench driver plus the OS competing for twelve cores. A real three-node cluster has three machines with twelve cores each, no inter-process scheduler contention, and no shared L3 cache pressure. The CPU available per node is roughly three times higher in production.

The two distortions cut in opposite directions. The fair statement is: a three-host deployment on a LAN with separate disks would probably measure higher than the loopback bench, perhaps two to four times higher, because the CPU win exceeds the network loss on a fast link. A three-host deployment on a WAN would measure lower. Neither has been measured by us, so neither is published as a number — only as a directional expectation. The bench number you can rely on is the loopback floor.

The WSL2 file system overlay adds another distortion. WSL2's 9P file system bridging the Windows host's NTFS is measurably slower than native ext4 on Linux; the loopback bench has been observed to vary by ten or twenty percent depending on whether the Pebble directory sits on the WSL2 ext4 mount or the bridged 9P mount. The reference number uses ext4. Operators reproducing the bench on bare-metal Linux should expect numbers at or above the WSL2 measurement.

## Why HTTP REST does not scale past four thousand

The smart-routing HTTP bench (`pkg/app/raft_smart_routing_bench_test.go`) measures roughly four thousand cycles per second on the same three-node cluster. The four-shard variant of the same bench does not improve. Both observations have the same root cause: the bottleneck is on the client.

The mutex profile from this bench, on the client side, showed 28.74% of contention inside `http.Transport.tryPutIdleConn`. Every request was a fresh `http.Client.Do` call, every call acquired the transport's idle-connection-list mutex to either pull a connection out of the pool or put one back. Thirty-two concurrent goroutines saturated that mutex. The server was nowhere near its ceiling — single-shard or four-shard, the server side had spare capacity. The client's HTTP transport was the wall.

This is a property of the Go standard library's connection pool, not of codeQ. Any HTTP client with the same concurrency would hit the same lock. The fix on the client side would be to shard the `http.Client` across multiple `http.Transport` instances or to switch transports entirely. The fix on the protocol side, which is what codeQ took, is to move the high-throughput paths off HTTP REST and onto bidirectional gRPC streams: one persistent HTTP/2 connection per session, no idle-pool churn, concurrent calls multiplex frames over the same connection.

The HTTP REST bench is kept as a reference for the smart-routing path because some clients — `curl`, scripts, third-party tools — will continue to use REST. Those clients are not throughput-critical. Throughput-critical clients use the SDK, which uses gRPC streams. The published throughput number for RAFT uses the gRPC bench accordingly.

## What this tells you about deployment choice

The order-of-magnitude difference between single-node Pebble and three-node RAFT is the central trade-off in choosing a codeQ deployment. The decision should be driven by failure tolerance requirements, not by throughput optimization.

If the workload can tolerate the rare unavailability of a single Pebble process during restart, single-node multi-shard is the throughput-optimal choice. Recovery from process restart is fast — the lease table is rebuilt from the `KeyInprog` scan at Open — but it is not instantaneous, and during the restart window the queue is unavailable. For most internal workloads this is acceptable.

If the workload requires the queue to survive the failure of any single machine, three-node RAFT is the correct choice. The throughput floor is roughly ten thousand cycles per second on loopback, probably higher on a real LAN, definitely high enough for most workloads. The trade is the order-of-magnitude drop versus single-node and the operational overhead of running three machines instead of one.

If the workload genuinely needs more than ten thousand cycles per second and also needs HA, the answer is not currently a single codeQ cluster. It is multi-cluster routing at the application layer with each cluster running RAFT for its own data. That deployment shape is not documented here because it is outside the scope of the bench data we have.

The decision matrix in `docs/41-deployment-modes.md` lays this out as a table. Most operators land on three-node RAFT for production and single-node Pebble for development. The bench numbers in this page are what makes that decision concrete.

## Where to go next

For the single-node baseline this page compares against, see [Performance Single Node Throughput](Performance-Single-Node-Throughput). For the within-process scaling story that applies orthogonally to both single-node and RAFT, see [Performance Multi Shard Scaling](Performance-Multi-Shard-Scaling). For the operational knobs that control RAFT behavior (heartbeat, election, apply timeout), see [Performance Tuning Knobs](Performance-Tuning-Knobs). For the RAFT protocol itself, see [Architecture Raft](Architecture-Raft) and [Architecture Replication](Architecture-Replication).
