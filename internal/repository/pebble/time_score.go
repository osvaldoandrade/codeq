package pebble

import (
	"math"
	"time"
)

// unixSeconds encodes time values for ordered on-disk indexes. Values before
// the Unix epoch sort first instead of wrapping to a far-future uint64 value.
func unixSeconds(value time.Time) uint64 {
	return nonNegativeInt64(value.Unix())
}

func nonNegativeInt64(value int64) uint64 {
	if value < 0 {
		return 0
	}
	return uint64(value) // #nosec G115 -- negative values are rejected above.
}

func storedInt64(value uint64) (int64, bool) {
	if value > math.MaxInt64 {
		return 0, false
	}
	return int64(value), true // #nosec G115 -- MaxInt64 bound is checked above.
}
