// Package producerclient is the Go SDK for the codeq producer streaming
// gRPC API. The high-level entry point is Client.Connect, which opens a
// long-lived bidirectional stream, authenticates once, and returns a
// Session whose Produce method pipelines CreateTask events. Each call
// to Produce blocks only until the matching CreateAck arrives — many
// Produces from different goroutines can be in flight at once.
//
// Phase 3 of the throughput refactor relies on this client to bypass
// the per-call HTTP middleware tax on POST /tasks.
package producerclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/osvaldoandrade/codeq/internal/producer/producerpb"
)

// Config controls Client behavior. Addr and Token are required.
type Config struct {
	// Addr is the gRPC dial target of the codeq producer stream server
	// (e.g. "localhost:9092"). Required.
	Addr string

	// Token is the bearer token presented in Hello. Required.
	Token string

	// DialOptions are forwarded to grpc.NewClient. If empty, the client
	// uses insecure transport credentials — set this for TLS/mTLS.
	DialOptions []grpc.DialOption

	// Logger receives structured info/warn/error events. Defaults to
	// slog.Default().
	Logger *slog.Logger
}

func (c *Config) defaults() {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Client owns the gRPC connection. Use Connect to open a streaming
// Session; one Client can serve many sessions over its lifetime.
type Client struct {
	cfg  Config
	conn *grpc.ClientConn
}

// New dials the server and returns a Client ready for Connect.
func New(cfg Config) (*Client, error) {
	if cfg.Addr == "" {
		return nil, errors.New("producerclient: Addr is required")
	}
	if cfg.Token == "" {
		return nil, errors.New("producerclient: Token is required")
	}
	cfg.defaults()
	opts := cfg.DialOptions
	if len(opts) == 0 {
		opts = []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	}
	conn, err := grpc.NewClient(cfg.Addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("producerclient: dial %s: %w", cfg.Addr, err)
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

// CreateRequest mirrors the REST POST /tasks body. Command and Payload
// are the only required fields; the rest default to zero.
type CreateRequest struct {
	Command        string
	Payload        []byte
	Priority       int
	Webhook        string
	MaxAttempts    int
	IdempotencyKey string
	RunAt          time.Time
	DelaySeconds   int
	TraceParent    string
	TraceState     string
}

// Session is one authenticated stream. Produce on it is safe to call
// from many goroutines concurrently. Close to release reader goroutine
// and stream resources.
type Session struct {
	cli     *Client
	stream  producerpb.ProducerStream_StreamClient
	cancel  context.CancelFunc
	closed  atomic.Bool
	closeMu sync.Mutex

	tenantID string
	subject  string

	sendMu  sync.Mutex // serializes stream.Send
	seq     atomic.Uint64
	pending sync.Map // seq → chan ackResult

	readErr   error
	readErrMu sync.Mutex
	readDone  chan struct{}
}

// ackResult is what slot goroutines receive from the reader.
type ackResult struct {
	taskID string
	err    error
}

// Connect opens the stream, completes the Hello handshake, and starts
// the reader goroutine. The returned Session lives until the parent
// context is cancelled or Close is called.
func (c *Client) Connect(ctx context.Context) (*Session, error) {
	// Use a derived context so we can cancel the stream on Close
	// independently of the caller's parent.
	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := producerpb.NewProducerStreamClient(c.conn).Stream(streamCtx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("producerclient: open stream: %w", err)
	}
	sess := &Session{
		cli:      c,
		stream:   stream,
		cancel:   cancel,
		readDone: make(chan struct{}),
	}
	if err := sess.handshake(); err != nil {
		cancel()
		return nil, err
	}
	go sess.readLoop()
	return sess, nil
}

func (s *Session) send(ev *producerpb.ProducerEvent) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.stream.Send(ev)
}

func (s *Session) handshake() error {
	if err := s.send(&producerpb.ProducerEvent{Event: &producerpb.ProducerEvent_Hello{
		Hello: &producerpb.Hello{Token: s.cli.cfg.Token},
	}}); err != nil {
		return fmt.Errorf("producerclient: send hello: %w", err)
	}
	ack, err := s.stream.Recv()
	if err != nil {
		return fmt.Errorf("producerclient: recv helloack: %w", err)
	}
	switch e := ack.Event.(type) {
	case *producerpb.ServerEvent_HelloAck:
		s.tenantID = e.HelloAck.TenantId
		s.subject = e.HelloAck.Subject
		s.cli.cfg.Logger.Debug("producerclient: hello ok",
			"tenantId", s.tenantID, "subject", s.subject)
		return nil
	case *producerpb.ServerEvent_Error:
		return fmt.Errorf("producerclient: server rejected hello: %s (%s)",
			e.Error.Message, e.Error.Code)
	default:
		return fmt.Errorf("producerclient: unexpected hello reply: %T", ack.Event)
	}
}

func (s *Session) readLoop() {
	defer close(s.readDone)
	for {
		ev, err := s.stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
			}
			s.readErrMu.Lock()
			s.readErr = err
			s.readErrMu.Unlock()
			// Fan out the error to any pending sequences so callers don't
			// block forever waiting for an ack that will never come.
			s.pending.Range(func(k, v any) bool {
				ch := v.(chan ackResult)
				select {
				case ch <- ackResult{err: deadStreamErr(err)}:
				default:
				}
				return true
			})
			return
		}
		switch e := ev.Event.(type) {
		case *producerpb.ServerEvent_CreateAck:
			s.deliver(e.CreateAck)
		case *producerpb.ServerEvent_Error:
			s.cli.cfg.Logger.Warn("producerclient: server error event",
				"code", e.Error.Code, "msg", e.Error.Message)
		case *producerpb.ServerEvent_HelloAck:
			// Re-HelloAck shouldn't happen; ignore.
		default:
			s.cli.cfg.Logger.Warn("producerclient: unknown server event",
				"type", fmt.Sprintf("%T", ev.Event))
		}
	}
}

