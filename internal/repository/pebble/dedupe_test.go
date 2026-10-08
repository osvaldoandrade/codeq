package pebble

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/internal/repository"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const (
	dedupeTenant = "tenant-a"
	dedupeKey    = "sync:channel-7"
)

type dedupeRepo interface {
	repository.TaskRepository
	ClaimMany(ctx context.Context, workerID string, commands []domain.Command, leaseSeconds int, max int, inspectLimit int, maxAttemptsDefault int, tenantID string) ([]*domain.Task, error)
}

func newShardedDedupeRepo(t *testing.T) *ShardedTaskRepository {
	t.Helper()
	shards := make([]*TaskRepository, 4)
	for i := range shards {
		shards[i] = NewTaskRepository(openTestDB(t), time.UTC, "fixed", 1, 5)
	}
	return NewShardedTaskRepository(shards)
}

func enqueueDedupe(t *testing.T, repo repository.TaskRepository, cmd domain.Command, tenant, key string, visibleAt time.Time) *domain.Task {
	t.Helper()
	task, err := repo.Enqueue(context.Background(), cmd, `{"n":1}`, 5, "", 3, "", key, visibleAt, tenant)
	if err != nil {
		t.Fatalf("enqueue %s/%s/%s: %v", tenant, cmd, key, err)
	}
	return task
}

func readyCount(t *testing.T, repo repository.TaskRepository, cmd domain.Command, tenant string) int64 {
	t.Helper()
	stats, err := repo.QueueStats(context.Background(), cmd, tenant)
	if err != nil {
		t.Fatalf("queue stats: %v", err)
	}
	return stats.Ready
}

// assertDedupeLifecycle drives one key through a full wait/claim cycle: a
// repeated create joins the waiting task, the claim releases the key, and the
// next create starts a new waiting task that later creates join in turn.
func assertDedupeLifecycle(t *testing.T, repo dedupeRepo, claimMany bool) {
	t.Helper()
	ctx := context.Background()
	cmd := domain.CmdGenerateMaster

	first := enqueueDedupe(t, repo, cmd, dedupeTenant, dedupeKey, time.Time{})
	if first.DeduplicationKey != dedupeKey {
		t.Fatalf("task carries key %q, want %q", first.DeduplicationKey, dedupeKey)
	}
	again := enqueueDedupe(t, repo, cmd, dedupeTenant, dedupeKey, time.Time{})
	if again.ID != first.ID {
		t.Fatalf("create while waiting returned %s, want the waiting task %s", again.ID, first.ID)
	}
	if n := readyCount(t, repo, cmd, dedupeTenant); n != 1 {
		t.Fatalf("ready = %d after a deduplicated create, want 1", n)
	}

	var claimed *domain.Task
	if claimMany {
		tasks, err := repo.ClaimMany(ctx, "w1", []domain.Command{cmd}, 60, 10, 50, 3, dedupeTenant)
		if err != nil || len(tasks) != 1 {
			t.Fatalf("claim many: %d tasks, err %v", len(tasks), err)
		}
		claimed = tasks[0]
	} else {
		task, ok, err := repo.Claim(ctx, "w1", []domain.Command{cmd}, 60, 50, 3, dedupeTenant)
		if err != nil || !ok {
			t.Fatalf("claim: ok=%v err=%v", ok, err)
		}
		claimed = task
	}
	if claimed.ID != first.ID {
		t.Fatalf("claimed %s, want %s", claimed.ID, first.ID)
	}

	second := enqueueDedupe(t, repo, cmd, dedupeTenant, dedupeKey, time.Time{})
	if second.ID == first.ID {
		t.Fatal("create after the claim joined the running task; the claim must release the key")
	}
	third := enqueueDedupe(t, repo, cmd, dedupeTenant, dedupeKey, time.Time{})
	if third.ID != second.ID {
		t.Fatalf("create while the second task waits returned %s, want %s", third.ID, second.ID)
	}
	if n := readyCount(t, repo, cmd, dedupeTenant); n != 1 {
		t.Fatalf("ready = %d, want only the second task waiting", n)
	}
}

