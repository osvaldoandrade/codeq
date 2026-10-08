package pebble

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/internal/repository"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const (
	idemTenantA = "local-tenant"
	idemTenantB = "othertenant"
	idemKey     = "order-42"
)

// assertTenantBoundIdempotency is the reviewer's probe at the storage layer:
// tenant A creates with a key, tenant B reusing it gets
// ErrIdempotencyConflict and no task, tenant A still replays its task, and
// no extra task is enqueued.
func assertTenantBoundIdempotency(t *testing.T, repo repository.TaskRepository) {
	t.Helper()
	ctx := context.Background()
	cmd := domain.CmdGenerateMaster
	first, err := repo.Enqueue(ctx, cmd, `{"secret":1}`, 5, "", 3, idemKey, time.Time{}, idemTenantA)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	got, err := repo.Enqueue(ctx, cmd, `{}`, 5, "", 3, idemKey, time.Time{}, idemTenantB)
	if !errors.Is(err, domain.ErrIdempotencyConflict) || got != nil {
		t.Fatalf("cross-tenant: got %v, %v; want nil, ErrIdempotencyConflict", got, err)
	}
	got, err = repo.Enqueue(ctx, cmd, `{}`, 5, "", 3, idemKey, time.Time{}, "")
	if !errors.Is(err, domain.ErrIdempotencyConflict) || got != nil {
		t.Fatalf("legacy empty tenant: got %v, %v; want nil, ErrIdempotencyConflict", got, err)
	}
	again, err := repo.Enqueue(ctx, cmd, `{"x":2}`, 5, "", 3, idemKey, time.Time{}, idemTenantA)
	if err != nil || again.ID != first.ID || again.Payload != first.Payload {
		t.Fatalf("same-tenant replay: got %v, %v; want original %s", again, err, first.ID)
	}
}

func TestIdempotencyIsTenantBound(t *testing.T) {
	assertTenantBoundIdempotency(t, NewTaskRepository(openTestDB(t), time.UTC, "fixed", 1, 5))
}

func TestShardedIdempotencyIsTenantBound(t *testing.T) {
	shards := make([]*TaskRepository, 4)
	for i := range shards {
		shards[i] = NewTaskRepository(openTestDB(t), time.UTC, "fixed", 1, 5)
	}
	assertTenantBoundIdempotency(t, NewShardedTaskRepository(shards))
}

func TestEnqueueWithIDIdempotencyIsTenantBound(t *testing.T) {
	ctx := context.Background()
	repo := NewTaskRepository(openTestDB(t), time.UTC, "fixed", 1, 5)
	cmd := domain.CmdGenerateMaster
	if _, _, err := repo.EnqueueWithID(ctx, "id-a", cmd, `{}`, 5, "", 3, idemKey, time.Time{}, idemTenantA); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	got, _, err := repo.EnqueueWithID(ctx, "id-b", cmd, `{}`, 5, "", 3, idemKey, time.Time{}, idemTenantB)
	if !errors.Is(err, domain.ErrIdempotencyConflict) || got != nil {
		t.Fatalf("cluster EnqueueWithID cross-tenant: got %v, %v", got, err)
	}
	if _, err := repo.Get(ctx, "id-b"); err == nil {
		t.Fatal("refused cross-tenant create stored a task")
	}
}

func TestConcurrentIdempotentCreateStoresOneTask(t *testing.T) {
	ctx := context.Background()
	repo := NewTaskRepository(openTestDB(t), time.UTC, "fixed", 1, 5)
	const n = 16
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			task, err := repo.Enqueue(ctx, domain.CmdGenerateMaster, `{}`, 0, "", 3, "same-key", time.Time{}, idemTenantA)
			if err != nil {
				t.Errorf("enqueue: %v", err)
				return
			}
			ids[i] = task.ID
		}()
	}
	wg.Wait()
	first := ids[0]
	for _, id := range ids[1:] {
		if id != first {
			t.Fatalf("concurrent creates stored %s and %s", first, id)
		}
	}
}

func TestTTLKeepsPendingTaskBody(t *testing.T) {
	ctx := context.Background()
	repo := NewTaskRepository(openTestDB(t), time.UTC, "fixed", 1, 5)
	task, err := repo.Enqueue(ctx, domain.CmdGenerateMaster, `{"keep":true}`, 0, "", 3, "", time.Time{}, idemTenantA)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	deleted, err := repo.CleanupExpired(ctx, 10, time.Now().Add(48*time.Hour))
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("pending body deleted: %d", deleted)
	}
	got, err := repo.Get(ctx, task.ID)
	if err != nil || got.Payload != task.Payload {
		t.Fatalf("body after ttl sweep: %+v err=%v", got, err)
	}
}
