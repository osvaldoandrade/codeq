# Storage and memory safety

Each CodeQ node keeps its queue in an embedded Pebble store on a local volume.
A full volume is the one failure Pebble cannot recover from by itself: it can
no longer write its WAL, MANIFEST or compaction output, the process exits and
the next start cannot open the store. Fixed Pebble buffers (256 MiB cache plus
64 MiB memtables per shard) also exceeded small container limits. These
safeguards add no infrastructure and do not change the on-disk format.

## Data-volume admission (`storage_capacity`)

The guard samples the filesystem holding the data path (`statfs`, space
available to the non-root process) every `checkIntervalSeconds`. At or above
`admissionMaxUsedFraction` used, **new** task creation is refused:

- `POST /v1/codeq/tasks`: HTTP 429, `Retry-After: 30`,
  `{"error":"storage_capacity", ...}`; batch items report `storage_capacity`;
  the producer gRPC stream acks with an `ErrorMessage` prefixed `storage_capacity:`;
  a recurring schedule fire is refused and retried on the next runner tick
  (the slot is not lost).
- Idempotent and deduplicated replays of existing tasks are still answered.
- Claims, lease renewals, progress, results, nacks, abandons, deletions,
  cleanup, compaction and Raft replication are never refused, so the queue
  drains and space can be reclaimed.

A failed sample leaves admission open (counted in `codeq_storage_stat_errors_total`).
On Raft, the leader decides with its own volume; followers still run the
reserve below. The guard is per node.

### Raft snapshot headroom

hashicorp/raft writes a new FSM snapshot completely before it reaps older
ones, so the snapshot footprint peaks at `retain + 1` full copies of the
queue state. On a 1 GiB volume a snapshot started just below 80% could still
hit `ENOSPC`. On Raft voters admission therefore uses an effective threshold:

```
effective = min(admissionMaxUsedFraction,
                1 - (max(E * (R + 1) - D, E) + reserve) / volume)
```

- `E` (snapshot estimate): per shard, the larger of the newest complete
  snapshot on disk and the live replicated state (Pebble's estimate of the
  `codeq/` range, a lower bound because sstables are compressed), summed over
  shards. The monitor refreshes it every `checkIntervalSeconds`; requests only
  read the cached value. A failed measurement keeps the last estimate.
- `R`: the snapshot retention in effect (below).
- `D`: bytes snapshots occupy now (retained plus in-progress). They are
  already counted as used, so they are credited; without the credit the
  retained copies would be counted twice and a drained queue could keep
  admission closed until the next snapshot. The credit never reduces the
  headroom below one full new copy `E`. With no snapshot on disk yet `D = 0`
  and the formula is exactly `1 - (E * (R + 1) + reserve) / volume`.
- `reserve`: the emergency reserve size (below), kept free at the peak.

The result is clamped to `[0, admissionMaxUsedFraction]`. Without Raft or with
an empty state it equals the configured threshold. A value of 0 means the
volume cannot hold the snapshot footprint: every new task is refused, the
closing log line says `reason=snapshot_headroom`, and the operator should
enlarge the volume or let the queue drain. Example, 1 GiB volume, retain 1,
no reserve: a 300 MiB snapshot lowers the threshold to 70.7%.

## Emergency reserve

While the volume is comfortably below the threshold the node writes
`codeq-reserve.ballast` (real zero blocks) into the data directory root. Its
size is `reserveBytes`, capped to 5% of the volume and rounded down to whole
MiB (51 MiB on a 1 GiB volume). When free space falls below the reserve size,
at runtime or at startup before Pebble opens, the reserve is deleted so Pebble
can replay its WAL, flush and compact without a manual purge. It is recreated
once usage plus the reserve stays below the effective admission threshold. Pebble and
older CodeQ releases ignore this file. After a rollback to a release without
this guard the file stays; deleting it is safe and frees its space.

## Memory sizing

The server reads the container memory limit (cgroup v2 `memory.max`, then v1
`memory.limit_in_bytes`, or `limitBytes`). Per Pebble shard it uses 15% of the
limit for the block cache (8–256 MiB) and 1/32 for each memtable (4–64 MiB);
without a limit the legacy 256 MiB / 64 MiB remain. It then sets the Go soft
memory limit to `goMemoryLimitFraction` of the container limit unless
`GOMEMLIMIT` is set explicitly. CodeQ builds with `CGO_ENABLED=0`, so Pebble's
buffers live on the Go heap and are covered by that limit. At a 512 MiB limit:
76 MiB cache, 16 MiB memtables, ~358 MiB Go soft limit.