func TestDedupeLifecycle(t *testing.T) {
	cases := []struct {
		name      string
		repo      func(*testing.T) dedupeRepo
		claimMany bool
	}{
		{"single/claim", func(t *testing.T) dedupeRepo { return NewTaskRepository(openTestDB(t), time.UTC, "fixed", 1, 5) }, false},
		{"single/claim-many", func(t *testing.T) dedupeRepo { return NewTaskRepository(openTestDB(t), time.UTC, "fixed", 1, 5) }, true},
		{"sharded/claim", func(t *testing.T) dedupeRepo { return newShardedDedupeRepo(t) }, false},
		{"sharded/claim-many", func(t *testing.T) dedupeRepo { return newShardedDedupeRepo(t) }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertDedupeLifecycle(t, tc.repo(t), tc.claimMany)
		})
	}
}

func TestDedupeJoinsDelayedTask(t *testing.T) {
	repo := NewTaskRepository(openTestDB(t), time.UTC, "fixed", 1, 5)
	cmd := domain.CmdGenerateMaster
	later := time.Now().Add(time.Hour)

	delayed := enqueueDedupe(t, repo, cmd, dedupeTenant, dedupeKey, later)
	if delayed.LastKnownLocation != domain.LocationDelayed {
		t.Fatalf("location %s, want delayed", delayed.LastKnownLocation)
	}
	again := enqueueDedupe(t, repo, cmd, dedupeTenant, dedupeKey, time.Time{})
	if again.ID != delayed.ID {
		t.Fatalf("create while delayed returned %s, want %s", again.ID, delayed.ID)
	}
	if again.LastKnownLocation != domain.LocationDelayed {
		t.Fatal("a deduplicated create must not move the waiting task")
	}
	stats, err := repo.QueueStats(context.Background(), cmd, dedupeTenant)
	if err != nil {
		t.Fatalf("queue stats: %v", err)
	}
	if stats.Ready != 0 || stats.Delayed != 1 {
		t.Fatalf("ready=%d delayed=%d, want 0 and 1", stats.Ready, stats.Delayed)
	}
}

func TestDedupeScope(t *testing.T) {
	repo := NewTaskRepository(openTestDB(t), time.UTC, "fixed", 1, 5)
	base := enqueueDedupe(t, repo, domain.CmdGenerateMaster, dedupeTenant, dedupeKey, time.Time{})

	distinct := []struct {
		name   string
		cmd    domain.Command
		tenant string
		key    string
	}{
		{"other tenant", domain.CmdGenerateMaster, "tenant-b", dedupeKey},
		{"legacy empty tenant", domain.CmdGenerateMaster, "", dedupeKey},
		{"other command", domain.CmdGenerateCreative, dedupeTenant, dedupeKey},
		{"other key", domain.CmdGenerateMaster, dedupeTenant, dedupeKey + "-2"},
	}
	for _, d := range distinct {
		if got := enqueueDedupe(t, repo, d.cmd, d.tenant, d.key, time.Time{}); got.ID == base.ID {
			t.Fatalf("%s joined the task of another scope", d.name)
		}
	}

	// Queue keys lowercase the command, so a differently cased command names
	// the same queue and the same deduplication scope.
	lower := domain.Command("generate_master")
	if got := enqueueDedupe(t, repo, lower, dedupeTenant, dedupeKey, time.Time{}); got.ID != base.ID {
		t.Fatalf("command %q did not join the task of %q", lower, domain.CmdGenerateMaster)
	}
}

func TestDedupeWithoutKeyNeverJoins(t *testing.T) {
	repo := NewTaskRepository(openTestDB(t), time.UTC, "fixed", 1, 5)
	a := enqueueDedupe(t, repo, domain.CmdGenerateMaster, dedupeTenant, "", time.Time{})
	b := enqueueDedupe(t, repo, domain.CmdGenerateMaster, dedupeTenant, "", time.Time{})
	if a.ID == b.ID || a.DeduplicationKey != "" {
		t.Fatalf("creates without a key must stay independent: %s %s %q", a.ID, b.ID, a.DeduplicationKey)
	}
}

