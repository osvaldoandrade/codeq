package pebble

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/internal/repository"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const (
	listTenant      = "tenant-a"
	listOtherTenant = "tenant-b"
)

type lister interface {
	ListTasks(ctx context.Context, cmd domain.Command, tenantID string, state domain.QueueState, limit int, cursor string) (*domain.TaskPage, error)
}

func mustEnqueue(t *testing.T, repo repository.TaskRepository, prio int, visibleAt time.Time, tenant string) *domain.Task {
	t.Helper()
	task, err := repo.Enqueue(context.Background(), domain.CmdGenerateMaster, `{}`, prio, "", 3, "", "", visibleAt, tenant)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return task
}

// listAll walks every page with the given page size and returns the task IDs
// in listing order, failing on a repeated ID or a runaway cursor.
func listAll(t *testing.T, l lister, state domain.QueueState, tenant string, pageSize int) []string {
	t.Helper()
	var ids []string
	seen := map[string]bool{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 1000 {
			t.Fatal("listing never ended")
		}
		page, err := l.ListTasks(context.Background(), domain.CmdGenerateMaster, tenant, state, pageSize, cursor)
		if err != nil {
			t.Fatalf("list %s page %d: %v", state, pages, err)
		}
		if len(page.Tasks) > pageSize {
			t.Fatalf("page of %d tasks exceeds limit %d", len(page.Tasks), pageSize)
		}
		for _, task := range page.Tasks {
			if seen[task.ID] {
				t.Fatalf("task %s listed twice", task.ID)
			}
			seen[task.ID] = true
			ids = append(ids, task.ID)
		}
		if page.NextCursor == "" {
			return ids
		}
		cursor = page.NextCursor
	}
}

func idsOf(tasks ...*domain.Task) []string {
	out := make([]string, len(tasks))
	for i, task := range tasks {
		out[i] = task.ID
	}
	return out
}

func assertIDs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("listed %d tasks %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("position %d: got %s, want %s (all: %v vs %v)", i, got[i], want[i], got, want)
		}
	}
}

// Ready follows claim order (highest priority first, FIFO within one) on
// every page size, including one that ends exactly on a priority boundary.
func TestListReadyFollowsClaimOrder(t *testing.T) {
	repo := NewTaskRepository(openTestDB(t), time.UTC, "fixed", 1, 5)
	low1 := mustEnqueue(t, repo, 1, time.Time{}, listTenant)
	high1 := mustEnqueue(t, repo, 9, time.Time{}, listTenant)
	low2 := mustEnqueue(t, repo, 1, time.Time{}, listTenant)
	high2 := mustEnqueue(t, repo, 9, time.Time{}, listTenant)
	mid := mustEnqueue(t, repo, 5, time.Time{}, listTenant)
	want := idsOf(high1, high2, mid, low1, low2)

	for _, size := range []int{1, 2, 3, 5, 100} {
		assertIDs(t, listAll(t, repo, domain.QueueStateReady, listTenant, size), want)
	}

	// Claims take tasks in the listed order.
	for i, id := range want[:2] {
		claimed, ok, err := repo.Claim(context.Background(), "w", []domain.Command{domain.CmdGenerateMaster}, 60, 50, 3, listTenant)
		if err != nil || !ok || claimed.ID != id {
			t.Fatalf("claim %d = %v, %v, %v; want %s", i, claimed, ok, err, id)
		}
	}
	assertIDs(t, listAll(t, repo, domain.QueueStateReady, listTenant, 2), want[2:])
	assertIDs(t, listAll(t, repo, domain.QueueStateInProgress, listTenant, 1), sortedPair(want[0], want[1]))
}

// In-progress keys are ordered by task ID.
func sortedPair(a, b string) []string {
	if a < b {
		return []string{a, b}
	}
	return []string{b, a}
}

func TestListDelayedByVisibleTime(t *testing.T) {
	repo := NewTaskRepository(openTestDB(t), time.UTC, "fixed", 1, 5)
	now := time.Now()
	late := mustEnqueue(t, repo, 5, now.Add(3*time.Hour), listTenant)
	early := mustEnqueue(t, repo, 5, now.Add(time.Hour), listTenant)
	mustEnqueue(t, repo, 5, time.Time{}, listTenant) // ready, not delayed

	assertIDs(t, listAll(t, repo, domain.QueueStateDelayed, listTenant, 1), idsOf(early, late))
}

