package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/osvaldoandrade/codeq/internal/cluster/clusterpb"
	"github.com/osvaldoandrade/codeq/internal/repository"
	pebblerepo "github.com/osvaldoandrade/codeq/internal/repository/pebble"
	"github.com/osvaldoandrade/codeq/internal/safeint"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// TaskRouter implements repository.TaskRepository on top of a local
// Pebble shard plus a gRPC client pool that can reach every peer. The
// service code above this layer (scheduler, controllers) keeps using the
// vanilla TaskRepository interface — sharding/routing is transparent.
//
// Routing decisions, in one place so they're easy to audit:
//   - Enqueue / EnqueueWithReady → router picks the ID, hash → owner.
//     Owner == self ⇒ local EnqueueWithID; else gRPC.Enqueue.
//   - Get / Heartbeat / Progress / Abandon / Nack → ID hash routes directly.
//   - Claim → scatter-gather LocalClaim across every node; first
//     non-empty response wins. Workers can pass a shard-affinity header
//     (handled at the controller layer) to force a single-node claim.
//   - MoveDueDelayed / CleanupExpired → local only (each node owns its
//     own delayed/dlq buckets for the ids it hashes to).
//   - PendingLength / QueueStats / AdminQueues → scatter-gather + sum.
//
// Errors: remote calls translate the structured response flags
// (NotFound / NotOwner / NotInProgress) back into the same "not-found"
// / "not-owner" / "not-in-progress" string sentinels the rest of the
// codebase already inspects.
type TaskRouter struct {
	local *pebblerepo.TaskRepository
	ring  *LocalRing
	pool  *ClientPool
	// cache is optional; when non-nil the router consults the peer
	// bloom before remote ID-routed RPCs and returns "not-found"
	// early when the bloom definitively excludes the key.
	cache *BloomCache
	// localBloom mirrors what Server.LocalBloom holds; the router calls
	// Add on it when it enqueues locally so peers eventually learn this
	// node holds the new id (via gossip).
	localBloom *Bloom
}

// NewTaskRouter constructs the router. local must be the same Pebble
// repository the local gRPC server delegates to so requests that hash
// back to self short-circuit the network path.
func NewTaskRouter(local *pebblerepo.TaskRepository, ring *LocalRing, pool *ClientPool) *TaskRouter {
	return &TaskRouter{local: local, ring: ring, pool: pool}
}

// WithBloomCache attaches a bloom cache used to short-circuit ID-routed
// RPCs whose peer bloom says "definitely not".
func (r *TaskRouter) WithBloomCache(c *BloomCache) *TaskRouter {
	r.cache = c
	return r
}

// WithLocalBloom installs the per-node bloom; the router publishes new
// task IDs into it whenever Enqueue lands on the local shard.
func (r *TaskRouter) WithLocalBloom(b *Bloom) *TaskRouter {
	r.localBloom = b
	return r
}

// peerHasLikely reports whether the cached bloom for ownerID (if any)
// considers key plausibly present. Returns true when there is no cache
// or no entry for the peer — the router then proceeds with the gRPC
// call, falling back to authoritative storage.
func (r *TaskRouter) peerHasLikely(ownerID, key string) bool {
	if r.cache == nil {
		return true
	}
	return r.cache.MaybeHas(ownerID, key)
}

// ---------------- Enqueue ----------------

// Enqueue creates a task on the node that owns its ID. See EnqueueWithReady.
func (r *TaskRouter) Enqueue(ctx context.Context, cmd domain.Command, payload string, priority int, webhook string, maxAttempts int, idempotencyKey, deduplicationKey string, visibleAt time.Time, tenantID string) (*domain.Task, error) {
	t, _, err := r.EnqueueWithReady(ctx, cmd, payload, priority, webhook, maxAttempts, idempotencyKey, deduplicationKey, visibleAt, tenantID)
	return t, err
}

