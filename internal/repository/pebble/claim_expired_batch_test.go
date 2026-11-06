package pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pebbledb "github.com/cockroachdb/pebble"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/osvaldoandrade/codeq/internal/core/queuetopic"
	raftpkg "github.com/osvaldoandrade/codeq/internal/raft"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// countWrites counts every write the DB submits from now on.
func countWrites(db *DB) *atomic.Int64 {
	n := &atomic.Int64{}
	db.onWrite = func() { n.Add(1) }
	return n
}

func claimLogs(ctx context.Context, repo *TaskRepository, inspect int) (*domain.Task, bool, error) {
	return repo.Claim(ctx, "worker", []domain.Command{absoluteTestTopic}, 60, inspect, 100, "tenant-a")
}

func assertRetired(t *testing.T, db *DB, ids ...string) {
	t.Helper()
	for _, id := range ids {
		assertNoPayloadIndexes(t, db, id)
		identity, err := db.readAbsoluteIdentity(id)
		if err != nil || identity == nil || identity.RetiredAt == nil {
			t.Fatalf("%s not retired: %+v %v", id, identity, err)
		}
	}
}

func expiredIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("expired-%04d", i)
	}
	return ids
}

// purgedTotal reads codeq_absolute_expired_purged_total.
func purgedTotal(t *testing.T) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "codeq_absolute_expired_purged_total" {
			return family.GetMetric()[0].GetCounter().GetValue()
		}
	}
	t.Fatal("codeq_absolute_expired_purged_total is not registered")
	return 0
}

func hintCount(repo *TaskRepository) int {
	return len(repo.channelFor(absoluteTestTopic, "tenant-a", 3).ch)
}

// A claim that meets N expired heads before a live task commits twice (one
// retirement batch plus its lease), not N+1 times, on standalone Pebble and
// through a replicator.
func TestClaimRetiresExpiredHeadsInOneCommit(t *testing.T) {
	for _, replicated := range []bool{false, true} {
		t.Run(fmt.Sprintf("replicated=%v", replicated), func(t *testing.T) {
			const expired = 150
			db, repo, _, clock := absoluteFixture(t)
			live := seedExpiredHeads(t, repo, clock, expired)
			if replicated {
				db.AttachReplicator(&ambiguousAbsoluteReplicator{db: db.Raw()})
			}
			purgedBefore := purgedTotal(t)
			writes := countWrites(db)
			got, ok, err := claimLogs(context.Background(), repo, defaultInspectLimit)
			if err != nil || !ok || got.ID != live.ID {
				t.Fatalf("claim=%+v ok=%v err=%v", got, ok, err)
			}
			if n := writes.Load(); n != 2 {
				t.Fatalf("claim behind %d expired heads made %d writes; want 2", expired, n)
			}
			assertRetired(t, db, expiredIDs(expired)...)
			if count, err := db.absoluteCount(); err != nil || count != expired+1 {
				t.Fatalf("identity count=%d err=%v want %d", count, err, expired+1)
			}
			if purged := purgedTotal(t) - purgedBefore; purged != expired {
				t.Fatalf("purged metric delta=%v want %d", purged, expired)
			}
			if hints := hintCount(repo); hints != 0 {
				t.Fatalf("remaining hints=%d", hints)
			}
			if len(repo.inflight) != 0 {
				t.Fatalf("hints left in flight: %v", repo.inflight)
			}
			if _, ok := db.Leases.Get(live.ID); !ok {
				t.Fatal("live task has no lease")
			}
		})
	}
}

