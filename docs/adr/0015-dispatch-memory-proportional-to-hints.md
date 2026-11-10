# ADR 0015: Leader dispatch memory proportional to queued hints (CFP-070)

- **Status**: Proposed
- **Date**: 2026-11-09
- **Deciders**: Osvaldo Andrade (CFP-070 coordinator request)
- **Amends**: nothing on the wire; process-local dispatch index, reaper
  scheduling and observability only
- **Ships in**: the CodeQ release after v0.5.13
- **Revised**: 2026-11-10 after the architecture and release reviews: a
  queue deeper than the hint cap no longer fails the rebuild (P2-2), a
  leader stuck before its rebuild warns (P2-1), the footprint sample no
  longer takes the dispatch lock (P3-3), the GC share excludes idle marking
  (P3-5), and the roll guidance and memory bound are restated (P3-1, P3-6).
  Revised again on 2026-11-10 after the second architecture review: the
  rebuild raises the sequence counter past every pending key, refused ones
  included (F1); the dispatch-lock hold of a refill or rebuild is bounded and
  documented (F2); overflow under failover, lease expiry, delayed moves and
  absolute expiry is tested (F3); and rollback has a precondition (F4).

## Context

On 2026-11-08/09 two Conveste MASTER CodeQ leaders ran away. Every voter had a
1 CPU / 512 MiB limit, and the logs reported `go soft limit=375809638`
(358 MiB) and a Pebble worst case of 124 MiB.

- `codeq-0` (pod `…4t2pj`) led from 20:59 UTC. After Persistent Logs went to
  standby at 21:25 it used 590 m CPU all night, slowly falling to 520–535 m,
  with no Logs traffic. Its followers sat at 5–9 m CPU and 53–57 MiB, and
  fewer than 8,192 Raft entries were committed between 21:21 and 08:51. At
  02:33 it stepped down after 5 s without contacting either follower, then
  won the next election.
- `codeq-0` (pod `…vll5h`) led from 08:53. It used 406 m CPU before Logs
  returned at 09:40, and was then pinned at its 1000 m limit from 10:30 to
  19:30. Its working set grew from 232 to 410 MiB. It committed about 1.3
  Raft entries/s, against about 44/s for the healthy leader that followed.
  40–66% of Logs publishes timed out, and the pod was OOMKilled at 20:16:17.
- `codeq-2` (`…nws85`) then took over and stayed healthy (85–256 m CPU,
  about 130 MiB).

The leaders logged nothing at INFO or WARN apart from Raft lines. The only
reaper warning in 24 hours was one benign `reap requeue failed: task_expired`
at 18:03. That refutes a reaper that keeps re-reading failing lease
entries: every such failure path logs a WARN every tick.

The cause is the leader-only claim fast path. Claims run only on the leader;
followers forward them. Every claim walks priorities 9→0 for every command it
names. Before this change, each walk created, for every
`(command, tenant, priority)` it touched, a `chan pendingHint` buffered to
100,000 slots: 2.4 MB of heap that holds pointers, created even for empty
priorities and never freed. One claim per topic therefore retained 24 MB.
Conveste has 13 `QueueTopic`s (12 for tenant `conveste` plus `cf-logs` for
`local-tenant`), and Logs also claims `cf-logs` for `conveste`.

`TestClaimProbesRetainNoDispatchMemory` measures 297 MiB retained by one
empty claim on each of 13 topics on v0.5.13. With Pebble's block cache (76 MiB,
Go heap because CodeQ builds with `CGO_ENABLED=0`) the live heap passes the
358 MiB soft limit. From then on:

- the GC runs back to back, capped at half of GOMAXPROCS (2 for a 1-CPU
  limit), i.e. the whole quota;
- each cycle scans every buffer, because the buffers hold pointers;
- the mutator gets what is left, so Raft commits, `absoluteMu` holders and
  requests crawl.

