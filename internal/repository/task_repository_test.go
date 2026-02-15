package repository

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

type redisCmdCountHook struct {
	gets  atomic.Int64
	lrems atomic.Int64
	hdels atomic.Int64
	zrems atomic.Int64
	dels  atomic.Int64
}

func (h *redisCmdCountHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	return ctx, nil
}

func (h *redisCmdCountHook) AfterProcess(ctx context.Context, cmd redis.Cmder) error {
	switch strings.ToLower(cmd.Name()) {
	case "get":
		h.gets.Add(1)
	case "lrem":
		h.lrems.Add(1)
	case "hdel":
		h.hdels.Add(1)
	case "zrem":
		h.zrems.Add(1)
	case "del":
		h.dels.Add(1)
	}
	return nil
}

func (h *redisCmdCountHook) BeforeProcessPipeline(ctx context.Context, cmds []redis.Cmder) (context.Context, error) {
	return ctx, nil
}

func (h *redisCmdCountHook) AfterProcessPipeline(ctx context.Context, cmds []redis.Cmder) error {
	for _, cmd := range cmds {
		_ = h.AfterProcess(ctx, cmd)
	}
	return nil
}

func setupRepo(t *testing.T) (context.Context, *miniredis.Miniredis, *redis.Client, TaskRepository) {
	return setupRepoWithBackoff(t, "exp_full_jitter", 1, 10)
}

func setupRepoWithBackoff(t *testing.T, policy string, baseSeconds int, maxSeconds int) (context.Context, *miniredis.Miniredis, *redis.Client, TaskRepository) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis start: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	repo := NewTaskRepository(rdb, time.UTC, policy, baseSeconds, maxSeconds)
	return context.Background(), mr, rdb, repo
}

