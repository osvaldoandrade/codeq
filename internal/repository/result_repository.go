package repository

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"

	"github.com/bytedance/sonic"
	"github.com/go-redis/redis/v8"
)

type ResultRepository interface {
	GetTask(ctx context.Context, id string) (*domain.Task, error)
	GetTaskAndResult(ctx context.Context, id string) (*domain.Task, *domain.ResultRecord, error)
	SaveResult(ctx context.Context, rec domain.ResultRecord) error
	GetResult(ctx context.Context, id string) (*domain.ResultRecord, error)
	UpdateTaskOnComplete(ctx context.Context, id string, status domain.TaskStatus, errorMsg string) error
	RemoveFromInprogAndClearLease(ctx context.Context, id string, cmd domain.Command) error
	DecodeBase64(s string) ([]byte, error)
}

type resultRedisRepo struct {
	rdb *redis.Client
	tz  *time.Location
}

func NewResultRepository(rdb *redis.Client, tz *time.Location) ResultRepository {
	return &resultRedisRepo{rdb: rdb, tz: tz}
}

func (r *resultRedisRepo) keyTasksHash() string   { return "codeq:tasks" }
func (r *resultRedisRepo) keyResultsHash() string { return "codeq:results" }
func (r *resultRedisRepo) keyTTLIndex() string    { return "codeq:tasks:ttl" }
func (r *resultRedisRepo) keyLease(id string) string {
	return fmt.Sprintf("codeq:lease:%s", id)
}
func (r *resultRedisRepo) keyQueueInprog(cmd domain.Command) string {
	return fmt.Sprintf("codeq:q:%s:inprog", strings.ToLower(string(cmd)))
}

func (r *resultRedisRepo) now() time.Time { return time.Now().In(r.tz) }

func (r *resultRedisRepo) GetTask(ctx context.Context, id string) (*domain.Task, error) {
	js, err := r.rdb.HGet(ctx, r.keyTasksHash(), id).Result()
	if err == redis.Nil || js == "" {
		return nil, fmt.Errorf("not-found")
	}
	if err != nil {
		return nil, fmt.Errorf("redis HGET task: %w", err)
	}
	var t domain.Task
	if err := sonic.Unmarshal([]byte(js), &t); err != nil {
		return nil, fmt.Errorf("unmarshal task: %w", err)
	}
	return &t, nil
}

func (r *resultRedisRepo) SaveResult(ctx context.Context, rec domain.ResultRecord) error {
	b, _ := sonic.Marshal(rec)

	// Fetch task first (required to compute new value with ResultKey)
	js, err := r.rdb.HGet(ctx, r.keyTasksHash(), rec.TaskID).Result()
	if err != nil && err != redis.Nil {
		return fmt.Errorf("redis HGET task: %w", err)
	}

	// If task not found or unmarshal fails, just save result (non-fatal)
	if err == redis.Nil || js == "" {
		return r.rdb.HSet(ctx, r.keyResultsHash(), rec.TaskID, string(b)).Err()
	}

	var t domain.Task
	if err := sonic.Unmarshal([]byte(js), &t); err != nil {
		// Task unmarshal failed, but still save result (non-fatal)
		return r.rdb.HSet(ctx, r.keyResultsHash(), rec.TaskID, string(b)).Err()
	}

	// Update task with result reference
	t.ResultKey = r.keyResultsHash()
	nb, _ := sonic.Marshal(t)

	// Consolidate all writes into single pipeline (1 RTT):
	// - Save result record
	// - Update task with result reference
	// - Bump task TTL for expiration tracking
	// This reduces latency by 50% vs. separate pipelines (2 RTTs → 1 RTT for writes)
	writePipe := r.rdb.Pipeline()
	writePipe.HSet(ctx, r.keyResultsHash(), rec.TaskID, string(b))
	writePipe.HSet(ctx, r.keyTasksHash(), rec.TaskID, string(nb))
	writePipe.ZAdd(ctx, r.keyTTLIndex(), &redis.Z{Score: float64(r.now().Add(24 * time.Hour).UTC().Unix()), Member: rec.TaskID})
	if _, err := writePipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis write pipeline: %w", err)
	}
	return nil
}

