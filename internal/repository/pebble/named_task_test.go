package pebble

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/internal/repository"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const (
	namedTenant = "tenant-a"
	namedID     = "ws-1.export-42"
)

func createNamed(t *testing.T, repo repository.TaskRepository, id, tenant, payload string) (*domain.Task, error) {
	t.Helper()
	return repo.Enqueue(context.Background(), domain.CmdGenerateMaster, payload, 5, "", 3, "", "", id, time.Time{}, tenant)
}

func assertNamedTaskLifecycle(t *testing.T, repo repository.TaskRepository) {
	t.Helper()
	ctx := context.Background()
	created, err := createNamed(t, repo, namedID, namedTenant, payloadN1)
	if err != nil || created.ID != namedID {
		t.Fatalf("named create = %+v, %v; want id %s", created, err, namedID)
	}
	got, err := repo.Get(ctx, namedID)
	if err != nil || got.ID != namedID || got.Payload != payloadN1 {
		t.Fatalf("get by the client's id = %+v, %v", got, err)
	}

	replay, err := createNamed(t, repo, namedID, namedTenant, `{"n":2}`)
	if err != nil || replay.ID != namedID || replay.Payload != payloadN1 {
		t.Fatalf("same-tenant replay = %+v, %v; want the original task unchanged", replay, err)
	}
	if n := namedReadyCount(t, repo, domain.CmdGenerateMaster, namedTenant); n != 1 {
		t.Fatalf("ready = %d after a replay, want 1", n)
	}

	other, err := createNamed(t, repo, namedID, "tenant-b", `{}`)
	if !errors.Is(err, domain.ErrTaskIDConflict) || other != nil {
		t.Fatalf("cross-tenant create = %+v, %v; want nil, ErrTaskIDConflict", other, err)
	}

	claimed, ok, err := repo.Claim(ctx, "w", []domain.Command{domain.CmdGenerateMaster}, 60, 50, 3, namedTenant)
	if err != nil || !ok || claimed.ID != namedID {
		t.Fatalf("claim = %+v, %v, %v; want the named task", claimed, ok, err)
	}
	if again, err := createNamed(t, repo, namedID, namedTenant, `{}`); err != nil || again.Status != domain.StatusInProgress {
		t.Fatalf("create of a running id = %+v, %v; want the running task back", again, err)
	}
}

func namedReadyCount(t *testing.T, repo repository.TaskRepository, cmd domain.Command, tenant string) int64 {
	t.Helper()
	stats, err := repo.QueueStats(context.Background(), cmd, tenant)
	if err != nil {
		t.Fatalf("queue stats: %v", err)
	}
	return stats.Ready
}

func TestNamedTaskLifecycle(t *testing.T) {
	assertNamedTaskLifecycle(t, NewTaskRepository(openTestDB(t), time.UTC, "fixed", 1, 5))
}

func TestShardedNamedTaskLifecycle(t *testing.T) {
	shards := make([]*TaskRepository, 4)
	for i := range shards {
		shards[i] = NewTaskRepository(openTestDB(t), time.UTC, "fixed", 1, 5)
	}
	sharded := NewShardedTaskRepository(shards)
	assertNamedTaskLifecycle(t, sharded)

	// Every named task lives on the shard its ID hashes to; cover all shards.
	covered := map[int]bool{}
	for i := 0; len(covered) < len(shards) && i < 1000; i++ {
		id := fmt.Sprintf("ws-2.job-%d", i)
		idx := sharded.shardOf(id)
		if covered[idx] {
			continue
		}
		covered[idx] = true
		if _, err := createNamed(t, sharded, id, namedTenant, `{}`); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		if _, err := shards[idx].db.Get(KeyTask(id)); err != nil {
			t.Fatalf("task %s not on shardOf(id)=%d: %v", id, idx, err)
		}
	}
	if len(covered) != len(shards) {
		t.Fatalf("covered %d of %d shards", len(covered), len(shards))
	}
}

func TestNamedTaskConcurrentCreatesWriteOneTask(t *testing.T) {
	repo := NewTaskRepository(openTestDB(t), time.UTC, "fixed", 1, 5)
	const creators = 64
	errs := make([]error, creators)
	var wg sync.WaitGroup
	for i := range creators {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = repo.Enqueue(context.Background(), domain.CmdGenerateMaster, `{}`, 5, "", 3, "", "", namedID, time.Time{}, namedTenant)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if n := namedReadyCount(t, repo, domain.CmdGenerateMaster, namedTenant); n != 1 {
		t.Fatalf("64 concurrent creates of one id left %d ready tasks, want 1", n)
	}
}

// Generated IDs keep the original path: no lookup, no lock, a fresh UUID.
func TestUnnamedCreatesStayIndependent(t *testing.T) {
	repo := NewTaskRepository(openTestDB(t), time.UTC, "fixed", 1, 5)
	a, errA := createNamed(t, repo, "", namedTenant, `{}`)
	b, errB := createNamed(t, repo, "", namedTenant, `{}`)
	if errA != nil || errB != nil || a.ID == b.ID || a.ID == "" {
		t.Fatalf("unnamed creates = %v %v (%v %v)", a, b, errA, errB)
	}
}
