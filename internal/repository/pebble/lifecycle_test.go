package pebble

import (
	"testing"
	"time"
)

// Close waits for background work that is already reading Pebble and refuses
// new work afterwards, so a leadership rebuild racing shutdown can never
// iterate a closed Pebble.
func TestCloseWaitsForRunningBackgroundWork(t *testing.T) {
	db, err := Open(Options{Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	ran := make(chan bool, 1)
	go func() {
		ran <- db.runOpen(func() {
			close(started)
			<-release
			// Still open: reading must work while Close waits.
			if _, e := db.Has([]byte(namespace + "probe")); e != nil {
				t.Errorf("read during close: %v", e)
			}
		})
	}()
	<-started
	closed := make(chan error, 1)
	go func() { closed <- db.Close() }()
	select {
	case <-closed:
		t.Fatal("Close returned while background work was still reading")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if !<-ran {
		t.Fatal("running work reported as refused")
	}
	if err = <-closed; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if db.runOpen(func() { t.Error("work ran after Close") }) {
		t.Fatal("runOpen accepted work after Close")
	}
}
