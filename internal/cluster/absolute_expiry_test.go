package cluster

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/internal/core/queuetopic"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

type absoluteTopicReader struct{}

func (absoluteTopicReader) Get(_ context.Context, tenant, name string) (queuetopic.Topic, error) {
	return queuetopic.Topic{TenantID: tenant, TopicName: name, Policy: queuetopic.Policy{AbsoluteExpiry: true, RetentionSeconds: 3600}}, nil
}

func TestAbsoluteExpiryCrossNodeCreateClaimAndReplay(t *testing.T) {
	a, b := newTestNode(t, "node-a", true), newTestNode(t, "node-b", true)
	t.Cleanup(a.stop)
	t.Cleanup(b.stop)
	a.db.SetTopicPolicyReader(absoluteTopicReader{})
	b.db.SetTopicPolicyReader(absoluteTopicReader{})
	pool := poolWithBufnet([]*testNode{a, b})
	defer pool.Close()
	ring := NewLocalRing(NewRing([]Node{a.node, b.node}), a.node.ID)
	for i := range ring.nodes {
		ring.nodes[i].GRPCAddr = "passthrough:///" + ring.nodes[i].GRPCAddr
		ring.byID[ring.nodes[i].ID] = ring.nodes[i]
	}
	router := NewTaskRouter(a.repo, ring, pool)
	var id string
	for i := 0; i < 1000; i++ {
		candidate := fmt.Sprintf("logs-cross-%d", i)
		if ring.Owner(candidate).ID == b.node.ID {
			id = candidate
			break
		}
	}
	if id == "" {
		t.Fatal("no cross-node identity")
	}
	deadline := time.Now().Add(time.Hour - time.Minute).UTC()
	ctx := domain.WithTaskExpiry(context.Background(), deadline)
	first, err := router.Enqueue(ctx, "cf-logs", `{"batch":1}`, 0, "", 100, "", "", id, time.Time{}, "tenant-a")
	if err != nil || first == nil || first.ExpiresAt == nil || !first.ExpiresAt.Equal(deadline) {
		t.Fatalf("forwarded create=%+v %v", first, err)
	}
	if local, _ := a.repo.Get(context.Background(), id); local != nil {
		t.Fatal("forwarded task also created locally")
	}
	replay, err := router.Enqueue(domain.WithTaskExpiry(context.Background(), deadline.Add(time.Second)), "cf-logs", `{"batch":1}`, 0, "", 100, "", "", id, time.Time{}, "tenant-a")
	if err != nil || replay == nil || !replay.CreatedAt.Equal(first.CreatedAt) || !replay.ExpiresAt.Equal(deadline) {
		t.Fatalf("forwarded replay=%+v %v", replay, err)
	}
	claimed, ok, err := router.Claim(context.Background(), "worker", []domain.Command{"cf-logs"}, 60, 100, 100, "tenant-a")
	if err != nil || !ok || claimed.ID != id || claimed.ExpiresAt == nil || !claimed.ExpiresAt.Equal(deadline) || !claimed.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("forwarded claim=%+v %v %v", claimed, ok, err)
	}
}