func TestListDLQ(t *testing.T) {
	ctx := context.Background()
	repo := NewTaskRepository(openTestDB(t), time.UTC, "fixed", 1, 1)
	repo.reconcile.interval = 0
	task := mustEnqueue(t, repo, 5, time.Time{}, listTenant)
	for i := range 2 {
		c, _, err := repo.Claim(ctx, "w", []domain.Command{domain.CmdGenerateMaster}, 60, 50, 3, listTenant)
		if err != nil || c == nil {
			t.Fatalf("claim %d: %v %v", i, c, err)
		}
		if _, _, err := repo.Nack(ctx, c.ID, "w", 0, 3, "ERR"); err != nil {
			t.Fatalf("nack %d: %v", i, err)
		}
	}
	page, err := repo.ListTasks(ctx, domain.CmdGenerateMaster, listTenant, domain.QueueStateDLQ, 10, "")
	if err != nil || len(page.Tasks) != 1 || page.Tasks[0].ID != task.ID || page.Tasks[0].Status != domain.StatusFailed {
		t.Fatalf("dlq page = %+v, %v; want the failed task %s", page, err, task.ID)
	}
}

func TestListSkipsStaleEntriesAndEndsCleanly(t *testing.T) {
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	a := mustEnqueue(t, repo, 5, time.Time{}, listTenant)
	gone := mustEnqueue(t, repo, 5, time.Time{}, listTenant)
	b := mustEnqueue(t, repo, 5, time.Time{}, listTenant)
	if err := db.Delete(KeyTask(gone.ID)); err != nil {
		t.Fatalf("delete body: %v", err)
	}
	assertIDs(t, listAll(t, repo, domain.QueueStateReady, listTenant, 1), idsOf(a, b))

	// A page that ends exactly on the last entry returns no cursor.
	page, err := repo.ListTasks(context.Background(), domain.CmdGenerateMaster, listTenant, domain.QueueStateReady, 3, "")
	if err != nil || len(page.Tasks) != 2 || page.NextCursor != "" {
		t.Fatalf("full listing = %d tasks, cursor %q, err %v; want 2 and no cursor", len(page.Tasks), page.NextCursor, err)
	}
	empty, err := repo.ListTasks(context.Background(), domain.CmdGenerateMaster, listTenant, domain.QueueStateDelayed, 3, "")
	if err != nil || len(empty.Tasks) != 0 || empty.NextCursor != "" || empty.Tasks == nil {
		t.Fatalf("empty listing = %+v, %v; want an empty, non-nil page", empty, err)
	}
}

func TestListIsTenantScopedAndRejectsForeignCursors(t *testing.T) {
	ctx := context.Background()
	repo := NewTaskRepository(openTestDB(t), time.UTC, "fixed", 1, 5)
	mine := mustEnqueue(t, repo, 5, time.Time{}, listTenant)
	mustEnqueue(t, repo, 5, time.Time{}, listTenant)
	theirs := mustEnqueue(t, repo, 5, time.Time{}, listOtherTenant)

	assertIDs(t, listAll(t, repo, domain.QueueStateReady, listOtherTenant, 10), idsOf(theirs))

	page, err := repo.ListTasks(ctx, domain.CmdGenerateMaster, listTenant, domain.QueueStateReady, 1, "")
	if err != nil || page.NextCursor == "" || page.Tasks[0].ID != mine.ID {
		t.Fatalf("first page = %+v, %v", page, err)
	}
	foreign := []struct {
		name, tenant string
		state        domain.QueueState
		cursor       string
	}{
		{"another tenant", listOtherTenant, domain.QueueStateReady, page.NextCursor},
		{"another state", listTenant, domain.QueueStateDelayed, page.NextCursor},
		{"not base64", listTenant, domain.QueueStateReady, "%%%"},
		{"empty key", listTenant, domain.QueueStateReady, base64.RawURLEncoding.EncodeToString(nil) + "="},
		{"task key", listTenant, domain.QueueStateReady, base64.RawURLEncoding.EncodeToString(KeyTask(theirs.ID))},
	}
	for _, f := range foreign {
		if _, err := repo.ListTasks(ctx, domain.CmdGenerateMaster, f.tenant, f.state, 10, f.cursor); !errors.Is(err, domain.ErrInvalidCursor) {
			t.Fatalf("%s: err %v, want ErrInvalidCursor", f.name, err)
		}
	}
	if _, err := repo.ListTasks(ctx, domain.CmdGenerateMaster, listTenant, "pending", 10, ""); !errors.Is(err, domain.ErrInvalidQueueState) {
		t.Fatalf("unknown state: err %v, want ErrInvalidQueueState", err)
	}
}