The channel pages were mostly untouched, so the working set stayed close to a
healthy leader's while the Go-accounted heap sat at the limit. Under traffic,
waiting requests pushed the live heap past the soft limit until the kernel
killed the pod. A scratch reproduction (130 such channels plus a 76 MiB cache
under a 358 MiB limit and GOMAXPROCS 2) spent 0.54 cores in GC at 5 MB/s of
allocation. The same program with 100 channels spent 0.01 cores. It is a
cliff, so a leader holding a few topics fewer stays healthy. That fits a
healthy leader and two failing ones serving the same installation.

Two more defects surfaced in the same investigation:

- **Stale-state sweeps.** The reaper swept whenever `IsLeader()` was true.
  A voter elected right after its own restart still holds the snapshot it
  restored, older than the committed log it is about to apply. The
  election-time dispatch rebuild (`Barrier` with a 2 s timeout) can fail,
  and nothing retries it on a leader that receives no request. Its first
  sweeps then computed requeues and retirements from a stale lease table
  and stale rows.
- **No telemetry.** Conveste scrapes no CodeQ metrics, and none of this left
  a log line.
- **Crash loop on a deep queue** (pre-existing). A rebuild that met more than
  100,000 pending tasks in one `(command, tenant, priority)` failed. At
  startup the constructor panicked, so every voter would crash-loop; on a
  running leader every claim failed, and with the reaper gate below, lease
  and TTL sweeps would stop too. A publish refused at the cap was recovered
  only by the next rebuild.

## Decision

- **Hint queues.** The claim index keeps one FIFO per
  `(command, tenant, priority)` (`hintQueue`, a slice with a consumed-prefix
  index) under `TaskRepository.dispatchMu`, instead of a buffered channel.
  - A queue exists only while it holds hints. It is created by the first
    publish, released when it drains, and dropped by the leadership rebuild.
  - Probing a priority never allocates, so dispatch memory is proportional
    to the hints held: about 24 bytes plus the id per hint.
  - The 100,000-hint cap per queue is kept (`maxHintsPerQueue`). A hint
    refused at the cap, by a publish or by a rebuild, keeps its pending key
    durable and marks the queue **overflowed**. An overflowed queue accepts
    no new hint, so tasks published meanwhile wait behind the refused ones
    and FIFO order stays the order of the pending keys.
  - A claim (single or batch) that finds an overflowed queue empty refills
    it from Pebble: it scans that tuple's pending keys in order, skips tasks
    in flight, and queues up to the cap. The scan therefore re-reads only the
    in-flight keys before it finds new work, a bounded cost per cap's worth
    of claims. The queue leaves the overflow set when a refill reaches the end
    of its keys; a storage error keeps it there for the next claim.
  - A rebuild or restart over a deeper queue seeds its oldest 100,000 tasks
    and marks it overflowed; it never fails and the constructor never panics
    for it. A rebuild that marks queues logs one
    `event=codeq_hint_queue_overflow` WARN with the number of queues and the
    cap (no command or tenant).
  - The rebuild raises the pending-sequence counter (`RaiseSeq`) for every
    pending key it scans, before the cap decides whether the key gets a hint.
    A voter's counter is seeded only when it opens its store; a follower that
    wins an election has enqueued nothing since, so the rebuild is what moves
    its counter past the committed backlog. The keys the cap refuses are the
    newest, highest sequences: raising only for queued hints let a new leader
    number its next task inside the backlog, where it was claimed before
    older tasks.
  - FIFO order, the queued/in-flight dedupe and the claim inspection budget
    are unchanged.
- **Reaper gate.** In Raft mode the reaper sweeps only after
  `TaskRepository.EnsureLeaderDispatch` succeeds (`ReaperOptions.LeaderReady`):
  this process leads, a barrier has applied every entry committed before its
  term, and the dispatch index and lease table were rebuilt from that state.
  - Until then the tick is deferred: counted as `deferred`, logged at DEBUG,
    and nothing is written.
  - Deferred ticks go through the same sustained-condition watch as stalled
    sweeps. After one minute of continuous deferral the sweep logs
    `event=codeq_reaper_deferred` at WARN with the sweep, the error class
    (`not_leader`, `timeout`, `canceled` or `barrier_or_storage`) and how long
    it has been deferred, at most once per 5 minutes. No error text or tenant
    data is logged. A tick that runs ends the deferral; a follower tick
    resets both watches.
  - The election-time rebuild logs `event=codeq_dispatch_rebuild_failed` at
    WARN with the error class when it fails while this voter still leads, at
    most once per 5 minutes.
  - The reaper therefore also completes a rebuild that failed at election on
    an idle leader, and a leader stuck before its rebuild is visible in logs
    within a minute.