func (r *resultRedisRepo) GetResult(ctx context.Context, id string) (*domain.ResultRecord, error) {
	js, err := r.rdb.HGet(ctx, r.keyResultsHash(), id).Result()
	if err == redis.Nil || js == "" {
		return nil, fmt.Errorf("not-found")
	}
	if err != nil {
		return nil, fmt.Errorf("redis HGET result: %w", err)
	}
	var rec domain.ResultRecord
	if err := sonic.Unmarshal([]byte(js), &rec); err != nil {
		return nil, fmt.Errorf("unmarshal result: %w", err)
	}
	return &rec, nil
}

func (r *resultRedisRepo) GetTaskAndResult(ctx context.Context, id string) (*domain.Task, *domain.ResultRecord, error) {
	// Pipeline both HGET calls to reduce RTT from 2 to 1
	pipe := r.rdb.Pipeline()
	taskCmd := pipe.HGet(ctx, r.keyTasksHash(), id)
	resultCmd := pipe.HGet(ctx, r.keyResultsHash(), id)
	_, err := pipe.Exec(ctx)
	if err != nil && err != redis.Nil {
		return nil, nil, fmt.Errorf("redis pipeline: %w", err)
	}

	// Parse task
	taskJS, err := taskCmd.Result()
	if err == redis.Nil || taskJS == "" {
		return nil, nil, fmt.Errorf("task not-found")
	}
	if err != nil {
		return nil, nil, fmt.Errorf("redis HGET task: %w", err)
	}
	var task domain.Task
	if err := sonic.Unmarshal([]byte(taskJS), &task); err != nil {
		return nil, nil, fmt.Errorf("unmarshal task: %w", err)
	}

	// Parse result
	resultJS, err := resultCmd.Result()
	if err == redis.Nil || resultJS == "" {
		return &task, nil, fmt.Errorf("result not-found")
	}
	if err != nil {
		return &task, nil, fmt.Errorf("redis HGET result: %w", err)
	}
	var rec domain.ResultRecord
	if err := sonic.Unmarshal([]byte(resultJS), &rec); err != nil {
		return &task, nil, fmt.Errorf("unmarshal result: %w", err)
	}

	return &task, &rec, nil
}

func (r *resultRedisRepo) UpdateTaskOnComplete(ctx context.Context, id string, status domain.TaskStatus, errorMsg string) error {
	// Fetch task
	js, err := r.rdb.HGet(ctx, r.keyTasksHash(), id).Result()
	if err == redis.Nil || js == "" {
		return fmt.Errorf("not-found")
	}
	if err != nil {
		return fmt.Errorf("redis HGET task: %w", err)
	}
	var t domain.Task
	if err := sonic.Unmarshal([]byte(js), &t); err != nil {
		return fmt.Errorf("unmarshal task: %w", err)
	}
	t.Status = status
	t.LastKnownLocation = domain.LocationNone
	t.WorkerID = ""
	t.LeaseUntil = ""
	t.UpdatedAt = r.now()

	b, _ := sonic.Marshal(t)

	// Pipeline both the task update and TTL bump in single RTT
	pipe := r.rdb.Pipeline()
	pipe.HSet(ctx, r.keyTasksHash(), id, string(b))
	pipe.ZAdd(ctx, r.keyTTLIndex(), &redis.Z{Score: float64(r.now().Add(24 * time.Hour).UTC().Unix()), Member: id})
	_, err = pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("redis pipeline: %w", err)
	}
	return nil
}

func (r *resultRedisRepo) RemoveFromInprogAndClearLease(ctx context.Context, id string, cmd domain.Command) error {
	inprog := r.keyQueueInprog(cmd)

	// Pipeline both cleanup operations in single RTT
	pipe := r.rdb.Pipeline()
	pipe.SRem(ctx, inprog, id)
	pipe.Del(ctx, r.keyLease(id))
	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("redis pipeline: %w", err)
	}
	return nil
}

func (r *resultRedisRepo) DecodeBase64(str string) ([]byte, error) {
	if m := len(str) % 4; m != 0 {
		str += strings.Repeat("=", 4-m)
	}
	return base64.StdEncoding.DecodeString(str)
}
