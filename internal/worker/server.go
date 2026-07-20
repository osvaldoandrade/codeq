// Package worker implements the worker-facing streaming gRPC server.
// Workers open a long-lived bidirectional stream, send Hello once to
// authenticate, then loop sending Ready/Result/Nack/Heartbeat/Abandon
// events. The server pushes Task assignments down the same stream when
// claims succeed. This removes the per-call HTTP middleware tax that
// dominated CPU at peak load after Phase 1.2.
package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bytedance/sonic"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/osvaldoandrade/codeq/internal/authclaims"
	"github.com/osvaldoandrade/codeq/internal/services"
	"github.com/osvaldoandrade/codeq/internal/worker/workerpb"
	"github.com/osvaldoandrade/codeq/pkg/auth"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// Server hosts the WorkerStream service. One instance per codeq process;
// concurrent streams are isolated via per-stream goroutines.
type Server struct {
	workerpb.UnimplementedWorkerStreamServer

	Scheduler             services.SchedulerService
	Results               services.ResultsService
	Validator             auth.Validator
	ProducerValidator     auth.Validator
	Logger                *slog.Logger
	DefaultLease          int
	WorkerAudience        string
	AllowProducerAsWorker bool
	MaxClaimRetries       int           // bounds the busy-wait loop when no tasks available
	ClaimPollDelay        time.Duration // delay between retries when no task is available
}

// New builds a Server with sensible defaults for the polling knobs.
func New(scheduler services.SchedulerService, results services.ResultsService, validator auth.Validator, logger *slog.Logger, defaultLease int, audience string) *Server {
	return &Server{
		Scheduler:       scheduler,
		Results:         results,
		Validator:       validator,
		Logger:          logger,
		DefaultLease:    defaultLease,
		WorkerAudience:  audience,
		MaxClaimRetries: 600, // ~30s total at 50ms delay
		ClaimPollDelay:  50 * time.Millisecond,
	}
}

// streamSession holds per-connection state. Sends are serialized via a
// dedicated writer goroutine reading from sendCh rather than a mutex
// around stream.Send: profile (Phase 6) showed the sendMu mutex
// accounting for ~74% of the mutex profile under load. The channel
// approach avoids the cache-line ping-pong that mutex contention
// produces across the many per-event handler goroutines.
type streamSession struct {
	stream   workerpb.WorkerStream_StreamServer
	workerID string
	tenantID string
	claims   *auth.Claims
	commands []domain.Command // resolved from scope/eventTypes claim
	hasWild  bool

	// sendCh is the writer's inbox. Buffered so per-event goroutines
	// pushing acks don't block during transient Send latency; on
	// overflow the producer falls through to ctx-cancelled or the
	// channel close path.
	sendCh chan *workerpb.ServerEvent
	// sendErr captures the first Send failure so subsequent senders bail
	// fast instead of queuing into a dead stream. atomic.Pointer for
	// lock-free read on the hot path.
	sendErr atomic.Pointer[error]
	// writerDone closes when the writer goroutine has fully drained and
	// stopped. Used by Stream() to ensure no in-flight Send races the
	// rpc handler return.
	writerDone chan struct{}
	// ctx mirrors stream.Context() so send() can fall through on
	// cancellation without consulting stream.Context() each call.
	ctx context.Context
}

// sendChanBuffer bounds how many ServerEvents we'll queue ahead of the
// writer. Sized large enough to absorb a burst of acks from a worker
// with N concurrent Ready/Result slots without blocking, small enough
// that pathological backpressure surfaces quickly as ctx cancellation.
const sendChanBuffer = 256

// newStreamSession wires the writer goroutine. Caller is responsible
// for invoking sess.close() before Stream() returns so the writer
// exits cleanly.
func newStreamSession(ctx context.Context, stream workerpb.WorkerStream_StreamServer) *streamSession {
	s := &streamSession{
		stream:     stream,
		sendCh:     make(chan *workerpb.ServerEvent, sendChanBuffer),
		writerDone: make(chan struct{}),
		ctx:        ctx,
	}
	go s.writeLoop()
	return s
}

