package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const progressWorker = "w-progress"

// newProgressRouter starts two bufconn nodes and returns a router on node-a.
func newProgressRouter(t *testing.T) (*TaskRouter, *testNode, *testNode) {
	t.Helper()
	a := newTestNode(t, "node-a")
	b := newTestNode(t, "node-b")
	t.Cleanup(a.stop)
	t.Cleanup(b.stop)
	ring := NewLocalRing(NewRing([]Node{a.node, b.node}), "node-a")
	pool := poolWithBufnet([]*testNode{a, b})
	t.Cleanup(func() { _ = pool.Close() })
	for i := range ring.nodes {
		ring.nodes[i].GRPCAddr = "passthrough:///" + ring.nodes[i].GRPCAddr
		ring.byID[ring.nodes[i].ID] = ring.nodes[i]
	}
	return NewTaskRouter(a.repo, ring, pool), a, b
}

// idOwnedBy returns the first prefix-N id the ring assigns to node.
func idOwnedBy(t *testing.T, router *TaskRouter, node *testNode, prefix string) string {
	t.Helper()
	for i := range 1000 {
		if id := fmt.Sprintf("%s-%d", prefix, i); router.ring.Owner(id).ID == node.node.ID {
			return id
		}
	}
	t.Fatalf("no id hashes to %s", node.node.ID)
	return ""
}

// claimedOn enqueues an id owned by node on that node's shard and, when
// claim is true, leases it to progressWorker there.
func claimedOn(t *testing.T, router *TaskRouter, node *testNode, prefix string, claim bool) string {
	t.Helper()
	ctx := context.Background()
	id := idOwnedBy(t, router, node, prefix)
	if _, _, err := node.repo.EnqueueWithID(ctx, id, domain.CmdGenerateMaster, `{}`, 0, "", 3, "", "", time.Time{}, ""); err != nil {
		t.Fatalf("enqueue %s: %v", id, err)
	}
	if !claim {
		return id
	}
	got, ok, err := node.repo.Claim(ctx, progressWorker, []domain.Command{domain.CmdGenerateMaster}, 60, 50, 3, "")
	if err != nil || !ok || got.ID != id {
		t.Fatalf("claim %s: ok=%v err=%v got=%v", id, ok, err, got)
	}
	return id
}

func TestRouterProgressOnPeerOwnedTask(t *testing.T) {
	ctx := context.Background()
	router, _, b := newProgressRouter(t)
	id := claimedOn(t, router, b, "remote", true)

	if err := router.Progress(ctx, id, progressWorker, json.RawMessage(`{"rows":500}`)); err != nil {
		t.Fatalf("remote progress: %v", err)
	}
	stored, err := b.repo.Get(ctx, id)
	if err != nil || string(stored.Progress) != `{"rows":500}` {
		t.Fatalf("owner shard progress %q err %v", stored.Progress, err)
	}
	read, err := router.Get(ctx, id)
	if err != nil || string(read.Progress) != `{"rows":500}` {
		t.Fatalf("progress read through the router %q err %v", read.Progress, err)
	}

	pending := claimedOn(t, router, b, "remote-pending", false)
	for name, tc := range map[string]struct{ id, worker, want string }{
		"other worker": {id, "someone-else", "not-owner"},
		"pending task": {pending, "", "not-in-progress"},
		"unknown task": {idOwnedBy(t, router, b, "remote-missing"), progressWorker, "not-found"},
	} {
		if err := router.Progress(ctx, tc.id, tc.worker, json.RawMessage(`1`)); err == nil || err.Error() != tc.want {
			t.Fatalf("%s: err %v, want %s", name, err, tc.want)
		}
	}
}

func TestRouterProgressOnLocalTask(t *testing.T) {
	ctx := context.Background()
	router, a, _ := newProgressRouter(t)
	id := claimedOn(t, router, a, "local", true)
	if err := router.Progress(ctx, id, progressWorker, json.RawMessage(`7`)); err != nil {
		t.Fatalf("local progress: %v", err)
	}
	got, err := a.repo.Get(ctx, id)
	if err != nil || string(got.Progress) != `7` {
		t.Fatalf("local shard progress %q err %v", got.Progress, err)
	}
}
