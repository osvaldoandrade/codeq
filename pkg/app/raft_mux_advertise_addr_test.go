package app

import (
	"net"
	"testing"

	"github.com/osvaldoandrade/codeq/pkg/config"
)

const (
	advertiseTestSelf     = "codeq-0"
	advertiseTestBind     = ":7000"
	advertiseTestSelfPeer = "codeq-0.ns:7000"
)

func TestMuxAdvertiseAddr(t *testing.T) {
	cases := map[string]struct {
		peers map[string]string
		want  string
	}{
		"no peers":                    {nil, ""},
		"single voter wildcard entry": {map[string]string{advertiseTestSelf: advertiseTestBind}, ""},
		"single voter named entry":    {map[string]string{advertiseTestSelf: advertiseTestSelfPeer}, ""},
		"voter group": {map[string]string{
			advertiseTestSelf: advertiseTestSelfPeer,
			"codeq-1":         "codeq-1.ns:7000",
			"codeq-2":         "codeq-2.ns:7000",
		}, advertiseTestSelfPeer},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := &config.Config{Raft: config.RaftConfig{SelfID: advertiseTestSelf, BindAddr: advertiseTestBind, MuxEnabled: true, Peers: tc.peers}}
			if got := muxAdvertiseAddr(cfg); got != tc.want {
				t.Fatalf("muxAdvertiseAddr = %q, want %q", got, tc.want)
			}
		})
	}
}

// A single voter whose own peers entry is the wildcard bind address passed
// Config.Validate before and must still start: it keeps advertising the
// listener address.
func TestOpenMuxAcceptorSingleVoterWildcardPeer(t *testing.T) {
	cfg := &config.Config{Raft: config.RaftConfig{
		Enabled: true, SelfID: advertiseTestSelf, BindAddr: net.JoinHostPort("127.0.0.1", "0"), MuxEnabled: true,
		Peers: map[string]string{advertiseTestSelf: advertiseTestBind},
	}}
	acc, err := openMuxAcceptor(cfg)
	if err != nil {
		t.Fatalf("openMuxAcceptor: %v", err)
	}
	defer acc.Close()
	if got, want := acc.AdvertiseAddr().String(), acc.Addr().String(); got != want {
		t.Fatalf("single voter advertises %q, want its listener %q", got, want)
	}
}
