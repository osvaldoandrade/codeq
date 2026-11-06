package pebble

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	pebbledb "github.com/cockroachdb/pebble"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// clockAdvancingReplicator models a CPU-starved Raft leader: its first
// replicated write takes long enough for the absolute-expiry clock to reach
// `to` before it returns.
type clockAdvancingReplicator struct {
	*ambiguousAbsoluteReplicator
	clock *atomic.Int64
	to    int64
	calls atomic.Int64
}

func (r *clockAdvancingReplicator) Replicate(repr []byte) error {
	err := r.ambiguousAbsoluteReplicator.Replicate(repr)
	if r.calls.Add(1) == 1 {
		r.clock.Store(r.to)
	}
	return err
}

func enqueueAbsoluteAt(t *testing.T, repo *TaskRepository, id string, deadline time.Time) {
	t.Helper()
	ctx := domain.WithTaskExpiry(context.Background(), deadline)
	if _, err := repo.Enqueue(ctx, absoluteTestTopic, `{}`, 3, "", 100, "", "", id, time.Time{}, "tenant-a"); err != nil {
		t.Fatal(err)
	}
}

func frontierIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("frontier-%d", i)
	}
	return ids
}

// A backlogged FIFO's first live row is the closest to its deadline. The
// claim leases it right after reading it, before the slow retirement commit
// of the expired heads, so the deadline cannot pass in between (v0.5.13 at
// 6e9ee41 retired first and then failed the claim with task_expired). The
// lease never outlives the row's deadline.
func TestClaimLeasesFrontierRowBeforeSlowRetirement(t *testing.T) {
	db, repo, _, clock := absoluteFixture(t)
	base := time.Unix(0, clock.Load()).UTC()
	for _, id := range expiredIDs(50) {
		enqueueAbsoluteAt(t, repo, id, base.Add(time.Minute))
	}
	frontierDeadline := base.Add(65 * time.Second)
	enqueueAbsoluteAt(t, repo, "frontier", frontierDeadline)
	enqueueAbsoluteAt(t, repo, "later", base.Add(10*time.Minute))
	clock.Store(base.Add(61 * time.Second).UnixNano())
	db.AttachReplicator(&clockAdvancingReplicator{ambiguousAbsoluteReplicator: &ambiguousAbsoluteReplicator{db: db.Raw()}, clock: clock, to: base.Add(66 * time.Second).UnixNano()})
	writes := countWrites(db)

	got, ok, err := claimLogs(context.Background(), repo, defaultInspectLimit)
	if err != nil || !ok || got.ID != "frontier" {
		t.Fatalf("claim=%+v ok=%v err=%v; want the frontier row leased", got, ok, err)
	}
	until, err := time.Parse(time.RFC3339Nano, got.LeaseUntil)
	if err != nil || until.After(frontierDeadline) {
		t.Fatalf("lease until %v (%v) outlives the deadline %v", until, err, frontierDeadline)
	}
	if n := writes.Load(); n != 2 {
		t.Fatalf("writes=%d; want the lease and one retirement batch", n)
	}
	assertRetired(t, db, expiredIDs(50)...)
	if hints := hintCount(repo); hints != 1 || len(repo.inflight) != 0 {
		t.Fatalf("hints=%d inflight=%v; want only the later row's", hints, repo.inflight)
	}
	// The next claim, after the slow commit moved the clock, leases the
	// later row.
	got, ok, err = claimLogs(context.Background(), repo, defaultInspectLimit)
	if err != nil || !ok || got.ID != "later" {
		t.Fatalf("next claim=%+v ok=%v err=%v", got, ok, err)
	}
}

// A live row whose deadline passes between its read and its lease commit is
// held and retired with the expired heads instead of failing the claim, and
// the claim leases the next live row. A frontier that keeps moving (every
// candidate expires at its lease) costs refused lease attempts that write
// nothing: the claim still commits once for the lease and once for the
// retirement batch.
func TestClaimHoldsRowsThatExpireAtLeaseTime(t *testing.T) {
	for _, frontier := range []int{1, 5} {
		t.Run(fmt.Sprintf("frontier=%d", frontier), func(t *testing.T) {
			db, repo, _, clock := absoluteFixture(t)
			base := time.Unix(0, clock.Load()).UTC()
			for _, id := range expiredIDs(50) {
				enqueueAbsoluteAt(t, repo, id, base.Add(time.Minute))
			}
			// frontier-i expires at base+62s+i; each lease attempt moves the
			// clock one second, onto the deadline of the row it stamps.
			for i, id := range frontierIDs(frontier) {
				enqueueAbsoluteAt(t, repo, id, base.Add(time.Duration(62+i)*time.Second))
			}
			enqueueAbsoluteAt(t, repo, "later", base.Add(10*time.Minute))
			clock.Store(base.Add(61 * time.Second).UnixNano())
			db.AttachReplicator(&ambiguousAbsoluteReplicator{db: db.Raw()})
			attempts := atomic.Int64{}
			repo.clock = func() time.Time {
				attempts.Add(1)
				clock.Add(int64(time.Second))
				return time.Now()
			}
			writes := countWrites(db)

			got, ok, err := claimLogs(context.Background(), repo, defaultInspectLimit)
			if err != nil || !ok || got.ID != "later" {
				t.Fatalf("claim=%+v ok=%v err=%v; want the later row leased", got, ok, err)
			}
			if n := attempts.Load(); n != int64(frontier+1) {
				t.Fatalf("lease attempts=%d want %d", n, frontier+1)
			}
			if n := writes.Load(); n != 2 {
				t.Fatalf("writes=%d; want the lease and one retirement batch", n)
			}
			assertRetired(t, db, append(expiredIDs(50), frontierIDs(frontier)...)...)
			if hints := hintCount(repo); hints != 0 || len(repo.inflight) != 0 {
				t.Fatalf("hints=%d inflight=%v", hints, repo.inflight)
			}
			if count, err := db.absoluteCount(); err != nil || count != int64(50+frontier+1) {
				t.Fatalf("count=%d err=%v", count, err)
			}
		})
	}
}