// writeLoop owns the stream's Send side. It drains sendCh until the
// channel is closed, marking the first Send failure on sendErr so
// readers can short-circuit.
func (s *streamSession) writeLoop() {
	defer close(s.writerDone)
	for ev := range s.sendCh {
		if s.sendErr.Load() != nil {
			// Already failed; drain remaining events without sending so
			// the producers' channel sends don't block.
			continue
		}
		if err := s.stream.Send(ev); err != nil {
			cp := err
			s.sendErr.Store(&cp)
		}
	}
}

// closeWriter signals the writer to drain and exit, then waits.
func (s *streamSession) closeWriter() {
	close(s.sendCh)
	<-s.writerDone
}

// Stream is the bidirectional rpc. First message MUST be Hello; the
// server validates the token and replies with HelloAck. Subsequent
// events fan out to per-event goroutines so the read loop never blocks
// on Pebble or the scheduler.
func (s *Server) Stream(stream workerpb.WorkerStream_StreamServer) error {
	ctx := stream.Context()
	sess, err := s.handleHello(stream)
	if err != nil {
		return err
	}
	// Ensure the writer goroutine drains and exits before we return,
	// otherwise the gRPC runtime can reap the stream while a final Send
	// is still in flight.
	defer sess.closeWriter()

	var wg sync.WaitGroup
	// Read loop runs in the caller goroutine; per-event work spawns child
	// goroutines so a slow ClaimTask can't head-of-line a Result.
	for {
		ev, err := stream.Recv()
		if err != nil {
			wg.Wait()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		select {
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		default:
		}

		switch e := ev.Event.(type) {
		case *workerpb.WorkerEvent_Hello:
			// Re-Hello is a protocol error.
			_ = sess.sendError(codes.FailedPrecondition, "already-authenticated")
		case *workerpb.WorkerEvent_Ready:
			if !sess.requireScope("codeq:claim") {
				continue
			}
			wg.Add(1)
			go func(req *workerpb.Ready) {
				defer wg.Done()
				s.handleReady(ctx, sess, req)
			}(e.Ready)
		case *workerpb.WorkerEvent_Result:
			if !sess.requireScope("codeq:result") {
				continue
			}
			wg.Add(1)
			go func(req *workerpb.Result) {
				defer wg.Done()
				s.handleResult(ctx, sess, req)
			}(e.Result)
		case *workerpb.WorkerEvent_ResultBatch:
			if !sess.requireScope("codeq:result") {
				continue
			}
			wg.Add(1)
			go func(req *workerpb.ResultBatch) {
				defer wg.Done()
				s.handleResultBatch(ctx, sess, req)
			}(e.ResultBatch)
		case *workerpb.WorkerEvent_Nack:
			if !sess.requireScope("codeq:nack") {
				continue
			}
			wg.Add(1)
			go func(req *workerpb.Nack) {
				defer wg.Done()
				s.handleNack(ctx, sess, req)
			}(e.Nack)
		case *workerpb.WorkerEvent_Heartbeat:
			if !sess.requireScope("codeq:heartbeat") {
				continue
			}
			wg.Add(1)
			go func(req *workerpb.Heartbeat) {
				defer wg.Done()
				s.handleHeartbeat(ctx, sess, req)
			}(e.Heartbeat)
		case *workerpb.WorkerEvent_Abandon:
			if !sess.requireScope("codeq:abandon") {
				continue
			}
			wg.Add(1)
			go func(req *workerpb.Abandon) {
				defer wg.Done()
				s.handleAbandon(ctx, sess, req)
			}(e.Abandon)
		default:
			_ = sess.sendError(codes.InvalidArgument, "unknown-event")
		}
	}
}

func (s *Server) handleHello(stream workerpb.WorkerStream_StreamServer) (*streamSession, error) {
	first, err := stream.Recv()
	if err != nil {
		return nil, err
	}
	hello, ok := first.Event.(*workerpb.WorkerEvent_Hello)
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "first event must be Hello")
	}
	if s.Validator == nil {
		return nil, status.Errorf(codes.Internal, "no worker validator configured")
	}
	claims, err := s.authenticate(hello.Hello.Token)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "auth failed: %v", err)
	}
	workerID := strings.TrimSpace(hello.Hello.WorkerId)
	if workerID == "" {
		workerID = claims.Subject
	}
	tenantID, err := authclaims.ResolveTenantID(claims)
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, "invalid tenant claims")
	}

	sess := newStreamSession(stream.Context(), stream)
	sess.workerID = workerID
	sess.tenantID = tenantID
	sess.claims = claims
	for _, ev := range claims.EventTypes {
		if ev == "*" {
			sess.hasWild = true
			continue
		}
		sess.commands = append(sess.commands, domain.Command(ev))
	}

	if err := sess.send(&workerpb.ServerEvent{
		Event: &workerpb.ServerEvent_HelloAck{
			HelloAck: &workerpb.HelloAck{WorkerId: workerID, TenantId: tenantID},
		},
	}); err != nil {
		sess.closeWriter()
		return nil, err
	}
	return sess, nil
}

