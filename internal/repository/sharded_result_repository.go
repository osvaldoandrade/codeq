package repository

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/osvaldoandrade/codeq/internal/shard"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// shardedResultRepository distributes result-side operations across multiple
// Redis backends. Tasks live on exactly one shard (placed by ShardedTaskRepository
// at create time), so result lookups and writes need to land on the same shard.
//
// Methods that already know (cmd, tenantID) — UpdateTaskOnComplete,
// RemoveFromInprogAndClearLease, BatchRemoveFromInprogAndClearLease — resolve
// the shard directly via the supplier. Methods that only have a task ID fan
// out to every shard in parallel, returning the first success (mirrors the
// pattern used by ShardedTaskRepository.Heartbeat / Get / Nack / Abandon).
type shardedResultRepository struct {
	repos         map[string]ResultRepository
	shardSupplier domain.ShardSupplier
	defaultShard  string
}

// NewShardedResultRepository wraps one inner ResultRepository per Redis backend.
func NewShardedResultRepository(clientMap *shard.ClientMap, tz *time.Location, supplier domain.ShardSupplier) ResultRepository {
	if supplier == nil {
		supplier = shard.NewDefaultShardSupplier()
	}
	shardIDs := clientMap.ShardIDs()
	repos := make(map[string]ResultRepository, len(shardIDs))
	for _, sid := range shardIDs {
		repos[sid] = NewResultRepository(clientMap.Client(sid), tz, supplier)
	}
	return &shardedResultRepository{
		repos:         repos,
		shardSupplier: supplier,
		defaultShard:  clientMap.DefaultShard(),
	}
}

func (s *shardedResultRepository) repoForShard(shardID string) ResultRepository {
	if r, ok := s.repos[shardID]; ok {
		return r
	}
	return s.repos[s.defaultShard]
}

func (s *shardedResultRepository) resolveShard(ctx context.Context, cmd domain.Command, tenantID string) string {
	sid, err := s.shardSupplier.CurrentShard(ctx, string(cmd), tenantID)
	if err != nil || sid == "" {
		return s.defaultShard
	}
	return sid
}

// fanOut runs op against every inner repo in parallel and returns the first
// non-not-found result. Mirrors the pattern in ShardedTaskRepository.Heartbeat.
func (s *shardedResultRepository) fanOutErr(ctx context.Context, op func(ResultRepository) error) error {
	resultChan := make(chan error, len(s.repos))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	for _, repo := range s.repos {
		repo := repo
		go func() {
			resultChan <- op(repo)
		}()
	}

	var lastNotFound error
	for i := 0; i < len(s.repos); i++ {
		err := <-resultChan
		if err == nil {
			return nil
		}
		if !isNotFoundErr(err) {
			return err
		}
		lastNotFound = err
	}
	return lastNotFound
}

func (s *shardedResultRepository) GetTask(ctx context.Context, id string) (*domain.Task, error) {
	type res struct {
		t   *domain.Task
		err error
	}
	resultChan := make(chan res, len(s.repos))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, repo := range s.repos {
		repo := repo
		go func() {
			t, err := repo.GetTask(ctx, id)
			resultChan <- res{t, err}
		}()
	}
	var lastNotFound error
	for i := 0; i < len(s.repos); i++ {
		r := <-resultChan
		if r.err == nil {
			return r.t, nil
		}
		if !isNotFoundErr(r.err) {
			return nil, r.err
		}
		lastNotFound = r.err
	}
	return nil, lastNotFound
}

func (s *shardedResultRepository) GetTaskAndResult(ctx context.Context, id string) (*domain.Task, *domain.ResultRecord, error) {
	type res struct {
		t   *domain.Task
		r   *domain.ResultRecord
		err error
	}
	resultChan := make(chan res, len(s.repos))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, repo := range s.repos {
		repo := repo
		go func() {
			t, r, err := repo.GetTaskAndResult(ctx, id)
			resultChan <- res{t, r, err}
		}()
	}
	// Order matters: a "result not-found" still returns a task; a "task not-found"
	// returns nil/nil. We prefer the response that yielded the task.
	var lastNotFound res
	for i := 0; i < len(s.repos); i++ {
		r := <-resultChan
		if r.err == nil {
			return r.t, r.r, nil
		}
		if r.t != nil && r.r == nil {
			// Task was found on this shard but result is missing — that's the
			// authoritative answer. Other shards won't have the task at all.
			return r.t, nil, r.err
		}
		lastNotFound = r
	}
	return lastNotFound.t, lastNotFound.r, lastNotFound.err
}

func (s *shardedResultRepository) SaveResult(ctx context.Context, rec domain.ResultRecord, cmd domain.Command, tenantID string) error {
	// Direct routing when caller knows (cmd, tenantID): zero fan-out goroutines
	// and zero wasted HGets. Falls back to fan-out when the hint is missing.
	if cmd != "" {
		sid := s.resolveShard(ctx, cmd, tenantID)
		return s.repoForShard(sid).SaveResult(ctx, rec, cmd, tenantID)
	}
	return s.fanOutErr(ctx, func(r ResultRepository) error {
		return r.SaveResult(ctx, rec, cmd, tenantID)
	})
}