- **Visibility.** Recurrences are visible in logs and metrics.
  - The storage monitor (every 5 s, every voter) publishes:
    - `codeq_memory_go_live_heap_bytes` and `codeq_memory_gc_cpu_fraction`.
      The GC share counts mark assists, dedicated mark workers and pauses,
      the GC work that takes CPU from the program. Idle-priority marking
      (counted by `/cpu/classes/gc/total`) runs only on otherwise idle CPU
      and would flag idle voters.
    - `codeq_dispatch_hint_queues`, `codeq_dispatch_hints`,
      `codeq_dispatch_inflight`, `codeq_dispatch_overflowed_queues` and
      `codeq_lease_entries` per shard. The dispatch sizes are atomic counters
      stored under the dispatch lock after each change, so the sample never
      waits behind a rebuild or a refill.
  - When the live heap stays at or above 90% of the soft limit, or the GC
    takes at least 25% of the CPU available to Go, for two consecutive
    samples, it logs `event=codeq_memory_pressure` at WARN at most once per
    5 minutes. The line carries the live heap, the limit, the GC share,
    whether the voter leads, and the dispatch footprint, overflowed queues
    included (`memory_pressure_warnings_total` counts these warnings).
  - Every reaper sweep is counted in
    `codeq_reaper_ticks_total{sweep,outcome}`, where outcome is `idle`,
    `progress`, `stalled` (due work found and none finished), `error` or
    `deferred`.
  - A sweep stalled for at least a minute logs `event=codeq_reaper_stalled`
    with the sweep and the count of due entries, at most once per 5 minutes.

## Consequences

- A leader's dispatch memory no longer depends on how many topics and
  priorities its claimers name. An idle topic holds nothing. A backlog costs
  memory per queued task, bounded by the per-queue cap as before.
- Hint memory has no global bound, only the per-queue cap. A voter's hints
  total at most 100,000 × the number of `(command, tenant, priority)` tuples
  with pending work, about 24 bytes plus the id each. Followers keep the hints
  their constructor recovered (their queues receive no publishes or claims)
  until they lead and rebuild. A process-wide cap was not added: it would
  have to choose which tenant's queue stops dispatching. `codeq_dispatch_hints`
  and the `codeq_memory_pressure` footprint expose the total.
- Every hint operation already took `dispatchMu`; the queue adds no lock and
  replaces channel operations with slice operations under the same mutex.
- Raft entries, the FSM, snapshots, the HTTP and gRPC APIs and the
  configuration are unchanged. No resource request, limit, voter count or
  default changes, so cost is unchanged.
- A newly elected leader's first sweep can wait up to 2 s for its barrier,
  and is deferred while the barrier cannot complete. Expired leases are
  requeued that much later; no lease can expire unnoticed.
- Mixed-version rolls are safe. Everything changed is process-local or
  observability, so a v0.5.13 voter and a voter with this change replicate
  and apply the same entries. The installation agent rolls voters one at a
  time in reverse ordinal order without regard to leadership, so the leader
  cannot be rolled last on purpose; a v0.5.13 voter can lead for about one
  roll step and carries the defect only while it does. After the roll,
  verify that the leader (`/raft/status`) is a Pod running the new image,
  and watch `event=codeq_memory_pressure` and `codeq_memory_gc_cpu_fraction`
  on it for the next hours.
- Operators get the first signal of a recurrence in the logs.
  `codeq_memory_pressure` with large `dispatchQueues` would point at a new
  per-tuple leak; with a small footprint it points elsewhere (Pebble, request
  backlog). `codeq_reaper_stalled` names the sweep and the work it keeps
  re-reading; `codeq_reaper_deferred` and `codeq_dispatch_rebuild_failed`
  name a leader that cannot complete its rebuild.
