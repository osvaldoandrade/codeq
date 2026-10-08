package schedules

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/internal/core/schedule"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// memStore is a one-schedule Store with the catalog's Due/Advance contract.
type memStore struct {
	mu         sync.Mutex
	sc         schedule.Schedule
	advanceErr error
	advances   int
	dueScans   int
}

func (m *memStore) Upsert(context.Context, schedule.Schedule) (schedule.Schedule, bool, error) {
	return schedule.Schedule{}, false, errors.New("unused")
}

func (m *memStore) Get(context.Context, string, string) (schedule.Schedule, error) {
	return m.sc, nil
}
func (m *memStore) List(context.Context, string) ([]schedule.Schedule, error) { return nil, nil }
func (m *memStore) Delete(context.Context, string, string) error              { return nil }

func (m *memStore) Due(_ context.Context, now time.Time) ([]schedule.Schedule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dueScans++
	if m.sc.Spec.Paused || m.sc.NextRunAt.After(now) {
		return nil, nil
	}
	return []schedule.Schedule{m.sc}, nil
}

func (m *memStore) Advance(_ context.Context, id string, version int64, fired time.Time, taskID string, next time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.advanceErr != nil {
		err := m.advanceErr
		m.advanceErr = nil
		return false, err
	}
	if id != m.sc.ScheduleID || version != m.sc.Version || !m.sc.NextRunAt.Equal(fired) {
		return false, nil
	}
	m.advances++
	f := fired
	m.sc.LastRunAt, m.sc.LastTaskID, m.sc.NextRunAt = &f, taskID, next
	return true, nil
}

// idempotentCreator records creates and, like the scheduler, returns the
// task already created for a repeated idempotency key.
type idempotentCreator struct {
	mu     sync.Mutex
	byKey  map[string]string
	calls  []string
	failOn int // fail the n-th call (1-based); 0 never
}

func (c *idempotentCreator) CreateTask(_ context.Context, cmd domain.Command, _ string, _ int, _ string, _ int, key string, _ time.Time, _ int, tenant string) (*domain.Task, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, key)
	if c.failOn == len(c.calls) {
		return nil, errors.New("store unavailable")
	}
	if c.byKey == nil {
		c.byKey = map[string]string{}
	}
	id, ok := c.byKey[key]
	if !ok {
		id = "task-" + time.Now().Format("150405.000000000")
		c.byKey[key] = id
	}
	return &domain.Task{ID: id, Command: cmd, TenantID: tenant}, nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

var runnerStart = time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)

func newRunnerFixture(t *testing.T, rule string) (*Runner, *memStore, *idempotentCreator, *clock) {
	t.Helper()
	clk := &clock{t: runnerStart}
	sc, err := schedule.New("tenant-a", "sync", schedule.Spec{Cron: rule, Command: "SYNC"}, ParseCron, clk.now())
	if err != nil {
		t.Fatalf("schedule.New: %v", err)
	}
	sc.Version = 1
	store := &memStore{sc: sc}
	creator := &idempotentCreator{}
	r := NewRunner(store, creator, ParseCron, RunnerOptions{Now: clk.now})
	return r, store, creator, clk
}

func TestRunnerFiresEachSlotOnce(t *testing.T) {
	r, store, creator, clk := newRunnerFixture(t, "*/5 * * * *")
	ctx := context.Background()

	r.tick(ctx) // 10:00:00 — first slot is 10:05
	if len(creator.calls) != 0 {
		t.Fatalf("fired before the first slot: %v", creator.calls)
	}
	for _, at := range []string{"10:05:00", "10:05:30", "10:09:59", "10:10:00", "10:10:00"} {
		clk.t, _ = time.Parse(time.RFC3339, "2026-10-08T"+at+"Z")
		r.tick(ctx)
	}
	want := []string{
		schedule.SlotKey("tenant-a.sync", runnerStart.Add(5*time.Minute)),
		schedule.SlotKey("tenant-a.sync", runnerStart.Add(10*time.Minute)),
	}
	if len(creator.calls) != 2 || creator.calls[0] != want[0] || creator.calls[1] != want[1] {
		t.Fatalf("creates = %q, want one per slot %q", creator.calls, want)
	}
	if !store.sc.NextRunAt.Equal(runnerStart.Add(15 * time.Minute)) {
		t.Fatalf("next slot = %s, want 10:15", store.sc.NextRunAt)
	}
}