// EnqueueWithReady is Enqueue that also reports whether the new task is
// immediately ready to claim.
func (r *TaskRouter) EnqueueWithReady(ctx context.Context, cmd domain.Command, payload string, priority int, webhook string, maxAttempts int, idempotencyKey, deduplicationKey string, visibleAt time.Time, tenantID string) (*domain.Task, bool, error) {
	// Pre-pick the ID so the hash → owner decision is deterministic.
	// Bias toward local ownership: the producer-side router would
	// otherwise pay a cross-node gRPC for (N-1)/N of all creates, which
	// dominated cluster overhead in Phase 4. GenerateLocalID picks a UUID
	// whose hash falls in this node's vnode arcs — same ID space, same
	// uniqueness guarantee, just biased toward "stay home".
	id, err := r.newTaskID(cmd, tenantID, deduplicationKey)
	if err != nil {
		return nil, false, err
	}
	if r.ring.IsLocal(id) {
		t, ready, err := r.local.EnqueueWithID(ctx, id, cmd, payload, priority, webhook, maxAttempts, idempotencyKey, deduplicationKey, visibleAt, tenantID)
		if err == nil && t != nil && r.localBloom != nil {
			r.localBloom.Add(t.ID)
		}
		return t, ready, err
	}
	owner := r.ring.Owner(id)
	c, err := r.pool.Client(owner)
	if err != nil {
		return nil, false, fmt.Errorf("dial owner %s: %w", owner.ID, err)
	}
	var visible int64
	if !visibleAt.IsZero() {
		visible = visibleAt.Unix()
	}
	resp, err := c.Enqueue(ctx, &clusterpb.EnqueueRequest{
		Id:             id,
		Command:        string(cmd),
		Payload:        []byte(payload),
		Priority:       safeint.Int32(priority),
		Webhook:        webhook,
		MaxAttempts:    safeint.Int32(maxAttempts),
		IdempotencyKey: idempotencyKey,
		VisibleAtUnix:  visible,
		TenantId:       tenantID,

		DeduplicationKey: deduplicationKey,
	})
	if err != nil {
		// The owner's ErrIdempotencyConflict crosses gRPC as a message;
		// restore the sentinel so the HTTP layer answers 409 with no task.
		if errMessageHas(err, domain.ErrIdempotencyConflict.Error()) {
			return nil, false, domain.ErrIdempotencyConflict
		}
		return nil, false, err
	}
	return protoToDomainTask(resp.Task), resp.Ready, nil
}

// newTaskID picks the ID of a new task. Without a deduplication key it is
// biased toward this node (see EnqueueWithReady). With one, every create of
// that key must meet the same lookup and lock, so the ID is owned by the
// node that owns the key, whichever node received the request.
func (r *TaskRouter) newTaskID(cmd domain.Command, tenantID, deduplicationKey string) (string, error) {
	if deduplicationKey == "" {
		return r.ring.GenerateLocalID(uuid.NewString), nil
	}
	owner := r.ring.Owner(string(pebblerepo.KeyDedupe(cmd, tenantID, deduplicationKey)))
	id, ok := r.ring.GenerateOwnedID(owner, uuid.NewString)
	if !ok {
		return "", fmt.Errorf("pick a task id owned by %s for a deduplication key", owner.ID)
	}
	return id, nil
}

// ---------------- ID-routed read/mutate ----------------

func (r *TaskRouter) Get(ctx context.Context, taskID string) (*domain.Task, error) {
	if r.ring.IsLocal(taskID) {
		return r.local.Get(ctx, taskID)
	}
	owner := r.ring.Owner(taskID)
	if !r.peerHasLikely(owner.ID, taskID) {
		// Peer bloom said "definitely not" — skip the network call.
		return nil, errors.New("not-found")
	}
	c, err := r.pool.Client(owner)
	if err != nil {
		return nil, err
	}
	resp, err := c.GetTask(ctx, &clusterpb.GetTaskRequest{Id: taskID})
	if err != nil {
		return nil, err
	}
	if resp.NotFound {
		return nil, errors.New("not-found")
	}
	return protoToDomainTask(resp.Task), nil
}