func (s *shardedResultRepository) GetResult(ctx context.Context, id string) (*domain.ResultRecord, error) {
	type res struct {
		r   *domain.ResultRecord
		err error
	}
	resultChan := make(chan res, len(s.repos))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, repo := range s.repos {
		repo := repo
		go func() {
			r, err := repo.GetResult(ctx, id)
			resultChan <- res{r, err}
		}()
	}
	var lastNotFound error
	for i := 0; i < len(s.repos); i++ {
		r := <-resultChan
		if r.err == nil {
			return r.r, nil
		}
		if !isNotFoundErr(r.err) {
			return nil, r.err
		}
		lastNotFound = r.err
	}
	return nil, lastNotFound
}

func (s *shardedResultRepository) UpdateTaskOnComplete(ctx context.Context, id string, cmd domain.Command, tenantID string, status domain.TaskStatus, errorMsg string) error {
	sid := s.resolveShard(ctx, cmd, tenantID)
	return s.repoForShard(sid).UpdateTaskOnComplete(ctx, id, cmd, tenantID, status, errorMsg)
}

func (s *shardedResultRepository) RemoveFromInprogAndClearLease(ctx context.Context, id string, cmd domain.Command, tenantID string) error {
	sid := s.resolveShard(ctx, cmd, tenantID)
	return s.repoForShard(sid).RemoveFromInprogAndClearLease(ctx, id, cmd, tenantID)
}

// DecodeBase64 is pure-CPU helper; any inner repo can answer it.
func (s *shardedResultRepository) DecodeBase64(str string) ([]byte, error) {
	for _, r := range s.repos {
		return r.DecodeBase64(str)
	}
	// Empty repo map: build a zero-value resultRedisRepo just for the helper.
	return (&resultRedisRepo{}).DecodeBase64(str)
}

// GetTasksBatch fans out the same id slice to every shard concurrently and
// merges the per-shard maps. Each inner GetTasksBatch silently skips ids it
// doesn't have, so missing ids in one shard are simply not in its result map.
func (s *shardedResultRepository) GetTasksBatch(ctx context.Context, ids []string) (map[string]*domain.Task, error) {
	if len(ids) == 0 {
		return map[string]*domain.Task{}, nil
	}
	type res struct {
		m   map[string]*domain.Task
		err error
	}
	resultChan := make(chan res, len(s.repos))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, repo := range s.repos {
		repo := repo
		go func() {
			m, err := repo.GetTasksBatch(ctx, ids)
			resultChan <- res{m, err}
		}()
	}
	merged := make(map[string]*domain.Task, len(ids))
	var firstErr error
	for i := 0; i < len(s.repos); i++ {
		r := <-resultChan
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		for k, v := range r.m {
			merged[k] = v
		}
	}
	if len(merged) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return merged, nil
}

// BatchUpdateTasksOnComplete groups updates by the shard that holds each task
// (probed via GetTasksBatch) and runs each shard's batch in parallel. Avoids
// fan-out per record while keeping the inner pipeline atomic per shard.
func (s *shardedResultRepository) BatchUpdateTasksOnComplete(ctx context.Context, updates []domain.TaskCompleteUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	ids := make([]string, len(updates))
	for i, u := range updates {
		ids[i] = u.ID
	}
	tasks, err := s.GetTasksBatch(ctx, ids)
	if err != nil {
		return fmt.Errorf("batch locate tasks: %w", err)
	}

	grouped := make(map[string][]domain.TaskCompleteUpdate)
	for _, u := range updates {
		t, ok := tasks[u.ID]
		if !ok {
			return fmt.Errorf("task %s not found", u.ID)
		}
		sid := s.resolveShard(ctx, t.Command, t.TenantID)
		grouped[sid] = append(grouped[sid], u)
	}

	if len(grouped) == 0 {
		return nil
	}
	if len(grouped) == 1 {
		for sid, group := range grouped {
			return s.repoForShard(sid).BatchUpdateTasksOnComplete(ctx, group)
		}
	}

	var wg sync.WaitGroup
	errChan := make(chan error, len(grouped))
	for sid, group := range grouped {
		wg.Add(1)
		sid, group := sid, group
		go func() {
			defer wg.Done()
			if err := s.repoForShard(sid).BatchUpdateTasksOnComplete(ctx, group); err != nil {
				errChan <- err
			}
		}()
	}
	wg.Wait()
	close(errChan)
	for err := range errChan {
		if err != nil {
			return err
		}
	}
	return nil
}

// BatchRemoveFromInprogAndClearLease groups deletions by their resolved shard
// (TaskDeleteInfo carries cmd+tenant) and runs each shard's pipeline in parallel.
func (s *shardedResultRepository) BatchRemoveFromInprogAndClearLease(ctx context.Context, deletes []domain.TaskDeleteInfo) error {
	if len(deletes) == 0 {
		return nil
	}
	grouped := make(map[string][]domain.TaskDeleteInfo)
	for _, d := range deletes {
		sid := s.resolveShard(ctx, d.Command, d.TenantID)
		grouped[sid] = append(grouped[sid], d)
	}

	if len(grouped) == 1 {
		for sid, group := range grouped {
			return s.repoForShard(sid).BatchRemoveFromInprogAndClearLease(ctx, group)
		}
	}

	var wg sync.WaitGroup
	errChan := make(chan error, len(grouped))
	for sid, group := range grouped {
		wg.Add(1)
		sid, group := sid, group
		go func() {
			defer wg.Done()
			if err := s.repoForShard(sid).BatchRemoveFromInprogAndClearLease(ctx, group); err != nil {
				errChan <- err
			}
		}()
	}
	wg.Wait()
	close(errChan)
	for err := range errChan {
		if err != nil {
			return err
		}
	}
	return nil
}
