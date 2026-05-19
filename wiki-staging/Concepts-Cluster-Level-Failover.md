# Cluster Level Failover

Failover is what happens when the leader of a replicated cluster stops responding. In a non-replicated deployment a leader death is just an outage; clients wait, an operator pages, the box comes back, the service resumes. In a raft-replicated deployment the surviving nodes elect a new leader, the cluster keeps serving, and well-behaved clients reconnect without code changes. The whole sequence is on the order of a few seconds end to end, dominated by raft's election timeout and the client's reconnect path.

This page walks through the sequence in detail. What the surviving followers do when AppendEntries stops arriving. How the election timeout, leader lease, and randomised jitter combine to elect exactly one new leader. How the new leader announces itself through the next AppendEntries. How HTTP clients learn about the new leader through a 307 redirect and how gRPC clients learn through `ErrNotLeader`. The end-to-end timing budget. The worked example uses the docker-compose layout at `deploy/docker-compose/raft-cluster/` because it is the canonical three-node deployment.

## What "the leader died" actually means

Death is detected by the absence of heartbeats. The leader sends an AppendEntries to every follower on the heartbeat cadence (`HeartbeatMS=1000`, `internal/raft/db.go:53-58`). An AppendEntries with no new entries IS the heartbeat — there is no separate ping. Followers watch the time since the last AE; when it exceeds the election timeout (`ElectionMS=1000`, `db.go:60-65`, with raft adding randomised jitter between 1× and 2× the timeout to avoid split-vote storms), the follower transitions to candidate.

"Died" can mean any of several things on the network. The leader process crashed, taking the OS socket with it — followers see TCP RSTs and miss the next heartbeat immediately. The leader machine lost power — followers see TCP timeouts after the kernel's keepalive deadline. The leader is alive but partitioned from the followers — followers stop seeing AEs even though the leader is still trying to send them. The leader is alive and connected but stuck in garbage collection or a long disk fsync — heartbeats queue up behind the stall and arrive in a burst when the stall releases.

All four look the same to raft: an election timeout that elapses without an AE. The algorithm does not distinguish causes; it distinguishes only "did I hear from the leader recently". This is one of the design strengths of raft — there is no single failure mode it specifically handles, only a single failure SHAPE.

## The leader lease

The leader has its own clock to watch. The leader lease (`LeaderLeaseMS=500`, `db.go:67-72`) is the tighter bound on how long a leader trusts its own status. If the leader cannot communicate with a majority of the cluster within the lease window, it steps down voluntarily, even before any follower would have started an election against it.

The lease matters in partition scenarios. Imagine a three-node cluster with nodes A (leader), B, and C. The network drops between A and the other two, but A is otherwise healthy. Without a lease, A would happily continue accepting writes for the full election timeout window — and even then, only learn it had been deposed when B or C eventually rejoined. With the lease, A notices it has not heard a majority's ack within 500ms and steps down, refusing further writes, before the new leader on the {B, C} side even finishes its election. The window where two nodes both think they are the leader shrinks to milliseconds.

## The election

When a follower's election timeout elapses with no AppendEntries, it transitions to candidate, increments its current term, votes for itself, and sends a RequestVote RPC to every other node in the cluster. The other nodes grant a vote if two conditions hold: the candidate's log is at least as up-to-date as their own, and they have not already voted for someone else in this term. A candidate that wins votes from a majority becomes leader and immediately sends an AppendEntries to assert leadership.

Two candidates can race. If two followers time out at almost exactly the same moment they both start an election, both vote for themselves, and neither wins a majority (each gets one self-vote plus possibly one external vote, but not a majority of three). The election fails. Both candidates time out, bump the term again, and retry — at which point raft's randomised election timeout kicks in. Each candidate's next timeout is a fresh random value between 1× and 2× the configured `ElectionMS`. The randomisation makes it overwhelmingly likely that one candidate's timeout fires first on the retry, and that candidate wins cleanly.

In practice elections are rare and fast. A three-node cluster with one healthy leader runs millions of writes between elections. When an election does happen the typical sequence is: leader dies at t=0, followers notice the missing heartbeat around t=1000ms, election starts, votes complete within a single network round-trip on a healthy LAN, new leader sends its first AppendEntries asserting authority, cluster resumes. End to end this is one to three seconds, dominated by the election timeout itself.