// A backlog larger than one retirement batch costs one commit per
// absoluteRetireBatch rows, and a backlog larger than the inspection budget
// leaves the rest for later claims, which retire it the same way.
func TestClaimRetirementStaysWithinBudgetAndBatch(t *testing.T) {
	const expired = absoluteRetireBatch + 44
	db, repo, _, clock := absoluteFixture(t)
	live := seedExpiredHeads(t, repo, clock, expired)
	writes := countWrites(db)
	got, ok, err := claimLogs(context.Background(), repo, expired+1)
	if err != nil || !ok || got.ID != live.ID {
		t.Fatalf("claim=%+v ok=%v err=%v", got, ok, err)
	}
	if n := writes.Load(); n != 3 {
		t.Fatalf("writes=%d; want two retirement batches and one lease", n)
	}
	assertRetired(t, db, expiredIDs(expired)...)

	db, repo, _, clock = absoluteFixture(t)
	live = seedExpiredHeads(t, repo, clock, 2*defaultInspectLimit)
	writes = countWrites(db)
	for round := range 2 {
		got, ok, err = claimLogs(context.Background(), repo, defaultInspectLimit)
		if err != nil || ok || got != nil {
			t.Fatalf("round %d: claim=%+v ok=%v err=%v", round, got, ok, err)
		}
		if n := writes.Swap(0); n != 1 {
			t.Fatalf("round %d writes=%d; want one retirement batch", round, n)
		}
	}
	got, ok, err = claimLogs(context.Background(), repo, defaultInspectLimit)
	if err != nil || !ok || got.ID != live.ID || writes.Load() != 1 {
		t.Fatalf("claim=%+v ok=%v err=%v writes=%d", got, ok, err, writes.Load())
	}
}

// failFirstReplicator fails (or cancels the claim at) its first replicated
// write, then behaves like a durable leader.
type failFirstReplicator struct {
	*ambiguousAbsoluteReplicator
	calls  atomic.Int64
	fail   bool
	cancel context.CancelFunc
}

func (r *failFirstReplicator) Replicate(repr []byte) error {
	if r.calls.Add(1) == 1 && r.fail {
		return errors.New("replication failed")
	}
	err := r.ambiguousAbsoluteReplicator.Replicate(repr)
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	return err
}

// The retirement batch commits whatever the claimant does; a failed
// retirement keeps every row and restores every hint; neither loses nor
// duplicates the live task.
func TestClaimRetirementFailureAndCancellation(t *testing.T) {
	const expired = 30
	t.Run("canceled-after-retirement", func(t *testing.T) {
		db, repo, _, clock := absoluteFixture(t)
		live := seedExpiredHeads(t, repo, clock, expired)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		db.AttachReplicator(&failFirstReplicator{ambiguousAbsoluteReplicator: &ambiguousAbsoluteReplicator{db: db.Raw()}, cancel: cancel})
		got, ok, err := claimLogs(ctx, repo, defaultInspectLimit)
		if !errors.Is(err, context.Canceled) || ok || got != nil {
			t.Fatalf("canceled claim=%+v ok=%v err=%v", got, ok, err)
		}
		assertRetired(t, db, expiredIDs(expired)...)
		stored, err := repo.Get(context.Background(), live.ID)
		if err != nil || stored.Status != domain.StatusPending || stored.Attempts != 0 {
			t.Fatalf("canceled claim consumed the live task: %+v %v", stored, err)
		}
		if hints := hintCount(repo); hints != 1 {
			t.Fatalf("hints=%d; want only the live task's", hints)
		}
		got, ok, err = claimLogs(context.Background(), repo, defaultInspectLimit)
		if err != nil || !ok || got.ID != live.ID {
			t.Fatalf("retry=%+v ok=%v err=%v", got, ok, err)
		}
	})
	t.Run("retirement-commit-fails", func(t *testing.T) {
		db, repo, _, clock := absoluteFixture(t)
		live := seedExpiredHeads(t, repo, clock, expired)
		countBefore, _ := db.absoluteCount()
		db.AttachReplicator(&failFirstReplicator{ambiguousAbsoluteReplicator: &ambiguousAbsoluteReplicator{db: db.Raw()}, fail: true})
		got, ok, err := claimLogs(context.Background(), repo, defaultInspectLimit)
		if err == nil || ok || got != nil {
			t.Fatalf("failed retirement hidden: %+v ok=%v err=%v", got, ok, err)
		}
		if hints := hintCount(repo); hints != expired+1 {
			t.Fatalf("hints=%d; want every hint restored (%d)", hints, expired+1)
		}
		if len(repo.inflight) != 0 {
			t.Fatalf("hints left in flight: %v", repo.inflight)
		}
		for _, id := range expiredIDs(expired) {
			if has, err := db.Has(KeyTask(id)); err != nil || !has {
				t.Fatalf("%s lost by a failed retirement: %v %v", id, has, err)
			}
		}
		if count, _ := db.absoluteCount(); count != countBefore {
			t.Fatalf("count=%d want %d", count, countBefore)
		}
		got, ok, err = claimLogs(context.Background(), repo, defaultInspectLimit)
		if err != nil || !ok || got.ID != live.ID {
			t.Fatalf("retry=%+v ok=%v err=%v", got, ok, err)
		}
		assertRetired(t, db, expiredIDs(expired)...)
	})
	t.Run("canceled-while-inspecting", func(t *testing.T) {
		db, repo, _, clock := absoluteFixture(t)
		live := seedExpiredHeads(t, repo, clock, expired)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// Cancel while the claim inspects the tenth expired row.
		inner, reads := db.policyReader, atomic.Int64{}
		db.SetTopicPolicyReader(expiryPolicyReader(func(c context.Context, tenant, name string) (queuetopic.Topic, error) {
			if reads.Add(1) == 10 {
				cancel()
			}
			return inner.Get(c, tenant, name)
		}))
		writes := countWrites(db)
		got, ok, err := claimLogs(ctx, repo, defaultInspectLimit)
		if !errors.Is(err, context.Canceled) || ok || got != nil {
			t.Fatalf("canceled claim=%+v ok=%v err=%v", got, ok, err)
		}
		if n := writes.Load(); n != 1 {
			t.Fatalf("writes=%d; want the rows inspected before cancel retired in one batch", n)
		}
		retired := 0
		for _, id := range expiredIDs(expired) {
			if has, _ := db.Has(KeyTask(id)); !has {
				retired++
			}
		}
		if retired == 0 || retired+hintCount(repo) != expired+1 {
			t.Fatalf("retired=%d hints=%d; every unretired row must keep its hint", retired, hintCount(repo))
		}
		db.SetTopicPolicyReader(inner)
		got, ok, err = claimLogs(context.Background(), repo, defaultInspectLimit)
		if err != nil || !ok || got.ID != live.ID {
			t.Fatalf("retry=%+v ok=%v err=%v", got, ok, err)
		}
		assertRetired(t, db, expiredIDs(expired)...)
	})
}

