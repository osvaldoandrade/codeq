package workerclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/osvaldoandrade/codeq/internal/worker/workerpb"
)

// Handler processes one task and returns the disposition. Implementations
// must be safe for concurrent invocation — Client.Run will fan out up to
// Config.Concurrency calls in parallel.
type Handler func(ctx context.Context, t Task) Result

// Config controls Client behavior. Addr and Token are required; the
// rest have sensible defaults documented per-field.
type Config struct {
	// Addr is the gRPC dial target of the codeq worker stream server
	// (e.g. "localhost:9091"). Required.
	Addr string

	// Token is the bearer token presented in Hello. Required.
	Token string

	// WorkerID identifies this worker for lease ownership. If empty the
	// server uses the JWT subject from Token.
	WorkerID string

	// Commands restricts what this worker pulls. nil/empty means "use
	// whatever the token's eventTypes claim allows".
	Commands []string

	// Concurrency is the number of in-flight tasks the client maintains.
	// Defaults to 1. Each slot holds one Ready→Task→Result cycle.
	Concurrency int

	// LeaseSeconds is sent on each Ready. 0 means "server default".
	LeaseSeconds int

	// IdleBackoff is how long a slot waits before re-sending Ready when
	// the previous Ready didn't yield a task within ReadyTimeout. Defaults
	// to 50ms.
	IdleBackoff time.Duration

	// DialOptions are forwarded to grpc.NewClient. If empty, the client
	// uses insecure transport credentials — set this for TLS/mTLS.
	DialOptions []grpc.DialOption

	// Logger receives structured info/warn/error events. Defaults to
	// slog.Default().
	Logger *slog.Logger
}

func (c *Config) defaults() {
	if c.Concurrency <= 0 {
		c.Concurrency = 1
	}
	if c.IdleBackoff <= 0 {
		c.IdleBackoff = 50 * time.Millisecond
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Client is the long-lived worker stream client. Use New to construct
// one, Run to start dispatching tasks, and Close to release the
// underlying connection.
type Client struct {
	cfg  Config
	conn *grpc.ClientConn
}

// New dials the server and returns a Client ready for Run. The
// connection stays open until Close is called.
func New(cfg Config) (*Client, error) {
	if cfg.Addr == "" {
		return nil, errors.New("workerclient: Addr is required")
	}
	if cfg.Token == "" {
		return nil, errors.New("workerclient: Token is required")
	}
	cfg.defaults()

	opts := cfg.DialOptions
	if len(opts) == 0 {
		opts = []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	}
	conn, err := grpc.NewClient(cfg.Addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("workerclient: dial %s: %w", cfg.Addr, err)
	}
	return &Client{cfg: cfg, conn: conn}, nil
}

// Close releases the underlying gRPC connection. Safe to call multiple
// times.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// Run opens a stream, completes the Hello handshake, and dispatches
// tasks to h until ctx is cancelled or a fatal stream error occurs.
// The returned error is the cause of termination — nil only when the
// server cleanly closed the stream.
func (c *Client) Run(ctx context.Context, h Handler) error {
	if h == nil {
		return errors.New("workerclient: Handler is required")
	}
	stream, err := workerpb.NewWorkerStreamClient(c.conn).Stream(ctx)
	if err != nil {
		return fmt.Errorf("workerclient: open stream: %w", err)
	}

	sess := &session{
		cfg:    c.cfg,
		stream: stream,
		taskCh: make(chan *workerpb.Task),
		log:    c.cfg.Logger,
	}
	return sess.run(ctx, h)
}

// session bundles the per-Run mutable state. One session per Run call.
type session struct {
	cfg    Config
	stream workerpb.WorkerStream_StreamClient
	taskCh chan *workerpb.Task
	log    *slog.Logger

	sendMu sync.Mutex // serializes stream.Send across slots + reader
}

func (s *session) send(ev *workerpb.WorkerEvent) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.stream.Send(ev)
}

func (s *session) run(ctx context.Context, h Handler) error {
	if err := s.handshake(); err != nil {
		return err
	}

	// Reader fan-outs Task assignments into taskCh. Ack messages
	// (Result/Heartbeat/Nack/Abandon) are intentionally dropped — the
	// slot that issued the corresponding send already moved on. Adding
	// pending-ack correlation is straightforward future work but
	// unnecessary for the throughput-focused MVP.
	readerErrCh := make(chan error, 1)
	go func() {
		readerErrCh <- s.readLoop(ctx)
		close(s.taskCh)
	}()

	// N slots in parallel, each looping: Ready → Task → Handler → Result.
	var wg sync.WaitGroup
	wg.Add(s.cfg.Concurrency)
	for i := 0; i < s.cfg.Concurrency; i++ {
		go func() {
			defer wg.Done()
			s.slotLoop(ctx, h)
		}()
	}
	wg.Wait()

	// Slot loop only exits on ctx.Done or reader closing taskCh.
	// readerErrCh delivers the underlying cause.
	if err := <-readerErrCh; err != nil {
		return err
	}
	return ctx.Err()
}

func (s *session) handshake() error {
	hello := &workerpb.WorkerEvent{Event: &workerpb.WorkerEvent_Hello{
		Hello: &workerpb.Hello{Token: s.cfg.Token, WorkerId: s.cfg.WorkerID},
	}}
	if err := s.send(hello); err != nil {
		return fmt.Errorf("workerclient: send hello: %w", err)
	}
	ack, err := s.stream.Recv()
	if err != nil {
		return fmt.Errorf("workerclient: recv helloack: %w", err)
	}
	switch e := ack.Event.(type) {
	case *workerpb.ServerEvent_HelloAck:
		s.log.Debug("workerclient: hello ok",
			"workerId", e.HelloAck.WorkerId,
			"tenantId", e.HelloAck.TenantId)
		return nil
	case *workerpb.ServerEvent_Error:
		return fmt.Errorf("workerclient: server rejected hello: %s (%s)",
			e.Error.Message, e.Error.Code)
	default:
		return fmt.Errorf("workerclient: unexpected hello reply: %T", ack.Event)
	}
}

func (s *session) readLoop(ctx context.Context) error {
	for {
		ev, err := s.stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("workerclient: stream recv: %w", err)
		}
		switch e := ev.Event.(type) {
		case *workerpb.ServerEvent_Task:
			select {
			case s.taskCh <- e.Task.Task:
			case <-ctx.Done():
				return nil
			}
		case *workerpb.ServerEvent_ResultAck,
			*workerpb.ServerEvent_HeartbeatAck,
			*workerpb.ServerEvent_NackAck,
			*workerpb.ServerEvent_AbandonAck:
			// MVP: drop. Slots don't block on acks.
		case *workerpb.ServerEvent_Error:
			s.log.Warn("workerclient: server error event",
				"code", e.Error.Code, "msg", e.Error.Message)
		case *workerpb.ServerEvent_HelloAck:
			// Re-HelloAck shouldn't happen; ignore.
		default:
			s.log.Warn("workerclient: unknown server event", "type", fmt.Sprintf("%T", ev.Event))
		}
	}
}

