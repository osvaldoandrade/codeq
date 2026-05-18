package config

import (
	"testing"
)

func TestLoadConfigOptional_RaftEnvOverrides(t *testing.T) {
	t.Setenv("RAFT_ENABLED", "true")
	t.Setenv("RAFT_SELF_ID", "node-7")
	t.Setenv("RAFT_BIND_ADDR", ":7100")
	t.Setenv("RAFT_BOOTSTRAP", "true")
	t.Setenv("RAFT_PEERS", "node-1=host1:7000,node-2=host2:7000,node-7=host7:7100")
	t.Setenv("RAFT_HEARTBEAT_MS", "250")
	t.Setenv("RAFT_ELECTION_MS", "500")
	t.Setenv("RAFT_LEADER_LEASE_MS", "200")
	t.Setenv("RAFT_COMMIT_MS", "20")
	t.Setenv("RAFT_APPLY_TIMEOUT_SECONDS", "8")

	cfg, err := LoadConfigOptional("")
	if err != nil {
		t.Fatalf("LoadConfigOptional: %v", err)
	}
	if !cfg.Raft.Enabled {
		t.Errorf("Raft.Enabled: want true, got false")
	}
	if cfg.Raft.SelfID != "node-7" {
		t.Errorf("Raft.SelfID: want node-7, got %q", cfg.Raft.SelfID)
	}
	if cfg.Raft.BindAddr != ":7100" {
		t.Errorf("Raft.BindAddr: want :7100, got %q", cfg.Raft.BindAddr)
	}
	if !cfg.Raft.Bootstrap {
		t.Errorf("Raft.Bootstrap: want true, got false")
	}
	wantPeers := map[string]string{
		"node-1": "host1:7000",
		"node-2": "host2:7000",
		"node-7": "host7:7100",
	}
	if len(cfg.Raft.Peers) != len(wantPeers) {
		t.Fatalf("Raft.Peers: want %d entries, got %d (%+v)", len(wantPeers), len(cfg.Raft.Peers), cfg.Raft.Peers)
	}
	for id, addr := range wantPeers {
		if got := cfg.Raft.Peers[id]; got != addr {
			t.Errorf("Raft.Peers[%s]: want %q, got %q", id, addr, got)
		}
	}
	if cfg.Raft.HeartbeatMS != 250 {
		t.Errorf("Raft.HeartbeatMS: want 250, got %d", cfg.Raft.HeartbeatMS)
	}
	if cfg.Raft.ElectionMS != 500 {
		t.Errorf("Raft.ElectionMS: want 500, got %d", cfg.Raft.ElectionMS)
	}
	if cfg.Raft.LeaderLeaseMS != 200 {
		t.Errorf("Raft.LeaderLeaseMS: want 200, got %d", cfg.Raft.LeaderLeaseMS)
	}
	if cfg.Raft.CommitMS != 20 {
		t.Errorf("Raft.CommitMS: want 20, got %d", cfg.Raft.CommitMS)
	}
	if cfg.Raft.ApplyTimeoutSeconds != 8 {
		t.Errorf("Raft.ApplyTimeoutSeconds: want 8, got %d", cfg.Raft.ApplyTimeoutSeconds)
	}
}

func TestLoadConfigOptional_RaftDisabledByDefault(t *testing.T) {
	cfg, err := LoadConfigOptional("")
	if err != nil {
		t.Fatalf("LoadConfigOptional: %v", err)
	}
	if cfg.Raft.Enabled {
		t.Errorf("default Raft.Enabled: want false, got true")
	}
}

func TestLoadConfigOptional_RaftPeersIgnoresMalformed(t *testing.T) {
	t.Setenv("RAFT_PEERS", "node-1=ok:7000,malformed-no-equals,=missing-id,trailing-empty=,node-2=ok:7001,")
	cfg, err := LoadConfigOptional("")
	if err != nil {
		t.Fatalf("LoadConfigOptional: %v", err)
	}
	if got := cfg.Raft.Peers["node-1"]; got != "ok:7000" {
		t.Errorf("node-1: want ok:7000, got %q", got)
	}
	if got := cfg.Raft.Peers["node-2"]; got != "ok:7001" {
		t.Errorf("node-2: want ok:7001, got %q", got)
	}
	// Malformed entries are silently dropped.
	if len(cfg.Raft.Peers) != 2 {
		t.Errorf("len(Peers): want 2 (malformed dropped), got %d: %+v", len(cfg.Raft.Peers), cfg.Raft.Peers)
	}
}