The election timeout is a tunable. `db.go:60-65` exposes `ElectionMS` through the config; the default of 1000ms is conservative. A deployment that wants tighter failover (and is willing to tolerate the increased rate of spurious elections under slow networks or GC pauses) can drop it to a few hundred milliseconds. The same trade applies in the other direction: a wide-area deployment with higher latency between nodes should raise the timeout, otherwise transient network blips trigger unnecessary elections.

## The new leader announces itself

A node that wins a majority of votes does not wait for anything. It immediately sends an AppendEntries to every other node, with the new term embedded in the RPC envelope. Followers receiving an AE with a higher term than their own current term update their view, recognise the sender as the new leader, and reset their election timer. The cluster now has one leader, all followers know its address, and the steady-state heartbeat cadence resumes.

The announcement is the AppendEntries itself. There is no separate "I am the leader now" RPC, no broadcast, no name-service update. Raft's contract is that the first AE in a new term implicitly carries authority, and codeQ relies on that. The application layer learns about leadership transitions through `LeaderObservation()` (`db.go:386`), which is a buffered channel that fires `true` when this node becomes leader and `false` when it loses leadership. The reaper subscribes to that channel to gate its sweeps — only the current leader should be writing through `raft.Apply`, and the leader gate prevents followers from doing redundant background work.

The full leader info — the leader's raft ID and bind address — is available locally on every node via `LeaderInfo()` (`db.go:392-398`). The local view can disagree with another node's view for a brief window after a transition: a follower that has not yet received the new leader's first AE still believes the old leader holds the seat. The disagreement window is bounded by the time it takes the new leader to send its first AE to every follower — typically a few milliseconds on a healthy LAN, longer on a partition.

## Client view: HTTP 307 Temporary Redirect

The application layer surfaces leadership to HTTP clients through a 307 redirect. Every write controller checks the returned error and, if it is a `*pebble.NotLeaderError` carrying a leader hint, writes an HTTP 307 with a `Location` header pointing at the leader's HTTP base URL. The implementation is `maybeRedirectLeader` at `internal/controllers/respond.go:34-50`:

```go
func maybeRedirectLeader(c *gin.Context, err error) bool {
    var hint domain.LeaderHint
    if !errors.As(err, &hint) {
        return false
    }
    addr := hint.LeaderHTTPAddr()
    if addr == "" {
        return false
    }
    location := strings.TrimSuffix(addr, "/") + c.Request.URL.RequestURI()
    c.Header("Location", location)
    c.JSON(http.StatusTemporaryRedirect, gin.H{
        "error":  "not leader",
        "leader": addr,
    })
    return true
}
```

The 307 status code (versus the older 302) is deliberate. RFC 7231 specifies that 307 preserves the request method and body across the redirect — a POST stays a POST, the body is replayed verbatim. Go's `http.Client` follows the redirect automatically when the request body supports `Seek` (which `bytes.Reader`, `strings.Reader`, and any request constructed via `GetBody` satisfy). For client libraries that do not auto-follow 307 — older PHP, some JavaScript HTTP libraries — the response body and the `Location` header are both readable so the client can explicitly retry against the leader URL.

The leader's HTTP base URL comes from the configured peer map. `RAFT_PEER_HTTP_ADDRS` in the docker-compose env (lines 62-65 in `raft-cluster/compose.yaml`) maps `node-id=http://host:port` for every peer. The current leader's entry is what `LeaderHTTPAddr()` (`db.go:404-410`) returns. If the map is unconfigured the redirect cannot be issued and the controller falls back to a `400 not leader` error — clients then have to discover the leader through `/v1/codeq/raft/status` or by retrying against a known peer.

## Client view: gRPC and ErrNotLeader

The gRPC path uses a different mechanism. The raft layer's `Replicate` returns a typed `ErrNotLeader` sentinel (`internal/raft/db.go:99-103`) which the engine wraps as a `*pebble.NotLeaderError`. The gRPC server propagates the error to the client; the client library, on receiving `not leader`, looks up the new leader's gRPC address from its connection pool and reconnects. The mechanics of the pool live in `pkg/cluster/`; the contract from the user-facing side is "the client transparently retries against the new leader".

