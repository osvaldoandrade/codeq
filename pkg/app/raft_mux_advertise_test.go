package app

import (
	"strconv"
	"testing"
	"time"
)

// TestRaft_Mux_WildcardBindAdvertisesOwnPeerAddress runs the production
// shape: every voter binds the wildcard Raft address and reaches its peers
// by their own names. Each voter must announce its own peer address. Before
// the fix every voter announced the shared listener address ("[::]:7000" in
// production), hashicorp/raft took competing candidates for one and granted
// a second vote in the same term, and one term could elect two leaders.
func TestRaft_Mux_WildcardBindAdvertisesOwnPeerAddress(t *testing.T) {
	const numShards = 2
	ports := pickThreeFreePorts(t)
	ids := make([]string, len(ports))
	peers := map[string]string{}
	for i := range ports {
		ids[i] = "node-" + strconv.Itoa(i+1)
		peers[ids[i]] = "127.0.0.1:" + ports[i]
	}
	nodes := make([]*raftTestNode, len(ids))
	for i := len(ids) - 1; i >= 0; i-- {
		nodes[i] = startMuxRaftNodeBound(t, ids[i], ":"+ports[i], peers, numShards, i == 0)
	}
	t.Cleanup(func() {
		for _, n := range nodes {
			_ = n.shutdown()
		}
	})

	deadline := time.Now().Add(15 * time.Second)
	for {
		agreed := true
		for shard := 0; shard < numShards && agreed; shard++ {
			leader := ""
			for _, n := range nodes {
				st := getStatus(t, n.server.URL)
				if len(st.Groups) != numShards || !st.Groups[shard].HasLeader {
					agreed = false
					break
				}
				if leader == "" {
					leader = st.Groups[shard].LeaderID
				} else if st.Groups[shard].LeaderID != leader {
					agreed = false
					break
				}
			}
		}
		if agreed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("voters did not agree on one leader per shard")
		}
		time.Sleep(50 * time.Millisecond)
	}

	for _, n := range nodes {
		st := getStatus(t, n.server.URL)
		for _, g := range st.Groups {
			want := peers[g.LeaderID]
			if want == "" {
				t.Fatalf("%s shard %d: unknown leader %q", n.id, g.ShardIdx, g.LeaderID)
			}
			if g.LeaderAddr != want {
				t.Fatalf("%s shard %d records leader %s at %q, want its own peer address %q",
					n.id, g.ShardIdx, g.LeaderID, g.LeaderAddr, want)
			}
		}
	}
}
