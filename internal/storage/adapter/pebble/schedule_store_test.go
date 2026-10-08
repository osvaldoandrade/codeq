package pebble

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/internal/core/schedule"
	pebblerepo "github.com/osvaldoandrade/codeq/internal/repository/pebble"
)

const (
	scheduleTenant = "acme"
	hourlyRule     = "@hourly"
)

// minuteRule fires on every minute boundary, enough to exercise the store
// without the cron dialect (which the application layer owns).
type minuteRule struct{}

func (minuteRule) Next(t time.Time) time.Time { return t.Truncate(time.Minute).Add(time.Minute) }

type hourRule struct{}

func (hourRule) Next(t time.Time) time.Time { return t.Truncate(time.Hour).Add(time.Hour) }

func ruleParser(cron, _ string) (schedule.Expression, error) {
	if cron == hourlyRule {
		return hourRule{}, nil
	}
	return minuteRule{}, nil
}

func mustSchedule(t *testing.T, tenant, name string, spec schedule.Spec, now time.Time) schedule.Schedule {
	t.Helper()
	if spec.Cron == "" {
		spec.Cron = "* * * * *"
	}
	if spec.Command == "" {
		spec.Command = "SYNC"
	}
	sc, err := schedule.New(tenant, name, spec, ruleParser, now)
	if err != nil {
		t.Fatalf("schedule.New: %v", err)
	}
	return sc
}

func openScheduleDB(t *testing.T, dir string) *pebblerepo.DB {
	t.Helper()
	db, err := pebblerepo.Open(pebblerepo.Options{Path: dir})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return db
}

func TestScheduleStoreUpsertSemanticsAndRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := openScheduleDB(t, dir)
	store := NewScheduleStore(db)

	created, wasCreated, err := store.Upsert(ctx, mustSchedule(t, scheduleTenant, "sync", schedule.Spec{Priority: 1}, testNow))
	if err != nil || !wasCreated || created.Version != 1 || !created.NextRunAt.Equal(testNow.Add(time.Minute)) {
		t.Fatalf("create = %+v created=%v err=%v", created, wasCreated, err)
	}
	replay, wasCreated, err := store.Upsert(ctx, mustSchedule(t, scheduleTenant, "sync", schedule.Spec{Priority: 1}, testNow.Add(5*time.Minute)))
	if err != nil || wasCreated || replay.Version != 1 || !replay.NextRunAt.Equal(created.NextRunAt) {
		t.Fatalf("identical replay must change nothing: %+v created=%v err=%v", replay, wasCreated, err)
	}

	// Same timing, new priority: the next slot is kept.
	sameTiming, _, err := store.Upsert(ctx, mustSchedule(t, scheduleTenant, "sync", schedule.Spec{Priority: 7}, testNow.Add(30*time.Minute)))
	if err != nil || sameTiming.Version != 2 || !sameTiming.NextRunAt.Equal(created.NextRunAt) || !sameTiming.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("same-timing update = %+v err=%v", sameTiming, err)
	}
	// New timing: the next slot is recomputed from the update time.
	retimed, _, err := store.Upsert(ctx, mustSchedule(t, scheduleTenant, "sync", schedule.Spec{Cron: hourlyRule, Priority: 7}, testNow.Add(30*time.Minute)))
	if err != nil || retimed.Version != 3 || !retimed.NextRunAt.Equal(testNow.Add(time.Hour)) {
		t.Fatalf("retimed update = %+v err=%v", retimed, err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	db = openScheduleDB(t, dir)
	defer db.Close()
	store = NewScheduleStore(db)
	got, err := store.Get(ctx, scheduleTenant, "sync")
	if err != nil || got.Version != 3 || got.Spec.Cron != hourlyRule {
		t.Fatalf("after restart = %+v err=%v", got, err)
	}
}

func TestScheduleStoreTenantIsolationListAndDelete(t *testing.T) {
	ctx := context.Background()
	db := openScheduleDB(t, t.TempDir())
	defer db.Close()
	store := NewScheduleStore(db)
	for _, n := range []string{"b-job", "a-job"} {
		if _, _, err := store.Upsert(ctx, mustSchedule(t, scheduleTenant, n, schedule.Spec{}, testNow)); err != nil {
			t.Fatalf("upsert %s: %v", n, err)
		}
	}
	if _, _, err := store.Upsert(ctx, mustSchedule(t, "acme-2", "a-job", schedule.Spec{}, testNow)); err != nil {
		t.Fatalf("upsert other tenant: %v", err)
	}

	list, err := store.List(ctx, scheduleTenant)
	if err != nil || len(list) != 2 || list[0].Name != "a-job" || list[1].Name != "b-job" {
		t.Fatalf("list = %+v err=%v; want a-job, b-job of acme only", list, err)
	}
	empty, err := store.List(ctx, "nobody")
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty tenant list = %#v err=%v; want a non-nil empty slice", empty, err)
	}
	var nf *schedule.NotFoundError
	if _, err := store.Get(ctx, "acme-2", "b-job"); !errors.As(err, &nf) {
		t.Fatalf("cross-tenant get: err %v, want NotFoundError", err)
	}
	for range 2 {
		if err := store.Delete(ctx, scheduleTenant, "a-job"); err != nil {
			t.Fatalf("delete (idempotent): %v", err)
		}
	}
	if _, err := store.Get(ctx, scheduleTenant, "a-job"); !errors.As(err, &nf) {
		t.Fatalf("after delete: err %v, want NotFoundError", err)
	}
	if _, err := store.Get(ctx, "acme-2", "a-job"); err != nil {
		t.Fatalf("other tenant's schedule must survive: %v", err)
	}
}