func (s *Server) handleReady(ctx context.Context, sess *streamSession, req *workerpb.Ready) {
	commands, ok := s.resolveCommands(sess, req.Commands)
	if !ok {
		_ = sess.sendError(codes.PermissionDenied, "event type not allowed")
		return
	}
	lease := int(req.LeaseSeconds)
	if lease <= 0 {
		lease = s.DefaultLease
	}
	// count=0 / count=1 → legacy single-task path. count > 1 → batch.
	count := int(req.Count)
	if count <= 1 {
		s.handleReadyOne(ctx, sess, commands, lease)
		return
	}
	s.handleReadyBatch(ctx, sess, commands, lease, count)
}

// handleReadyOne is the unchanged single-task path; kept verbatim so
// existing clients see no behavior change.
func (s *Server) handleReadyOne(ctx context.Context, sess *streamSession, commands []domain.Command, lease int) {
	for attempt := 0; attempt <= s.MaxClaimRetries; attempt++ {
		if ctx.Err() != nil {
			return
		}
		task, ok, err := s.Scheduler.ClaimTask(ctx, sess.workerID, commands, lease, 0, sess.tenantID)
		if err != nil {
			_ = sess.sendError(codes.Internal, err.Error())
			return
		}
		if ok && task != nil {
			_ = sess.send(&workerpb.ServerEvent{
				Event: &workerpb.ServerEvent_Task{Task: &workerpb.TaskAssignment{Task: taskToProto(task)}},
			})
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.ClaimPollDelay):
		}
	}
}

// handleReadyBatch claims up to `count` tasks and sends them as one
// TaskBatch. Phase 7 swap: instead of N serial ClaimTask calls (each
// committing its own Pebble batch), we issue a single ClaimManyTasks
// which the Pebble backend services with one pop loop + one batch
// commit. On Redis the service falls back to the N-call loop so the
// behavior degrades cleanly.
//
// Poll loop preserves the "wait if queue is empty" semantics for idle
// workers — first ClaimMany may come back empty, in which case we
// sleep and retry until we get something or hit the retry budget.
// Once the first batch returns we send TaskBatch immediately; we
// don't keep pulling while-we-have-tasks because ClaimMany already
// drained the channels in one shot.
func (s *Server) handleReadyBatch(ctx context.Context, sess *streamSession, commands []domain.Command, lease, count int) {
	var tasks []*workerpb.Task
	for attempt := 0; attempt <= s.MaxClaimRetries; attempt++ {
		if ctx.Err() != nil {
			return
		}
		got, err := s.Scheduler.ClaimManyTasks(ctx, sess.workerID, commands, lease, count, sess.tenantID)
		if err != nil {
			_ = sess.sendError(codes.Internal, err.Error())
			return
		}
		if len(got) > 0 {
			tasks = make([]*workerpb.Task, len(got))
			for i, t := range got {
				tasks[i] = taskToProto(t)
			}
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.ClaimPollDelay):
		}
	}
	if len(tasks) == 0 {
		return
	}
	_ = sess.send(&workerpb.ServerEvent{Event: &workerpb.ServerEvent_TaskBatch{
		TaskBatch: &workerpb.TaskBatch{Tasks: tasks},
	}})
}