The gRPC path has a tighter retry loop than HTTP because the client maintains long-lived connections to every peer and can reroute without paying for a fresh TCP handshake. This is part of why the gRPC bench (`pkg/app/raft_grpc_bench_test.go`) shows higher cycle throughput on a three-node loopback cluster (9-10k cycles/s) than the HTTP smart-routing bench (`pkg/app/raft_smart_routing_bench_test.go`, 3.9k cycles/s) — fewer HTTP framings, no per-request `Location` lookup, no DNS round-trips on retry.

## The status endpoint

The raft status endpoint at `GET /v1/codeq/raft/status` (`internal/controllers/raft_status_controller.go`) returns the local node's view of every raft group it participates in. The response includes per-group: shard index, whether this node is the leader, the local node's raft ID and bind address, the current leader's ID and address, and the leader's HTTP URL when known. Operators use the endpoint as the source of truth during failover — hit it on any node and the response tells you where the writes are going.

The endpoint always returns 200; the payload is the truth. A node with no raft groups answers `{"enabled": false, "numGroups": 0}` — useful for blue/green and migration scenarios where the same binary runs in both raft-enabled and non-raft modes. A node mid-election (no current leader) answers `hasLeader: false` for each group; clients that poll the endpoint can detect election-in-progress directly.

The status endpoint is also what client-side smart routers query at startup to discover the initial leader, before they have observed any 307 or ErrNotLeader. The HTTP smart router in `pkg/app/raft_smart_routing_bench_test.go` does exactly this: hit `/v1/codeq/raft/status` on any node, find the leader, send writes there, fall back to retry-on-307 if the leader changes during a session.

## End-to-end timing budget

The failover budget breaks down roughly as follows on a three-node cluster on healthy LAN:

t=0: leader process dies (or its network drops). Followers continue running with no new writes.

t=0-1000ms: heartbeat-miss window. Followers are waiting out the election timeout. The leader lease on the dying side (if it was a partition rather than a crash) expires around t=500ms and that side stops accepting writes.

t=1000-1500ms: election round. Whichever follower's randomised election timer fires first becomes candidate, requests votes, wins, sends the first AE. With low network latency the election completes in tens of milliseconds; the variance is in when the randomised timer fires.

t=1500-2000ms: client recovery. The first write attempt against the dead leader from a steady-state HTTP client fails — either TCP reset (process crash) or timeout (partition). The client retries; the retry either hits the new leader directly (if the client was sticky to a different node) or hits a follower and gets a 307 to the new leader. Either way the second attempt succeeds. gRPC clients with a healthy connection pool typically recover faster, often inside the election round itself.

End-to-end: roughly one to three seconds from leader death to first successful client write against the new leader. The cluster is back to full throughput within the same window — the new leader's apply coalescer is already accepting submissions on the moment it transitions, and `applyLoop` does not need to warm up.

This budget assumes `HeartbeatMS=1000` and `ElectionMS=1000`. Deployments with tighter timeouts (300ms heartbeat, 300ms election) cut the budget to roughly 600ms-2s; deployments with wider WAN-tuned timeouts (5s heartbeat, 5s election) stretch it to 10-30s. The trade is between failover latency and tolerance for transient network issues.

## Walking through the docker-compose layout

The `deploy/docker-compose/raft-cluster/compose.yaml` layout is the canonical three-node setup and a useful concrete example. Three services — `node-a`, `node-b`, `node-c` — each running the same image, each exposing HTTP on a different host port (8080, 8081, 8082) but the same container-internal port 7000 for raft. `RAFT_BOOTSTRAP=true` is set only on `node-a`; the other two start as followers and join the cluster on first start.

Cluster formation runs as follows. `node-a` starts, finds no existing raft state, runs `BootstrapCluster` (`db.go:256-278`) with the configured peer list, and elects itself leader on a single-node majority. `node-b` and `node-c` start in non-bootstrap mode; their raft state is empty, but `node-a`'s configuration entry is replicated to them on the next AE. After that the three-node configuration is the persisted truth, the bootstrap flag is ignored on subsequent restarts, and the cluster runs as a three-node f=1 raft.