func (s *session) slotLoop(ctx context.Context, h Handler) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := s.send(&workerpb.WorkerEvent{Event: &workerpb.WorkerEvent_Ready{
			Ready: &workerpb.Ready{
				Commands:     s.cfg.Commands,
				LeaseSeconds: int32(s.cfg.LeaseSeconds),
			},
		}}); err != nil {
			s.log.Warn("workerclient: send ready", "err", err)
			return
		}

		select {
		case t, ok := <-s.taskCh:
			if !ok {
				return
			}
			s.dispatch(ctx, h, t)
		case <-ctx.Done():
			return
		}
	}
}

func (s *session) dispatch(ctx context.Context, h Handler, pt *workerpb.Task) {
	t := protoToTask(pt)
	res := h(ctx, t)
	if err := s.applyResult(pt.Id, res); err != nil {
		s.log.Warn("workerclient: apply result",
			"taskId", pt.Id, "kind", res.kind, "err", err)
	}
}

func (s *session) applyResult(taskID string, r Result) error {
	switch r.kind {
	case resultCompleted:
		var payload []byte
		if r.body != nil {
			b, err := sonic.Marshal(r.body)
			if err != nil {
				return fmt.Errorf("marshal result body: %w", err)
			}
			payload = b
		}
		return s.send(&workerpb.WorkerEvent{Event: &workerpb.WorkerEvent_Result{
			Result: &workerpb.Result{
				TaskId:     taskID,
				Status:     "COMPLETED",
				ResultJson: payload,
			},
		}})
	case resultFailed:
		return s.send(&workerpb.WorkerEvent{Event: &workerpb.WorkerEvent_Result{
			Result: &workerpb.Result{
				TaskId: taskID,
				Status: "FAILED",
				Error:  r.err,
			},
		}})
	case resultNacked:
		return s.send(&workerpb.WorkerEvent{Event: &workerpb.WorkerEvent_Nack{
			Nack: &workerpb.Nack{
				TaskId:       taskID,
				DelaySeconds: int32(r.delaySeconds),
				Reason:       r.reason,
			},
		}})
	case resultAbandoned:
		return s.send(&workerpb.WorkerEvent{Event: &workerpb.WorkerEvent_Abandon{
			Abandon: &workerpb.Abandon{TaskId: taskID},
		}})
	default:
		return fmt.Errorf("workerclient: invalid Result (zero value?)")
	}
}

func protoToTask(p *workerpb.Task) Task {
	if p == nil {
		return Task{}
	}
	return Task{
		ID:          p.Id,
		Command:     p.Command,
		Payload:     p.Payload,
		Priority:    int(p.Priority),
		Attempts:    int(p.Attempts),
		MaxAttempts: int(p.MaxAttempts),
		TenantID:    p.TenantId,
		Webhook:     p.Webhook,
		LeaseUntil:  p.LeaseUntil,
	}
}