func deadStreamErr(err error) error {
	if err == nil {
		return errors.New("producerclient: stream closed")
	}
	return fmt.Errorf("producerclient: stream closed: %w", err)
}

func (s *Session) deliver(ack *producerpb.CreateAck) {
	v, ok := s.pending.LoadAndDelete(ack.Seq)
	if !ok {
		s.cli.cfg.Logger.Warn("producerclient: ack for unknown seq", "seq", ack.Seq)
		return
	}
	ch := v.(chan ackResult)
	res := ackResult{taskID: ack.TaskId}
	if !ack.Ok {
		res.err = errors.New(ack.ErrorMessage)
	}
	select {
	case ch <- res:
	default:
		// Buffered channel cap=1, should never be full unless caller
		// already moved on (e.g. ctx cancelled) and channel was drained.
	}
}

// Produce sends one CreateTask and blocks until the server acks (or
// ctx fires, or the underlying stream dies). Returns the assigned task
// id on success. Safe to call from many goroutines concurrently.
func (s *Session) Produce(ctx context.Context, req CreateRequest) (string, error) {
	if s.closed.Load() {
		return "", errors.New("producerclient: session closed")
	}
	if err := s.peekReadErr(); err != nil {
		return "", err
	}

	seq := s.seq.Add(1)
	ch := make(chan ackResult, 1)
	s.pending.Store(seq, ch)

	var runAt *timestamppb.Timestamp
	if !req.RunAt.IsZero() {
		runAt = timestamppb.New(req.RunAt)
	}
	ev := &producerpb.ProducerEvent{Event: &producerpb.ProducerEvent_Create{
		Create: &producerpb.CreateTask{
			Seq:            seq,
			Command:        req.Command,
			Payload:        req.Payload,
			Priority:       int32(req.Priority),
			Webhook:        req.Webhook,
			MaxAttempts:    int32(req.MaxAttempts),
			IdempotencyKey: req.IdempotencyKey,
			RunAt:          runAt,
			DelaySeconds:   int32(req.DelaySeconds),
			TraceParent:    req.TraceParent,
			TraceState:     req.TraceState,
		},
	}}
	if err := s.send(ev); err != nil {
		s.pending.Delete(seq)
		return "", fmt.Errorf("producerclient: send create: %w", err)
	}

	select {
	case res := <-ch:
		if res.err != nil {
			return "", res.err
		}
		return res.taskID, nil
	case <-ctx.Done():
		s.pending.Delete(seq)
		return "", ctx.Err()
	}
}

// peekReadErr returns a non-nil error if the reader goroutine has
// already terminated, so callers don't bother sending into a dead stream.
func (s *Session) peekReadErr() error {
	s.readErrMu.Lock()
	defer s.readErrMu.Unlock()
	if s.readErr != nil {
		return deadStreamErr(s.readErr)
	}
	select {
	case <-s.readDone:
		return errors.New("producerclient: stream closed")
	default:
		return nil
	}
}

// TenantID returns the tenant the server resolved this session to.
func (s *Session) TenantID() string { return s.tenantID }

// Subject returns the JWT subject the server resolved this session to.
func (s *Session) Subject() string { return s.subject }

// Close cancels the stream, releases the reader goroutine, and unblocks
// any pending Produce calls with an error. Safe to call multiple times.
func (s *Session) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed.Swap(true) {
		return nil
	}
	// CloseSend tells the server we're done writing — it gets a clean
	// EOF on its Recv loop. cancel() then propagates the close to the
	// reader so it exits as well.
	_ = s.stream.CloseSend()
	s.cancel()
	<-s.readDone
	return nil
}
