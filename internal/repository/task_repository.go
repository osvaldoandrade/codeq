package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/osvaldoandrade/codeq/internal/backoff"
	"github.com/osvaldoandrade/codeq/internal/metrics"
	"github.com/osvaldoandrade/codeq/pkg/domain"

	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
)

type TaskRepository interface {
	Enqueue(ctx context.Context, cmd domain.Command, payload string, priority int, webhook string, maxAttempts int, idempotencyKey string, visibleAt time.Time, tenantID string) (*domain.Task, error)
	Claim(ctx context.Context, workerID string, commands []domain.Command, leaseSeconds int, inspectLimit int, maxAttemptsDefault int, tenantID string) (*domain.Task, bool, error)
	Heartbeat(ctx context.Context, taskID string, workerID string, extendSeconds int) error
	Abandon(ctx context.Context, taskID string, workerID string) error
	Nack(ctx context.Context, taskID string, workerID string, delaySeconds int, maxAttemptsDefault int, reason string) (int, bool, error)
	MoveDueDelayed(ctx context.Context, cmd domain.Command, limit int) (int, error)
	PendingLength(ctx context.Context, cmd domain.Command) (int64, error)
	Get(ctx context.Context, taskID string) (*domain.Task, error)
	AdminQueues(ctx context.Context) (map[string]any, error)
	QueueStats(ctx context.Context, cmd domain.Command) (*domain.QueueStats, error)

	// Novo: limpeza administrativa por índice Z (sem custo no Claim/Get)
	CleanupExpired(ctx context.Context, limit int, before time.Time) (int, error)
}

type taskRedisRepo struct {
	rdb                *redis.Client
	tz                 *time.Location
	backoffPolicy      string
	backoffBaseSeconds int
	backoffMaxSeconds  int
	rng                *rand.Rand
}

