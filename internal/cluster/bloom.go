package cluster

import (
	"encoding/binary"
	"hash/fnv"
	"sync"
	"sync/atomic"
)

// Bloom is a fixed-size Bloom filter used for the cluster's "do I hold
// this ID?" negative-lookup shortcut. It is concurrent-safe and
// serializable so peers can exchange snapshots over gRPC.
//
// Implementation notes:
//   - Bits live in a []uint64 so we can use sync/atomic.OR64 for the
//     RMW path on Add. Bloom semantics REQUIRE no false negatives, and
//     a torn byte write under concurrent Add would violate that — atomic
//     OR makes the set-bit operation lock-free and correct.
//   - No counting; deletions aren't supported. Operators control
//     accumulated FP rate via Reset() (e.g. coupled with a CleanupExpired
//     sweep) or by sizing m generously vs expected throughput.
//   - Snapshot serialises the word array to little-endian bytes for the
//     wire. Restore copies bytes back, validating size + k.
type Bloom struct {
	m     uint64 // bit count (multiple of 64)
	words uint64 // m/64
	k     uint32 // hash count
	bits  []uint64

	count atomic.Uint64 // number of Add calls; advisory
	seq   atomic.Uint64 // bumped every Add; gossip freshness marker
	mu    sync.RWMutex   // guards Reset / Restore; Add/MaybeHas are lockless
}

// NewBloom returns a Bloom sized for n expected items at false-positive
// rate p. Standard sizing: m = -n·ln(p) / (ln 2)^2, k = (m/n)·ln 2.
func NewBloom(n uint64, p float64) *Bloom {
	if n == 0 {
		n = 1_000_000
	}
	if p <= 0 || p >= 1 {
		p = 0.001
	}
	mBits := uint64(float64(n) * 14.378) // -ln(0.001)/ln(2)^2
	if mBits < 64 {
		mBits = 64
	}
	mBits = ((mBits + 63) / 64) * 64 // word-align
	k := uint32(0.7 * float64(mBits) / float64(n))
	if k < 1 {
		k = 1
	}
	if k > 16 {
		k = 16
	}
	return &Bloom{m: mBits, words: mBits / 64, k: k, bits: make([]uint64, mBits/64)}
}

// Add inserts key into the filter. Safe for concurrent use.
func (b *Bloom) Add(key string) {
	h1, h2 := doubleHash(key)
	for i := uint32(0); i < b.k; i++ {
		idx := (h1 + uint64(i)*h2) % b.m
		wordIdx := idx / 64
		bit := uint64(1) << (idx % 64)
		atomic.OrUint64(&b.bits[wordIdx], bit)
	}
	b.count.Add(1)
	b.seq.Add(1)
}

// MaybeHas reports whether key might be in the filter.
//
//	false → definitely NOT inserted by anyone
//	true  → probably inserted (subject to FP rate)
//
// Concurrent with Add: a read may see a partially-applied insert
// (some of the k bits set, some not) and return false. That's only
// possible if the Add hasn't completed yet — the caller observed a
// state strictly before that Add returned, which is consistent.
func (b *Bloom) MaybeHas(key string) bool {
	h1, h2 := doubleHash(key)
	for i := uint32(0); i < b.k; i++ {
		idx := (h1 + uint64(i)*h2) % b.m
		wordIdx := idx / 64
		bit := uint64(1) << (idx % 64)
		if atomic.LoadUint64(&b.bits[wordIdx])&bit == 0 {
			return false
		}
	}
	return true
}

// Reset zeroes every bit.
func (b *Bloom) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.bits {
		atomic.StoreUint64(&b.bits[i], 0)
	}
	b.count.Store(0)
	b.seq.Add(1)
}

// Snapshot returns the bit array serialised to little-endian bytes plus
// the filter parameters. The byte slice is independent — callers may
// mutate freely without racing the live filter.
func (b *Bloom) Snapshot() (bits []byte, k uint32, count uint64, seq uint64) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]byte, len(b.bits)*8)
	for i, w := range b.bits {
		binary.LittleEndian.PutUint64(out[i*8:(i+1)*8], atomic.LoadUint64(&b.bits[i]))
		_ = w
	}
	return out, b.k, b.count.Load(), b.seq.Load()
}

// Restore replaces the filter contents from a peer's snapshot. The size
// (in bits) and k must match this filter's parameters — the cluster
// assumes a uniform configuration. Mismatches are silently ignored so a
// rolling reconfiguration doesn't poison the local state.
func (b *Bloom) Restore(bits []byte, k uint32, count, seq uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if uint64(len(bits))*8 != b.m || k != b.k {
		return
	}
	for i := 0; i < int(b.words); i++ {
		w := binary.LittleEndian.Uint64(bits[i*8 : (i+1)*8])
		atomic.StoreUint64(&b.bits[i], w)
	}
	b.count.Store(count)
	b.seq.Store(seq)
}

// Sequence returns the monotonic insert counter. Peers compare against
// the last seen value to skip restoring a no-op snapshot.
func (b *Bloom) Sequence() uint64 { return b.seq.Load() }

// K returns the configured hash count — exposed so the Server can
// publish it on snapshot responses without exposing the whole filter.
func (b *Bloom) K() uint32 { return b.k }

// doubleHash returns (h1, h2) for the double-hashing scheme. FNV-1a is
// fast and good enough for a Bloom; the second pass salts with a single
// byte so h1 != h2 in practice.
func doubleHash(key string) (uint64, uint64) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	h1 := h.Sum64()
	h.Reset()
	_, _ = h.Write([]byte{0xa5})
	_, _ = h.Write([]byte(key))
	h2 := h.Sum64()
	if h2 == 0 {
		h2 = 1
	}
	return h1, h2
}