// Concurrent claimers over a backlog of expired heads interleaved with live
// tasks lease every live task exactly once and retire every expired row.
func TestConcurrentClaimsRetireExpiredHeadsOnce(t *testing.T) {
	db, repo, _, clock := absoluteFixture(t)
	base := time.Unix(0, clock.Load()).UTC()
	enqueue := func(id string, deadline time.Time) {
		ctx := domain.WithTaskExpiry(context.Background(), deadline)
		if _, err := repo.Enqueue(ctx, absoluteTestTopic, `{}`, 3, "", 100, "", "", id, time.Time{}, "tenant-a"); err != nil {
			t.Fatal(err)
		}
	}
	var expired, live []string
	for i := range 240 {
		if i%8 == 7 {
			id := fmt.Sprintf("live-%04d", i)
			live = append(live, id)
			enqueue(id, base.Add(10*time.Minute))
			continue
		}
		id := fmt.Sprintf("expired-%04d", i)
		expired = append(expired, id)
		enqueue(id, base.Add(time.Minute))
	}
	clock.Store(base.Add(61 * time.Second).UnixNano())
	var mu sync.Mutex
	claimed := map[string]int{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				got, ok, err := repo.Claim(context.Background(), "worker", []domain.Command{absoluteTestTopic}, 60, 16, 100, "tenant-a")
				if err != nil {
					t.Error(err)
					return
				}
				if !ok {
					if hintCount(repo) == 0 {
						return
					}
					continue
				}
				mu.Lock()
				claimed[got.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	for _, id := range live {
		if claimed[id] != 1 {
			t.Fatalf("live %s claimed %d times", id, claimed[id])
		}
	}
	if len(claimed) != len(live) {
		t.Fatalf("claimed %d tasks, want %d", len(claimed), len(live))
	}
	assertRetired(t, db, expired...)
	if count, err := db.absoluteCount(); err != nil || count != int64(len(expired)+len(live)) {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

// ClaimMany retires every expired row it popped in one batch and commits no
// empty batch when nothing is left to lease.
func TestClaimManyRetiresExpiredRowsInOneCommit(t *testing.T) {
	db, repo, _, clock := absoluteFixture(t)
	base := time.Unix(0, clock.Load()).UTC()
	for i := range 3 {
		ctx := domain.WithTaskExpiry(context.Background(), base.Add(10*time.Minute))
		if _, err := repo.Enqueue(ctx, absoluteTestTopic, `{}`, 3, "", 100, "", "", fmt.Sprintf("live-%d", i), time.Time{}, "tenant-a"); err != nil {
			t.Fatal(err)
		}
	}
	seedExpiredHeads(t, repo, clock, 50)
	writes := countWrites(db)
	tasks, err := repo.ClaimMany(context.Background(), "worker", []domain.Command{absoluteTestTopic}, 60, 100, 0, 100, "tenant-a")
	if err != nil || len(tasks) != 4 {
		t.Fatalf("claim-many=%d err=%v", len(tasks), err)
	}
	if n := writes.Load(); n != 2 {
		t.Fatalf("writes=%d; want one retirement batch and one claim batch", n)
	}
	assertRetired(t, db, expiredIDs(50)...)

	db, repo, _, clock = absoluteFixture(t)
	live := seedExpiredHeads(t, repo, clock, 20)
	clock.Store(live.ExpiresAt.Add(time.Second).UnixNano())
	writes = countWrites(db)
	tasks, err = repo.ClaimMany(context.Background(), "worker", []domain.Command{absoluteTestTopic}, 60, 100, 0, 100, "tenant-a")
	if err != nil || len(tasks) != 0 {
		t.Fatalf("claim-many=%d err=%v", len(tasks), err)
	}
	if n := writes.Load(); n != 1 {
		t.Fatalf("writes=%d; want only the retirement batch", n)
	}
	assertRetired(t, db, append(expiredIDs(20), live.ID)...)
	if hints := hintCount(repo); hints != 0 || len(repo.inflight) != 0 {
		t.Fatalf("hints=%d inflight=%v", hints, repo.inflight)
	}
}

// ClaimMany keeps every hint when the retirement batch fails.
func TestClaimManyFailedRetirementRestoresHints(t *testing.T) {
	db, repo, _, clock := absoluteFixture(t)
	live := seedExpiredHeads(t, repo, clock, 10)
	db.AttachReplicator(&failFirstReplicator{ambiguousAbsoluteReplicator: &ambiguousAbsoluteReplicator{db: db.Raw()}, fail: true})
	if tasks, err := repo.ClaimMany(context.Background(), "worker", []domain.Command{absoluteTestTopic}, 60, 100, 0, 100, "tenant-a"); err == nil || len(tasks) != 0 {
		t.Fatalf("failed retirement hidden: %d %v", len(tasks), err)
	}
	if hints := hintCount(repo); hints != 11 {
		t.Fatalf("hints=%d want 11", hints)
	}
	tasks, err := repo.ClaimMany(context.Background(), "worker", []domain.Command{absoluteTestTopic}, 60, 100, 0, 100, "tenant-a")
	if err != nil || len(tasks) != 1 || tasks[0].ID != live.ID {
		t.Fatalf("retry=%v err=%v", tasks, err)
	}
}

// Due delayed rows that expired retire in one batch inside the claim prelude.
func TestMoveDueDelayedRetiresExpiredRowsInOneCommit(t *testing.T) {
	db, repo, _, clock := absoluteFixture(t)
	base := time.Unix(0, clock.Load()).UTC()
	visible := time.Now().Add(time.Second)
	const n = 25
	for _, id := range expiredIDs(n) {
		ctx := domain.WithTaskExpiry(context.Background(), base.Add(time.Minute))
		if _, err := repo.Enqueue(ctx, absoluteTestTopic, `{}`, 3, "", 100, "", "", id, visible, "tenant-a"); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(time.Until(visible.Truncate(time.Second).Add(time.Second)))
	clock.Store(base.Add(61 * time.Second).UnixNano())
	writes := countWrites(db)
	moved, err := repo.moveDueDelayedForTenant(context.Background(), absoluteTestTopic, 100, "tenant-a")
	if err != nil || moved != 0 {
		t.Fatalf("moved=%d err=%v", moved, err)
	}
	if w := writes.Load(); w != 1 {
		t.Fatalf("writes=%d; want one retirement batch", w)
	}
	assertRetired(t, db, expiredIDs(n)...)
	if c := repo.delayedCounter(absoluteTestTopic, "tenant-a").Load(); c != 0 {
		t.Fatalf("delayed counter=%d", c)
	}
}

// The reaper's lease sweep and the TTL cleanup retire expired rows in batches.
func TestReaperRetiresExpiredRowsInBatches(t *testing.T) {
	db, repo, _, clock := absoluteFixture(t)
	base := time.Unix(0, clock.Load()).UTC()
	const n = 40
	for _, id := range expiredIDs(n) {
		ctx := domain.WithTaskExpiry(context.Background(), base.Add(time.Minute))
		if _, err := repo.Enqueue(ctx, absoluteTestTopic, `{}`, 3, "", 100, "", "", id, time.Time{}, "tenant-a"); err != nil {
			t.Fatal(err)
		}
	}
	for range n / 2 {
		if _, ok, err := claimLogs(context.Background(), repo, 1); err != nil || !ok {
			t.Fatalf("claim ok=%v err=%v", ok, err)
		}
	}
	clock.Store(base.Add(61 * time.Second).UnixNano())
	past := time.Now().Add(-time.Minute).Unix()
	for _, id := range expiredIDs(n)[:n/2] {
		db.Leases.Set(id, "worker", absoluteTestTopic, "tenant-a", past)
	}
	reaper := NewReaper(db, time.UTC, nil, ReaperOptions{})
	writes := countWrites(db)
	moved, err := reaper.sweepLeases(context.Background())
	if err != nil || moved != n/2 {
		t.Fatalf("lease sweep moved=%d err=%v", moved, err)
	}
	if w := writes.Swap(0); w != 1 {
		t.Fatalf("lease sweep writes=%d; want one retirement batch", w)
	}
	assertRetired(t, db, expiredIDs(n)[:n/2]...)
	removed, err := repo.CleanupExpired(context.Background(), 512, time.Unix(0, clock.Load()))
	if err != nil || removed != n/2 {
		t.Fatalf("cleanup removed=%d err=%v", removed, err)
	}
	if w := writes.Load(); w > 2 {
		t.Fatalf("cleanup writes=%d; want at most a retirement and an index batch", w)
	}
	assertRetired(t, db, expiredIDs(n)...)
}

// A real single-voter Raft group: the claim's two writes are two Raft
// entries, and the retirement survives a restart.
func TestClaimRetiresExpiredHeadsThroughRaft(t *testing.T) {
	path := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clock := &atomic.Int64{}
	clock.Store(time.Now().UnixNano())
	open := func() (*DB, *raftpkg.DB, *TaskRepository) {
		t.Helper()
		db, err := Open(Options{Path: filepath.Join(path, "state"), FsyncOnCommit: true})
		if err != nil {
			t.Fatal(err)
		}
		owner, err := raftpkg.OpenWithPebble(ctx, raftpkg.Config{Path: path, FsyncOnCommit: true, SelfID: "node-1", BindAddr: "127.0.0.1:0", Bootstrap: true, HeartbeatMS: 50, ElectionMS: 50, LeaderLeaseMS: 50, CommitMS: 10}, db.Raw())
		if err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		db.AttachReplicator(owner)
		db.expiryNow = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
		attachAbsolutePolicy(db, 7200, nil, time.Now())
		if err = owner.WaitLeader(ctx); err != nil {
			_ = owner.Close()
			_ = db.Close()
			t.Fatal(err)
		}
		return db, owner, NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	}
	const expired = 120
	db, owner, repo := open()
	live := seedExpiredHeads(t, repo, clock, expired)
	writes := countWrites(db)
	got, ok, err := claimLogs(ctx, repo, defaultInspectLimit)
	if err != nil || !ok || got.ID != live.ID {
		t.Fatalf("claim=%+v ok=%v err=%v", got, ok, err)
	}
	if n := writes.Load(); n != 2 {
		t.Fatalf("Raft writes=%d; want 2", n)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, owner, repo = open()
	defer db.Close()
	defer owner.Close()
	if err := repo.ensureLeaderDispatch(ctx); err != nil {
		t.Fatal(err)
	}
	assertRetired(t, db, expiredIDs(expired)...)
	if count, err := db.absoluteCount(); err != nil || count != expired+1 {
		t.Fatalf("count after restart=%d err=%v", count, err)
	}
	stored, err := db.absoluteTaskRow(live.ID)
	if err != nil || stored == nil || stored.Status != domain.StatusInProgress {
		t.Fatalf("live task after restart: %+v %v", stored, err)
	}
	if hints := hintCount(repo); hints != 0 {
		t.Fatalf("restart recovered %d hints of retired rows", hints)
	}
}

// --- equivalence with the v0.5.12 per-row path ---

// legacyPurgeAbsoluteRow is the v0.5.12 purgeAbsoluteRow, kept verbatim as
// the oracle: one commit per row, count read from the store each time.
func (d *DB) legacyPurgeAbsoluteRow(task *domain.Task) error {
	d.absoluteMu.Lock()
	defer d.absoluteMu.Unlock()
	if err := d.checkTaskExpiry(task); !errors.Is(err, domain.ErrTaskExpired) {
		return err
	}
	identity, err := d.readAbsoluteIdentity(task.ID)
	if err != nil {
		return err
	}
	restored := identity == nil
	if restored {
		identity = &absoluteIdentity{ID: task.ID, TenantID: task.TenantID, TopicID: taskTopicID(task), Hash: taskContentHash(task.Command, task.Payload), CreatedAt: task.CreatedAt, ExpiresAt: *task.ExpiresAt}
	}
	b := d.Batch()
	defer b.Close()
	if restored {
		count, err := d.absoluteCount()
		if err != nil {
			return err
		}
		if err = b.Set([]byte(absoluteCountKey), []byte(strconv.FormatInt(count+1, 10)), nil); err != nil {
			return err
		}
		if err = b.Set(absoluteTopicKey(identity.TopicID, task.ID), nil, nil); err != nil {
			return err
		}
	}
	if err = d.stageAbsolutePurge(b, identity, task, d.expiryTime()); err != nil {
		return err
	}
	if err = d.commitBatchRaw(b); err != nil {
		return err
	}
	d.Leases.Delete(task.ID)
	return nil
}

// legacySweepAbsolute is the v0.5.12 sweep: one commit per identity.
func (d *DB) legacySweepAbsolute(limit int) (int, error) {
	d.absoluteMu.Lock()
	defer d.absoluteMu.Unlock()
	ids, _, _, err := d.absoluteSweepPage(limit)
	if err != nil {
		return 0, err
	}
	removed := 0
	now := d.expiryTime()
	for _, id := range ids {
		identity, err := d.readAbsoluteIdentity(id)
		if err != nil || identity == nil {
			return removed, err
		}
		task, err := d.absoluteTaskRow(id)
		if err != nil {
			return removed, err
		}
		if settled, err := d.retiredTombstoneSettled(identity, task, now); err != nil || settled {
			if err != nil {
				return removed, err
			}
			continue
		}
		if expired, err := d.absoluteRowExpired(task, now); err != nil || !expired {
			if err != nil {
				return removed, err
			}
			continue
		}
		b := d.Batch()
		if err = d.stageAbsolutePurge(b, identity, task, now); err == nil && !now.Before(identity.ReplayUntil) {
			err = d.stageAbsoluteIdentityRemoval(b, identity)
		}
		if err == nil {
			err = d.commitBatchRaw(b)
		}
		_ = b.Close()
		if err != nil {
			return removed, err
		}
		if task != nil {
			removed++
		}
	}
	return removed, nil
}

// cloneFixture copies every key of src into a fresh absolute fixture whose
// clock reads the same instant.
func cloneFixture(t *testing.T, src *DB, at int64) *DB {
	t.Helper()
	dst, _, _, clock := absoluteFixture(t)
	clock.Store(at)
	it, err := src.Raw().NewIter(nil)
	if err != nil {
		t.Fatal(err)
	}
	b := dst.Raw().NewBatch()
	for valid := it.First(); valid; valid = it.Next() {
		if err := b.Set(bytes.Clone(it.Key()), bytes.Clone(it.Value()), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(it.Error(), it.Close(), b.Commit(pebbledb.Sync), b.Close()); err != nil {
		t.Fatal(err)
	}
	return dst
}

func dumpKeyspace(t *testing.T, db *DB) []string {
	t.Helper()
	it, err := db.Raw().NewIter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	var out []string
	for valid := it.First(); valid; valid = it.Next() {
		out = append(out, fmt.Sprintf("%q=%q", it.Key(), it.Value()))
	}
	sort.Strings(out)
	return out
}

func assertSameKeyspace(t *testing.T, legacy, batched *DB) {
	t.Helper()
	a, b := dumpKeyspace(t, legacy), dumpKeyspace(t, batched)
	if len(a) != len(b) {
		t.Fatalf("legacy has %d keys, batched %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("key %d differs:\n legacy  %s\n batched %s", i, a[i], b[i])
		}
	}
}

// restoreRow makes an accepted row look like it predates its identity
// manifest: no identity, topic index or reverse references, and uncounted.
func restoreRow(t *testing.T, db *DB, task *domain.Task) {
	t.Helper()
	count, err := db.absoluteCount()
	if err != nil {
		t.Fatal(err)
	}
	b := db.Raw().NewBatch()
	defer b.Close()
	refs := absoluteRefs(task.ID)
	err = errors.Join(b.Delete(absoluteKey(task.ID), nil), b.Delete(absoluteTopicKey(taskTopicID(task), task.ID), nil),
		b.DeleteRange(refs, prefixUpper(refs), nil), b.Set([]byte(absoluteCountKey), []byte(strconv.FormatInt(count-1, 10)), nil), b.Commit(pebbledb.Sync))
	if err != nil {
		t.Fatal(err)
	}
}

// The batched retirement leaves exactly the keyspace (identities, tombstones,
// count, references) that the per-row v0.5.12 path leaves on the same state,
// including restored rows, idempotency/dedupe mappings, delayed, in-progress
// and failed rows, and a row that is not expired.
func TestBatchedPurgeMatchesPerRowPath(t *testing.T) {
	legacy, repo, _, clock := absoluteFixture(t)
	base := time.Unix(0, clock.Load()).UTC()
	ctx := context.Background()
	enqueue := func(id, idem, dedupe string, visible time.Time, deadline time.Time, maxAttempts int) *domain.Task {
		task, err := repo.Enqueue(domain.WithTaskExpiry(ctx, deadline), absoluteTestTopic, `{"id":"`+id+`"}`, 3, "", maxAttempts, idem, dedupe, "", visible, "tenant-a")
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	deadline := base.Add(time.Minute)
	tasks := make([]*domain.Task, 0, 10)
	for range 6 {
		tasks = append(tasks, enqueue("", "", "", time.Time{}, deadline, 100))
	}
	tasks = append(tasks, enqueue("", "idem-1", "", time.Time{}, deadline, 100), enqueue("", "", "dedupe-1", time.Time{}, deadline, 100),
		enqueue("", "", "", time.Now().Add(time.Hour), deadline, 100))
	inProgress, _, err := claimLogs(ctx, repo, defaultInspectLimit)
	if err != nil || inProgress == nil {
		t.Fatalf("claim %v", err)
	}
	failed, _, err := claimLogs(ctx, repo, defaultInspectLimit)
	if err != nil || failed == nil {
		t.Fatalf("claim %v", err)
	}
	if _, _, err = repo.Nack(ctx, failed.ID, "worker", 0, 1, "boom"); err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks[2:5] {
		restoreRow(t, legacy, task)
	}
	notExpired := enqueue("", "", "", time.Time{}, base.Add(10*time.Minute), 100)
	tasks = append(tasks, notExpired)
	at := base.Add(61 * time.Second).UnixNano()
	clock.Store(at)
	batched := cloneFixture(t, legacy, at)
	assertSameKeyspace(t, legacy, batched)

	rows := func(db *DB) []*domain.Task {
		out := make([]*domain.Task, 0, len(tasks))
		for _, task := range tasks {
			row, err := db.absoluteTaskRow(task.ID)
			if err != nil || row == nil {
				t.Fatalf("row %s: %v", task.ID, err)
			}
			out = append(out, row)
		}
		return out
	}
	for _, row := range rows(legacy) {
		if err := legacy.legacyPurgeAbsoluteRow(row); err != nil {
			t.Fatal(err)
		}
	}
	writes := countWrites(batched)
	retired, err := batched.purgeAbsoluteRows(rows(batched))
	if err != nil {
		t.Fatal(err)
	}
	if n := writes.Load(); n != 1 {
		t.Fatalf("batched writes=%d want 1", n)
	}
	for i, ok := range retired {
		if ok != (tasks[i].ID != notExpired.ID) {
			t.Fatalf("retired[%d]=%v for %s", i, ok, tasks[i].ID)
		}
	}
	assertSameKeyspace(t, legacy, batched)
	if count, err := batched.absoluteCount(); err != nil || count != int64(len(tasks)) {
		t.Fatalf("count=%d err=%v want %d (three restored rows counted)", count, err, len(tasks))
	}
}

// The batched sweep leaves the same keyspace as the per-identity sweep when it
// retires rows and drops identities whose replay window closed, including a
// stored count that is lower than the identities it drops (clamped at zero).
func TestBatchedSweepMatchesPerIdentitySweep(t *testing.T) {
	for _, skew := range []bool{false, true} {
		t.Run(fmt.Sprintf("count-skewed=%v", skew), func(t *testing.T) {
			legacy, repo, _, clock := absoluteFixture(t)
			base := time.Unix(0, clock.Load()).UTC()
			// Six rows retire now; their identities outlive the replay
			// window. Four rows expire later and are retired by the sweep.
			for i := range 6 {
				ctx := domain.WithTaskExpiry(context.Background(), base.Add(time.Minute))
				if _, err := repo.Enqueue(ctx, absoluteTestTopic, `{}`, 3, "", 100, "", "", fmt.Sprintf("old-%d", i), time.Time{}, "tenant-a"); err != nil {
					t.Fatal(err)
				}
			}
			for i := range 4 {
				enqueueAbsolute(t, repo, clock, fmt.Sprintf("new-%d", i), "", time.Time{}, 100)
			}
			clock.Store(base.Add(2 * time.Minute).UnixNano())
			if n, err := legacy.sweepAbsolute(context.Background(), 512); err != nil || n != 6 {
				t.Fatalf("setup sweep=%d %v", n, err)
			}
			if skew {
				if err := legacy.Set([]byte(absoluteCountKey), []byte("2")); err != nil {
					t.Fatal(err)
				}
			}
			at := base.Add(absoluteReplayWindow + 5*time.Minute).UnixNano()
			clock.Store(at)
			batched := cloneFixture(t, legacy, at)
			legacyRemoved, err := legacy.legacySweepAbsolute(512)
			if err != nil {
				t.Fatal(err)
			}
			writes := countWrites(batched)
			removed, err := batched.sweepAbsolute(context.Background(), 512)
			if err != nil || removed != legacyRemoved || removed != 4 {
				t.Fatalf("removed=%d legacy=%d err=%v", removed, legacyRemoved, err)
			}
			if n := writes.Load(); n != 1 {
				t.Fatalf("batched sweep writes=%d want 1", n)
			}
			assertSameKeyspace(t, legacy, batched)
		})
	}
}

// A row that fails to stage does not leave half its deletions in the batch:
// the rows before it retire, it and the rows after it stay, and the error
// is returned.
func TestBatchedPurgeStopsAtFirstStagingError(t *testing.T) {
	db, repo, _, clock := absoluteFixture(t)
	seedExpiredHeads(t, repo, clock, 5)
	ids := expiredIDs(5)
	if err := db.Set(absoluteKey(ids[2]), []byte(`{`)); err != nil {
		t.Fatal(err)
	}
	rows := make([]*domain.Task, 0, len(ids))
	for _, id := range ids {
		row, err := db.absoluteTaskRow(id)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	retired, err := db.purgeAbsoluteRows(rows)
	if err == nil {
		t.Fatal("staging error hidden")
	}
	for i, ok := range retired {
		if ok != (i < 2) {
			t.Fatalf("retired=%v", retired)
		}
	}
	assertRetired(t, db, ids[:2]...)
	for _, id := range ids[2:] {
		if has, _ := db.Has(KeyTask(id)); !has {
			t.Fatalf("%s retired past the failing row", id)
		}
	}
}
