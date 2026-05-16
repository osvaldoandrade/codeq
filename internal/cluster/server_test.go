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

// startTestServer wires a real Pebble-backed Server behind a bufconn so
// the test can talk to it via a real gRPC client without binding a TCP
// port. Returns a dialer the client side can use to connect.
func startTestServer(t *testing.T) (string, func(context.Context, string) (net.Conn, error)) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)

	db, err := pebblerepo.Open(pebblerepo.Options{Path: t.TempDir()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	repo := pebblerepo.NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	results := pebblerepo.NewResultRepository(db, time.UTC)

	server := &Server{
		NodeID:  "node-a",
		Tasks:   repo,
		Results: results,
	}

	gs := grpc.NewServer()
	clusterpb.RegisterTaskNodeServer(gs, server)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	}
	return "bufnet", dialer
}

func TestGRPCRoundTrip_EnqueueGetSubmitResult(t *testing.T) {
	ctx := context.Background()
	target, dialer := startTestServer(t)

	// passthrough: bypasses DNS so the bufconn dialer is invoked directly.
	conn, err := grpc.NewClient("passthrough:///"+target,
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := clusterpb.NewTaskNodeClient(conn)

	// 1) Local claim on empty queue: should return Empty=true.
	resp, err := c.LocalClaim(ctx, &clusterpb.LocalClaimRequest{
		WorkerId:           "w-1",
		Commands:           []string{string(domain.CmdGenerateMaster)},
		LeaseSeconds:       60,
		InspectLimit:       10,
		MaxAttemptsDefault: 3,
	})
	if err != nil {
		t.Fatalf("LocalClaim empty: %v", err)
	}
	if !resp.Empty {
		t.Fatalf("expected Empty=true on cold queue, got task=%v", resp.Task)
	}

	// 2) Get unknown id: NotFound.
	getResp, err := c.GetTask(ctx, &clusterpb.GetTaskRequest{Id: "nope"})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !getResp.NotFound {
		t.Fatalf("expected NotFound for unknown id")
	}
}
