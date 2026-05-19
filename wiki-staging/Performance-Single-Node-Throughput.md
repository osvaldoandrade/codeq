# Single-Node Throughput

The single-node ceiling for codeQ is roughly seventy-six thousand full task cycles per second on a twelve-core Linux box. The reading from the bench used as the canonical reference is 76,639 tasks/s, measured by `internal/bench/profile_full_cycle_test.go::TestProfile_FullCycle` in a twenty-second window with thirty-two producer goroutines pumping creates over one bidirectional gRPC stream and one hundred twenty-eight worker slots draining the queue over a second bidirectional stream. The deployment is one codeq process, one Pebble directory, one shard, `NoSync` mode (no per-commit fsync). This page explains what that number measures, how it composes, what makes it move, and what it does not include.

## What the cycle actually is

A "task cycle" in this bench is three operations chained: a producer sends `Produce(CreateRequest{Command: "GENERATE_MASTER", Payload: ...})` over its stream session; the server enqueues the task into the pending priority bucket on Pebble; a worker slot pulls the task via its `Ready` frame; the server hands back a `Task`; the worker handler runs (a no-op that just returns `workerclient.Completed(nil)`); the worker submits the result; the server marks the task done and frees the lease. Each of those three pieces — create, claim, complete — touches Pebble. Each goes through the group-commit coalescer at `internal/repository/pebble/db.go:71-82`.

The bench counters increment once per `created` and once per `completed`. The reported throughput is the smaller of the two divided by elapsed seconds, because the system cannot complete what has not been created. In the canonical run the two numbers are nearly equal — the worker drain rate keeps up with producer creation rate, which is the regime in which the bench is informative. If `completed` falls significantly behind `created`, the bench is measuring queue growth, not throughput, and the run should be discarded.

The producer side runs `startProducerLoad(t, prodSess, ctx, 32)` at `internal/bench/profile_full_cycle_test.go:124`, which spawns thirty-two goroutines each calling `sess.Produce` in a tight loop until the measurement context cancels. The worker side runs `startWorkerLoad(t, workerCli, ctx, 128)`, which calls `cli.Run` with a counting handler; `workerclient.Config.Concurrency` is set to 128, so the client itself fans out across one hundred twenty-eight in-flight slots inside the single stream session. Every operation goes through the same connection.

## Why the gRPC stream path is the published number

The first version of this bench used HTTP REST. Each create was a POST to `/v1/codeq/tasks`, each claim a POST to `/v1/codeq/tasks/claim`, each completion a POST to `/v1/codeq/tasks/{id}/result`. The measurement landed at roughly three to four thousand cycles per second on the same hardware, and the bench is preserved at `pkg/app/raft_smart_routing_bench_test.go` for the RAFT comparison.

The mutex profile on the client side showed why. Twenty-eight point seven four percent of contention sat inside `http.Transport.tryPutIdleConn` — the Go standard library's idle-connection pool lock. Every cycle was three HTTP requests, every request acquired the idle pool lock to either reuse a connection or return one. At thirty-two concurrent goroutines the lock saturated. The server side was nowhere near its ceiling.

The fix was the bidirectional gRPC streams. The producer stream (`pkg/app/producer_stream.go`) and the worker stream (`pkg/app/worker_stream.go`) each open one persistent HTTP/2 connection per session. Concurrent `Produce` calls multiplex frames over that connection — gRPC's HTTP/2 transport handles concurrent streams natively without per-call connection-pool churn. The same workload, same hardware, same Pebble, jumped from about four thousand cycles per second to about seventy-six thousand. The bottleneck moved from the client's HTTP transport into the server's Pebble commit path, which is where it should be.

This is why the published single-node number uses the gRPC stream bench. The HTTP REST number is preserved for documenting the smart-routing path in `Performance-Cost-Of-HA`, not as a representative single-node throughput.

## What `fsyncOnCommit` does to the number

The Pebble `DB` is opened with `Options{FsyncOnCommit: false}` by default (`internal/repository/pebble/db.go:135-138`). The bench inherits that. Every commit goes through Pebble's WAL and memtable, but the WAL fsync is deferred — Pebble's `NoSync` write option lets the kernel flush at its own pace.

Flipping `fsyncOnCommit` to true forces an `fsync(2)` on the WAL file at the end of every coalesced commit. The cost depends on the storage hardware. On NVMe with a battery-backed cache the cost is bounded; on cheap consumer SSDs or rotational disks the cost is dominated by the device's commit latency. The bench has not been run end-to-end with `FsyncOnCommit=true` on the reference hardware, so this page does not publish a number — but the order-of-magnitude expectation, consistent with the operator-facing guidance in `docs/41-deployment-modes.md`, is a single-digit-percentage-of-baseline outcome on the worst storage and a ten-to-thirty-percent drop on a decent NVMe. The coalescer amortizes the fsync over the merged batch (one fsync per coalesced commit, not one per submitter) which is the only reason the cost is not catastrophic.

The trade is explicit. `fsyncOnCommit=false` means a kernel panic between commit-return and fsync can lose roughly a millisecond of writes. `fsyncOnCommit=true` means no writes are lost across a kernel panic but every commit pays the device's fsync latency. Pick the side appropriate to the workload. Most codeQ deployments pick `false` because tasks already carry an at-least-once guarantee: a producer that does not get an ack will retry, and a worker that crashes mid-task will see the task re-delivered after the lease expires.

## Where the time actually goes

The `TestProfile_FullCycle` bench writes pprof profiles to `/tmp/codeq-profiles/` after the measurement window: a CPU profile, an allocation profile, a heap profile, a block profile, a mutex profile, and a goroutine snapshot. Reading them with `go tool pprof -top -cum` gives the runtime decomposition.

