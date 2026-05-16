// Package producer implements the producer-facing streaming gRPC server.
// Producers open a long-lived bidirectional stream, send Hello once to
// authenticate, then pipeline CreateTask events. The server fans out to
// SchedulerService.CreateTask and acks each event by seq once durable.
// This removes the per-call HTTP middleware tax that dominated CPU at
// peak load after Phase 2 lifted the worker side.
package producer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/osvaldoandrade/codeq/internal/producer/producerpb"
	"github.com/osvaldoandrade/codeq/internal/services"
	"github.com/osvaldoandrade/codeq/pkg/auth"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// Server hosts the ProducerStream service. One instance per codeq
// process; concurrent streams are isolated via per-stream goroutines.
type Server struct {
	producerpb.UnimplementedProducerStreamServer

	Scheduler services.SchedulerService
	Validator auth.Validator
	Logger    *slog.Logger
}

// New builds a Server. Validator is required — without it the server
// has no way to authenticate Hello messages.
func New(scheduler services.SchedulerService, validator auth.Validator, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		Scheduler: scheduler,
		Validator: validator,
		Logger:    logger,
	}
}

// streamSession is the per-connection state. sendMu serializes Send so
// the per-event handler goroutines never race on the gRPC stream.
type streamSession struct {
	stream   producerpb.ProducerStream_StreamServer
	sendMu   sync.Mutex
	tenantID string
	subject  string
}

func (s *streamSession) send(ev *producerpb.ServerEvent) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.stream.Send(ev)
}

func (s *streamSession) sendAck(seq uint64, ok bool, taskID, errMsg string) error {
	return s.send(&producerpb.ServerEvent{Event: &producerpb.ServerEvent_CreateAck{
		CreateAck: &producerpb.CreateAck{
			Seq:          seq,
			Ok:           ok,
			TaskId:       taskID,
			ErrorMessage: errMsg,
		},
	}})
}

// Stream is the bidirectional rpc. First message MUST be Hello; the
// server validates the token and replies with HelloAck. Subsequent
// CreateTask events fan out to per-event goroutines so a slow Pebble
// commit can't head-of-line the read loop.
func (s *Server) Stream(stream producerpb.ProducerStream_StreamServer) error {
	ctx := stream.Context()
	sess, err := s.handleHello(stream)
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
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
		case *producerpb.ProducerEvent_Hello:
			_ = sess.send(&producerpb.ServerEvent{Event: &producerpb.ServerEvent_Error{
				Error: &producerpb.ServerError{
					Code:    codes.FailedPrecondition.String(),
					Message: "already-authenticated",
				},
			}})
		case *producerpb.ProducerEvent_Create:
			wg.Add(1)
			go func(req *producerpb.CreateTask) {
				defer wg.Done()
				s.handleCreate(ctx, sess, req)
			}(e.Create)
		default:
			_ = sess.send(&producerpb.ServerEvent{Event: &producerpb.ServerEvent_Error{
				Error: &producerpb.ServerError{Code: codes.InvalidArgument.String(), Message: "unknown-event"},
			}})
		}
	}
}

func (s *Server) handleHello(stream producerpb.ProducerStream_StreamServer) (*streamSession, error) {
	first, err := stream.Recv()
	if err != nil {
		return nil, err
	}
	hello, ok := first.Event.(*producerpb.ProducerEvent_Hello)
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "first event must be Hello")
	}
	if s.Validator == nil {
		return nil, status.Errorf(codes.Internal, "no producer validator configured")
	}
	token := strings.TrimSpace(hello.Hello.Token)
	if token == "" {
		return nil, status.Errorf(codes.Unauthenticated, "missing token")
	}
	claims, err := s.Validator.Validate(token)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "auth failed: %v", err)
	}
	sess := &streamSession{
		stream:   stream,
		tenantID: tenantIDFromClaims(claims),
		subject:  claims.Subject,
	}
	if err := sess.send(&producerpb.ServerEvent{Event: &producerpb.ServerEvent_HelloAck{
		HelloAck: &producerpb.HelloAck{TenantId: sess.tenantID, Subject: sess.subject},
	}}); err != nil {
		return nil, err
	}
	return sess, nil
}

func (s *Server) handleCreate(ctx context.Context, sess *streamSession, req *producerpb.CreateTask) {
	if req == nil {
		return
	}
	cmd := domain.Command(strings.TrimSpace(req.Command))
	if cmd == "" {
		_ = sess.sendAck(req.Seq, false, "", "command is required")
		return
	}
	if req.DelaySeconds < 0 {
		_ = sess.sendAck(req.Seq, false, "", "delaySeconds must be >= 0")
		return
	}
	var runAt time.Time
	if req.RunAt != nil {
		runAt = req.RunAt.AsTime()
	}

	payload := string(req.Payload)
	if payload == "" {
		payload = "null"
	}

	task, err := s.Scheduler.CreateTask(
		ctx,
		cmd,
		payload,
		int(req.Priority),
		req.Webhook,
		int(req.MaxAttempts),
		req.IdempotencyKey,
		runAt,
		int(req.DelaySeconds),
		sess.tenantID,
	)
	if err != nil {
		_ = sess.sendAck(req.Seq, false, "", err.Error())
		return
	}
	_ = sess.sendAck(req.Seq, true, task.ID, "")
}

// tenantIDFromClaims mirrors middleware/tenant.go's extractTenantID but
// is duplicated here to avoid pulling the middleware (and its Gin deps)
// into the gRPC server. Falls back to JWT subject for single-tenant.
func tenantIDFromClaims(claims *auth.Claims) string {
	if claims == nil {
		return ""
	}
	if claims.Raw != nil {
		for _, key := range []string{"tenantId", "tenant_id", "organizationId", "organization_id"} {
			if v, ok := claims.Raw[key].(string); ok {
				if t := strings.TrimSpace(v); t != "" {
					return t
				}
			}
		}
	}
	return strings.TrimSpace(claims.Subject)
}

// pre-flight check that the gRPC server interface is satisfied.
var _ producerpb.ProducerStreamServer = (*Server)(nil)
