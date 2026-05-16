package cluster

import (
	"context"
	"fmt"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/osvaldoandrade/codeq/internal/cluster/clusterpb"
)

// ClientPool holds one lazily-dialed gRPC connection per peer. Calls
// share connections via gRPC's built-in multiplexing — we don't create
// a connection per RPC. Pool is goroutine-safe.
type ClientPool struct {
	mu    sync.Mutex
	dials map[string]*grpc.ClientConn // node ID → conn

	// dialOpts allow tests/embedders to inject credentials, interceptors,
	// or a bufconn dialer. Default is insecure (cluster-internal network).
	dialOpts []grpc.DialOption
}

// NewClientPool returns an empty pool. Pass extra grpc.DialOptions if you
// need TLS, interceptors, etc.
func NewClientPool(opts ...grpc.DialOption) *ClientPool {
	if len(opts) == 0 {
		opts = []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	}
	return &ClientPool{
		dials:    make(map[string]*grpc.ClientConn),
		dialOpts: opts,
	}
}

// Client returns a TaskNode stub for the given node. The first call dials
// and caches the connection; subsequent calls reuse it.
func (p *ClientPool) Client(node Node) (clusterpb.TaskNodeClient, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.dials[node.ID]; ok {
		return clusterpb.NewTaskNodeClient(c), nil
	}
	conn, err := grpc.NewClient(node.GRPCAddr, p.dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("dial %s (%s): %w", node.ID, node.GRPCAddr, err)
	}
	p.dials[node.ID] = conn
	return clusterpb.NewTaskNodeClient(conn), nil
}

// Close tears down every connection in the pool. Safe to call multiple
// times; subsequent Close() returns nil after the first.
func (p *ClientPool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var firstErr error
	for id, c := range p.dials {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("close %s: %w", id, err)
		}
		delete(p.dials, id)
	}
	return firstErr
}

// CallEach runs fn against every node in nodes in parallel and waits for
// all responses. Useful for scatter-gather (Claim, AdminQueues, bloom
// gossip). The returned slice has the same order as nodes.
func (p *ClientPool) CallEach(ctx context.Context, nodes []Node, fn func(ctx context.Context, c clusterpb.TaskNodeClient, n Node) (any, error)) []NodeResult {
	results := make([]NodeResult, len(nodes))
	var wg sync.WaitGroup
	wg.Add(len(nodes))
	for i, n := range nodes {
		i, n := i, n
		go func() {
			defer wg.Done()
			c, err := p.Client(n)
			if err != nil {
				results[i] = NodeResult{Node: n, Err: err}
				return
			}
			v, err := fn(ctx, c, n)
			results[i] = NodeResult{Node: n, Value: v, Err: err}
		}()
	}
	wg.Wait()
	return results
}

// NodeResult is the per-node outcome of a scatter-gather call.
type NodeResult struct {
	Node  Node
	Value any
	Err   error
}
