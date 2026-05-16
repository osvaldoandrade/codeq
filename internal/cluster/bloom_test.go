package cluster

import (
	"fmt"
	"testing"
)

func TestBloomAddMaybeHas(t *testing.T) {
	b := NewBloom(10_000, 0.001)

	added := []string{"task-1", "task-2", "task-99", "uuid-abc"}
	for _, k := range added {
		b.Add(k)
	}
	for _, k := range added {
		if !b.MaybeHas(k) {
			t.Fatalf("MaybeHas(%q) returned false for an added key", k)
		}
	}
}

func TestBloomFPRateWithinTarget(t *testing.T) {
	const expectedItems = 10_000
	b := NewBloom(expectedItems, 0.001)

	for i := 0; i < expectedItems; i++ {
		b.Add(fmt.Sprintf("present-%d", i))
	}

	const probes = 50_000
	fps := 0
	for i := 0; i < probes; i++ {
		if b.MaybeHas(fmt.Sprintf("absent-%d", i)) {
			fps++
		}
	}
	rate := float64(fps) / float64(probes)
	// FP target is 0.001; allow 5x headroom for tail noise on small N.
	if rate > 0.005 {
		t.Fatalf("FP rate %.4f exceeds 5x target (0.001)", rate)
	}
}

func TestBloomSnapshotRestoreRoundtrip(t *testing.T) {
	src := NewBloom(1_000, 0.001)
	for i := 0; i < 200; i++ {
		src.Add(fmt.Sprintf("id-%d", i))
	}
	bits, k, count, seq := src.Snapshot()

	dst := NewBloom(1_000, 0.001)
	dst.Restore(bits, k, count, seq)
	for i := 0; i < 200; i++ {
		if !dst.MaybeHas(fmt.Sprintf("id-%d", i)) {
			t.Fatalf("restore lost item %d", i)
		}
	}
	if dst.Sequence() != seq {
		t.Fatalf("seq not restored: got %d want %d", dst.Sequence(), seq)
	}
}

func TestBloomCacheDefaultsToMaybe(t *testing.T) {
	c := NewBloomCache(1000, 0.001)
	if !c.MaybeHas("unknown-peer", "some-id") {
		t.Fatal("cache without an entry must default to MaybeHas=true so the router falls back to gRPC")
	}
}

func TestBloomCacheShortCircuitsAfterReplace(t *testing.T) {
	c := NewBloomCache(1000, 0.001)
	src := NewBloom(1_000, 0.001)
	src.Add("alpha")
	src.Add("beta")
	bits, k, count, seq := src.Snapshot()
	c.Replace("peer-1", bits, k, count, seq)

	if !c.MaybeHas("peer-1", "alpha") {
		t.Fatal("expected MaybeHas=true for inserted key")
	}
	if c.MaybeHas("peer-1", "gamma-never-added") {
		// This may occasionally false-positive at the FP rate; we re-run
		// with a few different keys to make the test stable.
		var fps int
		for i := 0; i < 100; i++ {
			if c.MaybeHas("peer-1", fmt.Sprintf("never-added-%d", i)) {
				fps++
			}
		}
		if fps > 5 { // 0.001 * 100 = 0.1 expected, generous bound
			t.Fatalf("too many FPs: %d/100 absent keys flagged present", fps)
		}
	}
}
