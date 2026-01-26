package backoff

import (
	"math"
	"math/rand"
)

// Compute returns a delay in seconds based on attempts and policy.
// attempts is expected to be >= 0.
func Compute(policy string, baseSeconds int, maxSeconds int, attempts int, rng *rand.Rand) int {
	if attempts < 0 {
		attempts = 0
	}
	if baseSeconds <= 0 {
		baseSeconds = 1
	}
	if maxSeconds <= 0 {
		maxSeconds = baseSeconds
	}
	if rng == nil {
		rng = rand.New(rand.NewSource(1))
	}
	switch policy {
	case "fixed":
		return minInt(baseSeconds, maxSeconds)
	case "linear":
		return minInt(baseSeconds*maxInt(1, attempts), maxSeconds)
	case "exponential":
		return minInt(int(float64(baseSeconds)*math.Pow(2, float64(attempts))), maxSeconds)
	case "exp_equal_jitter":
		maxDelay := minInt(int(float64(baseSeconds)*math.Pow(2, float64(attempts))), maxSeconds)
		half := maxDelay / 2
		return half + rng.Intn(half+1)
	default: // exp_full_jitter
		maxDelay := minInt(int(float64(baseSeconds)*math.Pow(2, float64(attempts))), maxSeconds)
		if maxDelay <= 0 {
			return 0
		}
		return rng.Intn(maxDelay + 1)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