// After downtime, the earliest missed slot fires once and the schedule
// resumes from the first slot after now — no burst of catch-up tasks.
func TestRunnerCoalescesMissedSlots(t *testing.T) {
	r, store, creator, clk := newRunnerFixture(t, "* * * * *")
	clk.t = runnerStart.Add(37*time.Minute + 10*time.Second)
	r.tick(context.Background())
	r.tick(context.Background())
	if len(creator.calls) != 1 || creator.calls[0] != schedule.SlotKey("tenant-a.sync", runnerStart.Add(time.Minute)) {
		t.Fatalf("creates = %q, want only the earliest missed slot", creator.calls)
	}
	if !store.sc.NextRunAt.Equal(runnerStart.Add(38 * time.Minute)) {
		t.Fatalf("next slot = %s, want the first slot after now (10:38)", store.sc.NextRunAt)
	}
}

// A follower neither fires nor scans the catalog every tick.
func TestRunnerFiresOnlyOnTheLeader(t *testing.T) {
	r, store, creator, clk := newRunnerFixture(t, "* * * * *")
	leader := false
	r.leader = func() bool { return leader }
	clk.t = runnerStart.Add(time.Minute)
	r.tick(context.Background())
	if len(creator.calls) != 0 || store.dueScans != 0 {
		t.Fatalf("a follower fired (%d) or scanned the catalog (%d)", len(creator.calls), store.dueScans)
	}
	leader = true
	r.tick(context.Background())
	if len(creator.calls) != 1 {
		t.Fatalf("the leader did not fire: %v", creator.calls)
	}
}

// A failed create leaves the slot unrecorded; the next tick retries the same
// slot key.
func TestRunnerRetriesAFailedCreate(t *testing.T) {
	r, store, creator, clk := newRunnerFixture(t, "* * * * *")
	creator.failOn = 1
	clk.t = runnerStart.Add(time.Minute)
	r.tick(context.Background())
	if store.advances != 0 {
		t.Fatal("a failed create must not record the slot")
	}
	r.tick(context.Background())
	if len(creator.calls) != 2 || creator.calls[0] != creator.calls[1] || store.advances != 1 {
		t.Fatalf("retry = %q advances=%d; want the same slot key and one record", creator.calls, store.advances)
	}
}

// A slot whose task was created but whose record failed (a crash, a lost
// leadership) fires again with the same key and gets the same task: one
// task per slot, whoever fires it.
func TestRunnerReplayAfterFailedAdvanceKeepsOneTask(t *testing.T) {
	r, store, creator, clk := newRunnerFixture(t, "* * * * *")
	store.advanceErr = errors.New("not leader")
	clk.t = runnerStart.Add(time.Minute)
	r.tick(context.Background())
	r.tick(context.Background())
	if len(creator.calls) != 2 || creator.calls[0] != creator.calls[1] {
		t.Fatalf("calls = %q, want the same slot key twice", creator.calls)
	}
	if len(creator.byKey) != 1 || store.advances != 1 || store.sc.LastTaskID != creator.byKey[creator.calls[0]] {
		t.Fatalf("distinct tasks = %d, advances = %d; want 1 and 1", len(creator.byKey), store.advances)
	}
}

// A spec change between Due and Advance makes the fire a no-op: the next
// tick follows the new spec.
func TestRunnerSupersededByConcurrentUpdate(t *testing.T) {
	r, store, _, clk := newRunnerFixture(t, "* * * * *")
	clk.t = runnerStart.Add(time.Minute)
	due, _ := store.Due(context.Background(), clk.t)
	store.sc.Version = 2 // an update lands after the scan
	r.fire(context.Background(), due[0])
	if store.advances != 0 || store.sc.LastRunAt != nil {
		t.Fatal("a fire for a superseded version must not record the slot")
	}
}

func TestRunnerStartStopsWithContext(t *testing.T) {
	r, _, creator, clk := newRunnerFixture(t, "* * * * *")
	clk.t = runnerStart.Add(time.Minute)
	r.interval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	r.Start(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		creator.mu.Lock()
		n := len(creator.calls)
		creator.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	creator.mu.Lock()
	defer creator.mu.Unlock()
	if len(creator.calls) != 1 {
		t.Fatalf("the started loop fired %d times for one due slot, want 1", len(creator.calls))
	}
}