// A mapping whose task body is gone (the TTL reaper deletes bodies without
// reading them) must not block the key: the next create replaces it.
func TestDedupeStaleMappingIsReplaced(t *testing.T) {
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	cmd := domain.CmdGenerateMaster
	gone := enqueueDedupe(t, repo, cmd, dedupeTenant, dedupeKey, time.Time{})
	if err := db.Delete(KeyTask(gone.ID)); err != nil {
		t.Fatalf("delete task body: %v", err)
	}

	fresh := enqueueDedupe(t, repo, cmd, dedupeTenant, dedupeKey, time.Time{})
	if fresh.ID == gone.ID {
		t.Fatal("create joined a task whose body no longer exists")
	}
	mapped, err := db.Get(KeyDedupe(cmd, dedupeTenant, dedupeKey))
	if err != nil || string(mapped) != fresh.ID {
		t.Fatalf("mapping = %q, %v; want %s", mapped, err, fresh.ID)
	}
}

func TestDedupeConcurrentCreatesWriteOneTask(t *testing.T) {
	for _, tc := range []struct {
		name string
		repo func(*testing.T) repository.TaskRepository
	}{
		{"single", func(t *testing.T) repository.TaskRepository {
			return NewTaskRepository(openTestDB(t), time.UTC, "fixed", 1, 5)
		}},
		{"sharded", func(t *testing.T) repository.TaskRepository { return newShardedDedupeRepo(t) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := tc.repo(t)
			const creators = 64
			ids := make([]string, creators)
			errs := make([]error, creators)
			var wg sync.WaitGroup
			for i := range creators {
				wg.Add(1)
				go func() {
					defer wg.Done()
					task, err := repo.Enqueue(context.Background(), domain.CmdGenerateMaster, `{}`, 5, "", 3, "", dedupeKey, time.Time{}, dedupeTenant)
					if err == nil {
						ids[i] = task.ID
					}
					errs[i] = err
				}()
			}
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					t.Fatalf("create %d: %v", i, err)
				}
			}
			for _, id := range ids {
				if id != ids[0] {
					t.Fatalf("concurrent creates wrote %s and %s; want one task", ids[0], id)
				}
			}
			if n := readyCount(t, repo, domain.CmdGenerateMaster, dedupeTenant); n != 1 {
				t.Fatalf("ready = %d, want 1", n)
			}
		})
	}
}

// Naive "/"-joined keys would make these tuples collide; the length-prefixed
// encoding keeps one tenant from ever addressing another tenant's mapping.
func TestKeyDedupeIsUnambiguous(t *testing.T) {
	tuples := []struct {
		cmd    domain.Command
		tenant string
		key    string
	}{
		{"x/t2", "t1", "k"},
		{"x", "t2", "t1/k"},
		{"x", "t2/t1", "k"},
		{"x", "", "t2/t1/k"},
		{"", "x", "t2/t1/k"},
	}
	seen := make([][]byte, 0, len(tuples))
	for _, tp := range tuples {
		k := KeyDedupe(tp.cmd, tp.tenant, tp.key)
		if !bytes.HasPrefix(k, []byte(pDedupe)) {
			t.Fatalf("key %q outside the dedupe namespace", k)
		}
		for _, prev := range seen {
			if bytes.Equal(prev, k) {
				t.Fatalf("tuple %+v encodes to the key of another tuple", tp)
			}
		}
		seen = append(seen, k)
	}
}

// The claim batch deletes the mapping, so keys of finished tasks do not
// accumulate. Correctness does not depend on it (a create re-checks that the
// mapped task still waits), which is why this asserts the storage directly.
func TestDedupeClaimDeletesMapping(t *testing.T) {
	for _, claimMany := range []bool{false, true} {
		db := openTestDB(t)
		repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
		cmd := domain.CmdGenerateMaster
		enqueueDedupe(t, repo, cmd, dedupeTenant, dedupeKey, time.Time{})
		if _, err := db.Get(KeyDedupe(cmd, dedupeTenant, dedupeKey)); err != nil {
			t.Fatalf("mapping missing after create: %v", err)
		}
		var err error
		if claimMany {
			_, err = repo.ClaimMany(context.Background(), "w1", []domain.Command{cmd}, 60, 10, 50, 3, dedupeTenant)
		} else {
			_, _, err = repo.Claim(context.Background(), "w1", []domain.Command{cmd}, 60, 50, 3, dedupeTenant)
		}
		if err != nil {
			t.Fatalf("claim (many=%v): %v", claimMany, err)
		}
		if _, err := db.Get(KeyDedupe(cmd, dedupeTenant, dedupeKey)); !errors.Is(err, ErrNotFound) {
			t.Fatalf("claim (many=%v) left the mapping: %v", claimMany, err)
		}
	}
}

