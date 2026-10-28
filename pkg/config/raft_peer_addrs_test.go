package config

import (
	"strings"
	"testing"
)

const (
	peerTestEnv       = "dev"
	peerTestAudience  = "codeq-worker"
	peerTestSelf      = "codeq-0"
	peerTestOther     = "codeq-1"
	peerTestBind      = ":7000"
	peerTestOtherAddr = "codeq-1.ns:7000"
	peerTestWildcard  = "wildcard"
)

func raftPeersConfig(peers map[string]string) *Config {
	return &Config{
		Env:                 peerTestEnv,
		WorkerAudience:      peerTestAudience,
		PersistenceProvider: providerPebble,
		Raft: RaftConfig{
			Enabled:    true,
			SelfID:     peerTestSelf,
			BindAddr:   peerTestBind,
			MuxEnabled: true,
			Peers:      peers,
		},
	}
}

// The agent renders exactly this shape for the CODE MASTER voters.
func TestValidate_RaftPeersAcceptsPerVoterNames(t *testing.T) {
	cfg := raftPeersConfig(map[string]string{
		peerTestSelf:  "codeq-0.codecloud-queue.svc.cluster.local:7000",
		peerTestOther: "codeq-1.codecloud-queue.svc.cluster.local:7000",
		"codeq-2":     "codeq-2.codecloud-queue.svc.cluster.local:7000",
	})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("per-voter peer names rejected: %v", err)
	}
}

func TestValidate_RaftPeersRejectsSharedOrWildcardAddresses(t *testing.T) {
	cases := map[string]struct {
		self string
		want string
	}{
		"ipv6 any":   {"[::]:7000", peerTestWildcard},
		"empty host": {peerTestBind, "no host"},
		"ipv4 any":   {"0.0.0.0:7000", peerTestWildcard},
		"shared":     {"CODEQ-1.ns:7000", "share address"},
		"no port":    {"codeq-0.ns", "host:port"},
		"bad port":   {"codeq-0.ns:0", "invalid port"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			peers := map[string]string{peerTestSelf: tc.self, peerTestOther: peerTestOtherAddr}
			err := raftPeersConfig(peers).Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// A single voter has no competing candidate, so its address is not checked.
func TestValidate_RaftSingleVoterKeepsWildcardPeer(t *testing.T) {
	if err := raftPeersConfig(map[string]string{peerTestSelf: peerTestBind}).Validate(); err != nil {
		t.Fatalf("single voter rejected: %v", err)
	}
}
