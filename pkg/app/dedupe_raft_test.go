package app

import (
	"context"
	"net/http"
	"testing"
	"time"
)

const dedupeRaftCommand = "SYNC_CHANNEL"

// TestRaftDeduplicationSurvivesLeaderTransfer proves the deduplication
// mapping is replicated with the task: after the leader that wrote it stops,
// a create of the same key on the new leader still joins the waiting task.
func TestRaftDeduplicationSurvivesLeaderTransfer(t *testing.T) {
	ports := pickThreeFreePorts(t)
	peers := map[string]string{
		topicNodeOne:   "127.0.0.1:" + ports[0],
		topicNodeTwo:   "127.0.0.1:" + ports[1],
		topicNodeThree: "127.0.0.1:" + ports[2],
	}
	nodes := make([]*raftTestNode, 3)
	for i, id := range []string{topicNodeOne, topicNodeTwo, topicNodeThree} {
		nodes[i] = startRaftNode(t, id, peers, i == 0)
	}
	t.Cleanup(func() {
		for _, node := range nodes {
			if node != nil && !node.closed.Load() {
				_ = node.shutdown()
			}
		}
	})

	leader, _ := waitForLeader(t, nodes, 5*time.Second)
	first := createDeduplicated(t, leader, "channel-7")
	if again := createDeduplicated(t, leader, "channel-7"); again != first {
		t.Fatalf("create on the leader returned %s, want waiting task %s", again, first)
	}

	if err := leader.shutdown(); err != nil {
		t.Fatalf("leader shutdown: %v", err)
	}
	survivors := make([]*raftTestNode, 0, 2)
	for _, node := range nodes {
		if node != leader {
			survivors = append(survivors, node)
		}
	}
	newLeader, _ := waitForLeader(t, survivors, 5*time.Second)
	if got := createDeduplicated(t, newLeader, "channel-7"); got != first {
		t.Fatalf("create on the new leader returned %s, want replicated waiting task %s", got, first)
	}
	if other := createDeduplicated(t, newLeader, "channel-8"); other == first {
		t.Fatal("a different key joined the task of channel-7")
	}
}

func createDeduplicated(t *testing.T, node *raftTestNode, key string) string {
	t.Helper()
	var task struct {
		ID string `json:"id"`
	}
	body := map[string]any{"command": dedupeRaftCommand, "payload": map[string]string{"channel": key}, "deduplicationKey": key}
	status, raw := doJSON(t, context.Background(), http.MethodPost, node.server.URL+"/v1/codeq/tasks", "dev-token", body, &task)
	if status != http.StatusAccepted || task.ID == "" {
		t.Fatalf("create %s on %s: status %d body %s", key, node.id, status, raw)
	}
	return task.ID
}