At seventy-six thousand cycles per second the CPU profile distributes broadly: protobuf encode and decode for the stream frames, sonic JSON decode for the task payloads (the system uses `bytedance/sonic` instead of `encoding/json` on hot paths — see `internal/bench/sonic_bench_test.go`), Pebble batch construction and commit, FNV-1a hashing for shard routing on the producer side, and the workerclient slot's `Ready` / `Result` ping-pong logic. No single function dominates above roughly fifteen percent of CPU; the bench is broadly CPU-bound but not stuck on any one thing.

The mutex profile is the more interesting one. Post-coalescer, the Pebble `commitPipeline` mutex that used to be ninety-six percent of contention sits at a small fraction. The remaining contention scatters across stream send buffers, the lease table's atomic counters, and the GC's scan barriers. There is no obvious next bottleneck visible to the bench; whatever is next will surface only at a higher load than this hardware can drive.

The block profile shows where goroutines wait. Most blocking is on the bench's own channels: producer goroutines waiting for the stream session to accept a Send, worker slots waiting for `Ready` ack frames, the worker handler returning into the slot manager. These are not pathologies — they are the natural shape of a backpressured streaming system. The fact that they show up in the profile and the commit pipeline does not is itself the signal that the storage layer has stopped being the bottleneck.

## Why thirty-two producers and one hundred twenty-eight workers

The producer goroutine count and the worker slot count are not arbitrary. They are tuned for the coalescer.

The Pebble coalescer's job is to merge concurrent batches into one. The merge cap is `maxMergeBatch = 64` (`db.go:122`), and the queue depth is `commitChanBuf = 1024` (`db.go:128`). Fewer than four concurrent submitters and the coalescer rarely has more than one batch to merge — every commit goes through with a batch size of one and the coalescer is a no-op overhead. Sixty-four or more concurrent submitters and the coalescer hits its cap on every cycle, after which extra concurrency does not help. Thirty-two producers plus the worker slots' commit pressure puts the coalescer comfortably in the middle of its working range, where each merged commit averages several dozen sub-batches.

The worker concurrency of one hundred twenty-eight is sized to overdraw the producer: at thirty-two producer goroutines, the steady-state queue depth is small and a worker count of one hundred twenty-eight ensures every newly enqueued task is claimable within a single coalescer commit cycle. If the worker count were lower than the producer concurrency, the bench would measure queue growth, not throughput.

This is why dropping either knob in isolation does not necessarily improve the number. The bench measures the joint capacity of the two coalescers (one for create batches, one for complete batches) running with enough offered load to saturate them. Under-driving either side is what makes the bench informative rather than synthetic.

## What the number does not include

Several things matter operationally that this bench does not measure.

It does not measure cold-start throughput. The bench warms up for two seconds (`internal/bench/profile_full_cycle_test.go:103`), and the measurement window starts after the warmup. In a fresh process, the first few hundred milliseconds include Pebble compaction kicking off, the block cache filling, and the worker stream session establishing its first batch of slots. None of that appears in the published number.

It does not measure network round-trip time. The bench runs producer and worker against a loopback gRPC server in the same OS instance. There is no NIC, no switch, no DNS, no TLS handshake. A real client across a real network adds the link's RTT to every cycle. For LAN-local clients on a fast link, the additional latency is small enough to not move the throughput floor; for clients across an internet boundary, it does.

It does not measure long-tail GC. Twenty seconds is too short to catch a multi-hundred-megabyte heap-growth GC. The reference hardware sits comfortably under a hundred megabytes of resident set during the bench, but a production deployment with larger task payloads could see GC tail latency that the bench cannot.

It does not measure read throughput. Every operation in the cycle is a write or a write-equivalent (claim writes the lease, complete writes the final state). The bench has no read-heavy workload simulating list endpoints or DLQ scans. Read throughput is bounded by Pebble's iterator performance, which is a different story than the commit-pipeline story this bench tells.

It does not include the rate-limiter middleware, the tenant authorization check, or the audit log. The bench runs against the dev token configuration with rate limits at zero. A production deployment with realistic rate-limiter buckets and JWT validation against an external JWKS endpoint pays additional CPU per request. The cost is bounded — the cycle is still dominated by the storage commit — but it is not zero, and the bench does not include it.

## How to read a result you measure yourself

If you run the bench on different hardware, the number you should expect to see depends mostly on three things: CPU core count, disk fsync latency (if `fsyncOnCommit=true`), and the OS scheduler. Twelve cores at NoSync gets to roughly seventy-five thousand cycles per second; eight cores at NoSync sits closer to fifty thousand; four cores at NoSync sits around twenty-five to thirty thousand. The scaling is sub-linear because the bench is not purely CPU-bound — the LSM commit path has serial sections per shard.

Variance run-to-run is typically within five percent on a quiet box. Under load, on a shared host, or on WSL2, variance climbs to twenty percent or more. The discipline is the same as for any throughput benchmark: run it three to five times, take the median, and treat single-run numbers with suspicion. The WSL2 environment used to write this page has been observed exhibiting fifty-minute clock jumps inside an eight-second window — not an edit to the bench, real WSL2 behavior — which is the strongest argument for taking medians and ignoring outliers.

## Where to go next

For the cost of adding replication, see [Performance Cost Of HA](Performance-Cost-Of-HA). For multi-shard scaling inside one process, see [Performance Multi Shard Scaling](Performance-Multi-Shard-Scaling). For the producer/worker stream API surfaces that make this throughput possible, see [SDKs Overview](SDKs-Overview) and [SDKs Producer](SDKs-Producer). For the storage engine in depth, see [Pebble Storage](Pebble-Storage) and [Pebble Sharding](Pebble-Sharding).
