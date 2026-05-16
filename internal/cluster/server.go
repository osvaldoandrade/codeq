package cluster

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/osvaldoandrade/codeq/internal/cluster/clusterpb"
	"github.com/osvaldoandrade/codeq/internal/repository"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// decodeResultJSON unmarshals the wire-form result bytes into the typed
// map domain expects. Empty input yields a nil map; malformed input is
// silently ignored (caller still gets the empty map and the rest of the
// record).
func decodeResultJSON(raw []byte, out *map[string]any) error {
	if len(raw) == 0 {
		return nil
	}
	m := make(map[string]any)
	if err := sonic.Unmarshal(raw, &m); err != nil {
		return err
	}
	*out = m
	return nil
}

// Server implements the TaskNode gRPC service by delegating to the local
// repositories. Every RPC operates against the node's local Pebble shard
// only — fan-out and routing live in the caller (Client + Router).
//
// Errors: we encode "not found / not owner / not in progress" in the
// response struct flags rather than gRPC error codes so the caller can
// distinguish them cleanly without unwrapping status messages. Real I/O
// errors surface as gRPC errors normally.
type Server struct {
	clusterpb.UnimplementedTaskNodeServer

	NodeID  string
	Tasks   repository.TaskRepository
	Results repository.ResultRepository
	// LocalBloom is the per-node bloom of locally-stored task IDs.
	// Enqueue (the one path that introduces new IDs to this node) adds
	// the id here after a successful local write. BloomSnapshot returns
	// its serialised state for peer gossip.
	LocalBloom *Bloom
}

// ---------------- ID-routed methods ----------------

func (s *Server) Enqueue(ctx context.Context, req *clusterpb.EnqueueRequest) (*clusterpb.EnqueueResponse, error) {
	// Type-assert so we can use the cluster-aware EnqueueWithID — the
	// router pre-picked the task ID at the hash boundary, we MUST honour it.
	local, ok := s.Tasks.(interface {
		EnqueueWithID(ctx context.Context, id string, cmd domain.Command, payload string, priority int, webhook string, maxAttempts int, idempotencyKey string, visibleAt time.Time, tenantID string) (*domain.Task, bool, error)
	})
	if !ok {
		return nil, errors.New("local TaskRepository does not support EnqueueWithID; cannot serve cluster Enqueue")
	}
	var visibleAt time.Time
	if req.VisibleAtUnix > 0 {
		visibleAt = time.Unix(req.VisibleAtUnix, 0)
	}
	task, ready, err := local.EnqueueWithID(ctx,
		req.Id,
		domain.Command(req.Command),
		string(req.Payload),
		int(req.Priority),
		req.Webhook,
		int(req.MaxAttempts),
		req.IdempotencyKey,
		visibleAt,
		req.TenantId,
	)
	if err != nil {
		return nil, err
	}
	if s.LocalBloom != nil && task != nil {
		s.LocalBloom.Add(task.ID)
	}
	return &clusterpb.EnqueueResponse{Task: domainTaskToProto(task), Ready: ready}, nil
}

func (s *Server) GetTask(ctx context.Context, req *clusterpb.GetTaskRequest) (*clusterpb.GetTaskResponse, error) {
	t, err := s.Tasks.Get(ctx, req.Id)
	if err != nil {
		if isNotFound(err) {
			return &clusterpb.GetTaskResponse{NotFound: true}, nil
		}
		return nil, err
	}
	return &clusterpb.GetTaskResponse{Task: domainTaskToProto(t)}, nil
}

func (s *Server) Heartbeat(ctx context.Context, req *clusterpb.HeartbeatRequest) (*clusterpb.HeartbeatResponse, error) {
	if err := s.Tasks.Heartbeat(ctx, req.TaskId, req.WorkerId, int(req.ExtendSeconds)); err != nil {
		switch {
		case isNotFound(err):
			return &clusterpb.HeartbeatResponse{NotFound: true}, nil
		case isNotOwner(err):
			return &clusterpb.HeartbeatResponse{NotOwner: true}, nil
		default:
			return nil, err
		}
	}
	return &clusterpb.HeartbeatResponse{}, nil
}

func (s *Server) Abandon(ctx context.Context, req *clusterpb.AbandonRequest) (*clusterpb.AbandonResponse, error) {
	if err := s.Tasks.Abandon(ctx, req.TaskId, req.WorkerId); err != nil {
		switch {
		case isNotFound(err):
			return &clusterpb.AbandonResponse{NotFound: true}, nil
		case isNotOwner(err):
			return &clusterpb.AbandonResponse{NotOwner: true}, nil
		case isNotInProgress(err):
			return &clusterpb.AbandonResponse{NotInProgress: true}, nil
		default:
			return nil, err
		}
	}
	return &clusterpb.AbandonResponse{}, nil
}

