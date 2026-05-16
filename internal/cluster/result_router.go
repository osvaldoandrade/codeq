package cluster

import (
	"context"
	"errors"

	"github.com/bytedance/sonic"

	"github.com/osvaldoandrade/codeq/internal/cluster/clusterpb"
	"github.com/osvaldoandrade/codeq/internal/repository"
	pebblerepo "github.com/osvaldoandrade/codeq/internal/repository/pebble"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// ResultRouter wraps a local Pebble ResultRepository with cluster-aware
// routing. Same shape as TaskRouter but for the result/finalize path:
// every operation that knows a task ID hashes → owner, falls through to
// local when owner==self, gRPCs when not. Batch ops group by owner and
// fan out per group to keep one batch atomic per shard.
type ResultRouter struct {
	local *pebblerepo.ResultRepository
	ring  *LocalRing
	pool  *ClientPool
	cache *BloomCache
}

// NewResultRouter wires the router on top of a local ResultRepository.
func NewResultRouter(local *pebblerepo.ResultRepository, ring *LocalRing, pool *ClientPool) *ResultRouter {
	return &ResultRouter{local: local, ring: ring, pool: pool}
}

// WithBloomCache attaches a bloom cache used to short-circuit ID-routed
// RPCs whose peer bloom says "definitely not".
// protoToDomainResult wraps protoToResultRecord with a pointer return
// shape so it lines up with repository.ResultRepository.GetResult.
func protoToDomainResult(p *clusterpb.ResultRecord) *domain.ResultRecord {
	if p == nil {
		return nil
	}
	rec := protoToResultRecord(p)
	return &rec
}

func (r *ResultRouter) WithBloomCache(c *BloomCache) *ResultRouter {
	r.cache = c
	return r
}

func (r *ResultRouter) peerHasLikely(ownerID, key string) bool {
	if r.cache == nil {
		return true
	}
	return r.cache.MaybeHas(ownerID, key)
}

// ---------------- ID-routed methods ----------------

func (r *ResultRouter) GetTask(ctx context.Context, id string) (*domain.Task, error) {
	if r.ring.IsLocal(id) {
		return r.local.GetTask(ctx, id)
	}
	owner := r.ring.Owner(id)
	if !r.peerHasLikely(owner.ID, id) {
		return nil, errors.New("not-found")
	}
	c, err := r.pool.Client(owner)
	if err != nil {
		return nil, err
	}
	resp, err := c.GetTask(ctx, &clusterpb.GetTaskRequest{Id: id})
	if err != nil {
		return nil, err
	}
	if resp.NotFound {
		return nil, errors.New("not-found")
	}
	return protoToDomainTask(resp.Task), nil
}

func (r *ResultRouter) GetTaskAndResult(ctx context.Context, id string) (*domain.Task, *domain.ResultRecord, error) {
	// Two ID-routed calls in sequence is cheap (same owner for both).
	t, err := r.GetTask(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	rec, err := r.GetResult(ctx, id)
	if err != nil {
		return t, nil, err
	}
	return t, rec, nil
}

func (r *ResultRouter) SaveResult(ctx context.Context, rec domain.ResultRecord, cmd domain.Command, tenantID string) error {
	if r.ring.IsLocal(rec.TaskID) {
		return r.local.SaveResult(ctx, rec, cmd, tenantID)
	}
	owner := r.ring.Owner(rec.TaskID)
	if !r.peerHasLikely(owner.ID, rec.TaskID) {
		return errors.New("not-found")
	}
	c, err := r.pool.Client(owner)
	if err != nil {
		return err
	}
	body, _ := sonic.Marshal(rec.Result)
	pbRec := &clusterpb.ResultRecord{
		TaskId:     rec.TaskID,
		Status:     string(rec.Status),
		ResultJson: body,
		Error:      rec.Error,
	}
	resp, err := c.SaveResult(ctx, &clusterpb.SaveResultRequest{
		Record:   pbRec,
		Command:  string(cmd),
		TenantId: tenantID,
	})
	if err != nil {
		return err
	}
	if resp.NotFound {
		return errors.New("not-found")
	}
	return nil
}

func (r *ResultRouter) GetResult(ctx context.Context, id string) (*domain.ResultRecord, error) {
	if r.ring.IsLocal(id) {
		return r.local.GetResult(ctx, id)
	}
	owner := r.ring.Owner(id)
	if !r.peerHasLikely(owner.ID, id) {
		return nil, errors.New("not-found")
	}
	c, err := r.pool.Client(owner)
	if err != nil {
		return nil, err
	}
	resp, err := c.GetResult(ctx, &clusterpb.GetResultRequest{Id: id})
	if err != nil {
		return nil, err
	}
	if resp.NotFound {
		return nil, errors.New("not-found")
	}
	return protoToDomainResult(resp.Record), nil
}

func (r *ResultRouter) UpdateTaskOnComplete(ctx context.Context, id string, cmd domain.Command, tenantID string, status domain.TaskStatus, errorMsg string) error {
	if r.ring.IsLocal(id) {
		return r.local.UpdateTaskOnComplete(ctx, id, cmd, tenantID, status, errorMsg)
	}
	owner := r.ring.Owner(id)
	if !r.peerHasLikely(owner.ID, id) {
		return errors.New("not-found")
	}
	c, err := r.pool.Client(owner)
	if err != nil {
		return err
	}
	resp, err := c.UpdateOnComplete(ctx, &clusterpb.UpdateOnCompleteRequest{
		TaskId:   id,
		Command:  string(cmd),
		TenantId: tenantID,
		Status:   string(status),
		ErrorMsg: errorMsg,
	})
	if err != nil {
		return err
	}
	if resp.NotFound {
		return errors.New("not-found")
	}
	return nil
}

func (r *ResultRouter) RemoveFromInprogAndClearLease(ctx context.Context, id string, cmd domain.Command, tenantID string) error {
	// No dedicated gRPC for this — UpdateOnComplete handles the same
	// state transitions in practice. Hot-path callers use UpdateOnComplete.
	// For the local case we delegate; for remote we fall back to it.
	if r.ring.IsLocal(id) {
		return r.local.RemoveFromInprogAndClearLease(ctx, id, cmd, tenantID)
	}
	// Best-effort: ask the owner to clear via UpdateOnComplete with empty
	// status. The local Pebble impl tolerates this since it just removes
	// inprog + lease without status assertions in this code path.
	return r.UpdateTaskOnComplete(ctx, id, cmd, tenantID, "", "")
}

// ---------------- Helpers / batch ----------------

func (r *ResultRouter) DecodeBase64(s string) ([]byte, error) { return r.local.DecodeBase64(s) }

// GetTasksBatch groups IDs by owner, queries each owner in parallel, and
// merges the results. Each per-owner call is still a sequence of GetTask
// RPCs — a future optimization is a real batch gRPC.
func (r *ResultRouter) GetTasksBatch(ctx context.Context, ids []string) (map[string]*domain.Task, error) {
	groups := r.groupIDsByOwner(ids)
	out := make(map[string]*domain.Task, len(ids))
	for ownerID, ownedIDs := range groups {
		if ownerID == r.ring.SelfID() {
			m, err := r.local.GetTasksBatch(ctx, ownedIDs)
			if err != nil {
				return nil, err
			}
			for k, v := range m {
				out[k] = v
			}
			continue
		}
		owner, _ := r.ring.Node(ownerID)
		c, err := r.pool.Client(owner)
		if err != nil {
			return nil, err
		}
		for _, id := range ownedIDs {
			resp, err := c.GetTask(ctx, &clusterpb.GetTaskRequest{Id: id})
			if err != nil {
				return nil, err
			}
			if resp.NotFound {
				continue
			}
			out[id] = protoToDomainTask(resp.Task)
		}
	}
	return out, nil
}

// BatchUpdateTasksOnComplete groups updates by owner and dispatches each
// group as individual UpdateOnComplete RPCs (per shard). One batch op
// per local shard goes through the Pebble TxBatch path natively.
func (r *ResultRouter) BatchUpdateTasksOnComplete(ctx context.Context, updates []domain.TaskCompleteUpdate) error {
	ids := make([]string, len(updates))
	for i, u := range updates {
		ids[i] = u.ID
	}
	tasks, err := r.GetTasksBatch(ctx, ids)
	if err != nil {
		return err
	}
	type byOwner struct {
		ownerID string
		updates []domain.TaskCompleteUpdate
	}
	grouped := make(map[string]*byOwner)
	for _, u := range updates {
		t, ok := tasks[u.ID]
		if !ok {
			return errors.New("not-found: " + u.ID)
		}
		ownerID := r.ring.Owner(u.ID).ID
		g, exists := grouped[ownerID]
		if !exists {
			g = &byOwner{ownerID: ownerID}
			grouped[ownerID] = g
		}
		g.updates = append(g.updates, u)
		_ = t
	}
	for ownerID, g := range grouped {
		if ownerID == r.ring.SelfID() {
			if err := r.local.BatchUpdateTasksOnComplete(ctx, g.updates); err != nil {
				return err
			}
			continue
		}
		owner, _ := r.ring.Node(ownerID)
		c, err := r.pool.Client(owner)
		if err != nil {
			return err
		}
		for _, u := range g.updates {
			t := tasks[u.ID]
			if _, err := c.UpdateOnComplete(ctx, &clusterpb.UpdateOnCompleteRequest{
				TaskId:   u.ID,
				Command:  string(t.Command),
				TenantId: t.TenantID,
				Status:   string(u.Status),
				ErrorMsg: u.ErrorMsg,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// BatchRemoveFromInprogAndClearLease groups deletes by owner; each owner
// runs its own local batched cleanup.
func (r *ResultRouter) BatchRemoveFromInprogAndClearLease(ctx context.Context, deletes []domain.TaskDeleteInfo) error {
	grouped := make(map[string][]domain.TaskDeleteInfo)
	for _, d := range deletes {
		ownerID := r.ring.Owner(d.ID).ID
		grouped[ownerID] = append(grouped[ownerID], d)
	}
	for ownerID, group := range grouped {
		if ownerID == r.ring.SelfID() {
			if err := r.local.BatchRemoveFromInprogAndClearLease(ctx, group); err != nil {
				return err
			}
			continue
		}
		owner, _ := r.ring.Node(ownerID)
		c, err := r.pool.Client(owner)
		if err != nil {
			return err
		}
		for _, d := range group {
			if _, err := c.UpdateOnComplete(ctx, &clusterpb.UpdateOnCompleteRequest{
				TaskId:   d.ID,
				Command:  string(d.Command),
				TenantId: d.TenantID,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *ResultRouter) groupIDsByOwner(ids []string) map[string][]string {
	out := make(map[string][]string)
	for _, id := range ids {
		ownerID := r.ring.Owner(id).ID
		out[ownerID] = append(out[ownerID], id)
	}
	return out
}

// Compile-time assertion.
var _ repository.ResultRepository = (*ResultRouter)(nil)