// ClaimMany drops a candidate whose deadline passed before its lease batch
// committed, retries the rest without committing in between, and retires the
// dropped rows with the expired ones.
func TestClaimManyHoldsRowsThatExpireAtLeaseTime(t *testing.T) {
	db, repo, _, clock := absoluteFixture(t)
	base := time.Unix(0, clock.Load()).UTC()
	for _, id := range expiredIDs(10) {
		enqueueAbsoluteAt(t, repo, id, base.Add(time.Minute))
	}
	for i, id := range frontierIDs(3) {
		enqueueAbsoluteAt(t, repo, id, base.Add(time.Duration(62+i)*time.Second))
	}
	enqueueAbsoluteAt(t, repo, "later-0", base.Add(10*time.Minute))
	enqueueAbsoluteAt(t, repo, "later-1", base.Add(10*time.Minute))
	clock.Store(base.Add(61 * time.Second).UnixNano())
	repo.clock = func() time.Time {
		clock.Add(int64(time.Second))
		return time.Now()
	}
	writes := countWrites(db)
	tasks, err := repo.ClaimMany(context.Background(), "worker", []domain.Command{absoluteTestTopic}, 60, 100, 0, 100, "tenant-a")
	if err != nil || len(tasks) != 2 || tasks[0].ID != "later-0" || tasks[1].ID != "later-1" {
		t.Fatalf("claim-many=%v err=%v", tasks, err)
	}
	if n := writes.Load(); n != 2 {
		t.Fatalf("writes=%d; want one lease batch and one retirement batch", n)
	}
	assertRetired(t, db, append(expiredIDs(10), frontierIDs(3)...)...)
	for _, task := range tasks {
		if _, ok := db.Leases.Get(task.ID); !ok {
			t.Fatalf("%s has no lease", task.ID)
		}
	}
	if hints := hintCount(repo); hints != 0 || len(repo.inflight) != 0 {
		t.Fatalf("hints=%d inflight=%v", hints, repo.inflight)
	}
}

// An expired row whose identity cannot be staged does not cost the claim its
// lease: the live task is leased, the other expired rows retire, and the bad
// row keeps its hint. It cannot pin the rows behind it on later claims.
func TestClaimLeasesPastRowThatFailsToRetire(t *testing.T) {
	db, repo, _, clock := absoluteFixture(t)
	live := seedExpiredHeads(t, repo, clock, 5)
	ids := expiredIDs(5)
	if err := db.Set(absoluteKey(ids[1]), []byte(`{`)); err != nil {
		t.Fatal(err)
	}
	writes := countWrites(db)
	got, ok, err := claimLogs(context.Background(), repo, defaultInspectLimit)
	if err != nil || !ok || got.ID != live.ID {
		t.Fatalf("claim=%+v ok=%v err=%v; want the live task leased", got, ok, err)
	}
	if n := writes.Load(); n != 2 {
		t.Fatalf("writes=%d want 2", n)
	}
	assertRetired(t, db, ids[0], ids[2], ids[3], ids[4])
	if has, _ := db.Has(KeyTask(ids[1])); !has {
		t.Fatalf("%s retired with a corrupt identity", ids[1])
	}
	if hints := hintCount(repo); hints != 1 || len(repo.inflight) != 0 {
		t.Fatalf("hints=%d inflight=%v; want only the bad row's", hints, repo.inflight)
	}
	// With nothing live left, the claim reports the row it cannot retire.
	if got, ok, err = claimLogs(context.Background(), repo, defaultInspectLimit); err == nil || ok || got != nil {
		t.Fatalf("claim=%+v ok=%v err=%v; want the retirement error", got, ok, err)
	}
	if hints := hintCount(repo); hints != 1 {
		t.Fatalf("hints=%d; the bad row must keep its hint", hints)
	}
}