func (r *TaskRouter) Heartbeat(ctx context.Context, taskID string, workerID string, extendSeconds int) error {
	if r.ring.IsLocal(taskID) {
		return r.local.Heartbeat(ctx, taskID, workerID, extendSeconds)
	}
	owner := r.ring.Owner(taskID)
	if !r.peerHasLikely(owner.ID, taskID) {
		return errors.New("not-found")
	}
	c, err := r.pool.Client(owner)
	if err != nil {
		return err
	}
	resp, err := c.Heartbeat(ctx, &clusterpb.HeartbeatRequest{TaskId: taskID, WorkerId: workerID, ExtendSeconds: safeint.Int32(extendSeconds)})
	if err != nil {
		return err
	}
	switch {
	case resp.NotFound:
		return errors.New("not-found")
	case resp.NotOwner:
		return errors.New("not-owner")
	}
	return nil
}

// Progress stores the lease holder's progress value on the node that
// owns taskID, locally or through the owner's Progress RPC.
func (r *TaskRouter) Progress(ctx context.Context, taskID string, workerID string, progress json.RawMessage) error {
	if r.ring.IsLocal(taskID) {
		return r.local.Progress(ctx, taskID, workerID, progress)
	}
	owner := r.ring.Owner(taskID)
	if !r.peerHasLikely(owner.ID, taskID) {
		return errors.New("not-found")
	}
	c, err := r.pool.Client(owner)
	if err != nil {
		return err
	}
	resp, err := c.Progress(ctx, &clusterpb.ProgressRequest{TaskId: taskID, WorkerId: workerID, Progress: progress})
	if err != nil {
		return err
	}
	switch {
	case resp.NotFound:
		return errors.New("not-found")
	case resp.NotOwner:
		return errors.New("not-owner")
	case resp.NotInProgress:
		return errors.New("not-in-progress")
	}
	return nil
}

func (r *TaskRouter) Abandon(ctx context.Context, taskID string, workerID string) error {
	if r.ring.IsLocal(taskID) {
		return r.local.Abandon(ctx, taskID, workerID)
	}
	owner := r.ring.Owner(taskID)
	if !r.peerHasLikely(owner.ID, taskID) {
		return errors.New("not-found")
	}
	c, err := r.pool.Client(owner)
	if err != nil {
		return err
	}
	resp, err := c.Abandon(ctx, &clusterpb.AbandonRequest{TaskId: taskID, WorkerId: workerID})
	if err != nil {
		return err
	}
	switch {
	case resp.NotFound:
		return errors.New("not-found")
	case resp.NotOwner:
		return errors.New("not-owner")
	case resp.NotInProgress:
		return errors.New("not-in-progress")
	}
	return nil
}

func (r *TaskRouter) Nack(ctx context.Context, taskID string, workerID string, delaySeconds int, maxAttemptsDefault int, reason string) (int, bool, error) {
	if r.ring.IsLocal(taskID) {
		return r.local.Nack(ctx, taskID, workerID, delaySeconds, maxAttemptsDefault, reason)
	}
	owner := r.ring.Owner(taskID)
	if !r.peerHasLikely(owner.ID, taskID) {
		return 0, false, errors.New("not-found")
	}
	c, err := r.pool.Client(owner)
	if err != nil {
		return 0, false, err
	}
	resp, err := c.Nack(ctx, &clusterpb.NackRequest{
		TaskId:             taskID,
		WorkerId:           workerID,
		DelaySeconds:       safeint.Int32(delaySeconds),
		MaxAttemptsDefault: safeint.Int32(maxAttemptsDefault),
		Reason:             reason,
	})
	if err != nil {
		return 0, false, err
	}
	switch {
	case resp.NotFound:
		return 0, false, errors.New("not-found")
	case resp.NotOwner:
		return 0, false, errors.New("not-owner")
	case resp.NotInProgress:
		return 0, false, errors.New("not-in-progress")
	}
	return int(resp.AppliedDelaySeconds), resp.Dlq, nil
}