func NewTaskRepository(rdb *redis.Client, tz *time.Location, backoffPolicy string, backoffBaseSeconds int, backoffMaxSeconds int) TaskRepository {
	if backoffBaseSeconds <= 0 {
		backoffBaseSeconds = 5
	}
	if backoffMaxSeconds <= 0 {
		backoffMaxSeconds = 900
	}
	if backoffPolicy == "" {
		backoffPolicy = "exp_full_jitter"
	}
	return &taskRedisRepo{
		rdb:                rdb,
		tz:                 tz,
		backoffPolicy:      backoffPolicy,
		backoffBaseSeconds: backoffBaseSeconds,
		backoffMaxSeconds:  backoffMaxSeconds,
		rng:                rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// ===== Retenção lógica (não TTL nativo) =====
const taskRetention = 24 * time.Hour // 24h conforme solicitado

const (
	minPriority = 0
	maxPriority = 9
)

// ===== Chaves Redis =====
func (r *taskRedisRepo) keyTasksHash() string { return "codeq:tasks" }     // HASH único: field = id, value = JSON
func (r *taskRedisRepo) keyTTLIndex() string  { return "codeq:tasks:ttl" } // ZSET: member=id, score=expireAt (epoch)

func (r *taskRedisRepo) keyLease(id string) string { return fmt.Sprintf("codeq:lease:%s", id) }
func (r *taskRedisRepo) keyQueuePending(cmd domain.Command, priority int, tenantID string) string {
	if tenantID == "" {
		return fmt.Sprintf("codeq:q:%s:pending:%d", strings.ToLower(string(cmd)), priority)
	}
	return fmt.Sprintf("codeq:q:%s:%s:pending:%d", strings.ToLower(string(cmd)), tenantID, priority)
}
func (r *taskRedisRepo) keyQueueInprog(cmd domain.Command, tenantID string) string {
	if tenantID == "" {
		return fmt.Sprintf("codeq:q:%s:inprog", strings.ToLower(string(cmd)))
	}
	return fmt.Sprintf("codeq:q:%s:%s:inprog", strings.ToLower(string(cmd)), tenantID)
}
func (r *taskRedisRepo) keyQueueDelayed(cmd domain.Command, tenantID string) string {
	if tenantID == "" {
		return fmt.Sprintf("codeq:q:%s:delayed", strings.ToLower(string(cmd)))
	}
	return fmt.Sprintf("codeq:q:%s:%s:delayed", strings.ToLower(string(cmd)), tenantID)
}
func (r *taskRedisRepo) keyQueueDLQ(cmd domain.Command, tenantID string) string {
	if tenantID == "" {
		return fmt.Sprintf("codeq:q:%s:dlq", strings.ToLower(string(cmd)))
	}
	return fmt.Sprintf("codeq:q:%s:%s:dlq", strings.ToLower(string(cmd)), tenantID)
}
func (r *taskRedisRepo) keyIdempotency(key string) string {
	return fmt.Sprintf("codeq:idempo:%s", key)
}

func (r *taskRedisRepo) now() time.Time { return time.Now().In(r.tz) }

// ===== Helpers =====

func marshal(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func unmarshalTask(jsonStr string) (*domain.Task, error) {
	var t domain.Task
	if err := json.Unmarshal([]byte(jsonStr), &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func (r *taskRedisRepo) allCommands() []domain.Command {
	return []domain.Command{domain.CmdGenerateMaster, domain.CmdGenerateCreative}
}

func normalizePriority(p int) int {
	if p < minPriority {
		return minPriority
	}
	if p > maxPriority {
		return maxPriority
	}
	return p
}

// Índice de retenção (score = epoch seg)
func (r *taskRedisRepo) registerTTL(ctx context.Context, id string, expireAt time.Time) error {
	z := &redis.Z{Score: float64(expireAt.UTC().Unix()), Member: id}
	return r.rdb.ZAdd(ctx, r.keyTTLIndex(), z).Err()
}

func (r *taskRedisRepo) bumpTTL(ctx context.Context, id string) {
	_ = r.registerTTL(ctx, id, r.now().Add(taskRetention))
}

// Remoção defensiva total de uma task
func (r *taskRedisRepo) removeTaskFully(ctx context.Context, id string) error {
	// tenta descobrir a fila pelo JSON antes de deletar
	var cmdOpt *domain.Command
	var prioOpt *int
	var tenantIDOpt *string
	if js, err := r.rdb.HGet(ctx, r.keyTasksHash(), id).Result(); err == nil && js != "" {
		if t, err2 := unmarshalTask(js); err2 == nil {
			cmd := t.Command
			cmdOpt = &cmd
			prio := normalizePriority(t.Priority)
			prioOpt = &prio
			tid := t.TenantID
			tenantIDOpt = &tid
		}
	}

	pipe := r.rdb.TxPipeline()
	pipe.HDel(ctx, r.keyTasksHash(), id)
	pipe.ZRem(ctx, r.keyTTLIndex(), id)
	pipe.Del(ctx, r.keyLease(id))
	if cmdOpt != nil {
		tenantID := ""
		if tenantIDOpt != nil {
			tenantID = *tenantIDOpt
		}
		if prioOpt != nil {
			pipe.LRem(ctx, r.keyQueuePending(*cmdOpt, *prioOpt, tenantID), 0, id)
		} else {
			for p := maxPriority; p >= minPriority; p-- {
				pipe.LRem(ctx, r.keyQueuePending(*cmdOpt, p, tenantID), 0, id)
			}
		}
		pipe.SRem(ctx, r.keyQueueInprog(*cmdOpt, tenantID), id)
		pipe.ZRem(ctx, r.keyQueueDelayed(*cmdOpt, tenantID), id)
		pipe.LRem(ctx, r.keyQueueDLQ(*cmdOpt, tenantID), 0, id)
	} else {
		// Try both with and without tenant to ensure cleanup when tenant is unknown
		// This handles cases where we don't know which tenant the task belongs to
		for _, c := range r.allCommands() {
			for p := maxPriority; p >= minPriority; p-- {
				// Try legacy queue (no tenant)
				pipe.LRem(ctx, r.keyQueuePending(c, p, ""), 0, id)
				// Note: Cannot enumerate all possible tenants, so orphaned tenant-specific
				// tasks would need manual cleanup or a separate background job
			}
			pipe.SRem(ctx, r.keyQueueInprog(c, ""), id)
			pipe.ZRem(ctx, r.keyQueueDelayed(c, ""), id)
			pipe.LRem(ctx, r.keyQueueDLQ(c, ""), 0, id)
		}
	}
	_, err := pipe.Exec(ctx)
	return err
}

// ===== Implementação =====

func (r *taskRedisRepo) Enqueue(ctx context.Context, cmd domain.Command, payload string, priority int, webhook string, maxAttempts int, idempotencyKey string, visibleAt time.Time, tenantID string) (*domain.Task, error) {
	if strings.TrimSpace(idempotencyKey) != "" {
		return r.enqueueIdempotent(ctx, cmd, payload, priority, webhook, maxAttempts, idempotencyKey, visibleAt, tenantID)
	}
	id := uuid.NewString()
	return r.enqueueWithID(ctx, id, cmd, payload, priority, webhook, maxAttempts, visibleAt, tenantID)
}

func (r *taskRedisRepo) enqueueIdempotent(ctx context.Context, cmd domain.Command, payload string, priority int, webhook string, maxAttempts int, idempotencyKey string, visibleAt time.Time, tenantID string) (*domain.Task, error) {
	idKey := r.keyIdempotency(idempotencyKey)
	if existingID, err := r.rdb.Get(ctx, idKey).Result(); err == nil && existingID != "" {
		if task, err := r.Get(ctx, existingID); err == nil {
			return task, nil
		}
		_ = r.rdb.Del(ctx, idKey).Err()
	}
	id := uuid.NewString()
	ok, err := r.rdb.SetNX(ctx, idKey, id, taskRetention).Result()
	if err != nil {
		return nil, fmt.Errorf("redis SETNX idempotency: %w", err)
	}
	if !ok {
		if existingID, err := r.rdb.Get(ctx, idKey).Result(); err == nil && existingID != "" {
			if task, err := r.Get(ctx, existingID); err == nil {
				return task, nil
			}
		}
		return nil, fmt.Errorf("idempotency conflict")
	}
	task, err := r.enqueueWithID(ctx, id, cmd, payload, priority, webhook, maxAttempts, visibleAt, tenantID)
	if err != nil {
		_ = r.rdb.Del(ctx, idKey).Err()
		return nil, err
	}
	return task, nil
}

func (r *taskRedisRepo) enqueueWithID(ctx context.Context, id string, cmd domain.Command, payload string, priority int, webhook string, maxAttempts int, visibleAt time.Time, tenantID string) (*domain.Task, error) {
	now := r.now()
	priority = normalizePriority(priority)

	task := domain.Task{
		ID:          id,
		Command:     cmd,
		Payload:     payload,
		Priority:    priority,
		Webhook:     webhook,
		Attempts:    0,
		MaxAttempts: maxAttempts,
		Status:      domain.StatusPending,
		TenantID:    tenantID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	js := marshal(task)

	// HASH único: field=id, value=JSON
	if err := r.rdb.HSet(ctx, r.keyTasksHash(), id, js).Err(); err != nil {
		return nil, fmt.Errorf("redis HSET task: %w", err)
	}

	// Índice de retenção (não faz limpeza agora)
	if err := r.registerTTL(ctx, id, now.Add(taskRetention)); err != nil {
		return nil, fmt.Errorf("redis ZADD ttl-index: %w", err)
	}

	// Enfileira na lista pending (imediato) ou no ZSET delayed (agendado).
	if !visibleAt.IsZero() && visibleAt.After(now) {
		visibleAtUnix := visibleAt.UTC().Unix()
		if err := r.rdb.ZAdd(ctx, r.keyQueueDelayed(cmd, tenantID), &redis.Z{Score: float64(visibleAtUnix), Member: id}).Err(); err != nil {
			return nil, fmt.Errorf("redis ZADD delayed: %w", err)
		}
	} else {
		if err := r.rdb.LPush(ctx, r.keyQueuePending(cmd, priority, tenantID), id).Err(); err != nil {
			return nil, fmt.Errorf("redis LPUSH queue: %w", err)
		}
	}

	metrics.TaskCreatedTotal.WithLabelValues(string(cmd)).Inc()
	return &task, nil
}

// claimMoveScript atomically pops one ID from the pending list and tracks it in the in-progress set.
//
// It also skips duplicate IDs that may exist in pending while already in in-progress (best-effort
// self-healing for rare duplication bugs).
//
// KEYS[1] = pending list key
// KEYS[2] = in-progress set key
// ARGV[1] = max inner iterations (int)
var claimMoveScript = redis.NewScript(`
local src = KEYS[1]
local dst = KEYS[2]
local maxIter = tonumber(ARGV[1]) or 1
for i=1,maxIter do
  local id = redis.call("RPOP", src)
  if not id then
    return false
  end
  if redis.call("SADD", dst, id) == 1 then
    return id
  end
end
return false
`)

func (r *taskRedisRepo) requeueExpired(ctx context.Context, cmd domain.Command, inspectLimit int, maxAttemptsDefault int, tenantID string) (int, error) {
	inprog := r.keyQueueInprog(cmd, tenantID)
	if inspectLimit <= 0 {
		inspectLimit = 200
	}
	ids, err := r.rdb.SRandMemberN(ctx, inprog, int64(inspectLimit)).Result()
	if err != nil && err != redis.Nil {
		return 0, fmt.Errorf("SRANDMEMBER inprog: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}

	pipe := r.rdb.Pipeline()
	ttlCmds := make([]*redis.DurationCmd, 0, len(ids))
	for _, id := range ids {
		ttlCmds = append(ttlCmds, pipe.TTL(ctx, r.keyLease(id)))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return 0, fmt.Errorf("pipeline TTL leases: %w", err)
	}

	moved := 0
	for i, id := range ids {
		ttl, err := ttlCmds[i].Result()
		if err != nil && err != redis.Nil {
			return moved, fmt.Errorf("TTL lease: %w", err)
		}
		if ttl <= 0 {
			metrics.LeaseExpiredTotal.WithLabelValues(string(cmd)).Inc()

			delaySeconds := 0
			priority := minPriority
			if js, err := r.rdb.HGet(ctx, r.keyTasksHash(), id).Result(); err == nil && js != "" {
				if t, err2 := unmarshalTask(js); err2 == nil {
					delaySeconds = backoff.Compute(r.backoffPolicy, r.backoffBaseSeconds, r.backoffMaxSeconds, t.Attempts, r.rng)
					priority = normalizePriority(t.Priority)
				}
			}
			// Lease expirou -> requeue via delayed (backoff) or DLQ
			_, _, err := r.Nack(ctx, id, "", delaySeconds, maxAttemptsDefault, "LEASE_EXPIRED")
			if err != nil {
				// fallback to pending if nack fails
				if err := r.rdb.SRem(ctx, inprog, id).Err(); err != nil {
					return moved, fmt.Errorf("SREM inprog: %w", err)
				}
				if err := r.rdb.LPush(ctx, r.keyQueuePending(cmd, priority, tenantID), id).Err(); err != nil {
					return moved, fmt.Errorf("LPUSH pending: %w", err)
				}
			}
			moved++
		}
	}
	return moved, nil
}

func (r *taskRedisRepo) MoveDueDelayed(ctx context.Context, cmd domain.Command, limit int) (int, error) {
	// For backwards compatibility, check both legacy and tenant-specific queues
	// This handles tasks created before tenant isolation was implemented
	delayed := r.keyQueueDelayed(cmd, "")
	if limit <= 0 {
		limit = 200
	}
	maxTS := strconv.FormatInt(r.now().UTC().Unix(), 10)
	zrange := &redis.ZRangeBy{Min: "-inf", Max: maxTS, Offset: 0, Count: int64(limit)}

	ids, err := r.rdb.ZRangeByScore(ctx, delayed, zrange).Result()
	if err != nil && err != redis.Nil {
		return 0, fmt.Errorf("ZRANGEBYSCORE delayed: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	pipe := r.rdb.TxPipeline()
	moveIDs := make([]string, 0, len(ids))
	priorities := make(map[string]int, len(ids))
	for _, id := range ids {
		js, err := r.rdb.HGet(ctx, r.keyTasksHash(), id).Result()
		if err == redis.Nil || js == "" {
			pipe.ZRem(ctx, delayed, id)
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("HGET task json: %w", err)
		}
		t, err := unmarshalTask(js)
		if err != nil {
			pipe.ZRem(ctx, delayed, id)
			continue
		}
		prio := normalizePriority(t.Priority)
		priorities[id] = prio
		moveIDs = append(moveIDs, id)
		pipe.ZRem(ctx, delayed, id)
		pipe.LPush(ctx, r.keyQueuePending(cmd, prio, t.TenantID), id)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}

	for _, id := range moveIDs {
		if js, err := r.rdb.HGet(ctx, r.keyTasksHash(), id).Result(); err == nil && js != "" {
			if t, err2 := unmarshalTask(js); err2 == nil {
				t.Status = domain.StatusPending
				t.WorkerID = ""
				t.LeaseUntil = ""
				t.UpdatedAt = r.now()
				_ = r.rdb.HSet(ctx, r.keyTasksHash(), id, marshal(t)).Err()
			}
		}
		r.bumpTTL(ctx, id)
	}

	return len(moveIDs), nil
}

// moveDueDelayedForTenant moves due tasks from delayed queue to pending for a specific tenant
func (r *taskRedisRepo) moveDueDelayedForTenant(ctx context.Context, cmd domain.Command, limit int, tenantID string) (int, error) {
	delayed := r.keyQueueDelayed(cmd, tenantID)
	if limit <= 0 {
		limit = 200
	}
	maxTS := strconv.FormatInt(r.now().UTC().Unix(), 10)
	zrange := &redis.ZRangeBy{Min: "-inf", Max: maxTS, Offset: 0, Count: int64(limit)}

	ids, err := r.rdb.ZRangeByScore(ctx, delayed, zrange).Result()
	if err != nil && err != redis.Nil {
		return 0, fmt.Errorf("ZRANGEBYSCORE delayed: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	pipe := r.rdb.TxPipeline()
	moveIDs := make([]string, 0, len(ids))
	for _, id := range ids {
		js, err := r.rdb.HGet(ctx, r.keyTasksHash(), id).Result()
		if err == redis.Nil || js == "" {
			pipe.ZRem(ctx, delayed, id)
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("HGET task json: %w", err)
		}
		t, err := unmarshalTask(js)
		if err != nil {
			pipe.ZRem(ctx, delayed, id)
			continue
		}
		prio := normalizePriority(t.Priority)
		moveIDs = append(moveIDs, id)
		pipe.ZRem(ctx, delayed, id)
		pipe.LPush(ctx, r.keyQueuePending(cmd, prio, tenantID), id)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}

	for _, id := range moveIDs {
		if js, err := r.rdb.HGet(ctx, r.keyTasksHash(), id).Result(); err == nil && js != "" {
			if t, err2 := unmarshalTask(js); err2 == nil {
				t.Status = domain.StatusPending
				t.WorkerID = ""
				t.LeaseUntil = ""
				t.UpdatedAt = r.now()
				_ = r.rdb.HSet(ctx, r.keyTasksHash(), id, marshal(t)).Err()
			}
		}
		r.bumpTTL(ctx, id)
	}

	return len(moveIDs), nil
}

func (r *taskRedisRepo) Claim(ctx context.Context, workerID string, commands []domain.Command, leaseSeconds int, inspectLimit int, maxAttemptsDefault int, tenantID string) (*domain.Task, bool, error) {
	// Move delayed to pending (due tasks) and requeue expired leases
	for _, cmd := range commands {
		// Check legacy queue for backward compatibility
		if _, err := r.MoveDueDelayed(ctx, cmd, inspectLimit); err != nil {
			return nil, false, err
		}
		// Check tenant-specific queue
		if tenantID != "" {
			if _, err := r.moveDueDelayedForTenant(ctx, cmd, inspectLimit, tenantID); err != nil {
				return nil, false, err
			}
		}
		if _, err := r.requeueExpired(ctx, cmd, inspectLimit, maxAttemptsDefault, tenantID); err != nil {
			return nil, false, err
		}
	}

	tryPop := func(cmd domain.Command, priority int) (*domain.Task, bool, error) {
		src := r.keyQueuePending(cmd, priority, tenantID)
		dst := r.keyQueueInprog(cmd, tenantID)

		for i := 0; i < inspectLimit; i++ {
			res, err := claimMoveScript.Run(ctx, r.rdb, []string{src, dst}, 1).Result()
			if err == redis.Nil {
				return nil, false, nil // fila vazia
			}
			if err != nil {
				return nil, false, fmt.Errorf("claim move script: %w", err)
			}
			id, ok := res.(string)
			if !ok || id == "" {
				return nil, false, nil // fila vazia / no unique id found
			}

			// Carrega JSON; pode ter sido limpo por admin endpoint
			js, err := r.rdb.HGet(ctx, r.keyTasksHash(), id).Result()
			if err == redis.Nil || js == "" {
				// remove da inprog e tenta novamente
				_ = r.rdb.SRem(ctx, dst, id).Err()
				continue
			}
			if err != nil {
				return nil, false, fmt.Errorf("HGET task json: %w", err)
			}
			t, err := unmarshalTask(js)
			if err != nil {
				_ = r.rdb.SRem(ctx, dst, id).Err()
				continue
			}

			leaseKey := r.keyLease(id)
			if err := r.rdb.SetEX(ctx, leaseKey, workerID, time.Duration(leaseSeconds)*time.Second).Err(); err != nil {
				// Falha ao setar lease → desfaz o move e segue
				_ = r.rdb.SRem(ctx, dst, id).Err()
				_ = r.rdb.LPush(ctx, src, id).Err()
				return nil, false, fmt.Errorf("SETEX lease: %w", err)
			}
			leaseUntil := r.now().Add(time.Duration(leaseSeconds) * time.Second).UTC().Format(time.RFC3339)

			// Atualiza JSON
			t.Status = domain.StatusInProgress
			t.WorkerID = workerID
			t.LeaseUntil = leaseUntil
			t.Attempts++
			t.UpdatedAt = r.now()
			if err := r.rdb.HSet(ctx, r.keyTasksHash(), t.ID, marshal(t)).Err(); err != nil {
				return nil, false, fmt.Errorf("HSET task inprogress: %w", err)
			}

			// Bump retenção lógica
			r.bumpTTL(ctx, t.ID)

			return t, true, nil
		}
		return nil, false, nil
	}

	for _, cmd := range commands {
		for p := maxPriority; p >= minPriority; p-- {
			if task, ok, err := tryPop(cmd, p); err != nil {
				return nil, false, err
			} else if ok {
				return task, true, nil
			}
		}
	}
	return nil, false, nil
}

func (r *taskRedisRepo) Heartbeat(ctx context.Context, taskID string, workerID string, extendSeconds int) error {
	js, err := r.rdb.HGet(ctx, r.keyTasksHash(), taskID).Result()
	if err == redis.Nil || js == "" {
		return fmt.Errorf("not-found")
	}
	if err != nil {
		return fmt.Errorf("HGET task: %w", err)
	}
	t, err := unmarshalTask(js)
	if err != nil {
		return fmt.Errorf("unmarshal task: %w", err)
	}
	if t.WorkerID != workerID {
		return fmt.Errorf("not-owner")
	}

	if err := r.rdb.Expire(ctx, r.keyLease(taskID), time.Duration(extendSeconds)*time.Second).Err(); err != nil {
		return fmt.Errorf("lease expire: %w", err)
	}
	t.LeaseUntil = r.now().Add(time.Duration(extendSeconds) * time.Second).UTC().Format(time.RFC3339)
	t.UpdatedAt = r.now()

	if err := r.rdb.HSet(ctx, r.keyTasksHash(), t.ID, marshal(t)).Err(); err != nil {
		return fmt.Errorf("HSET task: %w", err)
	}

	// Bump retenção lógica
	r.bumpTTL(ctx, t.ID)

	return nil
}

func (r *taskRedisRepo) Abandon(ctx context.Context, taskID string, workerID string) error {
	js, err := r.rdb.HGet(ctx, r.keyTasksHash(), taskID).Result()
	if err == redis.Nil || js == "" {
		return fmt.Errorf("not-found")
	}
	if err != nil {
		return fmt.Errorf("HGET task: %w", err)
	}
	t, err := unmarshalTask(js)
	if err != nil {
		return fmt.Errorf("unmarshal task: %w", err)
	}
	inprog := r.keyQueueInprog(t.Command, t.TenantID)
	pending := r.keyQueuePending(t.Command, normalizePriority(t.Priority), t.TenantID)

	if t.WorkerID != workerID {
		return fmt.Errorf("not-owner")
	}
	if err := r.rdb.SRem(ctx, inprog, taskID).Err(); err != nil {
		return fmt.Errorf("SREM inprog: %w", err)
	}
	if err := r.rdb.LPush(ctx, pending, taskID).Err(); err != nil {
		return fmt.Errorf("LPUSH pending: %w", err)
	}
	_ = r.rdb.Del(ctx, r.keyLease(taskID)).Err()

	t.Status = domain.StatusPending
	t.WorkerID = ""
	t.LeaseUntil = ""
	t.UpdatedAt = r.now()

	if err := r.rdb.HSet(ctx, r.keyTasksHash(), t.ID, marshal(t)).Err(); err != nil {
		return fmt.Errorf("HSET task: %w", err)
	}

	// Bump retenção lógica
	r.bumpTTL(ctx, t.ID)

	return nil
}

func (r *taskRedisRepo) Nack(ctx context.Context, taskID string, workerID string, delaySeconds int, maxAttemptsDefault int, reason string) (int, bool, error) {
	js, err := r.rdb.HGet(ctx, r.keyTasksHash(), taskID).Result()
	if err == redis.Nil || js == "" {
		return 0, false, fmt.Errorf("not-found")
	}
	if err != nil {
		return 0, false, fmt.Errorf("HGET task: %w", err)
	}
	t, err := unmarshalTask(js)
	if err != nil {
		return 0, false, fmt.Errorf("unmarshal task: %w", err)
	}
	if workerID != "" && t.WorkerID != workerID {
		return 0, false, fmt.Errorf("not-owner")
	}
	if t.Status != domain.StatusInProgress {
		return 0, false, fmt.Errorf("not-in-progress")
	}
	if t.MaxAttempts <= 0 {
		t.MaxAttempts = maxAttemptsDefault
	}
	if t.MaxAttempts <= 0 {
		t.MaxAttempts = 1
	}

	inprog := r.keyQueueInprog(t.Command, t.TenantID)
	delayed := r.keyQueueDelayed(t.Command, t.TenantID)
	dlq := r.keyQueueDLQ(t.Command, t.TenantID)

	t.Attempts++

	if t.Attempts >= t.MaxAttempts {
		if reason == "" {
			reason = "MAX_ATTEMPTS"
		}
		t.Status = domain.StatusFailed
		t.WorkerID = ""
		t.LeaseUntil = ""
		t.Error = reason
		t.UpdatedAt = r.now()

		pipe := r.rdb.TxPipeline()
		pipe.SRem(ctx, inprog, taskID)
		pipe.Del(ctx, r.keyLease(taskID))
		pipe.LPush(ctx, dlq, taskID)
		pipe.HSet(ctx, r.keyTasksHash(), t.ID, marshal(t))
		pipe.ZAdd(ctx, r.keyTTLIndex(), &redis.Z{Score: float64(r.now().Add(taskRetention).UTC().Unix()), Member: t.ID})
		if _, err := pipe.Exec(ctx); err != nil {
			return 0, false, err
		}

		metrics.TaskCompletedTotal.WithLabelValues(string(t.Command), string(t.Status)).Inc()
		if d := r.now().Sub(t.CreatedAt).Seconds(); d >= 0 {
			metrics.TaskProcessingLatencySeconds.WithLabelValues(string(t.Command), string(t.Status)).Observe(d)
		}
		return 0, true, nil
	}

	if delaySeconds < 0 {
		delaySeconds = 0
	}
	visibleAt := r.now().Add(time.Duration(delaySeconds) * time.Second).UTC().Unix()

	t.Status = domain.StatusPending
	t.WorkerID = ""
	t.LeaseUntil = ""
	t.Error = ""
	t.UpdatedAt = r.now()

	pipe := r.rdb.TxPipeline()
	pipe.SRem(ctx, inprog, taskID)
	pipe.Del(ctx, r.keyLease(taskID))
	pipe.ZAdd(ctx, delayed, &redis.Z{Score: float64(visibleAt), Member: taskID})
	pipe.HSet(ctx, r.keyTasksHash(), t.ID, marshal(t))
	pipe.ZAdd(ctx, r.keyTTLIndex(), &redis.Z{Score: float64(r.now().Add(taskRetention).UTC().Unix()), Member: t.ID})
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, false, err
	}
	return delaySeconds, false, nil
}

func (r *taskRedisRepo) Get(ctx context.Context, taskID string) (*domain.Task, error) {
	js, err := r.rdb.HGet(ctx, r.keyTasksHash(), taskID).Result()
	if err == redis.Nil || js == "" {
		return nil, fmt.Errorf("not-found")
	}
	if err != nil {
		return nil, fmt.Errorf("HGET task: %w", err)
	}
	t, err := unmarshalTask(js)
	if err != nil {
		return nil, fmt.Errorf("unmarshal task: %w", err)
	}
	return t, nil
}

func (r *taskRedisRepo) AdminQueues(ctx context.Context) (map[string]any, error) {
	out := map[string]any{}
	for _, cmd := range r.allCommands() {
		ki := r.keyQueueInprog(cmd, "")
		for p := maxPriority; p >= minPriority; p-- {
			kp := r.keyQueuePending(cmd, p, "")
			lp, err := r.rdb.LLen(ctx, kp).Result()
			if err != nil && err != redis.Nil {
				return nil, err
			}
			out[kp] = lp
		}
		li, err := r.rdb.SCard(ctx, ki).Result()
		if err != nil && err != redis.Nil {
			return nil, err
		}
		out[ki] = li

		kd := r.keyQueueDelayed(cmd, "")
		ld, err := r.rdb.ZCard(ctx, kd).Result()
		if err != nil && err != redis.Nil {
			return nil, err
		}
		out[kd] = ld

		kdlq := r.keyQueueDLQ(cmd, "")
		ldlq, err := r.rdb.LLen(ctx, kdlq).Result()
		if err != nil && err != redis.Nil {
			return nil, err
		}
		out[kdlq] = ldlq
	}
	return out, nil
}

func (r *taskRedisRepo) QueueStats(ctx context.Context, cmd domain.Command) (*domain.QueueStats, error) {
	var ready int64
	for p := maxPriority; p >= minPriority; p-- {
		n, err := r.rdb.LLen(ctx, r.keyQueuePending(cmd, p, "")).Result()
		if err != nil && err != redis.Nil {
			return nil, err
		}
		ready += n
	}
	inprog, err := r.rdb.SCard(ctx, r.keyQueueInprog(cmd, "")).Result()
	if err != nil && err != redis.Nil {
		return nil, err
	}
	delayed, err := r.rdb.ZCard(ctx, r.keyQueueDelayed(cmd, "")).Result()
	if err != nil && err != redis.Nil {
		return nil, err
	}
	dlq, err := r.rdb.LLen(ctx, r.keyQueueDLQ(cmd, "")).Result()
	if err != nil && err != redis.Nil {
		return nil, err
	}
	return &domain.QueueStats{
		Command:    cmd,
		Ready:      ready,
		Delayed:    delayed,
		InProgress: inprog,
		DLQ:        dlq,
	}, nil
}

func (r *taskRedisRepo) PendingLength(ctx context.Context, cmd domain.Command) (int64, error) {
	var total int64
	for p := maxPriority; p >= minPriority; p-- {
		n, err := r.rdb.LLen(ctx, r.keyQueuePending(cmd, p, "")).Result()
		if err != nil && err != redis.Nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// Limpeza administrativa (sem custo no Claim/Get)
func (r *taskRedisRepo) CleanupExpired(ctx context.Context, limit int, before time.Time) (int, error) {
	if limit <= 0 {
		limit = 1000
	}
	maxTS := strconv.FormatInt(before.UTC().Unix(), 10)
	zrange := &redis.ZRangeBy{Min: "-inf", Max: maxTS, Offset: 0, Count: int64(limit)}

	ids, err := r.rdb.ZRangeByScore(ctx, r.keyTTLIndex(), zrange).Result()
	if err != nil && err != redis.Nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	deleted := 0
	for _, id := range ids {
		if err := r.removeTaskFully(ctx, id); err == nil {
			deleted++
		}
	}
	return deleted, nil
}
