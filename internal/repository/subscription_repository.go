package repository

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"

	"github.com/bytedance/sonic"
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

	b, _ := sonic.Marshal(sub)
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

	b, _ := sonic.Marshal(sub)
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
	if err := sonic.Unmarshal([]byte(js), &sub); err != nil {
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

	// Batch fetch all subscription data in a single pipeline (1 RTT instead of N RTTs)
	pipe := r.rdb.Pipeline()
	for _, id := range ids {
		pipe.HGet(ctx, r.keySubsHash(), id)
	}
	results, err := pipe.Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("redis pipeline HGET: %w", err)
	}

	var toRemove []string
	subs := make([]domain.Subscription, 0, len(ids))
	for i, id := range ids {
		strCmd, ok := results[i].(*redis.StringCmd)
		if !ok {
			_ = r.rdb.ZRem(ctx, key, id).Err()
			continue
		}
		js, err := strCmd.Result()
		if err == redis.Nil || js == "" {
			toRemove = append(toRemove, id)
			continue
		}
		if err != nil {
			_ = r.rdb.ZRem(ctx, key, id).Err()
			continue
		}
		var sub domain.Subscription
		if err := sonic.Unmarshal([]byte(js), &sub); err != nil {
			_ = r.rdb.ZRem(ctx, key, id).Err()
			continue
		}
		if sub.ExpiresAt.Before(now) {
			toRemove = append(toRemove, id)
			continue
		}
		subs = append(subs, sub)
	}

	// Batch remove expired/invalid subscriptions
	if len(toRemove) > 0 {
		args := make([]interface{}, len(toRemove))
		for i, v := range toRemove {
			args[i] = v
		}
		_ = r.rdb.ZRem(ctx, key, args...).Err()
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

		// Batch get all subscription data (1 RTT instead of N RTTs)
		subsMap := make(map[string]*domain.Subscription)
		invalidIDs := make([]string, 0)

		// Fetch all subscriptions in one batch
		pipe := r.rdb.Pipeline()
		for _, id := range ids {
			pipe.HGet(ctx, r.keySubsHash(), id)
		}
		results, err := pipe.Exec(ctx)
		if err != nil && err != redis.Nil {
			return removed, err
		}

		// Parse results
		for i, id := range ids {
			if i < len(results) {
				if strCmd, ok := results[i].(*redis.StringCmd); ok {
					js, err := strCmd.Result()
					if err == redis.Nil || js == "" || err != nil {
						invalidIDs = append(invalidIDs, id)
						continue
					}
					var sub domain.Subscription
					if err := sonic.Unmarshal([]byte(js), &sub); err != nil {
						invalidIDs = append(invalidIDs, id)
						continue
					}
					subsMap[id] = &sub
				}
			}
		}

		// Separate into expired and valid (rejuvenated)
		toDelete := make([]*domain.Subscription, 0)
		toRejuvenate := make([]*domain.Subscription, 0)

		for _, id := range ids {
			if sub, ok := subsMap[id]; ok {
				if sub.ExpiresAt.After(before) {
					toRejuvenate = append(toRejuvenate, sub)
				} else {
					toDelete = append(toDelete, sub)
				}
			}
		}

		// Add invalid IDs to delete list
		toDelete_IDs := make([]string, 0, len(invalidIDs)+len(toDelete))
		toDelete_IDs = append(toDelete_IDs, invalidIDs...)
		for _, sub := range toDelete {
			toDelete_IDs = append(toDelete_IDs, sub.ID)
		}

		// Batch re-additions in one pipeline
		if len(toRejuvenate) > 0 {
			rejuvPipe := r.rdb.Pipeline()
			for _, sub := range toRejuvenate {
				rejuvPipe.ZAdd(ctx, key, &redis.Z{Score: float64(sub.ExpiresAt.UTC().Unix()), Member: sub.ID})
			}
			_, _ = rejuvPipe.Exec(ctx)
		}

		// Batch cleanup of expired + invalid subscriptions (N operations → 1 RTT)
		if len(toDelete_IDs) > 0 {
			cleanupPipe := r.rdb.Pipeline()
			for _, id := range toDelete_IDs {
				// Cleanup this subscription's entries
				if sub, ok := subsMap[id]; ok {
					for _, ev := range sub.EventTypes {
						cleanupPipe.ZRem(ctx, r.keySubsEvent(ev), id)
					}
				}
				cleanupPipe.HDel(ctx, r.keySubsHash(), id)
				cleanupPipe.Del(ctx, r.keySubNotifyThrottle(id))
				cleanupPipe.ZRem(ctx, key, id)
			}
			if _, err := cleanupPipe.Exec(ctx); err == nil {
				removed += len(toDelete_IDs)
			}
		}
	}
	return removed, nil
}
