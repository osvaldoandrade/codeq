package cluster

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const (
	dlqTestTenant = "tenant-a"
	dlqTestWorker = "w-dlq"
)

// newDLQRouter starts two bufconn nodes and returns a router on node-a.
func newDLQRouter(t *testing.T) (*TaskRouter, *testNode, *testNode) {
	t.Helper()
	a := newTestNode(t, "node-a")
	b := newTestNode(t, "node-b")
	t.Cleanup(a.stop)
	b.stop = sync.OnceFunc(b.stop) // tests may stop node-b early
	t.Cleanup(b.stop)
	pool := poolWithBufnet([]*testNode{a, b})
	t.Cleanup(func() { _ = pool.Close() })
	ring := NewLocalRing(NewRing([]Node{a.node, b.node}), "node-a")
	for i := range ring.nodes {
		ring.nodes[i].GRPCAddr = "passthrough:///" + ring.nodes[i].GRPCAddr
		ring.byID[ring.nodes[i].ID] = ring.nodes[i]
	}
	return NewTaskRouter(a.repo, ring, pool), a, b
}

// taskOn enqueues a task whose ID the ring assigns to node, on that node's
// shard. With dead set it claims and nacks it into the dead-letter queue.
func taskOn(t *testing.T, router *TaskRouter, node *testNode, prefix string, dead bool) string {
	t.Helper()
	ctx := context.Background()
	id := ""
	for i := 0; id == ""; i++ {
		if i > 1000 {
			t.Fatalf("no id hashes to %s", node.node.ID)
		}
		if candidate := fmt.Sprintf("%s-%d", prefix, i); router.ring.Owner(candidate).ID == node.node.ID {
			id = candidate
		}
	}
	if _, _, err := node.repo.EnqueueWithID(ctx, id, domain.CmdGenerateMaster, `{}`, 0, "", 1, "", time.Time{}, dlqTestTenant); err != nil {
		t.Fatalf("enqueue %s: %v", id, err)
	}
	if !dead {
		return id
	}
	got, ok, err := node.repo.Claim(ctx, dlqTestWorker, []domain.Command{domain.CmdGenerateMaster}, 60, 50, 1, dlqTestTenant)
	if err != nil || !ok || got.ID != id {
		t.Fatalf("claim %s: ok=%v err=%v got=%v", id, ok, err, got)
	}
	if _, dlq, err := node.repo.Nack(ctx, id, dlqTestWorker, 0, 1, "boom"); err != nil || !dlq {
		t.Fatalf("nack %s: dlq=%v err=%v", id, dlq, err)
	}
	return id
}

func TestRouterRequeueDLQTaskOnEveryOwner(t *testing.T) {
	ctx := context.Background()
	router, a, b := newDLQRouter(t)
	for _, node := range []*testNode{a, b} {
		id := taskOn(t, router, node, "requeue-"+node.node.ID, true)
		task, err := router.RequeueDLQTask(ctx, id)
		if err != nil || task.ID != id || task.Status != domain.StatusPending || task.Attempts != 0 {
			t.Fatalf("%s: requeue = %+v, %v", node.node.ID, task, err)
		}
		if stored, err := node.repo.Get(ctx, id); err != nil || stored.Status != domain.StatusPending {
			t.Fatalf("%s: owner shard holds %+v, %v", node.node.ID, stored, err)
		}
		if _, err := router.RequeueDLQTask(ctx, id); !errors.Is(err, domain.ErrTaskNotInDLQ) {
			t.Fatalf("%s: second requeue err %v, want ErrTaskNotInDLQ", node.node.ID, err)
		}
	}
	missing := ""
	for i := 0; missing == ""; i++ {
		if id := fmt.Sprintf("missing-%d", i); router.ring.Owner(id).ID == b.node.ID {
			missing = id
		}
	}
	if _, err := router.RequeueDLQTask(ctx, missing); err == nil || err.Error() != "not-found" {
		t.Fatalf("missing remote task: err %v, want not-found", err)
	}
}

func TestRouterDeleteTaskOnPeer(t *testing.T) {
	ctx := context.Background()
	router, _, b := newDLQRouter(t)
	dead := taskOn(t, router, b, "delete-dead", true)
	if err := router.DeleteTask(ctx, dead); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := b.repo.Get(ctx, dead); err == nil {
		t.Fatal("deleted task still on the owner")
	}
	if err := router.DeleteTask(ctx, dead); err == nil || err.Error() != "not-found" {
		t.Fatalf("second delete err %v, want not-found", err)
	}

	leased := taskOn(t, router, b, "delete-leased", false)
	if _, ok, err := b.repo.Claim(ctx, dlqTestWorker, []domain.Command{domain.CmdGenerateMaster}, 60, 50, 3, dlqTestTenant); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err := router.DeleteTask(ctx, leased); !errors.Is(err, domain.ErrTaskInProgress) {
		t.Fatalf("leased task: err %v, want ErrTaskInProgress", err)
	}
}

// RequeueDLQ walks both nodes and splits the limit between them; a node
// that cannot answer fails the call instead of being taken for empty.
func TestRouterRequeueDLQWalksEveryNode(t *testing.T) {
	ctx := context.Background()
	router, a, b := newDLQRouter(t)
	for i := range 2 {
		taskOn(t, router, a, fmt.Sprintf("bulk-a%d", i), true)
		taskOn(t, router, b, fmt.Sprintf("bulk-b%d", i), true)
	}
	res, err := router.RequeueDLQ(ctx, domain.CmdGenerateMaster, dlqTestTenant, 3)
	if err != nil || res.Requeued != 3 || !res.Remaining {
		t.Fatalf("first call %+v, %v; want 3 requeued and remaining", res, err)
	}
	res, err = router.RequeueDLQ(ctx, domain.CmdGenerateMaster, dlqTestTenant, 3)
	if err != nil || res.Requeued != 1 || res.Remaining {
		t.Fatalf("second call %+v, %v; want the last one", res, err)
	}
	for _, node := range []*testNode{a, b} {
		if st, err := node.repo.QueueStats(ctx, domain.CmdGenerateMaster, dlqTestTenant); err != nil || st.DLQ != 0 || st.Ready != 2 {
			t.Fatalf("%s stats %+v, %v", node.node.ID, st, err)
		}
	}

	b.stop()
	if _, err := router.RequeueDLQ(ctx, domain.CmdGenerateMaster, dlqTestTenant, 3); err == nil {
		t.Fatal("bulk requeue with node-b down succeeded; want an error")
	}
}