// handleResultBatch is the batched submit path. Eliminates the per-task
// Send + Pebble commit overhead that handleResult pays for each result,
// by funnelling all N results through ResultsService.BatchSubmit (one
// GetTasksBatch + one BatchUpdateTasksOnComplete on Pebble). Acks come
// back as a single ResultAckBatch matching the input order so the
// client can correlate without extra bookkeeping.
func (s *Server) handleResultBatch(ctx context.Context, sess *streamSession, req *workerpb.ResultBatch) {
	if req == nil || len(req.Results) == 0 {
		return
	}
	acks := make([]*workerpb.ResultAck, len(req.Results))
	items := make([]domain.BatchSubmitItem, 0, len(req.Results))
	itemIdx := make([]int, 0, len(req.Results)) // results index for each item

	// First pass: validate per-result, parse JSON; rejects produce a
	// ResultAck in-place without touching the service. Valid results
	// get batched for one BatchSubmit call.
	for i, r := range req.Results {
		if r == nil {
			acks[i] = &workerpb.ResultAck{Ok: false, ErrorMessage: "nil result"}
			continue
		}
		if err := validateResultStatus(r.Status); err != nil {
			acks[i] = &workerpb.ResultAck{TaskId: r.TaskId, Ok: false, ErrorMessage: err.Error()}
			continue
		}
		var resultVal map[string]any
		if len(r.ResultJson) > 0 {
			if err := sonic.Unmarshal(r.ResultJson, &resultVal); err != nil {
				acks[i] = &workerpb.ResultAck{TaskId: r.TaskId, Ok: false, ErrorMessage: "invalid result_json"}
				continue
			}
		}
		items = append(items, domain.BatchSubmitItem{
			TaskID: r.TaskId,
			SubmitResultRequest: domain.SubmitResultRequest{
				Status:   domain.TaskStatus(r.Status),
				WorkerID: sess.workerID,
				Result:   resultVal,
				Error:    r.Error,
			},
		})
		itemIdx = append(itemIdx, i)
	}

	if len(items) > 0 {
		resps, err := s.Results.BatchSubmit(ctx, items)
		if err != nil {
			// Whole-batch failure: ack every queued item with the same error.
			for _, i := range itemIdx {
				acks[i] = &workerpb.ResultAck{TaskId: req.Results[i].TaskId, Ok: false, ErrorMessage: err.Error()}
			}
		} else {
			for k, resp := range resps {
				i := itemIdx[k]
				if resp.Error != "" {
					acks[i] = &workerpb.ResultAck{TaskId: resp.TaskID, Ok: false, ErrorMessage: resp.Error}
				} else {
					acks[i] = &workerpb.ResultAck{TaskId: resp.TaskID, Ok: true}
				}
			}
		}
	}

	_ = sess.send(&workerpb.ServerEvent{Event: &workerpb.ServerEvent_ResultAckBatch{
		ResultAckBatch: &workerpb.ResultAckBatch{Acks: acks},
	}})
}