// ---------------- Claim (scatter-gather) ----------------

// Claim fans LocalClaim out to every node in parallel, returns the first
// non-empty response, and cancels in-flight ones. The local node is
// queried first via a direct repo call so a happy-path single-node hit
// avoids gRPC even when peers exist.
func (r *TaskRouter) Claim(ctx context.Context, workerID string, commands []domain.Command, leaseSeconds int, inspectLimit int, maxAttemptsDefault int, tenantID string) (*domain.Task, bool, error) {
	// Fast path: most workloads have a worker pool per node so the local
	// shard typically has work. Hit it before paying gRPC.
	if t, ok, err := r.local.Claim(ctx, workerID, commands, leaseSeconds, inspectLimit, maxAttemptsDefault, tenantID); err != nil || ok {
		return t, ok, err
	}

	peers := r.ring.Peers()
	if len(peers) == 0 {
		return nil, false, nil
	}
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	commandsStr := make([]string, 0, len(commands))
	for _, c := range commands {
		commandsStr = append(commandsStr, string(c))
	}

	results := r.pool.CallEach(subCtx, peers, func(ctx context.Context, c clusterpb.TaskNodeClient, _ Node) (any, error) {
		return c.LocalClaim(ctx, &clusterpb.LocalClaimRequest{
			WorkerId:           workerID,
			Commands:           commandsStr,
			LeaseSeconds:       safeint.Int32(leaseSeconds),
			InspectLimit:       safeint.Int32(inspectLimit),
			MaxAttemptsDefault: safeint.Int32(maxAttemptsDefault),
			TenantId:           tenantID,
		})
	})
	for _, res := range results {
		if res.Err != nil {
			continue
		}
		lc, ok := res.Value.(*clusterpb.LocalClaimResponse)
		if !ok || lc.Empty {
			continue
		}
		return protoToDomainTask(lc.Task), true, nil
	}
	return nil, false, nil
}

// ---------------- Node-local methods ----------------

// MoveDueDelayed runs only against the local shard. Each node sweeps the
// delayed→pending transition for the IDs it owns; this is invoked by
// the scheduler on a per-claim or periodic basis already.
func (r *TaskRouter) MoveDueDelayed(ctx context.Context, cmd domain.Command, limit int) (int, error) {
	return r.local.MoveDueDelayed(ctx, cmd, limit)
}

// CleanupExpired likewise stays node-local. The TTL reaper runs once per
// node; cluster-wide cleanup is the sum of every node's local work.
func (r *TaskRouter) CleanupExpired(ctx context.Context, limit int, before time.Time) (int, error) {
	return r.local.CleanupExpired(ctx, limit, before)
}

// ---------------- Aggregate methods (sum across nodes) ----------------

func (r *TaskRouter) PendingLength(ctx context.Context, cmd domain.Command) (int64, error) {
	localN, err := r.local.PendingLength(ctx, cmd)
	if err != nil {
		return 0, err
	}
	peers := r.ring.Peers()
	if len(peers) == 0 {
		return localN, nil
	}
	results := r.pool.CallEach(ctx, peers, func(ctx context.Context, c clusterpb.TaskNodeClient, _ Node) (any, error) {
		return c.PendingLength(ctx, &clusterpb.PendingLengthRequest{Command: string(cmd)})
	})
	total := localN
	for _, res := range results {
		if res.Err != nil {
			continue
		}
		pl, ok := res.Value.(*clusterpb.PendingLengthResponse)
		if !ok {
			continue
		}
		total += pl.Length
	}
	return total, nil
}

