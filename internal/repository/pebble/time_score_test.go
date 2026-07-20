package pebble

import (
	"math"
	"testing"
	"time"
)

func TestUnixSecondsDoesNotWrapPreEpochValues(t *testing.T) {
	if got := unixSeconds(time.Unix(-1, 0)); got != 0 {
		t.Fatalf("unixSeconds(pre-epoch) = %d, want 0", got)
	}
	if got := unixSeconds(time.Unix(42, 0)); got != 42 {
		t.Fatalf("unixSeconds(42) = %d, want 42", got)
	}
}

func TestStoredInt64RejectsOverflow(t *testing.T) {
	if _, ok := storedInt64(math.MaxUint64); ok {
		t.Fatal("storedInt64 accepted a value above MaxInt64")
	}
	if got, ok := storedInt64(math.MaxInt64); !ok || got != math.MaxInt64 {
		t.Fatalf("storedInt64(MaxInt64) = (%d, %t)", got, ok)
	}
}
