package raft

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
)

// TestMux_TwoGroupsRouteIndependently is the core mux property:
// two RegisterGroup calls on the same MuxAcceptor receive only the
// connections destined for THEIR groupID. Cross-group leaks would
// break raft (a request meant for group 0 applied on group 1 = wrong
// FSM, wrong log, divergence).
func TestMux_TwoGroupsRouteIndependently(t *testing.T) {
	acc, err := NewMuxAcceptor("127.0.0.1:0", nil)
	if err != nil {
		t.Fatalf("acceptor: %v", err)
	}
	t.Cleanup(func() { _ = acc.Close() })

	g0, err := acc.RegisterGroup(0)
	if err != nil {
		t.Fatalf("register 0: %v", err)
	}
	g1, err := acc.RegisterGroup(1)
	if err != nil {
		t.Fatalf("register 1: %v", err)
	}

	target := hraft.ServerAddress(acc.Addr().String())

	// Dial each group; on the other side, Accept should yield the
	// matching connection. Sequence the dials so accept order is
	// deterministic (per-group queues remain independent regardless).
	c0Out, err := g0.Dial(target, time.Second)
	if err != nil {
		t.Fatalf("dial g0: %v", err)
	}
	c0In, err := g0.Accept()
	if err != nil {
		t.Fatalf("accept g0: %v", err)
	}

	c1Out, err := g1.Dial(target, time.Second)
	if err != nil {
		t.Fatalf("dial g1: %v", err)
	}
	c1In, err := g1.Accept()
	if err != nil {
		t.Fatalf("accept g1: %v", err)
	}

	// Send distinct bytes on each side and verify they land on the
	// peer-side accept of the same group.
	if err := writeAndExpect(c0Out, c0In, []byte("group-zero-msg")); err != nil {
		t.Errorf("g0 round-trip: %v", err)
	}
	if err := writeAndExpect(c1Out, c1In, []byte("group-one-msg")); err != nil {
		t.Errorf("g1 round-trip: %v", err)
	}

	_ = c0Out.Close()
	_ = c0In.Close()
	_ = c1Out.Close()
	_ = c1In.Close()
}

func TestMux_DuplicateRegistrationErrors(t *testing.T) {
	acc, err := NewMuxAcceptor("127.0.0.1:0", nil)
	if err != nil {
		t.Fatalf("acceptor: %v", err)
	}
	t.Cleanup(func() { _ = acc.Close() })

	if _, err := acc.RegisterGroup(7); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if _, err := acc.RegisterGroup(7); err == nil {
		t.Error("duplicate register: want error, got nil")
	}
}

func TestMux_UnknownGroupClosesConn(t *testing.T) {
	acc, err := NewMuxAcceptor("127.0.0.1:0", nil)
	if err != nil {
		t.Fatalf("acceptor: %v", err)
	}
	t.Cleanup(func() { _ = acc.Close() })

	// Register groupID=0 so the acceptor is fully wired; dial with
	// a different groupID (manually emit the prefix so we can mimic a
	// malformed peer).
	if _, err := acc.RegisterGroup(0); err != nil {
		t.Fatalf("register: %v", err)
	}

	d := net.Dialer{Timeout: time.Second}
	conn, err := d.Dial("tcp", acc.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Emit groupID=99 — unknown.
	if _, err := conn.Write([]byte{0x00, 0x00, 0x00, 0x63}); err != nil {
		t.Fatalf("write groupID: %v", err)
	}

	// The server should close the connection. A subsequent Read
	// should observe EOF (or RST → EOF/Connection reset).
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 4)
	_, err = conn.Read(buf)
	if err == nil {
		t.Error("expected connection close on unknown groupID")
	}
}

func TestMux_AcceptUnblocksOnClose(t *testing.T) {
	acc, err := NewMuxAcceptor("127.0.0.1:0", nil)
	if err != nil {
		t.Fatalf("acceptor: %v", err)
	}
	g, _ := acc.RegisterGroup(0)

	done := make(chan struct{})
	go func() {
		_, err := g.Accept()
		if err == nil {
			t.Errorf("Accept after Close: want error, got nil")
		}
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	_ = acc.Close()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Accept did not unblock within 1s of Close")
	}
}

// TestMux_ConcurrentTraffic verifies a barrage of concurrent dials on
// two groups all reach the right side without crossover. Each
// goroutine sends a tagged bytes; the receiver asserts the tag
// matches its group.
func TestMux_ConcurrentTraffic(t *testing.T) {
	acc, err := NewMuxAcceptor("127.0.0.1:0", nil)
	if err != nil {
		t.Fatalf("acceptor: %v", err)
	}
	t.Cleanup(func() { _ = acc.Close() })

	g0, _ := acc.RegisterGroup(0)
	g1, _ := acc.RegisterGroup(1)
	target := hraft.ServerAddress(acc.Addr().String())

	const perGroup = 10
	var wg sync.WaitGroup

	// Accept loops: pull `perGroup` connections, read a tag byte, and
	// fail if it doesn't match the expected group tag.
	acceptLoop := func(layer hraft.StreamLayer, want byte) {
		defer wg.Done()
		for i := 0; i < perGroup; i++ {
			conn, err := layer.Accept()
			if err != nil {
				t.Errorf("accept: %v", err)
				return
			}
			tag := make([]byte, 1)
			if _, err := io.ReadFull(conn, tag); err != nil {
				t.Errorf("read tag: %v", err)
			}
			if tag[0] != want {
				t.Errorf("group tag mismatch: want %d, got %d", want, tag[0])
			}
			_ = conn.Close()
		}
	}
	wg.Add(2)
	go acceptLoop(g0, 0)
	go acceptLoop(g1, 1)

	// Dialers.
	dialLoop := func(layer hraft.StreamLayer, tag byte) {
		defer wg.Done()
		for i := 0; i < perGroup; i++ {
			conn, err := layer.Dial(target, time.Second)
			if err != nil {
				t.Errorf("dial: %v", err)
				return
			}
			if _, err := conn.Write([]byte{tag}); err != nil {
				t.Errorf("write tag: %v", err)
			}
			_ = conn.Close()
		}
	}
	wg.Add(2)
	go dialLoop(g0, 0)
	go dialLoop(g1, 1)

	wg.Wait()
}

// writeAndExpect writes payload to w and verifies r reads it.
// Returns the first mismatch / IO error.
func writeAndExpect(w net.Conn, r net.Conn, payload []byte) error {
	_, err := w.Write(payload)
	if err != nil {
		return err
	}
	got := make([]byte, len(payload))
	_ = r.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := io.ReadFull(r, got); err != nil {
		return err
	}
	_ = r.SetReadDeadline(time.Time{})
	if !bytes.Equal(got, payload) {
		return errors.New("payload mismatch: got " + string(got))
	}
	return nil
}