func TestScheduleStoreDueAndAdvanceCompareAndSet(t *testing.T) {
	ctx := context.Background()
	db := openScheduleDB(t, t.TempDir())
	defer db.Close()
	store := NewScheduleStore(db)
	early, _, _ := store.Upsert(ctx, mustSchedule(t, scheduleTenant, "early", schedule.Spec{}, testNow))
	late, _, _ := store.Upsert(ctx, mustSchedule(t, "acme-2", "late", schedule.Spec{Cron: hourlyRule}, testNow))
	_, _, _ = store.Upsert(ctx, mustSchedule(t, scheduleTenant, "paused", schedule.Spec{Paused: true}, testNow))

	due, err := store.Due(ctx, testNow.Add(2*time.Hour))
	if err != nil || len(due) != 2 || due[0].ScheduleID != early.ScheduleID || due[1].ScheduleID != late.ScheduleID {
		t.Fatalf("due = %+v err=%v; want early then late, never the paused one", due, err)
	}
	if due, _ := store.Due(ctx, testNow); len(due) != 0 {
		t.Fatalf("nothing is due before its slot: %+v", due)
	}

	slot, next := early.NextRunAt, early.NextRunAt.Add(time.Minute)
	ok, err := store.Advance(ctx, early.ScheduleID, early.Version, slot, "task-1", next)
	if err != nil || !ok {
		t.Fatalf("advance: %v %v", ok, err)
	}
	got, _ := store.Get(ctx, scheduleTenant, "early")
	if got.LastTaskID != "task-1" || got.LastRunAt == nil || !got.LastRunAt.Equal(slot) || !got.NextRunAt.Equal(next) || got.Version != early.Version {
		t.Fatalf("after advance = %+v", got)
	}
	for _, tc := range []struct {
		name    string
		version int64
		slot    time.Time
	}{
		{"same slot again", early.Version, slot},
		{"stale version", early.Version - 1, next},
	} {
		if ok, err := store.Advance(ctx, early.ScheduleID, tc.version, tc.slot, "task-x", next.Add(time.Minute)); err != nil || ok {
			t.Fatalf("%s: advance applied=%v err=%v; want a no-op", tc.name, ok, err)
		}
	}
	if ok, _ := store.Advance(ctx, "acme.missing", 1, slot, "t", next); ok {
		t.Fatal("advancing a missing schedule must be a no-op")
	}
}

// Concurrent fires of one slot record it once: Advance is a
// compare-and-set under the store mutex.
func TestScheduleStoreConcurrentAdvanceAppliesOnce(t *testing.T) {
	ctx := context.Background()
	db := openScheduleDB(t, t.TempDir())
	defer db.Close()
	store := NewScheduleStore(db)
	sc, _, _ := store.Upsert(ctx, mustSchedule(t, scheduleTenant, "sync", schedule.Spec{}, testNow))

	var applied atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, err := store.Advance(ctx, sc.ScheduleID, sc.Version, sc.NextRunAt, "t", sc.NextRunAt.Add(time.Minute)); err == nil && ok {
				applied.Add(1)
			}
		}()
	}
	wg.Wait()
	if applied.Load() != 1 {
		t.Fatalf("slot recorded %d times, want 1", applied.Load())
	}
}
