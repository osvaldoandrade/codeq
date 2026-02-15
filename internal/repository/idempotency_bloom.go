package repository

import (
	"hash/maphash"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// idempotencyBloom is a small, in-process rotating Bloom filter used to avoid
// negative Redis lookups on the idempotency enqueue fast-path.
//
// It is intentionally best-effort: false positives only force a fallback to the
// Redis check; false negatives are acceptable (SETNX still guarantees dedupe).
type idempotencyBloom struct {
	n           uint64
	fpRate      float64
	rotateEvery time.Duration

	rotateMu sync.Mutex
	state    atomic.Value // *idempotencyBloomState
}

type idempotencyBloomState struct {
	curr       *bloomFilter
	prev       *bloomFilter
	nextRotate time.Time
}

func newIdempotencyBloom(n uint64, fpRate float64, rotateEvery time.Duration) *idempotencyBloom {
	if n == 0 {
		n = 1_000_000
	}
	if fpRate <= 0 || fpRate >= 1 {
		fpRate = 0.01
	}
	if rotateEvery <= 0 {
		rotateEvery = 30 * time.Minute
	}

	b := &idempotencyBloom{
		n:           n,
		fpRate:      fpRate,
		rotateEvery: rotateEvery,
	}

	now := time.Now()
	st := &idempotencyBloomState{
		curr:       newBloomFilter(n, fpRate),
		prev:       newBloomFilter(n, fpRate),
		nextRotate: now.Add(rotateEvery),
	}
	b.state.Store(st)
	return b
}

func (b *idempotencyBloom) rotateIfNeeded(now time.Time) {
	st, _ := b.state.Load().(*idempotencyBloomState)
	if st == nil || now.Before(st.nextRotate) {
		return
	}

	b.rotateMu.Lock()
	defer b.rotateMu.Unlock()

	st, _ = b.state.Load().(*idempotencyBloomState)
	if st == nil || now.Before(st.nextRotate) {
		return
	}

	next := &idempotencyBloomState{
		curr:       newBloomFilter(b.n, b.fpRate),
		prev:       st.curr,
		nextRotate: now.Add(b.rotateEvery),
	}
	b.state.Store(next)
}

func (b *idempotencyBloom) MaybeHas(key string) bool {
	if key == "" {
		return true
	}
	now := time.Now()
	b.rotateIfNeeded(now)

	st, _ := b.state.Load().(*idempotencyBloomState)
	if st == nil {
		return true
	}
	if st.curr != nil && st.curr.MaybeHas(key) {
		return true
	}
	if st.prev != nil && st.prev.MaybeHas(key) {
		return true
	}
	return false
}

func (b *idempotencyBloom) Add(key string) {
	if key == "" {
		return
	}
	now := time.Now()
	b.rotateIfNeeded(now)

	st, _ := b.state.Load().(*idempotencyBloomState)
	if st == nil || st.curr == nil {
		return
	}
	st.curr.Add(key)
}

type bloomFilter struct {
	mBits uint64
	k     uint64

	seed1 maphash.Seed
	seed2 maphash.Seed

	words []atomic.Uint64
}

func newBloomFilter(n uint64, fpRate float64) *bloomFilter {
	// Standard bloom params:
	// m = -(n * ln(p)) / (ln(2)^2)
	// k = (m/n) * ln(2)
	ln2 := math.Ln2
	m := uint64(math.Ceil(-(float64(n) * math.Log(fpRate)) / (ln2 * ln2)))
	if m < 64 {
		m = 64
	}
	k := uint64(math.Ceil((float64(m) / float64(n)) * ln2))
	if k < 1 {
		k = 1
	}
	words := make([]atomic.Uint64, (m+63)/64)
	return &bloomFilter{
		mBits: m,
		k:     k,
		seed1: maphash.MakeSeed(),
		seed2: maphash.MakeSeed(),
		words: words,
	}
}

func (f *bloomFilter) hashPair(key string) (uint64, uint64) {
	var h1 maphash.Hash
	h1.SetSeed(f.seed1)
	h1.WriteString(key)
	a := h1.Sum64()

	var h2 maphash.Hash
	h2.SetSeed(f.seed2)
	h2.WriteString(key)
	b := h2.Sum64()
	if b == 0 {
		// Avoid degenerate double-hash where all probes are the same.
		b = 0x9e3779b97f4a7c15
	}
	return a, b
}

func (f *bloomFilter) MaybeHas(key string) bool {
	if key == "" {
		return true
	}
	a, b := f.hashPair(key)
	for i := uint64(0); i < f.k; i++ {
		pos := (a + i*b) % f.mBits
		w := pos >> 6
		bit := pos & 63
		mask := uint64(1) << bit
		if f.words[w].Load()&mask == 0 {
			return false
		}
	}
	return true
}

func (f *bloomFilter) Add(key string) {
	if key == "" {
		return
	}
	a, b := f.hashPair(key)
	for i := uint64(0); i < f.k; i++ {
		pos := (a + i*b) % f.mBits
		w := pos >> 6
		bit := pos & 63
		mask := uint64(1) << bit

		for {
			old := f.words[w].Load()
			if old&mask != 0 {
				break
			}
			if f.words[w].CompareAndSwap(old, old|mask) {
				break
			}
		}
	}
}