func (s *Server) handleResult(ctx context.Context, sess *streamSession, req *workerpb.Result) {
	if err := validateResultStatus(req.Status); err != nil {
		_ = sess.send(&workerpb.ServerEvent{Event: &workerpb.ServerEvent_ResultAck{
			ResultAck: &workerpb.ResultAck{TaskId: req.TaskId, Ok: false, ErrorMessage: err.Error()},
		}})
		return
	}
	taskStatus := domain.TaskStatus(req.Status)
	var resultVal map[string]any
	if len(req.ResultJson) > 0 {
		if err := sonic.Unmarshal(req.ResultJson, &resultVal); err != nil {
			_ = sess.send(&workerpb.ServerEvent{Event: &workerpb.ServerEvent_ResultAck{
				ResultAck: &workerpb.ResultAck{TaskId: req.TaskId, Ok: false, ErrorMessage: "invalid result_json"},
			}})
			return
		}
	}
	submitReq := domain.SubmitResultRequest{
		Status:   taskStatus,
		WorkerID: sess.workerID,
		Result:   resultVal,
		Error:    req.Error,
	}
	if _, err := s.Results.Submit(ctx, req.TaskId, submitReq); err != nil {
		_ = sess.send(&workerpb.ServerEvent{Event: &workerpb.ServerEvent_ResultAck{
			ResultAck: &workerpb.ResultAck{TaskId: req.TaskId, Ok: false, ErrorMessage: err.Error()},
		}})
		return
	}
	_ = sess.send(&workerpb.ServerEvent{Event: &workerpb.ServerEvent_ResultAck{
		ResultAck: &workerpb.ResultAck{TaskId: req.TaskId, Ok: true},
	}})
}

func (s *Server) handleNack(ctx context.Context, sess *streamSession, req *workerpb.Nack) {
	applied, dlq, err := s.Scheduler.NackTask(ctx, req.TaskId, sess.workerID, int(req.DelaySeconds), req.Reason)
	if err != nil {
		_ = sess.send(&workerpb.ServerEvent{Event: &workerpb.ServerEvent_NackAck{
			NackAck: &workerpb.NackAck{TaskId: req.TaskId, Ok: false, ErrorMessage: err.Error()},
		}})
		return
	}
	_ = sess.send(&workerpb.ServerEvent{Event: &workerpb.ServerEvent_NackAck{
		NackAck: &workerpb.NackAck{TaskId: req.TaskId, Ok: true, AppliedDelaySeconds: int32(applied), Dlq: dlq},
	}})
}

func (s *Server) handleHeartbeat(ctx context.Context, sess *streamSession, req *workerpb.Heartbeat) {
	if err := s.Scheduler.Heartbeat(ctx, req.TaskId, sess.workerID, int(req.ExtendSeconds)); err != nil {
		_ = sess.send(&workerpb.ServerEvent{Event: &workerpb.ServerEvent_HeartbeatAck{
			HeartbeatAck: &workerpb.HeartbeatAck{TaskId: req.TaskId, Ok: false, ErrorMessage: err.Error()},
		}})
		return
	}
	_ = sess.send(&workerpb.ServerEvent{Event: &workerpb.ServerEvent_HeartbeatAck{
		HeartbeatAck: &workerpb.HeartbeatAck{TaskId: req.TaskId, Ok: true},
	}})
}

func (s *Server) handleAbandon(ctx context.Context, sess *streamSession, req *workerpb.Abandon) {
	if err := s.Scheduler.Abandon(ctx, req.TaskId, sess.workerID); err != nil {
		_ = sess.send(&workerpb.ServerEvent{Event: &workerpb.ServerEvent_AbandonAck{
			AbandonAck: &workerpb.AbandonAck{TaskId: req.TaskId, Ok: false, ErrorMessage: err.Error()},
		}})
		return
	}
	_ = sess.send(&workerpb.ServerEvent{Event: &workerpb.ServerEvent_AbandonAck{
		AbandonAck: &workerpb.AbandonAck{TaskId: req.TaskId, Ok: true},
	}})
}

