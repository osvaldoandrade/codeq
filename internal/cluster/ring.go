// Package cluster implements the codeq-to-codeq routing layer: a static
// node membership, a consistent-hash ring for ID→owner resolution, gRPC
// clients to talk to peers, and (in a follow-up) bloom-based negative
// lookup shortcuts.
//
// Membership model: every node in the cluster knows the full list of
// nodes at startup (config). Reconfiguration requires a rolling restart;
// dynamic membership (raft / gossip) is out of scope here and would
// layer on top.
package cluster

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"
	"sync"
)

// Node describes one peer in the cluster.
type Node struct {
	ID       string // stable identifier; appears in keys, logs, metrics
	GRPCAddr string // host:port for the internal gRPC server
}

// Ring is a consistent-hash ring with virtual nodes. ID→Node lookup is
// O(log V) where V is the total number of vnodes (numVirtualNodes ×
// |nodes|). Thread-safe for concurrent Owner / All calls; mutation is
// not supported after construction (config is static).
type Ring struct {
	nodes []Node            // sorted by ID for stable iteration
	byID  map[string]Node   // O(1) lookup by node ID

	vhashes []uint64         // sorted virtual-node hash positions
	vowner  map[uint64]string // vhash → node ID

	// vnodes determines load smoothness. 128 virtual nodes per real node
	// keeps standard-deviation of share well under 5% for typical N≤16.
	vnodes int
}

// NewRing builds a ring from the given nodes. Panics if nodes is empty
// (caller's responsibility to validate first).
func NewRing(nodes []Node) *Ring {
	if len(nodes) == 0 {
		panic("cluster.NewRing: nodes must not be empty")
	}
	// 256 virtual replicas per real node keeps the standard deviation of
	// share under ~5% for N in [3, 16] (verified empirically by the
	// distribution test). Higher counts buy marginal smoothness at
	// linear startup cost; the ring itself is queried with O(log V).
	const vnodes = 256

	r := &Ring{
		nodes:  make([]Node, len(nodes)),
		byID:   make(map[string]Node, len(nodes)),
		vowner: make(map[uint64]string, len(nodes)*vnodes),
		vnodes: vnodes,
	}
	copy(r.nodes, nodes)
	sort.Slice(r.nodes, func(i, j int) bool { return r.nodes[i].ID < r.nodes[j].ID })

	for _, n := range r.nodes {
		r.byID[n.ID] = n
		for v := 0; v < vnodes; v++ {
			h := vhash(n.ID, v)
			r.vhashes = append(r.vhashes, h)
			r.vowner[h] = n.ID
		}
	}
	sort.Slice(r.vhashes, func(i, j int) bool { return r.vhashes[i] < r.vhashes[j] })
	return r
}

// Owner returns the node that owns the given key (typically a task ID).
// The same key always resolves to the same node for a given Ring instance.
func (r *Ring) Owner(key string) Node {
	h := keyHash(key)
	// First vhash >= h; wraps around to the smallest if past the end.
	idx := sort.Search(len(r.vhashes), func(i int) bool { return r.vhashes[i] >= h })
	if idx == len(r.vhashes) {
		idx = 0
	}
	return r.byID[r.vowner[r.vhashes[idx]]]
}

// All returns every node in the ring in deterministic order. Used by
// scatter-gather paths (Claim, AdminQueues, BloomGossip target list).
func (r *Ring) All() []Node {
	out := make([]Node, len(r.nodes))
	copy(out, r.nodes)
	return out
}

// Node returns the node with the given ID; ok=false if unknown.
func (r *Ring) Node(id string) (Node, bool) {
	n, ok := r.byID[id]
	return n, ok
}

// Size returns the number of nodes (NOT vnodes).
func (r *Ring) Size() int { return len(r.nodes) }

// vhash composes a stable hash for the v-th virtual instance of nodeID.
// We use SHA-256 (truncated to 64 bits) because plain FNV-1a over
// "nodeID || smallInteger" inputs produces visibly clustered outputs —
// neighbouring v values share most of the hash state, so a few nodes
// end up dominating large arcs of the ring. SHA's avalanche property
// fixes this. Cost is paid once at startup.
func vhash(nodeID string, v int) uint64 {
	var input [64]byte
	n := copy(input[:], nodeID)
	input[n] = '#'
	// Decimal-encode v so each virtual replica is its own distinct string.
	tail := strconvAppendInt(input[:n+1], int64(v))
	sum := sha256.Sum256(tail)
	return binary.BigEndian.Uint64(sum[:8])
}

func keyHash(key string) uint64 {
	sum := sha256.Sum256([]byte(key))
	return binary.BigEndian.Uint64(sum[:8])
}

// strconvAppendInt avoids importing strconv into the hot ring path while
// staying allocation-free for the small v values we encounter.
func strconvAppendInt(prefix []byte, n int64) []byte {
	if n == 0 {
		return append(prefix, '0')
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return append(prefix, buf[i:]...)
}

// ---------------- runtime helpers ----------------

// LocalRing pairs a Ring with the local node's identity so handlers can
// quickly check "do I own this id?".
type LocalRing struct {
	*Ring
	selfID string
	mu     sync.Mutex // reserved for future hot-reload
}

// NewLocalRing wraps a Ring with the local node identity. selfID must
// match one of the nodes in the ring.
func NewLocalRing(ring *Ring, selfID string) *LocalRing {
	if _, ok := ring.Node(selfID); !ok {
		panic("cluster.NewLocalRing: selfID " + selfID + " not present in ring")
	}
	return &LocalRing{Ring: ring, selfID: selfID}
}

// SelfID returns the local node's ID.
func (l *LocalRing) SelfID() string { return l.selfID }

// IsLocal reports whether the local node owns the given key.
func (l *LocalRing) IsLocal(key string) bool {
	return l.Owner(key).ID == l.selfID
}

// Peers returns every node in the ring EXCEPT the local one.
func (l *LocalRing) Peers() []Node {
	all := l.All()
	out := make([]Node, 0, len(all)-1)
	for _, n := range all {
		if n.ID != l.selfID {
			out = append(out, n)
		}
	}
	return out
}
