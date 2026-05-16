package cluster

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/osvaldoandrade/codeq/internal/cluster/clusterpb"
	pebblerepo "github.com/osvaldoandrade/codeq/internal/repository/pebble"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// testNode wires a Pebble shard, a gRPC server, and a bufconn listener.
// Returned tuple gives the cluster.Node config + a Dialer the pool can
// use, plus the underlying repo for direct assertions.
type testNode struct {
	node   Node
	dialer func(context.Context, string) (net.Conn, error)
	repo   *pebblerepo.TaskRepository
	stop   func()
}

func newTestNode(t *testing.T, id string) *testNode {
	t.Helper()
	dir := t.TempDir()
	db, err := pebblerepo.Open(pebblerepo.Options{Path: dir})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	repo := pebblerepo.NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	results := pebblerepo.NewResultRepository(db, time.UTC)

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	clusterpb.RegisterTaskNodeServer(gs, &Server{NodeID: id, Tasks: repo, Results: results})
	go func() { _ = gs.Serve(lis) }()

	return &testNode{
		node:   Node{ID: id, GRPCAddr: "bufnet-" + id},
		dialer: func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) },
		repo:   repo,
		stop: func() {
			gs.Stop()
			_ = db.Close()
		},
	}
}

// poolWithBufnet wires each node's bufconn dialer into a single ClientPool
// by dispatching on the GRPCAddr (which we picked as "bufnet-<id>").
func poolWithBufnet(nodes []*testNode) *ClientPool {
	dialer := func(ctx context.Context, addr string) (net.Conn, error) {
		for _, n := range nodes {
			if n.node.GRPCAddr == addr {
				return n.dialer(ctx, addr)
			}
		}
		return nil, &net.OpError{Op: "dial", Err: net.UnknownNetworkError(addr)}
	}
	return NewClientPool(
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		// Pass-through resolver means grpc treats the target string literally
		// and hands it to our context dialer — no DNS attempted.
		grpc.WithAuthority("bufnet"),
	)
}

func TestRouterEnqueueRoutesByHash(t *testing.T) {
	ctx := context.Background()
	a := newTestNode(t, "node-a")
	b := newTestNode(t, "node-b")
	t.Cleanup(a.stop)
	t.Cleanup(b.stop)

	ring := NewLocalRing(NewRing([]Node{a.node, b.node}), "node-a")
	pool := poolWithBufnet([]*testNode{a, b})
	defer pool.Close()

	// Force the pool to use "passthrough://<addr>" form so the context dialer is invoked.
	// Wrap each Node's GRPCAddr in "passthrough:///<addr>" via a small adapter:
	// (the simplest path is to mutate Node.GRPCAddr in the ring's view since
	// ClientPool.Client uses node.GRPCAddr directly).
	for i := range ring.Ring.nodes {
		ring.Ring.nodes[i].GRPCAddr = "passthrough:///" + ring.Ring.nodes[i].GRPCAddr
		ring.Ring.byID[ring.Ring.nodes[i].ID] = ring.Ring.nodes[i]
	}

	router := NewTaskRouter(a.repo, ring, pool)

	// Enqueue 200 tasks via node A; expect roughly half to land on node B's
	// local Pebble shard (since IDs are random UUIDs and the ring has 2 nodes).
	const N = 200
	for i := 0; i < N; i++ {
		if _, err := router.Enqueue(ctx, domain.CmdGenerateMaster, `{"x":1}`, 5, "", 3, "", time.Time{}, ""); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	// Count tasks actually persisted on each node.
	aCount, _ := a.repo.PendingLength(ctx, domain.CmdGenerateMaster)
	bCount, _ := b.repo.PendingLength(ctx, domain.CmdGenerateMaster)
	if aCount+bCount != int64(N) {
		t.Fatalf("expected %d total tasks across nodes, got a=%d b=%d", N, aCount, bCount)
	}
	if aCount == 0 || bCount == 0 {
		t.Fatalf("expected both nodes to receive tasks via routing, got a=%d b=%d", aCount, bCount)
	}
}

func TestRouterClaimScatterGather(t *testing.T) {
	ctx := context.Background()
	a := newTestNode(t, "node-a")
	b := newTestNode(t, "node-b")
	t.Cleanup(a.stop)
	t.Cleanup(b.stop)

	ring := NewLocalRing(NewRing([]Node{a.node, b.node}), "node-a")
	pool := poolWithBufnet([]*testNode{a, b})
	defer pool.Close()
	for i := range ring.Ring.nodes {
		ring.Ring.nodes[i].GRPCAddr = "passthrough:///" + ring.Ring.nodes[i].GRPCAddr
		ring.Ring.byID[ring.Ring.nodes[i].ID] = ring.Ring.nodes[i]
	}

	router := NewTaskRouter(a.repo, ring, pool)

	// Put a task DIRECTLY on node B's repo with an ID that hashes there
	// (we don't need the hash decision here — we just want a task that lives
	// on B but is claimed via the router on A).
	_, _, err := b.repo.EnqueueWithID(ctx, "b-only-task", domain.CmdGenerateMaster, `{"a":1}`, 5, "", 3, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("seed B: %v", err)
	}

	claimed, ok, err := router.Claim(ctx, "w-x", []domain.Command{domain.CmdGenerateMaster}, 60, 50, 3, "")
	if err != nil {
		t.Fatalf("router.Claim: %v", err)
	}
	if !ok || claimed == nil {
		t.Fatalf("expected scatter-gather to pull task from peer, got empty")
	}
	if claimed.ID != "b-only-task" {
		t.Fatalf("expected b-only-task, got %s", claimed.ID)
	}
}
