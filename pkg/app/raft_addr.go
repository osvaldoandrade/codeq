package app

import (
	"fmt"
	"net"
	"strconv"
)

// bindAddrForShard derives a per-shard listen address from a base
// "host:port" by adding shardIdx to the port. Multi-raft (M2) opens
// one raft group per Pebble shard; each group needs its own listening
// socket. Convention: base BindAddr is for shard 0, +1 for shard 1, …
//
// Port 0 (ephemeral) is only valid for shardIdx==0 — without a known
// base port we can't predict the offsets, which breaks the peer
// configuration of the other shards. Tests that need ephemeral ports
// pre-grab N free ports and pass them explicitly via Raft.Peers.
func bindAddrForShard(baseAddr string, shardIdx int) (string, error) {
	if baseAddr == "" {
		return "", fmt.Errorf("bindAddrForShard: empty base address")
	}
	host, portStr, err := net.SplitHostPort(baseAddr)
	if err != nil {
		return "", fmt.Errorf("bindAddrForShard: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", fmt.Errorf("bindAddrForShard: invalid port %q: %w", portStr, err)
	}
	if port == 0 {
		if shardIdx > 0 {
			return "", fmt.Errorf("bindAddrForShard: port 0 only supported with shardIdx=0 (got %d)", shardIdx)
		}
		return baseAddr, nil
	}
	return net.JoinHostPort(host, strconv.Itoa(port+shardIdx)), nil
}

// peersForShard derives the per-shard peer map by applying
// bindAddrForShard to every peer's base address. Returns a fresh map.
func peersForShard(basePeers map[string]string, shardIdx int) (map[string]string, error) {
	if len(basePeers) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(basePeers))
	for id, addr := range basePeers {
		shardAddr, err := bindAddrForShard(addr, shardIdx)
		if err != nil {
			return nil, fmt.Errorf("peersForShard: peer %s: %w", id, err)
		}
		out[id] = shardAddr
	}
	return out, nil
}