- A queue deeper than the cap costs one bounded scan per refill instead of a
  crash loop or a stuck leader; it is visible as
  `codeq_dispatch_overflowed_queues` and `codeq_hint_queue_overflow`.
- A refill and a rebuild hold `dispatchMu` for their whole Pebble scan, so
  every claim, publish and lease release of the shard waits behind them.
  Measured on Apple silicon: a refill that queues 100,000 hints holds it
  14–15 ms, and a rebuild over 250,000 pending keys 76–87 ms (architecture
  review; a re-run on an M4 Max gave 8–10 ms and about 60 ms). Expect 3–5×
  that on a 1-vCPU voter: about 45–75 ms per refill and 0.25–0.45 s for such
  a rebuild. A refill runs only when an overflowed queue drains, so at most
  once per 100,000 claims of that tuple; a rebuild runs once per election
  win and at startup. The scans were not moved out of the lock: the hold is
  rare, bounded by the cap and the backlog, and visible in claim latency.

## Rollback

v0.5.13 and v0.5.12 recover a queue by publishing every pending key into a
100,000-slot channel and fail when one is full: with more than 100,000
pending tasks in any `(command, tenant, priority)` their constructor panics
and every voter that restarts on that image crash-loops. Roll back only
after confirming that no queue is that deep: no `event=codeq_hint_queue_overflow`
WARN since the voters last started and `codeq_dispatch_overflowed_queues`
is 0 on every voter (or `QueueStats` ready counts below 100,000 per
priority). Otherwise drain the deep queue first, or roll forward.

## Alternatives considered

- **Smaller channel buffer.** Memory would still scale with probed tuples,
  and the cap would lose hints sooner. Rejected.
- **Probe only priorities with a channel, keep channels forever.** This
  removes the 10× probe factor, but each topic ever published still pins a
  fixed 2.4 MB pointer-bearing buffer. Rejected.
- **Raise the memory limit or the Go soft limit.** It costs capacity, does
  not stop the per-topic growth, and is outside this change's cost mandate.
  Rejected.
- **Reaper failure back-off and payload-free decoding.** These were the
  candidate fix for a reaper re-reading failing rows. The production logs
  rule that mechanism out, so this ADR adds only the stall metric and
  warning that would expose it.

## References

- Conveste `codecloud-queue` logs and Cloud Monitoring, 2026-11-08 20:30 to
  2026-11-09 21:00 UTC (CFP-070 coordinator evidence).
- `internal/repository/pebble/dispatch_memory_test.go`
  (`TestClaimProbesRetainNoDispatchMemory` fails on v0.5.13 with 297 MiB
  retained).
- `internal/repository/pebble/dispatch_overflow_test.go`
  (`TestRestartWithMorePendingThanCapSeedsCapAndDrainsInOrder`,
  `TestRebuildWithMorePendingThanCapSucceeds`,
  `TestPublishBeyondCapRefillsInOrder`,
  `TestClaimManyDrainsOverflowedQueueInOrder`,
  `TestOverflowConcurrentPublishAndClaimDeliversEveryTaskOnce`,
  `TestDispatchFootprintDoesNotWaitForDispatchLock`),
  `dispatch_overflow_lifecycle_test.go`
  (`TestRebuildRaisesSeqPastOverflowedBacklog`,
  `TestLeaseExpiryRequeueIntoOverflowedQueue`,
  `TestDelayedMoveIntoOverflowedQueue`,
  `TestAbsoluteExpiryRetirementWhileOverflowed`) and
  `dispatch_overflow_failover_test.go`
  (`TestRaftFailoverOverOverflowedQueueKeepsFIFO`: three in-process voters,
  the leader closes over a backlog deeper than the cap, and a follower that
  never enqueued wins).
- `internal/repository/pebble/reaper_leader_ready_test.go`
  (`TestReaperWarnsWhenDeferredForAMinute`), `reaper_progress_test.go`,
  `internal/memlimit/pressure_test.go`
  (`TestGCBusyMetricsExcludeIdleMarkAndExist`).
- ADR 0010, ADR 0012, ADR 0013; `internal/memlimit` (soft limit derivation).