// A retried task still carries its key but no longer holds it: re-claiming
// it must leave the mapping of the newer task that took the key meanwhile,
// or the next create would enqueue a second waiting task.
func TestDedupeReclaimOfARetryKeepsTheNewerHolder(t *testing.T) {
	for _, claimMany := range []bool{false, true} {
		ctx := context.Background()
		repo := NewTaskRepository(openTestDB(t), time.UTC, "fixed", 1, 5)
		cmd := domain.CmdGenerateMaster
		claim := func() *domain.Task {
			t.Helper()
			if claimMany {
				got, err := repo.ClaimMany(ctx, "w1", []domain.Command{cmd}, 60, 1, 50, 3, dedupeTenant)
				if err != nil || len(got) != 1 {
					t.Fatalf("claim many: %v, %v", got, err)
				}
				return got[0]
			}
			got, ok, err := repo.Claim(ctx, "w1", []domain.Command{cmd}, 60, 50, 3, dedupeTenant)
			if err != nil || !ok {
				t.Fatalf("claim: %v, %v", ok, err)
			}
			return got
		}
		first, err := repo.Enqueue(ctx, cmd, `{"run":1}`, 5, "", 3, "", dedupeKey, time.Time{}, dedupeTenant)
		if err != nil {
			t.Fatalf("create first: %v", err)
		}
		if got := claim(); got.ID != first.ID {
			t.Fatalf("claimed %s, want %s", got.ID, first.ID)
		}
		// A lower priority keeps the newer task waiting while the retry runs.
		newer, err := repo.Enqueue(ctx, cmd, `{"run":2}`, 1, "", 3, "", dedupeKey, time.Time{}, dedupeTenant)
		if err != nil || newer.ID == first.ID {
			t.Fatalf("create after the claim = %v, %v; want a new task", newer, err)
		}
		if _, _, err := repo.Nack(ctx, first.ID, "w1", 0, 3, "retry"); err != nil {
			t.Fatalf("nack: %v", err)
		}
		if got := claim(); got.ID != first.ID {
			t.Fatalf("re-claimed %s, want the retried %s", got.ID, first.ID)
		}
		joined, err := repo.Enqueue(ctx, cmd, `{"run":3}`, 5, "", 3, "", dedupeKey, time.Time{}, dedupeTenant)
		if err != nil || joined.ID != newer.ID {
			t.Fatalf("create while the newer task waits (many=%v) = %v, %v; want it to join %s", claimMany, joined, err, newer.ID)
		}
	}
}

// snapshotStore returns every key and value under the codeq/ namespace.
func snapshotStore(t *testing.T, db *DB) map[string]string {
	t.Helper()
	lower := []byte(namespace)
	it, err := db.Iter(lower, prefixUpper(lower))
	if err != nil {
		t.Fatalf("iter: %v", err)
	}
	defer it.Close()
	out := map[string]string{}
	for valid := it.First(); valid; valid = it.Next() {
		out[string(it.Key())] = string(it.Value())
	}
	return out
}

// A create that joins a waiting task is a pure read: it writes nothing,
// reports the task as not newly ready (so no worker is notified), and
// returns the same task however many times it repeats.
func TestDedupeJoinIsSideEffectFree(t *testing.T) {
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	cmd := domain.CmdGenerateMaster
	first, ready, err := repo.EnqueueWithReady(context.Background(), cmd, `{"n":1}`, 5, "", 3, "", dedupeKey, time.Time{}, dedupeTenant)
	if err != nil || !ready {
		t.Fatalf("first create: ready=%v err=%v", ready, err)
	}
	before := snapshotStore(t, db)

	for i := range 3 {
		again, ready, err := repo.EnqueueWithReady(context.Background(), cmd, `{"n":2}`, 9, "", 3, "", dedupeKey, time.Now().Add(time.Hour), dedupeTenant)
		if err != nil || ready || again.ID != first.ID || again.Payload != first.Payload || again.Priority != first.Priority {
			t.Fatalf("join %d = %+v ready=%v err=%v; want the unchanged first task, not ready", i, again, ready, err)
		}
	}
	after := snapshotStore(t, db)
	if len(after) != len(before) {
		t.Fatalf("joins changed the store: %d keys before, %d after", len(before), len(after))
	}
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("joins rewrote key %q", k)
		}
	}
}