func (s *Server) authenticate(token string) (*auth.Claims, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("missing token")
	}

	claims, err := s.Validator.Validate(token)
	if err == nil {
		if claimErr := validateWorkerClaims(claims); claimErr != nil {
			err = claimErr
		} else {
			return claims, nil
		}
	}

	if s.AllowProducerAsWorker && s.ProducerValidator != nil {
		pclaims, perr := s.ProducerValidator.Validate(token)
		if perr == nil {
			raw := pclaims.Raw
			if raw == nil {
				raw = map[string]any{}
			}
			return &auth.Claims{
				Subject:    pclaims.Subject,
				Email:      pclaims.Email,
				Issuer:     "producer",
				Audience:   []string{s.WorkerAudience},
				ExpiresAt:  pclaims.ExpiresAt,
				IssuedAt:   pclaims.IssuedAt,
				Scopes:     []string{"codeq:claim", "codeq:heartbeat", "codeq:abandon", "codeq:nack", "codeq:result", "codeq:subscribe"},
				EventTypes: []string{"*"},
				Raw:        raw,
			}, nil
		}
	}

	return nil, err
}

func validateWorkerClaims(claims *auth.Claims) error {
	if claims == nil {
		return errors.New("missing claims")
	}
	if len(claims.EventTypes) == 0 {
		return errors.New("missing eventTypes")
	}
	if len(claims.Scopes) == 0 {
		return errors.New("missing scope")
	}
	return nil
}

// resolveCommands intersects the worker's requested commands with the
// event types it was issued. Wildcard scope passes everything through.
func (s *Server) resolveCommands(sess *streamSession, requested []string) ([]domain.Command, bool) {
	if len(requested) == 0 {
		if sess.hasWild {
			return nil, true // claim repo treats nil as "any"
		}
		return sess.commands, len(sess.commands) > 0
	}
	if sess.hasWild {
		out := make([]domain.Command, 0, len(requested))
		for _, c := range requested {
			if c = strings.TrimSpace(c); c != "" {
				out = append(out, domain.Command(c))
			}
		}
		return out, len(out) > 0
	}
	allowed := make(map[domain.Command]bool, len(sess.commands))
	for _, c := range sess.commands {
		allowed[c] = true
	}
	out := make([]domain.Command, 0, len(requested))
	for _, c := range requested {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		dc := domain.Command(c)
		if !allowed[dc] {
			return nil, false
		}
		out = append(out, dc)
	}
	return out, len(out) > 0
}

func (s *streamSession) send(ev *workerpb.ServerEvent) error {
	if errPtr := s.sendErr.Load(); errPtr != nil {
		return *errPtr
	}
	select {
	case s.sendCh <- ev:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func (s *streamSession) sendError(code codes.Code, msg string) error {
	return s.send(&workerpb.ServerEvent{Event: &workerpb.ServerEvent_Error{
		Error: &workerpb.ServerError{Code: code.String(), Message: msg},
	}})
}

func (s *streamSession) requireScope(scope string) bool {
	if s.claims != nil && s.claims.HasScope(scope) {
		return true
	}
	_ = s.sendError(codes.PermissionDenied, "missing scope")
	return false
}

func taskToProto(t *domain.Task) *workerpb.Task {
	if t == nil {
		return nil
	}
	pt := &workerpb.Task{
		Id:          t.ID,
		Command:     string(t.Command),
		Payload:     []byte(t.Payload),
		Priority:    int32(t.Priority),
		Webhook:     t.Webhook,
		MaxAttempts: int32(t.MaxAttempts),
		Status:      string(t.Status),
		WorkerId:    t.WorkerID,
		LeaseUntil:  t.LeaseUntil,
		Attempts:    int32(t.Attempts),
		TenantId:    t.TenantID,
	}
	if !t.CreatedAt.IsZero() {
		pt.CreatedAt = timestamppb.New(t.CreatedAt)
	}
	if !t.UpdatedAt.IsZero() {
		pt.UpdatedAt = timestamppb.New(t.UpdatedAt)
	}
	return pt
}

// errInvalidStatus is returned when a Result event carries a status the
// API doesn't recognize. Plumbed through to ResultAck.error_message.
var errInvalidStatus = errors.New("invalid status (must be COMPLETED or FAILED)")

func validateResultStatus(s string) error {
	if s != "COMPLETED" && s != "FAILED" {
		return fmt.Errorf("%w: %q", errInvalidStatus, s)
	}
	return nil
}
