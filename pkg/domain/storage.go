package domain

import "errors"

// StorageCapacityCode is the stable machine-readable code for a create refused
// by the local data-volume admission guard.
const StorageCapacityCode = "storage_capacity"

// ErrStorageCapacity refuses new task acceptance while the local data volume is
// above its configured used-space threshold. Claims, lease renewals,
// completions, deletions, cleanup and Raft replication are never refused by it,
// so the queue can drain and compaction can reclaim space.
var ErrStorageCapacity = errors.New(StorageCapacityCode + ": data volume is above its admission threshold; new tasks are refused until cleanup or compaction frees space")
