package pebble

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/internal/core/queuetopic"
	pebblerepo "github.com/osvaldoandrade/codeq/internal/repository/pebble"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

func TestAbsolutePolicyMonotonicFloor(t *testing.T) {
	db, err := pebblerepo.Open(pebblerepo.Options{Path: t.TempDir(), FsyncOnCommit: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewTopicStore(db)
	now := time.Now().Add(-time.Hour)
	policy := testPolicy(20)
	policy.AbsoluteExpiry = true
	policy.RetentionSeconds = 3600
	first, err := queuetopic.New("acme", "events", policy, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = store.Upsert(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	policy.RetentionSeconds = 1800
	second, err := queuetopic.New("acme", "events", policy, now.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	shortened, _, _, err := store.Upsert(context.Background(), second)
	if err != nil || shortened.Policy.VisibilityFloor == nil || !shortened.Policy.VisibilityFloor.Equal(now) {
		t.Fatalf("shortened=%+v %v", shortened, err)
	}
	policy.RetentionSeconds = 7200
	third, err := queuetopic.New("acme", "events", policy, now.Add(45*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	increased, _, _, err := store.Upsert(context.Background(), third)
	if err != nil || increased.Policy.VisibilityFloor == nil || !increased.Policy.VisibilityFloor.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("increased=%+v %v", increased, err)
	}
	stable, created, changed, err := store.Upsert(context.Background(), third)
	if err != nil || created || changed || stable.Version != increased.Version {
		t.Fatalf("reconcile rewrote cutoff: %+v %v %v %v", stable, created, changed, err)
	}
}

func TestAbsolutePolicyCustodyIncludesOtherShardsAndEventuallyRetires(t *testing.T) {
	dbs := make([]*pebblerepo.DB, 2)
	for i := range dbs {
		db, err := pebblerepo.Open(pebblerepo.Options{Path: t.TempDir(), FsyncOnCommit: true})
		if err != nil {
			t.Fatal(err)
		}
		dbs[i] = db
		t.Cleanup(func() { _ = db.Close() })
	}
	store := NewTopicStore(dbs[0], dbs[1])
	for _, db := range dbs {
		db.SetTopicPolicyReader(store)
	}
	now := time.Now()
	policy := testPolicy(20)
	policy.AbsoluteExpiry = true
	policy.RetentionSeconds = 3600
	desired, err := queuetopic.New("acme", "events", policy, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = store.Upsert(context.Background(), desired); err != nil {
		t.Fatal(err)
	}
	repo := pebblerepo.NewTaskRepository(dbs[1], time.UTC, "fixed", 1, 5)
	deadline := time.Now().Add(time.Hour)
	task, err := repo.Enqueue(domain.WithTaskExpiry(context.Background(), deadline), "events", `{"private":"body"}`, 0, "", 3, "", "", "logs-other-shard", time.Time{}, "acme")
	if err != nil {
		t.Fatal(err)
	}
	var conflict *queuetopic.ConflictError
	if err = store.Delete(context.Background(), "acme", "events"); !errors.As(err, &conflict) {
		t.Fatalf("delete while shard1 owns task = %v", err)
	}
	policy.AbsoluteExpiry = false
	ordinary, err := queuetopic.New("acme", "events", policy, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = store.Upsert(context.Background(), ordinary); !errors.As(err, &conflict) {
		t.Fatalf("disable while custody=%v", err)
	}
	if err = repo.DeleteTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.CleanupExpired(context.Background(), 100, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = store.Delete(context.Background(), "acme", "events"); !errors.As(err, &conflict) {
		t.Fatalf("delete during tombstone=%v", err)
	}
	// Advance only this fixture's persisted metadata, not the process clock.
	key := []byte("codeq/absolute/" + task.ID)
	body, err := dbs[1].Get(key)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err = json.Unmarshal(body, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata["retiredAt"] = time.Now().Add(-49 * time.Hour)
	metadata["replayUntil"] = time.Now().Add(-time.Hour)
	body, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err = dbs[1].Set(key, body); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.CleanupExpired(context.Background(), 100, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = store.Upsert(context.Background(), ordinary); err != nil {
		t.Fatalf("disable after complete drain=%v", err)
	}
	if err = store.Delete(context.Background(), "acme", "events"); err != nil {
		t.Fatalf("delete after drain=%v", err)
	}
}
