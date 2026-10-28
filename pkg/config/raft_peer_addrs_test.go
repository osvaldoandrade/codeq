package config

import (
	"strings"
	"testing"
)

func raftPeersConfig(peers map[string]string) *Config {
	return &Config{
		Env:                 "dev",
		WorkerAudience:      "codeq-worker",
		PersistenceProvider: "pebble",
		Raft: RaftConfig{
			Enabled:    true,
			SelfID:     "codeq-0",
			BindAddr:   ":7000",
			MuxEnabled: true,
			Peers:      peers,
		},
	}
}

// The agent renders exactly this shape for the CODE MASTER voters.
func TestValidate_RaftPeersAcceptsPerVoterNames(t *testing.T) {
	cfg := raftPeersConfig(map[string]string{
		"codeq-0": "codeq-0.codecloud-queue.svc.cluster.local:7000",
		"codeq-1": "codeq-1.codecloud-queue.svc.cluster.local:7000",
		"codeq-2": "codeq-2.codecloud-queue.svc.cluster.local:7000",
	})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("per-voter peer names rejected: %v", err)
	}
}

func TestValidate_RaftPeersRejectsSharedOrWildcardAddresses(t *testing.T) {
	cases := map[string]struct {
		peers map[string]string
		want  string
	}{
		"wildcard":   {map[string]string{"codeq-0": "[::]:7000", "codeq-1": "codeq-1.ns:7000"}, "wildcard"},
		"empty host": {map[string]string{"codeq-0": ":7000", "codeq-1": "codeq-1.ns:7000"}, "no host"},
		"ipv4 any":   {map[string]string{"codeq-0": "0.0.0.0:7000", "codeq-1": "codeq-1.ns:7000"}, "wildcard"},
		"shared":     {map[string]string{"codeq-0": "codeq.ns:7000", "codeq-1": "CODEQ.ns:7000"}, "share address"},
		"no port":    {map[string]string{"codeq-0": "codeq-0.ns", "codeq-1": "codeq-1.ns:7000"}, "host:port"},
		"bad port":   {map[string]string{"codeq-0": "codeq-0.ns:0", "codeq-1": "codeq-1.ns:7000"}, "invalid port"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := raftPeersConfig(tc.peers).Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// A single voter has no competing candidate, so its address is not checked.
func TestValidate_RaftSingleVoterKeepsWildcardPeer(t *testing.T) {
	if err := raftPeersConfig(map[string]string{"codeq-0": ":7000"}).Validate(); err != nil {
		t.Fatalf("single voter rejected: %v", err)
	}
}