To simulate failover: kill `node-a`. The compose layout maps each node's HTTP port to a distinct host port, so writes can be aimed at any node from outside the docker network. A write aimed at `node-a` (host port 8080) fails immediately with a connection refused once the container is down. A write aimed at `node-b` (host port 8081), which is now a follower under the elected leader `node-c` (or whichever node won the election), returns a 307 with `Location: http://node-c:8080/...` — except that the Location URL points at the docker network's internal hostname, which is not reachable from the host. This is the operational caveat of running the compose layout: the internal HTTP URLs only resolve inside the docker network. Production deployments configure `RAFT_PEER_HTTP_ADDRS` with externally-resolvable hostnames or load-balancer URLs so the 307 works for external clients.

A more realistic test starts a worker container inside the same `raft` docker network. The worker resolves `node-b` via docker DNS, hits port 8080 on the container, gets a 307 to `http://node-c:8080`, follows the redirect, and the next request lands on the leader. Elapsed time from `docker kill codeq-raft-node-a` to a successful claim on node-c is typically two to three seconds on a developer laptop.

## What can go wrong

Three things to be aware of operationally.

First, an N=2 cluster cannot fail over. With two nodes the majority is two; one failure leaves one node, which is a minority of itself. The remaining node knows it has lost quorum and refuses writes. This is correct raft behaviour — the alternative would be to allow a partitioned single node to keep accepting writes, which is exactly the split-brain scenario raft is designed to prevent. The configuration validator does not reject N=2 (it cannot distinguish "the operator wants two for cost reasons" from "the operator made a typo"), so the responsibility lands on whoever picks the deployment topology. `Deployment Modes` calls this out explicitly: pick one node or three, never two.

Second, an election storm under chronic network flakiness can stall the cluster. If the network drops AEs frequently enough that elections fire on every cycle, the cluster spends more time electing than committing, and effective throughput collapses. The fix is to raise the election timeout above the typical drop-recovery time, or to fix the network. There is no algorithmic mitigation; raft assumes the underlying network is good enough most of the time.

Third, snapshot install latency on a far-behind follower. A node that has been down long enough to fall behind the leader's snapshot threshold (8192 log entries by default, or about two minutes at moderate write rates) needs an InstallSnapshot RPC to catch up. The snapshot stream is the entire codeq/ keyspace, which can be hundreds of megabytes on a busy cluster. During the install, the follower does not participate in elections and is effectively absent from the cluster. With N=3 and the leader plus one healthy follower the quorum still holds, but a second failure during the install window stalls the cluster. This is rare in practice but worth knowing about during prolonged maintenance windows.

## Idempotency under failover

Producers and workers retry, and retries can land on a different leader than the original request. This is normal; the question is whether the second attempt produces a duplicate task. codeQ handles this with the per-task UUID and the ghost index. When a producer submits with an `Idempotency-Key`, the server hashes it into the task UUID and writes a small ghost record before the full task row. On retry, the server hits the ghost, returns the cached UUID, and refuses to create a second task. The ghost lookup is bloom-filtered, so the hot path costs roughly one bit test on the new leader.

The retry crossing a leader transition is the case the ghost is most useful for. Without it, a worker that submitted a result, hit a transient timeout during the leader's death, and retried against the new leader could end up double-submitting and incrementing the attempt counter twice. The ghost makes the retry idempotent by content: same content, same UUID, same result. The mechanism is covered in detail in [Tasks And Results](Concepts-Tasks-And-Results); failover just turns it from a nice-to-have into a hard requirement.

## See also

- [Consensus And Replication](Concepts-Consensus-And-Replication) for the algorithm and the FSM
- [Deployment Modes](Concepts-Deployment-Modes) for why three nodes, not two
- [IO Raft Replication](IO-Raft-Replication) for the wire format
- [REST API](IO-REST-API) for the 307 redirect on every write endpoint
- [Architecture Overview](Concepts-Architecture-Overview) for the full request-flow picture