func (s *Server) Nack(ctx context.Context, req *clusterpb.NackRequest) (*clusterpb.NackResponse, error) {
	delay, dlq, err := s.Tasks.Nack(ctx, req.TaskId, req.WorkerId, int(req.DelaySeconds), int(req.MaxAttemptsDefault), req.Reason)
	if err != nil {
		switch {
		case isNotFound(err):
			return &clusterpb.NackResponse{NotFound: true}, nil
		case isNotOwner(err):
			return &clusterpb.NackResponse{NotOwner: true}, nil
		case isNotInProgress(err):
			return &clusterpb.NackResponse{NotInProgress: true}, nil
		default:
			return nil, err
		}
	}
	return &clusterpb.NackResponse{AppliedDelaySeconds: int32(delay), Dlq: dlq}, nil
}

func (s *Server) SaveResult(ctx context.Context, req *clusterpb.SaveResultRequest) (*clusterpb.SaveResultResponse, error) {
	rec := protoToResultRecord(req.Record)
	if err := s.Results.SaveResult(ctx, rec, domain.Command(req.Command), req.TenantId); err != nil {
		if isNotFound(err) {
			return &clusterpb.SaveResultResponse{NotFound: true}, nil
		}
		return nil, err
	}
	return &clusterpb.SaveResultResponse{}, nil
}

func (s *Server) GetResult(ctx context.Context, req *clusterpb.GetResultRequest) (*clusterpb.GetResultResponse, error) {
	rec, err := s.Results.GetResult(ctx, req.Id)
	if err != nil {
		if isNotFound(err) {
			return &clusterpb.GetResultResponse{NotFound: true}, nil
		}
		return nil, err
	}
	return &clusterpb.GetResultResponse{Record: domainResultToProto(rec)}, nil
}

func (s *Server) UpdateOnComplete(ctx context.Context, req *clusterpb.UpdateOnCompleteRequest) (*clusterpb.UpdateOnCompleteResponse, error) {
	err := s.Results.UpdateTaskOnComplete(ctx, req.TaskId, domain.Command(req.Command), req.TenantId, domain.TaskStatus(req.Status), req.ErrorMsg)
	if err != nil {
		if isNotFound(err) {
			return &clusterpb.UpdateOnCompleteResponse{NotFound: true}, nil
		}
		return nil, err
	}
	return &clusterpb.UpdateOnCompleteResponse{}, nil
}

// ---------------- Scatter-gather methods ----------------

// LocalClaim asks this specific node to pop from its local shard. The
// caller (Router) is expected to fan this out to every peer in parallel
// and accept the first non-empty response.
func (s *Server) LocalClaim(ctx context.Context, req *clusterpb.LocalClaimRequest) (*clusterpb.LocalClaimResponse, error) {
	cmds := make([]domain.Command, 0, len(req.Commands))
	for _, c := range req.Commands {
		cmds = append(cmds, domain.Command(c))
	}
	inspect := int(req.InspectLimit)
	if inspect <= 0 {
		inspect = 50
	}
	t, ok, err := s.Tasks.Claim(ctx, req.WorkerId, cmds, int(req.LeaseSeconds), inspect, int(req.MaxAttemptsDefault), req.TenantId)
	if err != nil {
		return nil, err
	}
	if !ok {
		return &clusterpb.LocalClaimResponse{Empty: true}, nil
	}
	return &clusterpb.LocalClaimResponse{Task: domainTaskToProto(t)}, nil
}

func (s *Server) PendingLength(ctx context.Context, req *clusterpb.PendingLengthRequest) (*clusterpb.PendingLengthResponse, error) {
	n, err := s.Tasks.PendingLength(ctx, domain.Command(req.Command))
	if err != nil {
		return nil, err
	}
	return &clusterpb.PendingLengthResponse{Length: n}, nil
}

func (s *Server) QueueStats(ctx context.Context, req *clusterpb.QueueStatsRequest) (*clusterpb.QueueStatsResponse, error) {
	q, err := s.Tasks.QueueStats(ctx, domain.Command(req.Command), req.TenantId)
	if err != nil {
		return nil, err
	}
	return &clusterpb.QueueStatsResponse{
		Ready:      q.Ready,
		Delayed:    q.Delayed,
		InProgress: q.InProgress,
		Dlq:        q.DLQ,
	}, nil
}

func (s *Server) AdminQueues(ctx context.Context, req *clusterpb.AdminQueuesRequest) (*clusterpb.AdminQueuesResponse, error) {
	m, err := s.Tasks.AdminQueues(ctx)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int64, len(m))
	for k, v := range m {
		switch x := v.(type) {
		case int64:
			counts[k] = x
		case int:
			counts[k] = int64(x)
		}
	}
	return &clusterpb.AdminQueuesResponse{Counts: counts}, nil
}

// ---------------- Bloom gossip ----------------

