package schedules

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/internal/core/schedule"
	pebblerepo "github.com/osvaldoandrade/codeq/internal/repository/pebble"
	"github.com/osvaldoandrade/codeq/internal/services"
	schedulestore "github.com/osvaldoandrade/codeq/internal/storage/adapter/pebble"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// flakyAdvance fails the first Advance, as a crash or a lost leadership
// between creating a slot's task and recording the slot would.
type flakyAdvance struct {
	*schedulestore.ScheduleStore
	once sync.Once
}

func (f *flakyAdvance) Advance(ctx context.Context, id string, version int64, fired time.Time, taskID string, next time.Time) (bool, error) {
	var err error
	f.once.Do(func() { err = errors.New("lost leadership") })
	if err != nil {
		return false, err
	}
	return f.ScheduleStore.Advance(ctx, id, version, fired, taskID, next)
}

// TestRunnerOneTaskPerSlotWithRealStorage wires the real Pebble task
// repository, scheduler service and schedule catalog, fails the first slot
// record, and checks that re-firing the slot reuses the task: the queue
// holds exactly one task for the slot.
func TestRunnerOneTaskPerSlotWithRealStorage(t *testing.T) {
	ctx := context.Background()
	db, err := pebblerepo.Open(pebblerepo.Options{Path: t.TempDir()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := pebblerepo.NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	scheduler := services.NewSchedulerService(repo, nil, nil, time.UTC, time.Now, 60, 50, 5, "fixed", 1, 5)
	store := &flakyAdvance{ScheduleStore: schedulestore.NewScheduleStore(db)}

	clk := &clock{t: runnerStart}
	desired, err := schedule.New("tenant-a", "sync", schedule.Spec{Cron: "* * * * *", Command: "SYNC", Payload: []byte(`{"full":true}`)}, ParseCron, clk.now())
	if err != nil {
		t.Fatalf("schedule.New: %v", err)
	}
	if _, _, err := store.Upsert(ctx, desired); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	r := NewRunner(store, scheduler, ParseCron, RunnerOptions{Now: clk.now})

	clk.t = runnerStart.Add(time.Minute + time.Second)
	r.tick(ctx) // creates the task, fails to record the slot
	r.tick(ctx) // re-fires the slot: same key, same task, slot recorded
	r.tick(ctx) // nothing due until 10:02

	stats, err := repo.QueueStats(ctx, domain.Command("SYNC"), "tenant-a")
	if err != nil || stats.Ready != 1 {
		t.Fatalf("ready = %+v, %v; want exactly one task for the slot", stats, err)
	}
	got, err := store.Get(ctx, "tenant-a", "sync")
	if err != nil || got.LastTaskID == "" || !got.NextRunAt.Equal(runnerStart.Add(2*time.Minute)) {
		t.Fatalf("schedule after replay = %+v, %v", got, err)
	}
	task, err := repo.Get(ctx, got.LastTaskID)
	if err != nil || task.Payload != `{"full":true}` || task.TenantID != "tenant-a" {
		t.Fatalf("enqueued task = %+v, %v", task, err)
	}

	clk.t = runnerStart.Add(2*time.Minute + time.Second)
	r.tick(ctx)
	if stats, _ := repo.QueueStats(ctx, domain.Command("SYNC"), "tenant-a"); stats.Ready != 2 {
		t.Fatalf("next slot: ready = %d, want 2", stats.Ready)
	}
}
