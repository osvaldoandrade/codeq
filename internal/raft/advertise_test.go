package raft

import (
	"io"
	"net"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
)

func TestValidateAdvertiseAddr(t *testing.T) {
	cases := []struct {
		addr string
		ok   bool
	}{
		{"codeq-0.codecloud-queue.svc.cluster.local:7000", true},
		{"10.1.2.3:7000", true},
		{"[fd00::1]:7000", true},
		{"[::]:7000", false},
		{":7000", false},
		{"0.0.0.0:7000", false},
		{"codeq-0:0", false},
		{"codeq-0:http", false},
		{"codeq-0", false},
		{"", false},
	}
	for _, tc := range cases {
		if err := ValidateAdvertiseAddr(tc.addr); (err == nil) != tc.ok {
			t.Errorf("ValidateAdvertiseAddr(%q) = %v, want ok=%v", tc.addr, err, tc.ok)
		}
	}
}

// The transport advertises its StreamLayer's Addr in every RPC header and
// hashicorp/raft persists and compares votes by those bytes. A voter bound to
// the wildcard listener must therefore advertise its own peer address.
func TestMuxWildcardBindAdvertisesOwnPeerAddress(t *testing.T) {
	const advertise = "codeq-1.codecloud-queue.svc.cluster.local:7000"
	acc, err := NewMuxAcceptorAdvertising("[::]:0", advertise, io.Discard)
	if err != nil {
		t.Fatalf("NewMuxAcceptorAdvertising: %v", err)
	}
	defer acc.Close()
	if ip := acc.Addr().(*net.TCPAddr).IP; !ip.IsUnspecified() {
		t.Fatalf("listener should stay on the wildcard address, got %s", acc.Addr())
	}
	for _, group := range []uint32{0, 1} {
		sl, err := acc.RegisterGroup(group)
		if err != nil {
			t.Fatalf("RegisterGroup(%d): %v", group, err)
		}
		if got := sl.Addr().String(); got != advertise {
			t.Fatalf("group %d StreamLayer.Addr() = %q, want %q", group, got, advertise)
		}
		trans := hraft.NewNetworkTransport(sl, 1, time.Second, io.Discard)
		if got := string(trans.LocalAddr()); got != advertise {
			t.Fatalf("group %d transport advertises %q, want %q", group, got, advertise)
		}
	}
}

func TestMuxWithoutAdvertiseKeepsListenerAddress(t *testing.T) {
	acc, err := NewMuxAcceptor("127.0.0.1:0", io.Discard)
	if err != nil {
		t.Fatalf("NewMuxAcceptor: %v", err)
	}
	defer acc.Close()
	sl, err := acc.RegisterGroup(0)
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}
	if got, want := sl.Addr().String(), acc.Addr().String(); got != want {
		t.Fatalf("StreamLayer.Addr() = %q, want listener %q", got, want)
	}
}

func TestMuxRejectsWildcardAdvertiseBeforeListening(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	bind := l.Addr().String()
	_ = l.Close()
	if _, err := NewMuxAcceptorAdvertising(bind, "[::]:7000", io.Discard); err == nil {
		t.Fatal("wildcard advertise address accepted")
	}
	// The rejected call must not have kept the port.
	l, err = net.Listen("tcp", bind)
	if err != nil {
		t.Fatalf("port %s still held after rejection: %v", bind, err)
	}
	_ = l.Close()
}
