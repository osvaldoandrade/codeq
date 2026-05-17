package pebble

import (
	"sync"
	"time"

	"github.com/bytedance/sonic"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// leaseEntry is the in-memory record for one in-progress task's lease.
// Kept tiny because the reaper iterates the whole table on every sweep
// — 32 bytes per active task × 1M concurrent leases = 32 MiB cap.
type leaseEntry struct {
	workerID string
	cmd      domain.Command // for reaper's per-(cmd,tenant) backoff bookkeeping
	tenantID string
	untilU   int64 // unix seconds; reaper compares against time.Now().Unix()
}

// leaseTable is the Phase 6 / M2 replacement for the on-disk KeyLease
// index. Every Claim sets an entry; every Heartbeat extends it; every
// Submit / Nack / Abandon clears it; the reaper sweeps in-memory for
// expired entries and Nacks via the existing requeueExpiredOne path.
//
// Crash recovery: the lease table is volatile by design. On Open() we
// scan KeyInprog and rebuild the table from each task's persisted
// LeaseUntil RFC3339 timestamp. Workers whose lease is recovered get
// to keep running until their lease expires for real; workers whose
// lease has already passed get their task requeued by the reaper's
// first sweep — exactly the behavior an expired lease ALWAYS had,
// just without 1 KeyLease Set per Claim and 1 Get per reaper tick.
//
// Concurrency: sync.RWMutex around a plain map. Tried sync.Map; it
// makes ForEach iteration (reaper) much more expensive because of
// the load-then-CAS-replace pattern, and reaper iteration cost is
// the hot path we're trying to win back from Pebble.
type leaseTable struct {
	mu sync.RWMutex
	m  map[string]leaseEntry
}

func newLeaseTable() *leaseTable {
	return &leaseTable{m: make(map[string]leaseEntry, 1024)}
}

// Set installs the lease unconditionally, replacing any previous
// entry. Called by Claim / ClaimMany / completeClaim.
func (t *leaseTable) Set(taskID, workerID string, cmd domain.Command, tenantID string, untilU int64) {
	t.mu.Lock()
	t.m[taskID] = leaseEntry{workerID: workerID, cmd: cmd, tenantID: tenantID, untilU: untilU}
	t.mu.Unlock()
}

// Delete drops the entry for taskID. Idempotent.
func (t *leaseTable) Delete(taskID string) {
	t.mu.Lock()
	delete(t.m, taskID)
	t.mu.Unlock()
}

// Get returns the entry and whether it exists. Reaper uses this to
// double-check at requeue time so a Heartbeat that arrived between
// the sweep snapshot and the requeue doesn't lose its task.
func (t *leaseTable) Get(taskID string) (leaseEntry, bool) {
	t.mu.RLock()
	e, ok := t.m[taskID]
	t.mu.RUnlock()
	return e, ok
}

// Extend updates untilU for taskID, but only if workerID still owns
// the entry. Returns false if the task isn't owned by workerID (or
// has no lease anymore — e.g. another worker re-claimed after expiry).
func (t *leaseTable) Extend(taskID, workerID string, untilU int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[taskID]
	if !ok || e.workerID != workerID {
		return false
	}
	e.untilU = untilU
	t.m[taskID] = e
	return true
}

// SnapshotExpired returns IDs whose untilU is in the past (≤ now).
// Caller is the reaper; it iterates the returned slice and Nacks
// each one. Bounded by `limit` so a huge table doesn't lock the
// reaper for too long per sweep — subsequent sweeps catch the rest.
func (t *leaseTable) SnapshotExpired(now int64, limit int) []string {
	if limit <= 0 {
		limit = 256
	}
	out := make([]string, 0, limit)
	t.mu.RLock()
	for id, e := range t.m {
		if e.untilU <= now {
			out = append(out, id)
			if len(out) >= limit {
				break
			}
		}
	}
	t.mu.RUnlock()
	return out
}

// Len returns the current entry count. Used by tests and metrics.
func (t *leaseTable) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.m)
}

// recoverLeases rebuilds the in-memory lease table from the on-disk
// inprog index. Runs once at Open(). Walks every key under codeq/q/
// .../inprog/, fetches the task body, and uses its LeaseUntil
// timestamp to seed the table. Tasks whose LeaseUntil is in the
// past land in the table with an already-expired entry — the
// reaper's first sweep picks them up and Nacks via the existing
// requeueExpiredOne path. Net effect: a restart is transparent to
// workers whose lease hasn't expired and indistinguishable from
// "the reaper noticed the expiry one tick later" for those whose
// lease has.
//
// Errors are best-effort: a corrupt entry skips, doesn't abort the
// whole recovery. The on-disk inprog set + task bodies are the
// source of truth; this is only the in-memory cache.
func (r *TaskRepository) recoverLeases() error {
	if r.leases == nil {
		r.leases = r.db.Leases
	}
	lower := []byte(pQueue)
	upper := prefixUpper(lower)
	it, err := r.db.Iter(lower, upper)
	if err != nil {
		return err
	}
	defer it.Close()

	inprogMarker := []byte(segInprog)
	for valid := it.First(); valid; valid = it.Next() {
		k := it.Key()
		idx := indexOf(k, inprogMarker)
		if idx < 0 {
			continue
		}
		// inprog key layout: codeq/q/<cmd>/<tenant>/inprog/<id> — the
		// suffix after the marker is the bare task ID with no further
		// separators because IDs are uuid v4 strings.
		id := string(k[idx+len(inprogMarker):])
		taskJSON, err := r.db.Get(KeyTask(id))
		if err != nil {
			continue
		}
		var t domain.Task
		if err := sonic.Unmarshal(taskJSON, &t); err != nil {
			continue
		}
		if t.LeaseUntil == "" || t.WorkerID == "" {
			continue
		}
		until, err := time.Parse(time.RFC3339, t.LeaseUntil)
		if err != nil {
			continue
		}
		r.leases.Set(id, t.WorkerID, t.Command, t.TenantID, until.Unix())
	}
	return it.Error()
}

