package pebble

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

type countingClaimReplicator struct {
	*ambiguousAbsoluteReplicator
	writes atomic.Int64
}

func (r *countingClaimReplicator) Replicate(repr []byte) error {
	r.writes.Add(1)
	return r.ambiguousAbsoluteReplicator.Replicate(repr)
}

func TestClaimReapedHintsDoNotReplicateMissingKeys(t *testing.T) {
	db, repo, _, clock := absoluteFixture(t)
	repl := &countingClaimReplicator{ambiguousAbsoluteReplicator: &ambiguousAbsoluteReplicator{db: db.Raw()}}
	db.AttachReplicator(repl)
	base := time.Now().UTC()
	clock.Store(base.UnixNano())
	enqueue := func(id string, deadline time.Time) *domain.Task {
		ctx := domain.WithTaskExpiry(context.Background(), deadline)
		task, err := repo.Enqueue(ctx, absoluteTestTopic, `{}`, 3, "", 100, "", "", id, time.Time{}, "tenant-a")
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	for i := 0; i < 40; i++ {
		enqueue(fmt.Sprintf("expired-%03d", i), base.Add(time.Minute))
	}
	clock.Store(base.Add(61 * time.Second).UnixNano())
	if removed, err := db.sweepAbsolute(context.Background(), 100); err != nil || removed != 40 {
		t.Fatalf("sweep: removed=%d err=%v", removed, err)
	}
	fresh := enqueue("fresh", base.Add(121*time.Second))
	repl.writes.Store(0)
	got, ok, err := repo.Claim(context.Background(), "worker", []domain.Command{absoluteTestTopic}, 60, 100, 100, "tenant-a")
	if err != nil || !ok || got == nil || got.ID != fresh.ID {
		t.Fatalf("claim: task=%+v ok=%v err=%v", got, ok, err)
	}
	if writes := repl.writes.Load(); writes != 1 {
		t.Fatalf("replicated writes=%d; want only the live task's lease commit", writes)
	}
	if got.Status != domain.StatusInProgress || !got.ExpiresAt.Equal(*fresh.ExpiresAt) {
		t.Fatalf("task lifetime/status changed: %+v", got)
	}
}

type signalClaimPolicyRead struct {
	TopicPolicyReader
	once sync.Once
	read chan struct{}
}

func (r *signalClaimPolicyRead) AcquireAbsolutePolicyRead() func() {
	r.once.Do(func() { close(r.read) })
	return func() {}
}

func TestClaimCanceledDuringAbsoluteWaitDoesNotLease(t *testing.T) {
	db, repo, _, clock := absoluteFixture(t)
	other := enqueueAbsolute(t, repo, clock, "other-worker-task", "", time.Time{}, 100)
	if _, ok, err := repo.Claim(context.Background(), "other-worker", []domain.Command{absoluteTestTopic}, 60, 100, 100, "tenant-a"); err != nil || !ok {
		t.Fatalf("other worker claim: ok=%v err=%v", ok, err)
	}
	otherBefore, err := db.Get(KeyTask(other.ID))
	if err != nil {
		t.Fatal(err)
	}
	fresh := enqueueAbsolute(t, repo, clock, "waiting-task", "", time.Time{}, 100)
	reader := &signalClaimPolicyRead{TopicPolicyReader: db.policyReader, read: make(chan struct{})}
	db.SetTopicPolicyReader(reader)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type claimResult struct {
		task *domain.Task
		ok   bool
		err  error
	}
	done := make(chan claimResult, 1)
	// The expiry sweep holds this same mutex while replicating retirement.
	db.absoluteMu.Lock()
	go func() {
		task, ok, err := repo.Claim(ctx, "worker", []domain.Command{absoluteTestTopic}, 60, 100, 100, "tenant-a")
		done <- claimResult{task, ok, err}
	}()
	select {
	case <-reader.read:
	case <-time.After(time.Second):
		db.absoluteMu.Unlock()
		t.Fatal("claim did not reach absolute-expiry coordination")
	}
	cancel()
	db.absoluteMu.Unlock()
	var result claimResult
	select {
	case result = <-done:
	case <-time.After(time.Second):
		t.Fatal("claim did not finish after the mutex was released")
	}
	if !errors.Is(result.err, context.Canceled) || result.ok || result.task != nil {
		t.Fatalf("canceled claim: task=%+v ok=%v err=%v", result.task, result.ok, result.err)
	}
	stored, err := repo.Get(context.Background(), fresh.ID)
	if err != nil || stored.Status != domain.StatusPending || stored.WorkerID != "" || stored.Attempts != 0 || !stored.ExpiresAt.Equal(*fresh.ExpiresAt) {
		t.Fatalf("canceled claim mutated accepted task: %+v err=%v", stored, err)
	}
	if _, ok := db.Leases.Get(fresh.ID); ok {
		t.Fatal("canceled claim installed a memory lease")
	}
	if has, err := db.Has(KeyInprog(absoluteTestTopic, "tenant-a", fresh.ID)); err != nil || has {
		t.Fatalf("canceled claim installed in-progress index: has=%v err=%v", has, err)
	}
	otherAfter, err := db.Get(KeyTask(other.ID))
	if err != nil || string(otherAfter) != string(otherBefore) {
		t.Fatalf("other worker's lease changed: err=%v", err)
	}
	if count, err := db.absoluteCount(); err != nil || count != 2 {
		t.Fatalf("canceled claim changed identity accounting: count=%d err=%v", count, err)
	}
	got, ok, err := repo.Claim(context.Background(), "retry-worker", []domain.Command{absoluteTestTopic}, 60, 100, 100, "tenant-a")
	if err != nil || !ok || got == nil || got.ID != fresh.ID {
		t.Fatalf("canceled claim lost its retry hint: task=%+v ok=%v err=%v", got, ok, err)
	}
}
