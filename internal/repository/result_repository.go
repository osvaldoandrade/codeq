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

	// Get the task
	js, err := r.rdb.HGet(ctx, r.keyTasksHash(), rec.TaskID).Result()
	if err == redis.Nil || js == "" || err != nil {
		// Even if task doesn't exist, save the result
		if err := r.rdb.HSet(ctx, r.keyResultsHash(), rec.TaskID, string(b)).Err(); err != nil {
			return fmt.Errorf("redis HSET result: %w", err)
		}
		return nil
	}

	var t domain.Task
	if err := sonic.Unmarshal([]byte(js), &t); err != nil {
		// If unmarshal fails, still save the result and task
		if err := r.rdb.HSet(ctx, r.keyResultsHash(), rec.TaskID, string(b)).Err(); err != nil {
			return fmt.Errorf("redis HSET result: %w", err)
		}
		return nil
	}

	// Update task with result reference and save both in a single pipeline
	t.ResultKey = r.keyResultsHash()
	nb, _ := sonic.Marshal(t)
	
	pipe := r.rdb.Pipeline()
	pipe.HSet(ctx, r.keyResultsHash(), rec.TaskID, string(b))
	pipe.HSet(ctx, r.keyTasksHash(), rec.TaskID, string(nb))
	_, err = pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("redis pipeline: %w", err)
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