func (r *TaskRouter) QueueStats(ctx context.Context, cmd domain.Command, tenantID string) (*domain.QueueStats, error) {
	local, err := r.local.QueueStats(ctx, cmd, tenantID)
	if err != nil {
		return nil, err
	}
	peers := r.ring.Peers()
	if len(peers) == 0 {
		return local, nil
	}
	results := r.pool.CallEach(ctx, peers, func(ctx context.Context, c clusterpb.TaskNodeClient, _ Node) (any, error) {
		return c.QueueStats(ctx, &clusterpb.QueueStatsRequest{Command: string(cmd), TenantId: tenantID})
	})
	for _, res := range results {
		if res.Err != nil {
			continue
		}
		qs, ok := res.Value.(*clusterpb.QueueStatsResponse)
		if !ok {
			continue
		}
		local.Ready += qs.Ready
		local.Delayed += qs.Delayed
		local.InProgress += qs.InProgress
		local.DLQ += qs.Dlq
	}
	return local, nil
}

// ListTasks pages a queue state across the nodes in ring order: this node
// reads its own tasks, every other node answers over the cluster RPC. A node
// that cannot answer fails the page instead of being skipped, since a caller
// listing in-flight work must not mistake a missing node for an empty one.
func (r *TaskRouter) ListTasks(ctx context.Context, cmd domain.Command, tenantID string, state domain.QueueState, limit int, cursor string) (*domain.TaskPage, error) {
	nodes := r.ring.All()
	partitions := make([]string, len(nodes))
	for i, n := range nodes {
		partitions[i] = n.ID
	}
	return repository.ListAcrossPartitions(ctx, partitions, limit, cursor, func(ctx context.Context, nodeID string, limit int, cursor string) (*domain.TaskPage, error) {
		if nodeID == r.ring.SelfID() {
			return r.local.ListTasks(ctx, cmd, tenantID, state, limit, cursor)
		}
		return r.listRemote(ctx, nodeID, &clusterpb.ListTasksRequest{
			Command:  string(cmd),
			TenantId: tenantID,
			State:    string(state),
			Limit:    safeint.Int32(limit),
			Cursor:   cursor,
		})
	})
}

func (r *TaskRouter) listRemote(ctx context.Context, nodeID string, req *clusterpb.ListTasksRequest) (*domain.TaskPage, error) {
	node, ok := r.ring.Node(nodeID)
	if !ok {
		return nil, fmt.Errorf("list tasks: unknown node %s", nodeID)
	}
	c, err := r.pool.Client(node)
	if err != nil {
		return nil, fmt.Errorf("dial node %s: %w", nodeID, err)
	}
	resp, err := c.ListTasks(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("list tasks on node %s: %w", nodeID, err)
	}
	page := &domain.TaskPage{Tasks: make([]*domain.Task, len(resp.Tasks)), NextCursor: resp.NextCursor}
	for i, t := range resp.Tasks {
		page.Tasks[i] = protoToDomainTask(t)
	}
	return page, nil
}

func (r *TaskRouter) AdminQueues(ctx context.Context) (map[string]any, error) {
	local, err := r.local.AdminQueues(ctx)
	if err != nil {
		return nil, err
	}
	peers := r.ring.Peers()
	if len(peers) == 0 {
		return local, nil
	}
	results := r.pool.CallEach(ctx, peers, func(ctx context.Context, c clusterpb.TaskNodeClient, _ Node) (any, error) {
		return c.AdminQueues(ctx, &clusterpb.AdminQueuesRequest{})
	})
	for _, res := range results {
		if res.Err != nil {
			continue
		}
		aq, ok := res.Value.(*clusterpb.AdminQueuesResponse)
		if !ok {
			continue
		}
		for k, v := range aq.Counts {
			if cur, ok := local[k].(int64); ok {
				local[k] = cur + v
			} else {
				local[k] = v
			}
		}
	}
	return local, nil
}

// Compile-time assertion.
var _ repository.TaskRepository = (*TaskRouter)(nil)

// ---------------- timestamppb shim ----------------

// timeOrNil is here so the import is used even if no helper needs it
// directly; gen'd proto Get methods already handle nils.
var _ = func() *timestamppb.Timestamp { return nil }
var _ = strings.Repeat