func TestShardedListWalksEveryShard(t *testing.T) {
	shards := make([]*TaskRepository, 4)
	for i := range shards {
		shards[i] = NewTaskRepository(openTestDB(t), time.UTC, "fixed", 1, 5)
	}
	repo := NewShardedTaskRepository(shards)
	want := map[string]bool{}
	for range 25 {
		want[mustEnqueue(t, repo, 5, time.Time{}, listTenant).ID] = true
	}
	for _, size := range []int{1, 7, 25, 100} {
		got := listAll(t, repo, domain.QueueStateReady, listTenant, size)
		if len(got) != len(want) {
			t.Fatalf("page size %d listed %d tasks, want %d", size, len(got), len(want))
		}
		for _, id := range got {
			if !want[id] {
				t.Fatalf("page size %d listed unknown task %s", size, id)
			}
		}
	}
	if _, err := repo.ListTasks(context.Background(), domain.CmdGenerateMaster, listTenant, domain.QueueStateReady, 5, "bm90LWpzb24"); !errors.Is(err, domain.ErrInvalidCursor) {
		t.Fatalf("garbage sharded cursor: err %v, want ErrInvalidCursor", err)
	}
}

// storeContents returns every key and value under the codeq/ namespace.
func storeContents(t *testing.T, db *DB) map[string]string {
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

// Listing is a pure read: it leaves the store byte-for-byte unchanged, and
// the same (state, limit, cursor) returns the same page every time.
func TestListIsReadOnlyAndRepeatable(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	for i := range 5 {
		mustEnqueue(t, repo, i, time.Time{}, listTenant)
	}
	mustEnqueue(t, repo, 5, time.Now().Add(time.Hour), listTenant)
	before := storeContents(t, db)

	first, err := repo.ListTasks(ctx, domain.CmdGenerateMaster, listTenant, domain.QueueStateReady, 2, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, state := range []domain.QueueState{domain.QueueStateReady, domain.QueueStateDelayed, domain.QueueStateInProgress, domain.QueueStateDLQ} {
		for range 3 {
			if _, err := repo.ListTasks(ctx, domain.CmdGenerateMaster, listTenant, state, 2, ""); err != nil {
				t.Fatalf("list %s: %v", state, err)
			}
		}
	}
	for i := range 3 {
		again, err := repo.ListTasks(ctx, domain.CmdGenerateMaster, listTenant, domain.QueueStateReady, 2, "")
		if err != nil || again.NextCursor != first.NextCursor || len(again.Tasks) != len(first.Tasks) {
			t.Fatalf("repeat %d differs: %+v vs %+v (%v)", i, again, first, err)
		}
		for j := range first.Tasks {
			if again.Tasks[j].ID != first.Tasks[j].ID {
				t.Fatalf("repeat %d position %d: %s vs %s", i, j, again.Tasks[j].ID, first.Tasks[j].ID)
			}
		}
		next1, err1 := repo.ListTasks(ctx, domain.CmdGenerateMaster, listTenant, domain.QueueStateReady, 2, first.NextCursor)
		next2, err2 := repo.ListTasks(ctx, domain.CmdGenerateMaster, listTenant, domain.QueueStateReady, 2, first.NextCursor)
		if err1 != nil || err2 != nil || next1.Tasks[0].ID != next2.Tasks[0].ID || next1.NextCursor != next2.NextCursor {
			t.Fatalf("same cursor gave different pages: %+v / %+v (%v %v)", next1, next2, err1, err2)
		}
	}

	after := storeContents(t, db)
	if len(after) != len(before) {
		t.Fatalf("listing changed the store: %d keys before, %d after", len(before), len(after))
	}
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("listing rewrote key %q", k)
		}
	}
}
