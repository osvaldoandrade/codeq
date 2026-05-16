package cluster

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/osvaldoandrade/codeq/internal/cluster/clusterpb"
)

// BloomCache holds the most recent bloom snapshot we've received from
// each peer. The router consults it before issuing remote calls: if a
// peer's bloom says "definitely not", the call is short-circuited to a
// not-found response without burning a gRPC RTT.
//
// All peer filters share the same parameters (expectedItems, fpRate);
// the cluster mandates a uniform bloom shape so snapshot byte arrays
// can be Restored verbatim without reshaping.
type BloomCache struct {
	expectedItems uint64
	fpRate        float64
	peers         sync.Map // nodeID string → *peerEntry
}

type peerEntry struct {
	bloom *Bloom
	seq   uint64
}

// NewBloomCache returns an empty cache pre-sized for the cluster's bloom
// shape. Pass the same expectedItems / fpRate used for the local bloom
// so peer snapshots Restore cleanly.
func NewBloomCache(expectedItems uint64, fpRate float64) *BloomCache {
	return &BloomCache{expectedItems: expectedItems, fpRate: fpRate}
}

// MaybeHas returns true if peerID's last-known bloom indicates the ID
// might be present. If we have no cached bloom for that peer (gossip
// hasn't run yet, or the peer is unreachable), we default to true so
// the router falls back to the actual gRPC call — the bloom is purely
// an optimisation, not a source of truth.
func (c *BloomCache) MaybeHas(peerID, key string) bool {
	v, ok := c.peers.Load(peerID)
	if !ok {
		return true
	}
	pe := v.(*peerEntry)
	return pe.bloom.MaybeHas(key)
}

// Replace stores or replaces the cached bloom for peerID. Skips the
// store if the incoming sequence is older than what we already have.
// New peers get a freshly-allocated bloom with the cache's shape.
func (c *BloomCache) Replace(peerID string, bits []byte, k uint32, count, seq uint64) {
	v, _ := c.peers.LoadOrStore(peerID, &peerEntry{bloom: NewBloom(c.expectedItems, c.fpRate)})
	pe := v.(*peerEntry)
	if seq != 0 && seq <= pe.seq {
		return
	}
	pe.bloom.Restore(bits, k, count, seq)
	pe.seq = seq
}

// Gossiper periodically polls every peer's BloomSnapshot RPC and feeds
// the response back into the cache. One goroutine per Gossiper; it
// stops cleanly when ctx is cancelled.
type Gossiper struct {
	ring     *LocalRing
	pool     *ClientPool
	cache    *BloomCache
	interval time.Duration
	logger   *slog.Logger
}

// NewGossiper wires the bloom-sync goroutine. interval defaults to 1s
// if non-positive; that's slow enough to keep network traffic minimal
// while letting newly enqueued IDs become "claimable from peers" within
// ~1s of being persisted.
func NewGossiper(ring *LocalRing, pool *ClientPool, cache *BloomCache, interval time.Duration, logger *slog.Logger) *Gossiper {
	if interval <= 0 {
		interval = time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Gossiper{
		ring:     ring,
		pool:     pool,
		cache:    cache,
		interval: interval,
		logger:   logger.With("component", "cluster.bloom-gossiper"),
	}
}

// Start launches the gossip loop. Cancel ctx to stop it.
func (g *Gossiper) Start(ctx context.Context) {
	go g.loop(ctx)
}

func (g *Gossiper) loop(ctx context.Context) {
	t := time.NewTicker(g.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.pollAll(ctx)
		}
	}
}

// pollAll fans out a BloomSnapshot RPC to every peer in parallel and
// feeds responses into the cache. Failures are logged and otherwise
// ignored — a stale entry is better than no entry.
func (g *Gossiper) pollAll(ctx context.Context) {
	peers := g.ring.Peers()
	if len(peers) == 0 {
		return
	}
	results := g.pool.CallEach(ctx, peers, func(ctx context.Context, c clusterpb.TaskNodeClient, _ Node) (any, error) {
		return c.BloomSnapshot(ctx, &clusterpb.BloomSnapshotRequest{})
	})
	for _, r := range results {
		if r.Err != nil {
			g.logger.Debug("bloom poll failed", "peer", r.Node.ID, "err", r.Err)
			continue
		}
		snap, ok := r.Value.(*clusterpb.BloomSnapshotResponse)
		if !ok || len(snap.MBits) == 0 {
			continue
		}
		g.cache.Replace(snap.NodeId, snap.MBits, snap.NumHashes, snap.NumItems, snap.Sequence)
	}
}
