package cluster

import (
	"context"
	"errors"
	"fmt"

	"github.com/osvaldoandrade/codeq/internal/cluster/clusterpb"
	"github.com/osvaldoandrade/codeq/internal/safeint"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// Dead-letter administration (ADR 0009). The two task-ID operations route to
// the owner like Nack; the bulk requeue walks every node in ring order like
// ListTasks, because each node holds the dead-letter entries of the IDs it
// owns.

// ---------------- router ----------------

// RequeueDLQTask requeues a dead-lettered task on the node that owns
// taskID, locally or through the owner's RequeueTask RPC.
func (r *TaskRouter) RequeueDLQTask(ctx context.Context, taskID string) (*domain.Task, error) {
	if r.ring.IsLocal(taskID) {
		return r.local.RequeueDLQTask(ctx, taskID)
	}
	c, err := r.ownerClient(taskID)
	if err != nil {
		return nil, err
	}
	resp, err := c.RequeueTask(ctx, &clusterpb.RequeueTaskRequest{TaskId: taskID})
	if err != nil {
		return nil, err
	}
	switch {
	case resp.NotFound:
		return nil, errors.New("not-found")
	case resp.NotInDlq:
		return nil, domain.ErrTaskNotInDLQ
	}
	return protoToDomainTask(resp.Task), nil
}

// DeleteTask deletes a task on the node that owns taskID, locally or
// through the owner's DeleteTask RPC.
func (r *TaskRouter) DeleteTask(ctx context.Context, taskID string) error {
	if r.ring.IsLocal(taskID) {
		return r.local.DeleteTask(ctx, taskID)
	}
	c, err := r.ownerClient(taskID)
	if err != nil {
		return err
	}
	resp, err := c.DeleteTask(ctx, &clusterpb.DeleteTaskRequest{TaskId: taskID})
	if err != nil {
		return err
	}
	switch {
	case resp.NotFound:
		return errors.New("not-found")
	case resp.InProgress:
		return domain.ErrTaskInProgress
	}
	return nil
}

// ownerClient returns the client of the peer that owns taskID, or
// "not-found" when that peer's bloom excludes the ID.
func (r *TaskRouter) ownerClient(taskID string) (clusterpb.TaskNodeClient, error) {
	owner := r.ring.Owner(taskID)
	if !r.peerHasLikely(owner.ID, taskID) {
		return nil, errors.New("not-found")
	}
	return r.pool.Client(owner)
}

// RequeueDLQ walks the nodes in ring order, each requeueing up to the part
// of limit still unspent from its own dead-letter queue. Remaining is true
// when any node still holds entries. A node that cannot answer fails the
// call instead of being skipped, so a caller draining the queue never takes
// an unreachable node for an empty one; tasks already requeued stay
// requeued and a repeated call continues from there.
func (r *TaskRouter) RequeueDLQ(ctx context.Context, cmd domain.Command, tenantID string, limit int) (*domain.DLQRequeue, error) {
	out := &domain.DLQRequeue{}
	for _, node := range r.ring.All() {
		res, err := r.requeueDLQOn(ctx, node, cmd, tenantID, limit-out.Requeued)
		if err != nil {
			return nil, err
		}
		out.Requeued += res.Requeued
		out.Remaining = out.Remaining || res.Remaining
	}
	return out, nil
}

func (r *TaskRouter) requeueDLQOn(ctx context.Context, node Node, cmd domain.Command, tenantID string, limit int) (*domain.DLQRequeue, error) {
	if node.ID == r.ring.SelfID() {
		return r.local.RequeueDLQ(ctx, cmd, tenantID, limit)
	}
	c, err := r.pool.Client(node)
	if err != nil {
		return nil, fmt.Errorf("dial node %s: %w", node.ID, err)
	}
	resp, err := c.RequeueDLQ(ctx, &clusterpb.RequeueDLQRequest{
		Command:  string(cmd),
		TenantId: tenantID,
		Limit:    safeint.Int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("requeue dlq on node %s: %w", node.ID, err)
	}
	return &domain.DLQRequeue{Requeued: int(resp.Requeued), Remaining: resp.Remaining}, nil
}

// ---------------- server ----------------

// RequeueTask requeues a dead-lettered task held by this node.
func (s *Server) RequeueTask(ctx context.Context, req *clusterpb.RequeueTaskRequest) (*clusterpb.RequeueTaskResponse, error) {
	task, err := s.Tasks.RequeueDLQTask(ctx, req.TaskId)
	switch {
	case err == nil:
		return &clusterpb.RequeueTaskResponse{Task: domainTaskToProto(task)}, nil
	case isNotFound(err):
		return &clusterpb.RequeueTaskResponse{NotFound: true}, nil
	case errors.Is(err, domain.ErrTaskNotInDLQ):
		return &clusterpb.RequeueTaskResponse{NotInDlq: true}, nil
	default:
		return nil, err
	}
}

// DeleteTask deletes a task held by this node.
func (s *Server) DeleteTask(ctx context.Context, req *clusterpb.DeleteTaskRequest) (*clusterpb.DeleteTaskResponse, error) {
	err := s.Tasks.DeleteTask(ctx, req.TaskId)
	switch {
	case err == nil:
		return &clusterpb.DeleteTaskResponse{}, nil
	case isNotFound(err):
		return &clusterpb.DeleteTaskResponse{NotFound: true}, nil
	case errors.Is(err, domain.ErrTaskInProgress):
		return &clusterpb.DeleteTaskResponse{InProgress: true}, nil
	default:
		return nil, err
	}
}

// RequeueDLQ requeues tasks of a dead-letter queue held by this node.
func (s *Server) RequeueDLQ(ctx context.Context, req *clusterpb.RequeueDLQRequest) (*clusterpb.RequeueDLQResponse, error) {
	res, err := s.Tasks.RequeueDLQ(ctx, domain.Command(req.Command), req.TenantId, int(req.Limit))
	if err != nil {
		return nil, err
	}
	return &clusterpb.RequeueDLQResponse{Requeued: safeint.Int32(res.Requeued), Remaining: res.Remaining}, nil
}
