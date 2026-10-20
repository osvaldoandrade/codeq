package pebble

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	raftpkg "github.com/osvaldoandrade/codeq/internal/raft"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

func TestAbsoluteExpiryRaftRestartKeepsRetirement(t *testing.T) {
	path := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	clock := &atomic.Int64{}
	clock.Store(time.Now().UnixNano())
	open := func() (*DB, *raftpkg.DB, *TaskRepository) {
		t.Helper()
		db, err := Open(Options{Path: filepath.Join(path, "state"), FsyncOnCommit: true})
		if err != nil {
			t.Fatal(err)
		}
		owner, err := raftpkg.OpenWithPebble(ctx, raftpkg.Config{Path: path, FsyncOnCommit: true, SelfID: "node-1", BindAddr: "127.0.0.1:0", Bootstrap: true, HeartbeatMS: 50, ElectionMS: 50, LeaderLeaseMS: 50, CommitMS: 10}, db.Raw())
		if err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		db.AttachReplicator(owner)
		db.expiryNow = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
		attachAbsolutePolicy(db, 7200, nil, time.Now())
		if err = owner.WaitLeader(ctx); err != nil {
			_ = owner.Close()
			_ = db.Close()
			t.Fatal(err)
		}
		return db, owner, NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	}
	db, owner, repo := open()
	task := enqueueAbsolute(t, repo, clock, "logs-raft-restart", "", time.Time{}, 100)
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, owner, repo = open()
	defer db.Close()
	defer owner.Close()
	replay, err := repo.Enqueue(domain.WithTaskExpiry(ctx, task.ExpiresAt.Add(time.Minute)), absoluteTestTopic, task.Payload, 3, "", 100, "", "", task.ID, time.Time{}, "tenant-a")
	if err != nil || replay == nil || !replay.CreatedAt.Equal(task.CreatedAt) || !replay.ExpiresAt.Equal(*task.ExpiresAt) {
		t.Fatalf("Raft restart replay=%+v %v", replay, err)
	}
	clock.Store(task.ExpiresAt.Add(time.Second).UnixNano())
	if _, err = repo.CleanupExpired(ctx, 100, db.expiryTime()); err != nil {
		t.Fatal(err)
	}
	assertNoPayloadIndexes(t, db, task.ID)
	if _, err = repo.Enqueue(domain.WithTaskExpiry(ctx, db.expiryTime().Add(time.Hour)), absoluteTestTopic, task.Payload, 3, "", 100, "", "", task.ID, time.Time{}, "tenant-a"); !errors.Is(err, domain.ErrTaskExpired) {
		t.Fatalf("Raft retirement replay=%v", err)
	}
}

func TestAbsoluteExpiryShardedCleanupPreservesIsolation(t *testing.T) {
	dbA, repoA, resultsA, clockA := absoluteFixture(t)
	dbB, repoB, resultsB, clockB := absoluteFixture(t)
	repo := NewShardedTaskRepository([]*TaskRepository{repoA, repoB})
	results := NewShardedResultRepository([]*ResultRepository{resultsA, resultsB})
	seen := map[int]bool{}
	for i := 0; len(seen) < 2 && i < 100; i++ {
		id := "logs-sharded-" + time.Unix(int64(i), 0).Format("150405")
		shard := repo.shardOf(id)
		if seen[shard] {
			continue
		}
		seen[shard] = true
		deadline := time.Now().Add(time.Minute).UTC()
		task, err := repo.Enqueue(domain.WithTaskExpiry(context.Background(), deadline), absoluteTestTopic, `{}`, 0, "", 100, "", "", id, time.Time{}, "tenant-a")
		if err != nil {
			t.Fatal(err)
		}
		if err = results.SaveResult(context.Background(), domain.ResultRecord{TaskID: id, Result: map[string]any{"count": 1}}, absoluteTestTopic, "tenant-a"); err != nil {
			t.Fatal(err)
		}
		clockA.Store(task.ExpiresAt.Add(time.Second).UnixNano())
		clockB.Store(clockA.Load())
		if got, err := repo.Get(context.Background(), id); got != nil || !errors.Is(err, domain.ErrTaskExpired) {
			t.Fatalf("shard%d exposed expired task=%+v %v", shard, got, err)
		}
		if _, err = repo.CleanupExpired(context.Background(), 100, time.Unix(0, clockA.Load())); err != nil {
			t.Fatal(err)
		}
		assertNoPayloadIndexes(t, dbA, id)
		assertNoPayloadIndexes(t, dbB, id)
		// Reset only the test clock for the next independent identity.
		clockA.Store(time.Now().UnixNano())
		clockB.Store(clockA.Load())
	}
	if len(seen) != 2 {
		t.Fatal("test did not cover every shard")
	}
	legacy, err := repo.Enqueue(context.Background(), "ordinary", `{}`, 0, "", 5, "", "", "", time.Time{}, "tenant-b")
	if err != nil || legacy.ExpiresAt != nil {
		t.Fatalf("legacy shard=%+v %v", legacy, err)
	}
}
