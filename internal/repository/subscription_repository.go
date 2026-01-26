package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"

	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
)

type SubscriptionRepository interface {
	Create(ctx context.Context, sub domain.Subscription, ttlSeconds int) (*domain.Subscription, error)
	Heartbeat(ctx context.Context, id string, ttlSeconds int) (*domain.Subscription, error)
	Get(ctx context.Context, id string) (*domain.Subscription, error)
	ListActive(ctx context.Context, cmd domain.Command, now time.Time) ([]domain.Subscription, error)
	AllowNotify(ctx context.Context, id string, minIntervalSeconds int) (bool, error)
	NextGroupIndex(ctx context.Context, cmd domain.Command, groupID string, mod int) (int, error)
	CleanupExpired(ctx context.Context, limit int, before time.Time) (int, error)
}

type subscriptionRedisRepo struct {
	rdb *redis.Client
	tz  *time.Location
}

func NewSubscriptionRepository(rdb *redis.Client, tz *time.Location) SubscriptionRepository {
	return &subscriptionRedisRepo{rdb: rdb, tz: tz}
}

func (r *subscriptionRedisRepo) keySubsHash() string {
	return "codeq:subs"
}

func (r *subscriptionRedisRepo) keySubsEvent(cmd domain.Command) string {
	return fmt.Sprintf("codeq:subs:%s", strings.ToLower(string(cmd)))
}

func (r *subscriptionRedisRepo) keySubNotifyThrottle(id string) string {
	return fmt.Sprintf("codeq:subs:last:%s", id)
}

func (r *subscriptionRedisRepo) keyGroupRR(cmd domain.Command, groupID string) string {
	return fmt.Sprintf("codeq:subs:rr:%s:%s", strings.ToLower(string(cmd)), groupID)
}

func (r *subscriptionRedisRepo) now() time.Time { return time.Now().In(r.tz) }

func (r *subscriptionRedisRepo) allCommands() []domain.Command {
	return []domain.Command{domain.CmdGenerateMaster, domain.CmdGenerateCreative}
}

