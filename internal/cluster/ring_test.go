package cluster

import (
	"fmt"
	"math"
	"testing"
)

func TestRingOwnerStable(t *testing.T) {
	r := NewRing([]Node{
		{ID: "a", GRPCAddr: "a:9000"},
		{ID: "b", GRPCAddr: "b:9000"},
		{ID: "c", GRPCAddr: "c:9000"},
	})
	o1 := r.Owner("task-42")
	o2 := r.Owner("task-42")
	if o1.ID != o2.ID {
		t.Fatalf("Owner not stable: %s vs %s", o1.ID, o2.ID)
	}
}

func TestRingDistributionEvenness(t *testing.T) {
	nodes := []Node{{ID: "n1"}, {ID: "n2"}, {ID: "n3"}, {ID: "n4"}}
	r := NewRing(nodes)
	const N = 100_000
	counts := make(map[string]int, len(nodes))
	for i := 0; i < N; i++ {
		counts[r.Owner(fmt.Sprintf("task-%d", i)).ID]++
	}
	avg := float64(N) / float64(len(nodes))
	for id, c := range counts {
		diff := math.Abs(float64(c)-avg) / avg
		if diff > 0.10 {
			t.Errorf("node %s share %.4f off by %.2f%% (>10%%)", id, float64(c)/float64(N), diff*100)
		}
	}
}

func TestRingMinimalReshuffleOnAdd(t *testing.T) {
	before := NewRing([]Node{{ID: "n1"}, {ID: "n2"}, {ID: "n3"}})
	after := NewRing([]Node{{ID: "n1"}, {ID: "n2"}, {ID: "n3"}, {ID: "n4"}})
	const N = 10_000
	moved := 0
	for i := 0; i < N; i++ {
		k := fmt.Sprintf("task-%d", i)
		if before.Owner(k).ID != after.Owner(k).ID {
			moved++
		}
	}
	// With one new node out of four, ideal reshuffle is ~25%. Allow 30%.
	pct := float64(moved) / float64(N)
	if pct > 0.30 {
		t.Fatalf("too many keys moved: %.2f%% (expected ~25%%)", pct*100)
	}
}

func TestLocalRingIsLocal(t *testing.T) {
	r := NewRing([]Node{{ID: "a"}, {ID: "b"}, {ID: "c"}})
	lr := NewLocalRing(r, "b")
	// "task-42" deterministically resolves to some node; only b should match locally.
	owner := r.Owner("task-42").ID
	if lr.IsLocal("task-42") != (owner == "b") {
		t.Fatalf("IsLocal disagrees with Owner")
	}
}