// epochReplicator reports a leadership epoch and, after its first replicated
// write, applies a foreign identity-count change and/or a new leadership
// term, as another leader's writes would appear between two flushes.
type epochReplicator struct {
	*ambiguousAbsoluteReplicator
	epoch      atomic.Uint64
	calls      atomic.Int64
	foreign    int64
	bumpEpoch  bool
	foreignErr error
}

func (r *epochReplicator) LeadershipEpoch() uint64 { return r.epoch.Load() }

func (r *epochReplicator) Replicate(repr []byte) error {
	if err := r.ambiguousAbsoluteReplicator.Replicate(repr); err != nil {
		return err
	}
	if r.calls.Add(1) != 1 {
		return nil
	}
	if r.foreign != 0 {
		r.foreignErr = r.addForeignCount()
	}
	if r.bumpEpoch {
		r.epoch.Add(1)
	}
	return nil
}

func (r *epochReplicator) addForeignCount() error {
	raw, closer, err := r.db.Get([]byte(absoluteCountKey))
	if err != nil {
		return err
	}
	count, err := strconv.ParseInt(string(raw), 10, 64)
	_ = closer.Close()
	if err != nil {
		return err
	}
	return r.db.Set([]byte(absoluteCountKey), []byte(strconv.FormatInt(count+r.foreign, 10)), pebbledb.Sync)
}

// A retirement larger than one batch rereads the identity count after each
// commit instead of carrying the value it read at the start, and abandons the
// rest when leadership changed since it started.
func TestPurgeAbsoluteRowsAcrossFlushes(t *testing.T) {
	const expired = absoluteRetireBatch + 44
	setup := func(t *testing.T, repl *epochReplicator) (*DB, []*domain.Task) {
		t.Helper()
		db, repo, _, clock := absoluteFixture(t)
		seedExpiredHeads(t, repo, clock, expired)
		rows := make([]*domain.Task, 0, expired)
		for _, id := range expiredIDs(expired) {
			row, err := db.absoluteTaskRow(id)
			if err != nil || row == nil {
				t.Fatalf("row %s: %v", id, err)
			}
			rows = append(rows, row)
		}
		// A restored row in the second flush makes it write the count.
		restoreRow(t, db, rows[expired-1])
		repl.ambiguousAbsoluteReplicator = &ambiguousAbsoluteReplicator{db: db.Raw()}
		db.AttachReplicator(repl)
		return db, rows
	}
	t.Run("count-reread", func(t *testing.T) {
		repl := &epochReplicator{foreign: 7}
		db, rows := setup(t, repl)
		before, err := db.absoluteCount()
		if err != nil {
			t.Fatal(err)
		}
		writes := countWrites(db)
		retired, err := db.purgeAbsoluteRows(rows)
		if err != nil || repl.foreignErr != nil {
			t.Fatalf("purge err=%v foreign=%v", err, repl.foreignErr)
		}
		if n := writes.Load(); n != 2 {
			t.Fatalf("writes=%d want 2", n)
		}
		for i, ok := range retired {
			if !ok {
				t.Fatalf("row %d not retired", i)
			}
		}
		// +7 from the foreign write, +1 for the restored row.
		if count, err := db.absoluteCount(); err != nil || count != before+8 {
			t.Fatalf("count=%d err=%v want %d", count, err, before+8)
		}
		// The restored row has no reverse references to purge by; only its
		// payload and identity are checked.
		assertRetired(t, db, expiredIDs(expired-1)...)
		if has, _ := db.Has(KeyTask(rows[expired-1].ID)); has {
			t.Fatal("restored row kept its payload")
		}
	})
	t.Run("leadership-changed", func(t *testing.T) {
		repl := &epochReplicator{foreign: 7, bumpEpoch: true}
		db, rows := setup(t, repl)
		before, err := db.absoluteCount()
		if err != nil {
			t.Fatal(err)
		}
		writes := countWrites(db)
		retired, err := db.purgeAbsoluteRows(rows)
		if !errors.Is(err, errAbsoluteRetirementTermChanged) {
			t.Fatalf("err=%v; want the term change", err)
		}
		if n := writes.Load(); n != 1 {
			t.Fatalf("writes=%d; want only the first flush", n)
		}
		for i, ok := range retired {
			if ok != (i < absoluteRetireBatch) {
				t.Fatalf("retired[%d]=%v", i, ok)
			}
		}
		assertRetired(t, db, expiredIDs(absoluteRetireBatch)...)
		assertUnretained(t, db, expiredIDs(expired)[absoluteRetireBatch:]...)
		// The abandoned flush did not overwrite the other leader's count.
		if count, err := db.absoluteCount(); err != nil || count != before+7 {
			t.Fatalf("count=%d err=%v want %d", count, err, before+7)
		}
	})
}
