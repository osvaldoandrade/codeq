package workerclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
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
	// Defaults to 1. Each slot holds one Ready→Task(s)→Result(s) cycle.
	Concurrency int

	// LeaseSeconds is sent on each Ready. 0 means "server default".
	LeaseSeconds int

	// BatchSize controls how many tasks each slot tries to claim per
	// Ready, and how many Results coalesce into one ResultBatch. 0/1
	// keeps the legacy single-task path (one Ready → one Task → one
	// Result per cycle). >1 enables Phase 6 / Q2 batching: each cycle
	// pulls up to BatchSize Tasks via Ready{count=BatchSize} → server
	// replies with TaskBatch, and the resulting Results are sent back
	// as one ResultBatch. Amortises gRPC framing + Pebble commit cost
	// across the batch.
	BatchSize int

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

	sess := newSession(ctx, c.cfg, stream)
	defer sess.closeWriter()
	return sess.run(ctx, h)
}

// session bundles the per-Run mutable state. One session per Run call.
// Sends are funnelled through a single writer goroutine reading from
// sendCh instead of a mutex around stream.Send: profile (Phase 6)
// pinned the client-side sendMu at ~26% of the mutex profile under
// 128-slot load, with the per-slot Ready/Result loops fighting each
// other for the lock.
type session struct {
	cfg    Config
	stream workerpb.WorkerStream_StreamClient
	// batchCh delivers each Ready's response to one slot. For
	// BatchSize<=1 the batch contains exactly one task (legacy single-
	// task path). For BatchSize>1 it contains up to BatchSize tasks
	// from a single TaskBatch — kept together so the slot can issue
	// one ResultBatch in response without serial Send overhead.
	batchCh chan []*workerpb.Task
	log     *slog.Logger

	ctx        context.Context
	sendCh     chan *workerpb.WorkerEvent
	sendErr    atomic.Pointer[error]
	writerDone chan struct{}
}

// sendChanBufferClient is sized to absorb a burst of Ready/Result
// messages from every slot in flight. 256 keeps queueing latency low
// while avoiding spurious context cancellations during gRPC flushes.
const sendChanBufferClient = 256

func newSession(ctx context.Context, cfg Config, stream workerpb.WorkerStream_StreamClient) *session {
	s := &session{
		cfg:        cfg,
		stream:     stream,
		batchCh:    make(chan []*workerpb.Task),
		log:        cfg.Logger,
		ctx:        ctx,
		sendCh:     make(chan *workerpb.WorkerEvent, sendChanBufferClient),
		writerDone: make(chan struct{}),
	}
	go s.writeLoop()
	return s
}

func (s *session) writeLoop() {
	defer close(s.writerDone)
	for ev := range s.sendCh {
		if s.sendErr.Load() != nil {
			continue
		}
		if err := s.stream.Send(ev); err != nil {
			cp := err
			s.sendErr.Store(&cp)
		}
	}
}

func (s *session) closeWriter() {
	close(s.sendCh)
	<-s.writerDone
}

func (s *session) send(ev *workerpb.WorkerEvent) error {
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
		close(s.batchCh)
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
			case s.batchCh <- []*workerpb.Task{e.Task.Task}:
			case <-ctx.Done():
				return nil
			}
		case *workerpb.ServerEvent_TaskBatch:
			if len(e.TaskBatch.Tasks) == 0 {
				continue
			}
			select {
			case s.batchCh <- e.TaskBatch.Tasks:
			case <-ctx.Done():
				return nil
			}
		case *workerpb.ServerEvent_ResultAck,
			*workerpb.ServerEvent_ResultAckBatch,
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
	batchSize := s.cfg.BatchSize
	if batchSize < 1 {
		batchSize = 1
	}
	for {
		if ctx.Err() != nil {
			return
		}
		if err := s.send(&workerpb.WorkerEvent{Event: &workerpb.WorkerEvent_Ready{
			Ready: &workerpb.Ready{
				Commands:     s.cfg.Commands,
				LeaseSeconds: int32(s.cfg.LeaseSeconds),
				Count:        int32(batchSize),
			},
		}}); err != nil {
			s.log.Warn("workerclient: send ready", "err", err)
			return
		}

		select {
		case batch, ok := <-s.batchCh:
			if !ok {
				return
			}
			s.dispatchBatch(ctx, h, batch)
		case <-ctx.Done():
			return
		}
	}
}

// dispatchBatch invokes the handler for every task in the batch and
// coalesces the results into one ResultBatch send when len(batch)>1.
// Splits Result vs Nack vs Abandon per result kind: only "Completed"
// and "Failed" carry through the batch path because BatchSubmit on
// the server only handles Submit. Nack and Abandon are rare under
// load and fall back to per-message sends.
func (s *session) dispatchBatch(ctx context.Context, h Handler, batch []*workerpb.Task) {
	if len(batch) == 1 {
		s.dispatch(ctx, h, batch[0])
		return
	}
	results := make([]*workerpb.Result, 0, len(batch))
	for _, pt := range batch {
		t := protoToTask(pt)
		res := h(ctx, t)
		switch res.kind {
		case resultCompleted:
			var payload []byte
			if res.body != nil {
				if b, err := sonic.Marshal(res.body); err == nil {
					payload = b
				}
			}
			results = append(results, &workerpb.Result{
				TaskId: pt.Id, Status: "COMPLETED", ResultJson: payload,
			})
		case resultFailed:
			results = append(results, &workerpb.Result{
				TaskId: pt.Id, Status: "FAILED", Error: res.err,
			})
		default:
			// Nack / Abandon don't go through BatchSubmit on the server;
			// send per-task so they take the rare-path handler.
			_ = s.applyResult(pt.Id, res)
		}
	}
	if len(results) > 0 {
		if err := s.send(&workerpb.WorkerEvent{Event: &workerpb.WorkerEvent_ResultBatch{
			ResultBatch: &workerpb.ResultBatch{Results: results},
		}}); err != nil {
			s.log.Warn("workerclient: send result batch", "err", err)
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
