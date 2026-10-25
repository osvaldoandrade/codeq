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

## Emergency reserve

While the volume is comfortably below the threshold the node writes
`codeq-reserve.ballast` (real zero blocks) into the data directory root. Its
size is `reserveBytes`, capped to 5% of the volume and rounded down to whole
MiB (51 MiB on a 1 GiB volume). When free space falls below the reserve size,
at runtime or at startup before Pebble opens, the reserve is deleted so Pebble
can replay its WAL, flush and compact without a manual purge. It is recreated
once usage plus the reserve stays below the admission threshold. Pebble and
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

## Raft retention

Each voter keeps `snapshotRetain` FSM snapshots (was 3) and `trailingLogs`
applied entries after a snapshot (was hashicorp's 10240). A follower that lags
further installs a snapshot.

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
| `raft.snapshotRetain` | `RAFT_SNAPSHOT_RETAIN` | `2` |
| `raft.trailingLogs` | `RAFT_TRAILING_LOGS` | `2048` |

Defaults are applied by the configuration loader. Malformed or out-of-range
values fail startup validation. A `Config` built in code without the loader
keeps the guard disabled.

## Metrics

`codeq_storage_data_dir_{total,free}_bytes`, `codeq_storage_data_dir_used_ratio`,
`codeq_storage_admission_threshold_ratio`, `codeq_storage_admission_blocked`,
`codeq_storage_admission_refused_total`, `codeq_storage_stat_errors_total`,
`codeq_storage_reserve_bytes`, `codeq_storage_reserve_released_total`,
`codeq_pebble_l0_files{shard}`, `codeq_pebble_l0_sublevels{shard}`,
`codeq_pebble_compaction_debt_bytes{shard}`, `codeq_pebble_disk_usage_bytes{shard}`,
`codeq_pebble_cache_capacity_bytes{shard}`, `codeq_pebble_memtable_bytes{shard}`,
`codeq_memory_container_limit_bytes`, `codeq_memory_go_limit_bytes`.
