package app

import "testing"

func TestBindAddrForShard(t *testing.T) {
	cases := []struct {
		name    string
		base    string
		shard   int
		want    string
		wantErr bool
	}{
		{"shard0", "127.0.0.1:7000", 0, "127.0.0.1:7000", false},
		{"shard3", "127.0.0.1:7000", 3, "127.0.0.1:7003", false},
		{"ipv6", "[::1]:7000", 2, "[::1]:7002", false},
		{"hostname", "localhost:9000", 1, "localhost:9001", false},
		{"port0_shard0_ok", "127.0.0.1:0", 0, "127.0.0.1:0", false},
		{"port0_shardN_err", "127.0.0.1:0", 1, "", true},
		{"empty", "", 0, "", true},
		{"no_port", "127.0.0.1", 1, "", true},
		{"bad_port", "127.0.0.1:abc", 1, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := bindAddrForShard(tc.base, tc.shard)
			if tc.wantErr {
				if err == nil {
					t.Errorf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected err: %v", err)
				return
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPeersForShard(t *testing.T) {
	base := map[string]string{
		"node-1": "127.0.0.1:7000",
		"node-2": "127.0.0.1:7100",
		"node-3": "127.0.0.1:7200",
	}
	got, err := peersForShard(base, 2)
	if err != nil {
		t.Fatalf("peersForShard: %v", err)
	}
	want := map[string]string{
		"node-1": "127.0.0.1:7002",
		"node-2": "127.0.0.1:7102",
		"node-3": "127.0.0.1:7202",
	}
	for id, addr := range want {
		if got[id] != addr {
			t.Errorf("peer %s: got %q, want %q", id, got[id], addr)
		}
	}
}

func TestPeersForShard_EmptyReturnsNil(t *testing.T) {
	got, err := peersForShard(nil, 0)
	if err != nil {
		t.Errorf("err: %v", err)
	}
	if got != nil {
		t.Errorf("want nil, got %v", got)
	}
}
