package pebble

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/internal/core/queuetopic"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const (
	absolutePendingScenario  = "pending"
	absoluteRequeuedScenario = "requeued"
	absoluteNackedScenario   = "nacked"
	absoluteTestWorker       = "worker"

	absoluteDelayedScenario   = "delayed"
	absoluteFailedScenario    = "failed"
	absoluteTestTopic         = "cf-logs"
	absoluteCompletedScenario = "completed"
)

func TestAbsoluteExpiryHidesAndPurgesPendingBody(t *testing.T) {
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	task, err := repo.Enqueue(context.Background(), domain.Command(absoluteTestTopic), `{"batch":1}`, 0, "", 100, "", "", "logs-expired", time.Time{}, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err = json.Unmarshal(body, &row); err != nil {
		t.Fatal(err)
	}
	row["expiresAt"] = time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
	body, err = json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an already accepted absolute row surviving a restart.
	if err = db.Set(KeyTask(task.ID), body); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.Get(context.Background(), task.ID); err == nil || got != nil {
		t.Fatalf("expired body visible: %+v, %v", got, err)
	}
	if n, err := repo.CleanupExpired(context.Background(), 100, time.Now().Add(48*time.Hour)); err != nil || n != 1 {
		t.Fatalf("absolute pending cleanup = %d, %v", n, err)
	}
	if exists, err := db.Has(KeyTask(task.ID)); err != nil || exists {
		t.Fatalf("body remained: %v %v", exists, err)
	}
	if count, err := db.absoluteCount(); err != nil || count != 1 {
		t.Fatalf("restored identity not counted: %d %v", count, err)
	}
	if active, err := db.HasAbsoluteTopicCustody(context.Background(), "tenant-a", absoluteTestTopic); err != nil || !active {
		t.Fatalf("restored tombstone lost topic custody: %v %v", active, err)
	}
}

type expiryPolicyReader func(context.Context, string, string) (queuetopic.Topic, error)

func (f expiryPolicyReader) Get(ctx context.Context, tenant, name string) (queuetopic.Topic, error) {
	return f(ctx, tenant, name)
}

func absoluteFixture(t *testing.T) (*DB, *TaskRepository, *ResultRepository, *atomic.Int64) {
	t.Helper()
	db, err := Open(Options{Path: t.TempDir(), FsyncOnCommit: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clock := &atomic.Int64{}
	clock.Store(time.Now().UTC().UnixNano())
	db.expiryNow = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	attachAbsolutePolicy(db, 7200, nil, time.Now())
	return db, NewTaskRepository(db, time.UTC, "fixed", 1, 5), NewResultRepository(db, time.UTC), clock
}

func attachAbsolutePolicy(db *DB, retention int, floor *time.Time, updated time.Time) {
	db.SetTopicPolicyReader(expiryPolicyReader(func(_ context.Context, tenant, name string) (queuetopic.Topic, error) {
		if tenant != "tenant-a" || name != absoluteTestTopic {
			return queuetopic.Topic{}, &queuetopic.NotFoundError{TopicID: tenant + "." + name}
		}
		return queuetopic.Topic{TenantID: tenant, TopicName: name, UpdatedAt: updated, Policy: queuetopic.Policy{AbsoluteExpiry: true, RetentionSeconds: retention, VisibilityFloor: floor}}, nil
	}))
}

func enqueueAbsolute(t *testing.T, repo *TaskRepository, clock *atomic.Int64, id, idem string, visible time.Time, maxAttempts int) *domain.Task {
	t.Helper()
	deadline := time.Unix(0, clock.Load()).Add(time.Hour).UTC()
	ctx := domain.WithTaskExpiry(context.Background(), deadline)
	task, err := repo.Enqueue(ctx, absoluteTestTopic, `{"batch":"sensitive-payload"}`, 3, "", maxAttempts, idem, "", id, visible, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if task.ExpiresAt == nil || !task.ExpiresAt.Equal(deadline) {
		t.Fatalf("deadline=%v want %v", task.ExpiresAt, deadline)
	}
	return task
}

func assertNoPayloadIndexes(t *testing.T, db *DB, id string) {
	t.Helper()
	for _, key := range [][]byte{KeyTask(id), KeyResult(id), KeyLease(id)} {
		if yes, e := db.Has(key); e != nil || yes {
			t.Fatalf("retained %q: %v %v", key, yes, e)
		}
	}
	for _, prefix := range []string{pQueue, pTTL, pAbsoluteRefs} {
		it, e := db.Iter([]byte(prefix), prefixUpper([]byte(prefix)))
		if e != nil {
			t.Fatal(e)
		}
		for valid := it.First(); valid; valid = it.Next() {
			if bytes.HasSuffix(it.Key(), []byte("/"+id)) || bytes.HasPrefix(it.Key(), absoluteRefs(id)) {
				t.Errorf("retained reference %x", it.Key())
			}
		}
		if e = it.Close(); e != nil {
			t.Fatal(e)
		}
	}
}

func exerciseAbsoluteState(t *testing.T, repo *TaskRepository, results *ResultRepository, task *domain.Task, scenario string) {
	t.Helper()
	if scenario == absolutePendingScenario || scenario == absoluteDelayedScenario {
		return
	}
	ctx := context.Background()
	if claimed, ok, e := repo.Claim(ctx, absoluteTestWorker, []domain.Command{absoluteTestTopic}, 60, 100, 100, "tenant-a"); e != nil || !ok || claimed.ID != task.ID {
		t.Fatalf("claim=%v,%v,%v", claimed, ok, e)
	}
	switch scenario {
	case absoluteCompletedScenario:
		if e := results.SaveResult(ctx, domain.ResultRecord{TaskID: task.ID, Result: map[string]any{"result": "private"}}, absoluteTestTopic, "tenant-a"); e != nil {
			t.Fatal(e)
		}
		if e := results.UpdateTaskOnComplete(ctx, task.ID, absoluteTestTopic, "tenant-a", domain.StatusCompleted, ""); e != nil {
			t.Fatal(e)
		}
	case absoluteFailedScenario, absoluteNackedScenario, absoluteRequeuedScenario:
		if _, _, e := repo.Nack(ctx, task.ID, absoluteTestWorker, 60, 100, "retry"); e != nil {
			t.Fatal(e)
		}
		if scenario == absoluteRequeuedScenario {
			if _, e := repo.RequeueDLQTask(ctx, task.ID); e != nil {
				t.Fatal(e)
			}
		}
	case "abandoned":
		if e := repo.Abandon(ctx, task.ID, absoluteTestWorker); e != nil {
			t.Fatal(e)
		}
	}
}

func TestAbsoluteExpiryPurgesEveryQueueStateAndResult(t *testing.T) {
	for _, scenario := range []string{absolutePendingScenario, absoluteDelayedScenario, "in-progress", absoluteCompletedScenario, absoluteFailedScenario, absoluteNackedScenario, "abandoned", absoluteRequeuedScenario} {
		t.Run(scenario, func(t *testing.T) {
			db, repo, results, clock := absoluteFixture(t)
			ctx := context.Background()
			visible := time.Time{}
			maxAttempts := 100
			if scenario == absoluteDelayedScenario {
				visible = time.Now().Add(time.Minute)
			}
			if scenario == absoluteFailedScenario || scenario == absoluteRequeuedScenario {
				maxAttempts = 1
			}
			task := enqueueAbsolute(t, repo, clock, "logs-"+scenario, "", visible, maxAttempts)
			exerciseAbsoluteState(t, repo, results, task, scenario)
			clock.Store(task.ExpiresAt.Add(time.Second).UnixNano())
			if got, e := repo.Get(ctx, task.ID); !errors.Is(e, domain.ErrTaskExpired) || got != nil {
				t.Fatalf("expired get=%+v %v", got, e)
			}
			if got, e := results.GetResult(ctx, task.ID); got != nil || e == nil {
				t.Fatalf("expired result=%+v %v", got, e)
			}
			if _, e := repo.CleanupExpired(ctx, 100, db.expiryTime()); e != nil {
				t.Fatal(e)
			}
			assertNoPayloadIndexes(t, db, task.ID)
			identity, e := db.readAbsoluteIdentity(task.ID)
			if e != nil || identity == nil || identity.RetiredAt == nil {
				t.Fatalf("tombstone=%+v %v", identity, e)
			}
			body, e := db.Get(absoluteKey(task.ID))
			if e != nil || bytes.Contains(body, []byte("sensitive-payload")) || bytes.Contains(body, []byte("private")) {
				t.Fatalf("unsafe metadata=%s %v", body, e)
			}
		})
	}
}

func TestAbsoluteExpiryNeverRenewsAndRejectsLateMutations(t *testing.T) {
	db, repo, results, clock := absoluteFixture(t)
	ctx := context.Background()
	task := enqueueAbsolute(t, repo, clock, "logs-fixed", "", time.Time{}, 100)
	claimed, ok, e := repo.Claim(ctx, absoluteTestWorker, []domain.Command{absoluteTestTopic}, 60, 100, 100, "tenant-a")
	if e != nil || !ok {
		t.Fatal(e)
	}
	if e = repo.Heartbeat(ctx, task.ID, absoluteTestWorker, 7200); e != nil {
		t.Fatal(e)
	}
	if e = results.SaveResult(ctx, domain.ResultRecord{TaskID: task.ID}, absoluteTestTopic, "tenant-a"); e != nil {
		t.Fatal(e)
	}
	got, e := repo.Get(ctx, task.ID)
	if e != nil || !got.ExpiresAt.Equal(*task.ExpiresAt) || !claimed.ExpiresAt.Equal(*task.ExpiresAt) {
		t.Fatalf("renewed deadline: %+v %v", got, e)
	}
	until, e := time.Parse(time.RFC3339Nano, got.LeaseUntil)
	if e != nil || until.After(*task.ExpiresAt) {
		t.Fatalf("lease escaped deadline: %s expiry %v", got.LeaseUntil, task.ExpiresAt)
	}
	lo, hi := PrefixTTL()
	it, e := db.Iter(lo, hi)
	if e != nil {
		t.Fatal(e)
	}
	n := 0
	for valid := it.First(); valid; valid = it.Next() {
		n++
	}
	_ = it.Close()
	if n != 1 {
		t.Fatalf("TTL entries=%d want1", n)
	}
	clock.Store(task.ExpiresAt.Add(time.Second).UnixNano())
	for name, run := range map[string]func() error{"heartbeat": func() error { return repo.Heartbeat(ctx, task.ID, absoluteTestWorker, 60) }, "progress": func() error { return repo.Progress(ctx, task.ID, absoluteTestWorker, json.RawMessage(`{"step":1}`)) }, "abandon": func() error { return repo.Abandon(ctx, task.ID, absoluteTestWorker) }, "nack": func() error { _, _, e := repo.Nack(ctx, task.ID, absoluteTestWorker, 0, 100, ""); return e }, "result": func() error {
		return results.SaveResult(ctx, domain.ResultRecord{TaskID: task.ID}, absoluteTestTopic, "tenant-a")
	}} {
		if e := run(); !errors.Is(e, domain.ErrTaskExpired) {
			t.Errorf("%s after expiry=%v", name, e)
		}
	}
}

func TestAbsoluteExpiryReplayWindowAndIdempotencyCleanup(t *testing.T) {
	db, repo, _, clock := absoluteFixture(t)
	ctx := context.Background()
	task := enqueueAbsolute(t, repo, clock, "", "logs-key", time.Time{}, 100)
	clock.Store(task.ExpiresAt.Add(time.Second).UnixNano())
	if _, e := repo.CleanupExpired(ctx, 100, db.expiryTime()); e != nil {
		t.Fatal(e)
	}
	if yes, e := db.Has(KeyIdempo("logs-key")); e != nil || yes {
		t.Fatalf("idempotency retained %v %v", yes, e)
	}
	deadline := db.expiryTime().Add(time.Hour)
	retry := domain.WithTaskExpiry(ctx, deadline)
	if _, _, e := repo.EnqueueNamed(retry, task.ID, task.Command, task.Payload, 3, "", 100, time.Time{}, task.TenantID); !errors.Is(e, domain.ErrTaskExpired) {
		t.Fatalf("retired replay=%v", e)
	}
	if _, _, e := repo.EnqueueNamed(retry, task.ID, task.Command, `{"changed":true}`, 3, "", 100, time.Time{}, task.TenantID); !errors.Is(e, domain.ErrTaskIDConflict) {
		t.Fatalf("retired changed=%v", e)
	}
	if e := db.replayRetired(task.ID, "tenant-b", task.Command, task.Payload); !errors.Is(e, domain.ErrTaskIDConflict) {
		t.Fatalf("cross-tenant tombstone=%v", e)
	}
	identity, e := db.readAbsoluteIdentity(task.ID)
	if e != nil {
		t.Fatal(e)
	}
	clock.Store(identity.ReplayUntil.Add(time.Second).UnixNano())
	if _, e := repo.CleanupExpired(ctx, 100, db.expiryTime()); e != nil {
		t.Fatal(e)
	}
	if identity, e = db.readAbsoluteIdentity(task.ID); e != nil || identity != nil {
		t.Fatalf("metadata exceeded window=%+v %v", identity, e)
	}
}

func TestAbsoluteExpiryFloorShorteningAndIncreaseDoNotResurrect(t *testing.T) {
	db, repo, _, clock := absoluteFixture(t)
	task := enqueueAbsolute(t, repo, clock, "logs-floor", "", time.Time{}, 100)
	clock.Store(task.CreatedAt.Add(45 * time.Minute).UnixNano())
	floor := task.CreatedAt.Add(15 * time.Minute)
	attachAbsolutePolicy(db, 1800, &floor, db.expiryTime())
	if _, e := repo.Get(context.Background(), task.ID); !errors.Is(e, domain.ErrTaskExpired) {
		t.Fatalf("shortened visibility=%v", e)
	}
	attachAbsolutePolicy(db, 7200, &floor, db.expiryTime())
	if _, e := repo.Get(context.Background(), task.ID); !errors.Is(e, domain.ErrTaskExpired) {
		t.Fatalf("increased retention resurrected=%v", e)
	}
	if _, e := repo.CleanupExpired(context.Background(), 100, db.expiryTime()); e != nil {
		t.Fatal(e)
	}
	assertNoPayloadIndexes(t, db, task.ID)
}

func TestAbsoluteExpiryLateBatchCannotResurrectAfterConcurrentCleanup(t *testing.T) {
	db, repo, _, clock := absoluteFixture(t)
	task := enqueueAbsolute(t, repo, clock, "logs-race", "", time.Time{}, 100)
	body, e := json.Marshal(task)
	if e != nil {
		t.Fatal(e)
	}
	clock.Store(task.ExpiresAt.Add(time.Second).UnixNano())
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_, err := repo.CleanupExpired(context.Background(), 100, db.expiryTime())
				if err != nil {
					t.Errorf("cleanup %v", err)
				}
				return
			}
			b := db.Batch()
			defer b.Close()
			_ = b.Set(KeyTask(task.ID), body, nil)
			_ = b.Set(KeyResult(task.ID), []byte(`{"result":"late"}`), nil)
			if err := db.CommitBatch(b); !errors.Is(err, domain.ErrTaskExpired) {
				t.Errorf("late commit=%v", err)
			}
		}(i)
	}
	wg.Wait()
	assertNoPayloadIndexes(t, db, task.ID)
}

func TestAbsoluteExpiryCapacityDoesNotAffectLegacy(t *testing.T) {
	db, repo, _, clock := absoluteFixture(t)
	if e := db.Set([]byte(absoluteCountKey), []byte(fmt.Sprint(maxAbsoluteIdentities))); e != nil {
		t.Fatal(e)
	}
	ctx := domain.WithTaskExpiry(context.Background(), time.Unix(0, clock.Load()).Add(time.Hour))
	if _, e := repo.Enqueue(ctx, absoluteTestTopic, `{}`, 0, "", 100, "", "", "logs-cap", time.Time{}, "tenant-a"); e == nil {
		t.Fatal("capacity bypass")
	}
	if _, e := repo.Enqueue(context.Background(), "ordinary", `{}`, 0, "", 3, "", "", "ordinary-task", time.Time{}, "tenant-a"); e != nil {
		t.Fatalf("legacy affected %v", e)
	}
}

func TestAbsoluteExpiryReducedBudgetKeepsAcceptedWork(t *testing.T) {
	db, repo, results, clock := absoluteFixture(t)
	enqueueAbsolute(t, repo, clock, "logs-budget-a", "", time.Time{}, 100)
	enqueueAbsolute(t, repo, clock, "logs-budget-b", "", time.Time{}, 100)
	// Models a restart with a smaller operational budget than retained usage.
	if err := db.SetAbsoluteExpiryCapacity(1, "0"); err != nil {
		t.Fatal(err)
	}
	task, ok, err := repo.Claim(context.Background(), absoluteTestWorker, []domain.Command{absoluteTestTopic}, 60, 100, 100, "tenant-a")
	if err != nil || !ok {
		t.Fatalf("accepted work blocked by lower budget: %v %v", ok, err)
	}
	if err = results.SaveResult(context.Background(), domain.ResultRecord{TaskID: task.ID}, absoluteTestTopic, "tenant-a"); err != nil {
		t.Fatalf("accepted result blocked: %v", err)
	}
	ctx := domain.WithTaskExpiry(context.Background(), time.Unix(0, clock.Load()).Add(time.Hour))
	if _, err = repo.Enqueue(ctx, absoluteTestTopic, `{}`, 0, "", 100, "", "", "logs-budget-c", time.Time{}, "tenant-a"); !errors.Is(err, domain.ErrAbsoluteExpiryCapacity) {
		t.Fatalf("new work bypassed reduced budget: %v", err)
	}
}
