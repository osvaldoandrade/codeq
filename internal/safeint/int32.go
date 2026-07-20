// Package safeint contains explicit numeric narrowing helpers used at wire
// boundaries. Saturating is preferable to Go's wraparound conversion when a
// value has escaped its normal domain validation.
package safeint

import "math"

// Int32 returns value represented as int32, saturating values outside the
// representable range. Normal callers validate tighter business bounds first;
// saturation is the final defense against wire-level wraparound.
func Int32(value int) int32 {
	if value > math.MaxInt32 {
		return math.MaxInt32
	}
	if value < math.MinInt32 {
		return math.MinInt32
	}
	return int32(value) // #nosec G115 -- explicit MinInt32/MaxInt32 bounds are checked above.
}