func (r *subscriptionRedisRepo) Create(ctx context.Context, sub domain.Subscription, ttlSeconds int) (*domain.Subscription, error) {
	if sub.ID == "" {
		sub.ID = uuid.NewString()
	}
	if ttlSeconds <= 0 {
		ttlSeconds = 300
	}
	if sub.MinIntervalSeconds <= 0 {
		sub.MinIntervalSeconds = 5
	}
	now := r.now()
	sub.CreatedAt = now
	sub.ExpiresAt = now.Add(time.Duration(ttlSeconds) * time.Second)

	b, _ := json.Marshal(sub)
	pipe := r.rdb.TxPipeline()
	pipe.HSet(ctx, r.keySubsHash(), sub.ID, string(b))
	for _, cmd := range sub.EventTypes {
		pipe.ZAdd(ctx, r.keySubsEvent(cmd), &redis.Z{Score: float64(sub.ExpiresAt.UTC().Unix()), Member: sub.ID})
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	return &sub, nil
}

func (r *subscriptionRedisRepo) Heartbeat(ctx context.Context, id string, ttlSeconds int) (*domain.Subscription, error) {
	sub, err := r.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if ttlSeconds <= 0 {
		ttlSeconds = 300
	}
	sub.ExpiresAt = r.now().Add(time.Duration(ttlSeconds) * time.Second)

	b, _ := json.Marshal(sub)
	pipe := r.rdb.TxPipeline()
	pipe.HSet(ctx, r.keySubsHash(), id, string(b))
	for _, cmd := range sub.EventTypes {
		pipe.ZAdd(ctx, r.keySubsEvent(cmd), &redis.Z{Score: float64(sub.ExpiresAt.UTC().Unix()), Member: sub.ID})
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	return sub, nil
}

func (r *subscriptionRedisRepo) Get(ctx context.Context, id string) (*domain.Subscription, error) {
	js, err := r.rdb.HGet(ctx, r.keySubsHash(), id).Result()
	if err == redis.Nil || js == "" {
		return nil, fmt.Errorf("not-found")
	}
	if err != nil {
		return nil, fmt.Errorf("redis HGET sub: %w", err)
	}
	var sub domain.Subscription
	if err := json.Unmarshal([]byte(js), &sub); err != nil {
		return nil, fmt.Errorf("unmarshal sub: %w", err)
	}
	return &sub, nil
}

func (r *subscriptionRedisRepo) ListActive(ctx context.Context, cmd domain.Command, now time.Time) ([]domain.Subscription, error) {
	key := r.keySubsEvent(cmd)
	min := fmt.Sprintf("%d", now.UTC().Unix())
	zrange := &redis.ZRangeBy{Min: min, Max: "+inf", Offset: 0, Count: 1000}
	ids, err := r.rdb.ZRangeByScore(ctx, key, zrange).Result()
	if err != nil && err != redis.Nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	subs := make([]domain.Subscription, 0, len(ids))
	for _, id := range ids {
		sub, err := r.Get(ctx, id)
		if err != nil {
			_ = r.rdb.ZRem(ctx, key, id).Err()
			continue
		}
		if sub.ExpiresAt.Before(now) {
			_ = r.rdb.ZRem(ctx, key, id).Err()
			continue
		}
		subs = append(subs, *sub)
	}
	return subs, nil
}

func (r *subscriptionRedisRepo) AllowNotify(ctx context.Context, id string, minIntervalSeconds int) (bool, error) {
	if minIntervalSeconds <= 0 {
		minIntervalSeconds = 5
	}
	ok, err := r.rdb.SetNX(ctx, r.keySubNotifyThrottle(id), "1", time.Duration(minIntervalSeconds)*time.Second).Result()
	if err != nil && err != redis.Nil {
		return false, err
	}
	return ok, nil
}

func (r *subscriptionRedisRepo) NextGroupIndex(ctx context.Context, cmd domain.Command, groupID string, mod int) (int, error) {
	if mod <= 0 {
		return 0, nil
	}
	n, err := r.rdb.Incr(ctx, r.keyGroupRR(cmd, groupID)).Result()
	if err != nil && err != redis.Nil {
		return 0, err
	}
	return int(n % int64(mod)), nil
}

func (r *subscriptionRedisRepo) CleanupExpired(ctx context.Context, limit int, before time.Time) (int, error) {
	if limit <= 0 {
		limit = 1000
	}
	maxTS := fmt.Sprintf("%d", before.UTC().Unix())
	removed := 0

	for _, cmd := range r.allCommands() {
		key := r.keySubsEvent(cmd)
		zrange := &redis.ZRangeBy{Min: "-inf", Max: maxTS, Offset: 0, Count: int64(limit)}
		ids, err := r.rdb.ZRangeByScore(ctx, key, zrange).Result()
		if err != nil && err != redis.Nil {
			return removed, err
		}
		for _, id := range ids {
			sub, err := r.Get(ctx, id)
			if err != nil {
				_ = r.rdb.ZRem(ctx, key, id).Err()
				_ = r.rdb.HDel(ctx, r.keySubsHash(), id).Err()
				_ = r.rdb.Del(ctx, r.keySubNotifyThrottle(id)).Err()
				removed++
				continue
			}
			if sub.ExpiresAt.After(before) {
				_ = r.rdb.ZAdd(ctx, key, &redis.Z{Score: float64(sub.ExpiresAt.UTC().Unix()), Member: sub.ID}).Err()
				continue
			}
			pipe := r.rdb.TxPipeline()
			for _, ev := range sub.EventTypes {
				pipe.ZRem(ctx, r.keySubsEvent(ev), sub.ID)
			}
			pipe.HDel(ctx, r.keySubsHash(), sub.ID)
			pipe.Del(ctx, r.keySubNotifyThrottle(sub.ID))
			if _, err := pipe.Exec(ctx); err == nil {
				removed++
			}
		}
	}
	return removed, nil
}