func TestEnqueueIdempotent(t *testing.T) {
	ctx, _, rdb, repo := setupRepo(t)
	cmd := domain.CmdGenerateMaster
	task1, err := repo.Enqueue(ctx, cmd, `{"a":1}`, 0, "", 5, "job-123", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	task2, err := repo.Enqueue(ctx, cmd, `{"a":1}`, 0, "", 5, "job-123", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}
	if task1.ID != task2.ID {
		t.Fatalf("expected same task id for idempotency key, got %s vs %s", task1.ID, task2.ID)
	}
	key := "codeq:q:" + strings.ToLower(string(cmd)) + ":pending:0"
	if n, _ := rdb.LLen(ctx, key).Result(); n != 1 {
		t.Fatalf("expected 1 pending item, got %d", n)
	}
}

func TestEnqueueIdempotentBloomSkipsNegativeGet(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis start: %v", err)
	}
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	hook := &redisCmdCountHook{}
	rdb.AddHook(hook)

	repo := NewTaskRepository(rdb, time.UTC, "exp_full_jitter", 1, 10)
	ctx := context.Background()
	cmd := domain.CmdGenerateMaster

	_, err = repo.Enqueue(ctx, cmd, `{"a":1}`, 0, "", 5, "job-uniq-1", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	if got := hook.gets.Load(); got != 0 {
		t.Fatalf("expected 0 Redis GETs on first-seen idempotency key, got %d", got)
	}

	_, err = repo.Enqueue(ctx, cmd, `{"a":1}`, 0, "", 5, "job-uniq-1", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}
	if got := hook.gets.Load(); got != 1 {
		t.Fatalf("expected exactly 1 Redis GET on second enqueue, got %d", got)
	}
}

func TestPriorityClaim(t *testing.T) {
	ctx, _, _, repo := setupRepo(t)
	cmd := domain.CmdGenerateMaster
	low, err := repo.Enqueue(ctx, cmd, `{"p":0}`, 0, "", 5, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue low: %v", err)
	}
	high, err := repo.Enqueue(ctx, cmd, `{"p":9}`, 9, "", 5, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue high: %v", err)
	}
	got, ok, err := repo.Claim(ctx, "worker-1", []domain.Command{cmd}, 60, 50, 5, "")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !ok {
		t.Fatalf("expected claim to succeed")
	}
	if got.LastKnownLocation != domain.LocationInProgress {
		t.Fatalf("expected lastKnownLocation=%s, got %s", domain.LocationInProgress, got.LastKnownLocation)
	}
	if got.ID != high.ID {
		t.Fatalf("expected high priority task, got %s (low=%s)", got.ID, low.ID)
	}
}

func TestClaimRepairRequeuesExpiredLease(t *testing.T) {
	ctx, mr, rdb, repo := setupRepoWithBackoff(t, "fixed", 1, 1)
	cmd := domain.CmdGenerateMaster

	task, err := repo.Enqueue(ctx, cmd, `{"x":1}`, 0, "", 5, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	claimed, ok, err := repo.Claim(ctx, "worker-1", []domain.Command{cmd}, 1, 50, 5, "")
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if claimed.ID != task.ID {
		t.Fatalf("expected claimed id=%s, got %s", task.ID, claimed.ID)
	}

	inprogKey := "codeq:q:" + strings.ToLower(string(cmd)) + ":inprog"
	if n, _ := rdb.SCard(ctx, inprogKey).Result(); n != 1 {
		t.Fatalf("expected inprog size=1, got %d", n)
	}

	// Expire the lease key in Redis without waiting on wall clock time.
	mr.FastForward(2 * time.Second)

	// Next claim triggers repair; task is requeued to delayed with backoff.
	_, ok, err = repo.Claim(ctx, "worker-2", []domain.Command{cmd}, 60, 50, 5, "")
	if err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	if ok {
		t.Fatalf("expected no immediate claim; expired task should be requeued to delayed")
	}

	if n, _ := rdb.SCard(ctx, inprogKey).Result(); n != 0 {
		t.Fatalf("expected inprog size=0 after repair, got %d", n)
	}

	delayedKey := "codeq:q:" + strings.ToLower(string(cmd)) + ":delayed"
	if _, err := rdb.ZScore(ctx, delayedKey, task.ID).Result(); err != nil {
		t.Fatalf("expected task in delayed after repair, got err=%v", err)
	}
}

func TestNackDelayedAndDLQ(t *testing.T) {
	ctx, _, rdb, repo := setupRepo(t)
	cmd := domain.CmdGenerateMaster
	task, err := repo.Enqueue(ctx, cmd, `{"x":1}`, 0, "", 3, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, ok, err := repo.Claim(ctx, "worker-1", []domain.Command{cmd}, 60, 50, 3, "")
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if claimed.Attempts != 1 {
		t.Fatalf("expected attempts=1 after claim, got %d", claimed.Attempts)
	}

	delay, dlq, err := repo.Nack(ctx, task.ID, "worker-1", 0, 3, "ERR")
	if err != nil {
		t.Fatalf("nack: %v", err)
	}
	if dlq {
		t.Fatalf("expected not in dlq on first nack")
	}
	if delay != 0 {
		t.Fatalf("expected delay 0, got %d", delay)
	}
	if storedAfterNack, err := repo.Get(ctx, task.ID); err != nil {
		t.Fatalf("get after nack: %v", err)
	} else if storedAfterNack.LastKnownLocation != domain.LocationDelayed {
		t.Fatalf("expected lastKnownLocation=%s after nack, got %s", domain.LocationDelayed, storedAfterNack.LastKnownLocation)
	}

	if moved, err := repo.MoveDueDelayed(ctx, cmd, 10); err != nil || moved != 1 {
		t.Fatalf("move due delayed: moved=%d err=%v", moved, err)
	}

	claimed2, ok, err := repo.Claim(ctx, "worker-1", []domain.Command{cmd}, 60, 50, 3, "")
	if err != nil || !ok {
		t.Fatalf("claim 2: ok=%v err=%v", ok, err)
	}
	if claimed2.Attempts != 3 {
		t.Fatalf("expected attempts=3 after second claim, got %d", claimed2.Attempts)
	}

	_, dlq, err = repo.Nack(ctx, task.ID, "worker-1", 0, 3, "ERR")
	if err != nil {
		t.Fatalf("nack 2: %v", err)
	}
	if !dlq {
		t.Fatalf("expected dlq on second nack")
	}
	stored, err := repo.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if stored.Status != domain.StatusFailed {
		t.Fatalf("expected failed status, got %s", stored.Status)
	}
	if stored.LastKnownLocation != domain.LocationDLQ {
		t.Fatalf("expected lastKnownLocation=%s in dlq, got %s", domain.LocationDLQ, stored.LastKnownLocation)
	}
	dlqKey := "codeq:q:" + strings.ToLower(string(cmd)) + ":dlq"
	if n, _ := rdb.SCard(ctx, dlqKey).Result(); n != 1 {
		t.Fatalf("expected 1 item in dlq, got %d", n)
	}
}

func TestCleanupExpired(t *testing.T) {
	ctx, _, _, repo := setupRepo(t)
	cmd := domain.CmdGenerateMaster
	task1, err := repo.Enqueue(ctx, cmd, `{"x":1}`, 0, "", 5, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	task2, err := repo.Enqueue(ctx, cmd, `{"x":2}`, 0, "", 5, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}
	deleted, err := repo.CleanupExpired(ctx, 10, time.Now().Add(25*time.Hour))
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if deleted < 2 {
		t.Fatalf("expected at least 2 deletions, got %d", deleted)
	}
	if _, err := repo.Get(ctx, task1.ID); err == nil {
		t.Fatalf("expected task1 to be deleted")
	}
	if _, err := repo.Get(ctx, task2.ID); err == nil {
		t.Fatalf("expected task2 to be deleted")
	}
}

func TestCleanupExpiredBloomSkipsAlreadyRemovedIDs(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis start: %v", err)
	}
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	hook := &redisCmdCountHook{}
	rdb.AddHook(hook)

	repo := NewTaskRepository(rdb, time.UTC, "exp_full_jitter", 1, 10)
	r := repo.(*taskRedisRepo)
	ctx := context.Background()
	cmd := domain.CmdGenerateMaster

	task, err := repo.Enqueue(ctx, cmd, `{"x":1}`, 0, "", 5, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Simulate that another cleanup already removed this task fully, but this cleanup run
	// still observes the same ID (e.g., concurrent cleanup cycles / stale read).
	if err := r.removeTaskFully(ctx, task.ID); err != nil {
		t.Fatalf("removeTaskFully: %v", err)
	}
	expiredScore := float64(time.Now().Add(-1 * time.Hour).UTC().Unix())
	if err := rdb.ZAdd(ctx, r.keyTTLIndex(), &redis.Z{Score: expiredScore, Member: task.ID}).Err(); err != nil {
		t.Fatalf("ZADD ttl-index: %v", err)
	}

	hook.lrems.Store(0)
	hook.hdels.Store(0)
	hook.zrems.Store(0)
	hook.dels.Store(0)

	deleted, err := repo.CleanupExpired(ctx, 10, time.Now())
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("expected 0 deletions (skipped via bloom), got %d", deleted)
	}
	if got := hook.lrems.Load(); got != 0 {
		t.Fatalf("expected 0 LREM calls when skipping, got %d", got)
	}
	if got := hook.hdels.Load(); got != 0 {
		t.Fatalf("expected 0 HDEL calls when skipping, got %d", got)
	}
	if got := hook.zrems.Load(); got != 0 {
		t.Fatalf("expected 0 ZREM calls when skipping, got %d", got)
	}
	if got := hook.dels.Load(); got != 0 {
		t.Fatalf("expected 0 DEL calls when skipping, got %d", got)
	}
}

func TestEnqueueScheduledGoesToDelayed(t *testing.T) {
	ctx, _, rdb, repo := setupRepo(t)
	cmd := domain.CmdGenerateMaster
	runAt := time.Now().UTC().Add(1 * time.Hour)

	task, err := repo.Enqueue(ctx, cmd, `{"x":1}`, 0, "", 5, "", runAt, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if task.LastKnownLocation != domain.LocationDelayed {
		t.Fatalf("expected lastKnownLocation=%s, got %s", domain.LocationDelayed, task.LastKnownLocation)
	}

	// Should not be visible in pending yet.
	pendingKey := "codeq:q:" + strings.ToLower(string(cmd)) + ":pending:0"
	if n, _ := rdb.LLen(ctx, pendingKey).Result(); n != 0 {
		t.Fatalf("expected 0 pending items, got %d", n)
	}

	delayedKey := "codeq:q:" + strings.ToLower(string(cmd)) + ":delayed"
	score, err := rdb.ZScore(ctx, delayedKey, task.ID).Result()
	if err != nil {
		t.Fatalf("expected task in delayed zset: %v", err)
	}
	if int64(score) != runAt.UTC().Unix() {
		t.Fatalf("expected delayed score=%d, got %d", runAt.UTC().Unix(), int64(score))
	}
}

func TestHeartbeat(t *testing.T) {
	ctx, _, _, repo := setupRepo(t)
	cmd := domain.CmdGenerateMaster
	_, err := repo.Enqueue(ctx, cmd, `{"x":1}`, 0, "", 5, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	claimed, ok, err := repo.Claim(ctx, "worker-1", []domain.Command{cmd}, 60, 50, 5, "")
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}

	err = repo.Heartbeat(ctx, claimed.ID, "worker-1", 120)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
}

func TestAbandon(t *testing.T) {
	ctx, _, _, repo := setupRepo(t)
	cmd := domain.CmdGenerateMaster
	_, err := repo.Enqueue(ctx, cmd, `{"x":1}`, 0, "", 5, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	claimed, ok, err := repo.Claim(ctx, "worker-1", []domain.Command{cmd}, 60, 50, 5, "")
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}

	err = repo.Abandon(ctx, claimed.ID, "worker-1")
	if err != nil {
		t.Fatalf("abandon: %v", err)
	}
}

func TestAdminQueues(t *testing.T) {
	ctx, _, _, repo := setupRepo(t)
	cmd := domain.CmdGenerateMaster
	_, err := repo.Enqueue(ctx, cmd, `{"x":1}`, 0, "", 5, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	queues, err := repo.AdminQueues(ctx)
	if err != nil {
		t.Fatalf("admin queues: %v", err)
	}
	if queues == nil {
		t.Fatalf("expected non-nil queues map")
	}
}

func TestQueueStats(t *testing.T) {
	ctx, _, _, repo := setupRepo(t)
	cmd := domain.CmdGenerateMaster
	_, err := repo.Enqueue(ctx, cmd, `{"x":1}`, 0, "", 5, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	stats, err := repo.QueueStats(ctx, cmd)
	if err != nil {
		t.Fatalf("queue stats: %v", err)
	}
	if stats == nil {
		t.Fatalf("expected non-nil stats")
	}
	if stats.Ready < 1 {
		t.Fatalf("expected at least 1 ready task, got %d", stats.Ready)
	}
}

func TestPendingLength(t *testing.T) {
	ctx, _, _, repo := setupRepo(t)
	cmd := domain.CmdGenerateMaster
	_, err := repo.Enqueue(ctx, cmd, `{"x":1}`, 0, "", 5, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	length, err := repo.PendingLength(ctx, cmd)
	if err != nil {
		t.Fatalf("pending length: %v", err)
	}
	if length < 1 {
		t.Fatalf("expected at least 1 pending task, got %d", length)
	}
}