GKE Autopilot sets limit = request, so a small request becomes a small limit.
When the limit is too small for the shard count (below 128 MiB × shards
for derived sizes: 1/32 of the limit must give each shard a 4 MiB memtable,
and 15% must give it an 8 MiB cache), the per-shard floors
apply and Pebble takes more than the intended share of the limit. The server
still starts, logs a `WARN` naming `minSafeLimitBytes`, and sets
`codeq_memory_pebble_sizing_floored` to 1 and
`codeq_memory_pebble_min_safe_limit_bytes` to that minimum (0 when explicit
overrides cover both sizes, or when more than 16 shards cannot be split above
the floors at any limit). Raise the memory request or lower `numShards`.

## Raft retention

Each voter keeps `snapshotRetain` FSM snapshots (was 3) and `trailingLogs`
applied entries after a snapshot (was hashicorp's 10240). A follower that lags
further installs a snapshot.

`snapshotRetain` defaults to `0` = auto: **1** on data volumes of at most
1 GiB and **2** on larger volumes or when the size cannot be read. Each
retained snapshot is a full copy of the queue state; on a 1 GiB voter the
second copy is what pushes the snapshot cycle over the volume. Retain 1 never
deletes the only complete snapshot (the new one is published before the old
one is reaped), so the cost is losing the older fallback snapshot if the
newest is corrupted on disk. Snapshots are CRC-checked; a voter whose only
snapshot cannot be restored fails to start and must be re-seeded (empty its
data volume so it rejoins and installs the leader's snapshot), while the other
voters keep their own copies. Set an explicit value to override. The on-disk format is unchanged, so a
rollback simply resumes the older release's retention (3) and accumulates
snapshots again; nothing needs migrating.

The retention is also applied once at startup, **before Pebble opens**, on
every shard of a Raft node: complete snapshots beyond the retention are reaped
with hashicorp's own ordering (the newest always stays) and in-progress
`<term>-<index>-<msec>.tmp` directories left by a process that died while
writing a snapshot are deleted (hashicorp/raft never reaps those). A voter
filled under the older retention of 3, or by an abandoned snapshot, otherwise
cannot replay its WAL, so Raft would never run to reap them. The step holds the
Pebble directory lock (a store open in another process is left untouched),
logs `raft snapshot space reclaimed before open` with the freed bytes, and is
never fatal. It frees only snapshot copies; if the volume is still full the
store fails to open as before and needs more space.

## Configuration

| YAML key | Env | Default |
| --- | --- | --- |
| `storage.admissionMaxUsedFraction` | `STORAGE_ADMISSION_MAX_USED_FRACTION` | `0.80` (in (0, 1]) |
| `storage.reserveBytes` | `STORAGE_RESERVE_BYTES` | `67108864`; `0` disables and removes the reserve |
| `storage.checkIntervalSeconds` | `STORAGE_CHECK_INTERVAL_SECONDS` | `5` |
| `memory.limitBytes` | `MEMORY_LIMIT_BYTES` | `0` = detect from cgroup |
| `memory.goMemoryLimitFraction` | `GO_MEMORY_LIMIT_FRACTION` | `0.70`; `GOMEMLIMIT` wins |
| `memory.pebbleCacheBytes` | `PEBBLE_CACHE_BYTES` | `0` = derived, per shard |
| `memory.pebbleMemTableBytes` | `PEBBLE_MEMTABLE_BYTES` | `0` = derived, per shard |
| `memory.pebbleL0StopWritesThreshold` | `PEBBLE_L0_STOP_WRITES_THRESHOLD` | `64` (unchanged) |
| `raft.snapshotRetain` | `RAFT_SNAPSHOT_RETAIN` | `0` = auto (1 on ≤ 1 GiB volumes, else 2) |
| `raft.trailingLogs` | `RAFT_TRAILING_LOGS` | `2048` |

Defaults are applied by the configuration loader. Malformed or out-of-range
values fail startup validation. A `Config` built in code without the loader
keeps the guard disabled.

## Metrics

`codeq_storage_data_dir_{total,free}_bytes`, `codeq_storage_data_dir_used_ratio`,
`codeq_storage_admission_threshold_ratio` (configured),
`codeq_storage_admission_effective_threshold_ratio` (applied, after snapshot headroom),
`codeq_storage_snapshot_estimate_bytes`, `codeq_storage_snapshot_disk_bytes`,
`codeq_raft_snapshot_retain`, `codeq_storage_admission_blocked`,
`codeq_storage_admission_refused_total`, `codeq_storage_stat_errors_total`,
`codeq_storage_reserve_bytes`, `codeq_storage_reserve_released_total`,
`codeq_pebble_l0_files{shard}`, `codeq_pebble_l0_sublevels{shard}`,
`codeq_pebble_compaction_debt_bytes{shard}`, `codeq_pebble_disk_usage_bytes{shard}`,
`codeq_pebble_cache_capacity_bytes{shard}`, `codeq_pebble_memtable_bytes{shard}`,
`codeq_memory_container_limit_bytes`, `codeq_memory_go_limit_bytes`,
`codeq_memory_pebble_sizing_floored`, `codeq_memory_pebble_min_safe_limit_bytes`.
