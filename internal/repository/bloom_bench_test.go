package repository

import (
	"fmt"
	"math"
	"testing"
	"unsafe"
)

// ---------- Core operation benchmarks ----------

func BenchmarkBloom_Add(b *testing.B) {
	bf := newIdempotencyBloom(1_000_000, 0.01, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bf.Add(fmt.Sprintf("key-%d", i))
	}
}

func BenchmarkBloom_MaybeHas_Positive(b *testing.B) {
	bf := newIdempotencyBloom(1_000_000, 0.01, 0)
	const preload = 10_000
	for i := 0; i < preload; i++ {
		bf.Add(fmt.Sprintf("key-%d", i))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bf.MaybeHas(fmt.Sprintf("key-%d", i%preload))
	}
}

func BenchmarkBloom_MaybeHas_Negative(b *testing.B) {
	bf := newIdempotencyBloom(1_000_000, 0.01, 0)
	const preload = 10_000
	for i := 0; i < preload; i++ {
		bf.Add(fmt.Sprintf("key-%d", i))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bf.MaybeHas(fmt.Sprintf("miss-%d", i))
	}
}

// ---------- Memory footprint benchmarks ----------

func BenchmarkBloom_MemoryFootprint(b *testing.B) {
	capacities := []uint64{100_000, 500_000, 1_000_000, 5_000_000}
	for _, n := range capacities {
		b.Run(fmt.Sprintf("cap_%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				bf := newBloomFilter(n, 0.01)
				// Prevent compiler from optimizing away the allocation.
				if bf.mBits == 0 {
					b.Fatal("unexpected zero mBits")
				}
			}
		})
	}
}

// ---------- False positive rate validation ----------

func TestBloom_FalsePositiveRate(t *testing.T) {
	const (
		capacity = 100_000
		fpTarget = 0.01
		// Allow up to 2× the theoretical FP rate to account for variance.
		fpMax  = fpTarget * 2
		probes = 100_000
	)

	bf := newBloomFilter(capacity, fpTarget)

	// Insert exactly `capacity` distinct keys.
	for i := 0; i < capacity; i++ {
		bf.Add(fmt.Sprintf("inserted-%d", i))
	}

	// Probe with keys that were never inserted.
	falsePositives := 0
	for i := 0; i < probes; i++ {
		if bf.MaybeHas(fmt.Sprintf("absent-%d", i)) {
			falsePositives++
		}
	}

	observedRate := float64(falsePositives) / float64(probes)
	t.Logf("observed FP rate: %.4f%% (%d / %d)", observedRate*100, falsePositives, probes)

	if observedRate > fpMax {
		t.Errorf("false-positive rate %.4f exceeds 2× target (%.4f)", observedRate, fpMax)
	}
}

// TestBloom_MemorySizeConsistency verifies the theoretical bit-array size
// formula matches the allocated words slice.
func TestBloom_MemorySizeConsistency(t *testing.T) {
	cases := []struct {
		n      uint64
		fpRate float64
	}{
		{100_000, 0.01},
		{1_000_000, 0.01},
		{2_000_000, 1e-12},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("n=%d_fp=%.0e", tc.n, tc.fpRate), func(t *testing.T) {
			bf := newBloomFilter(tc.n, tc.fpRate)

			ln2 := math.Ln2
			expectedM := uint64(math.Ceil(-(float64(tc.n) * math.Log(tc.fpRate)) / (ln2 * ln2)))
			if expectedM < 64 {
				expectedM = 64
			}
			expectedWords := (expectedM + 63) / 64

			if uint64(len(bf.words)) != expectedWords {
				t.Errorf("words slice length %d, want %d (mBits=%d)", len(bf.words), expectedWords, bf.mBits)
			}

			bytesUsed := uint64(len(bf.words)) * uint64(unsafe.Sizeof(bf.words[0]))
			t.Logf("n=%d fpRate=%.0e → mBits=%d k=%d words=%d memBytes=%d",
				tc.n, tc.fpRate, bf.mBits, bf.k, len(bf.words), bytesUsed)
		})
	}
}