func (s *Server) BloomSnapshot(ctx context.Context, _ *clusterpb.BloomSnapshotRequest) (*clusterpb.BloomSnapshotResponse, error) {
	if s.LocalBloom == nil {
		return &clusterpb.BloomSnapshotResponse{NodeId: s.NodeID}, nil
	}
	mBits, k, items, seq := s.LocalBloom.Snapshot()
	return &clusterpb.BloomSnapshotResponse{
		MBits:     mBits,
		NumHashes: k,
		NumItems:  items,
		Sequence:  seq,
		NodeId:    s.NodeID,
	}, nil
}

// ---------------- helpers ----------------

func isNotFound(err error) bool       { return errMessageHas(err, "not-found") }
func isNotOwner(err error) bool       { return errMessageHas(err, "not-owner") }
func isNotInProgress(err error) bool  { return errMessageHas(err, "not-in-progress") }

func errMessageHas(err error, substr string) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), substr) || errors.Is(err, errSentinel(substr))
}

// errSentinel turns a substring back into an error so errors.Is comparison
// works in the rare case a caller wraps the message.
type errSentinel string

func (e errSentinel) Error() string { return string(e) }

func domainTaskToProto(t *domain.Task) *clusterpb.Task {
	if t == nil {
		return nil
	}
	return &clusterpb.Task{
		Id:                t.ID,
		Command:           string(t.Command),
		Payload:           []byte(t.Payload),
		Priority:          int32(t.Priority),
		Webhook:           t.Webhook,
		MaxAttempts:       int32(t.MaxAttempts),
		Status:            string(t.Status),
		LastKnownLocation: string(t.LastKnownLocation),
		WorkerId:          t.WorkerID,
		LeaseUntil:        t.LeaseUntil,
		Attempts:          int32(t.Attempts),
		TenantId:          t.TenantID,
		Error:             t.Error,
		ResultKey:         t.ResultKey,
		CreatedAt:         timestamppb.New(t.CreatedAt),
		UpdatedAt:         timestamppb.New(t.UpdatedAt),
		TraceParent:       t.TraceParent,
		TraceState:        t.TraceState,
	}
}

func protoToDomainTask(p *clusterpb.Task) *domain.Task {
	if p == nil {
		return nil
	}
	t := &domain.Task{
		ID:                p.Id,
		Command:           domain.Command(p.Command),
		Payload:           string(p.Payload),
		Priority:          int(p.Priority),
		Webhook:           p.Webhook,
		MaxAttempts:       int(p.MaxAttempts),
		Status:            domain.TaskStatus(p.Status),
		LastKnownLocation: domain.TaskLocation(p.LastKnownLocation),
		WorkerID:          p.WorkerId,
		LeaseUntil:        p.LeaseUntil,
		Attempts:          int(p.Attempts),
		TenantID:          p.TenantId,
		Error:             p.Error,
		ResultKey:         p.ResultKey,
		TraceParent:       p.TraceParent,
		TraceState:        p.TraceState,
	}
	if p.CreatedAt != nil {
		t.CreatedAt = p.CreatedAt.AsTime()
	}
	if p.UpdatedAt != nil {
		t.UpdatedAt = p.UpdatedAt.AsTime()
	}
	return t
}

// domainResultToProto is the inverse of protoToResultRecord; serializes
// the typed Result map back to JSON bytes for the wire.
func domainResultToProto(rec *domain.ResultRecord) *clusterpb.ResultRecord {
	if rec == nil {
		return nil
	}
	p := &clusterpb.ResultRecord{
		TaskId: rec.TaskID,
		Status: string(rec.Status),
		Error:  rec.Error,
	}
	if !rec.CompletedAt.IsZero() {
		p.CompletedAt = timestamppb.New(rec.CompletedAt)
	}
	if rec.Result != nil {
		if b, err := sonic.Marshal(rec.Result); err == nil {
			p.ResultJson = b
		}
	}
	if len(rec.Artifacts) > 0 {
		p.Artifacts = make([]string, 0, len(rec.Artifacts))
		for _, a := range rec.Artifacts {
			p.Artifacts = append(p.Artifacts, a.Name)
		}
	}
	return p
}

func protoToResultRecord(p *clusterpb.ResultRecord) domain.ResultRecord {
	if p == nil {
		return domain.ResultRecord{}
	}
	r := domain.ResultRecord{
		TaskID: p.TaskId,
		Status: domain.TaskStatus(p.Status),
		Error:  p.Error,
	}
	if p.CompletedAt != nil {
		r.CompletedAt = p.CompletedAt.AsTime()
	}
	if len(p.ResultJson) > 0 {
		// Result is a typed map in domain; the wire form is raw JSON so
		// remote callers don't need to know the schema. Unmarshal lazily.
		_ = decodeResultJSON(p.ResultJson, &r.Result)
	}
	// Artifacts: each entry is JSON of ArtifactOut. We just hold the raw
	// list of objects in the record's Artifacts field.
	if len(p.Artifacts) > 0 {
		out := make([]domain.ArtifactOut, 0, len(p.Artifacts))
		for _, a := range p.Artifacts {
			out = append(out, domain.ArtifactOut{Name: a})
		}
		r.Artifacts = out
	}
	return r
}
