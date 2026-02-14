package backoff

import (
	"math/rand"
	"testing"
)

func TestComputeFixed(t *testing.T) {
	tests := []struct {
		name        string
		baseSeconds int
		maxSeconds  int
		attempts    int
		want        int
	}{
		{"base 5 max 10", 5, 10, 0, 5},
		{"base 5 max 10 many attempts", 5, 10, 100, 5},
		{"base exceeds max", 20, 10, 0, 10},
		{"zero base defaults to 1", 0, 10, 0, 1},
		{"negative base defaults to 1", -5, 10, 0, 1},
		{"zero max equals base", 5, 0, 0, 5},
		{"negative max equals base", 5, -1, 0, 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rng := rand.New(rand.NewSource(42))
			got := Compute("fixed", tt.baseSeconds, tt.maxSeconds, tt.attempts, rng)
			if got != tt.want {
				t.Errorf("Compute(fixed) = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestComputeLinear(t *testing.T) {
	tests := []struct {
		name        string
		baseSeconds int
		maxSeconds  int
		attempts    int
		want        int
	}{
		{"zero attempts", 5, 100, 0, 5},
		{"one attempt", 5, 100, 1, 5},
		{"two attempts", 5, 100, 2, 10},
		{"three attempts", 5, 100, 3, 15},
		{"capped at max", 5, 20, 10, 20},
		{"negative attempts treated as zero", 5, 100, -1, 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rng := rand.New(rand.NewSource(42))
			got := Compute("linear", tt.baseSeconds, tt.maxSeconds, tt.attempts, rng)
			if got != tt.want {
				t.Errorf("Compute(linear) = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestComputeExponential(t *testing.T) {
	tests := []struct {
		name        string
		baseSeconds int
		maxSeconds  int
		attempts    int
		wantMin     int
		wantMax     int
	}{
		{"zero attempts", 5, 1000, 0, 5, 5},
		{"one attempt", 5, 1000, 1, 10, 10},
		{"two attempts", 5, 1000, 2, 20, 20},
		{"three attempts", 5, 1000, 3, 40, 40},
		{"capped at max", 5, 50, 10, 50, 50},
		{"negative attempts treated as zero", 5, 1000, -1, 5, 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rng := rand.New(rand.NewSource(42))
			got := Compute("exponential", tt.baseSeconds, tt.maxSeconds, tt.attempts, rng)
			if got < tt.wantMin || got > tt.wantMax {
				t.Errorf("Compute(exponential) = %d, want between %d and %d", got, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestComputeExpEqualJitter(t *testing.T) {
	tests := []struct {
		name        string
		baseSeconds int
		maxSeconds  int
		attempts    int
		wantMin     int
		wantMax     int
	}{
		{"zero attempts", 5, 1000, 0, 2, 5},
		{"one attempt", 5, 1000, 1, 5, 10},
		{"two attempts", 5, 1000, 2, 10, 20},
		{"capped at max", 5, 50, 10, 25, 50},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rng := rand.New(rand.NewSource(42))
			got := Compute("exp_equal_jitter", tt.baseSeconds, tt.maxSeconds, tt.attempts, rng)
			if got < tt.wantMin || got > tt.wantMax {
				t.Errorf("Compute(exp_equal_jitter) = %d, want between %d and %d", got, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestComputeExpFullJitter(t *testing.T) {
	tests := []struct {
		name        string
		baseSeconds int
		maxSeconds  int
		attempts    int
		wantMin     int
		wantMax     int
	}{
		{"zero attempts", 5, 1000, 0, 0, 5},
		{"one attempt", 5, 1000, 1, 0, 10},
		{"two attempts", 5, 1000, 2, 0, 20},
		{"capped at max", 5, 50, 10, 0, 50},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rng := rand.New(rand.NewSource(42))
			got := Compute("exp_full_jitter", tt.baseSeconds, tt.maxSeconds, tt.attempts, rng)
			if got < tt.wantMin || got > tt.wantMax {
				t.Errorf("Compute(exp_full_jitter) = %d, want between %d and %d", got, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestComputeDefaultPolicy(t *testing.T) {
	// Default (unknown policy) should behave like exp_full_jitter
	rng := rand.New(rand.NewSource(42))
	got := Compute("unknown_policy", 5, 1000, 2, rng)
	if got < 0 || got > 20 {
		t.Errorf("Compute(unknown_policy) = %d, want between 0 and 20", got)
	}
}

func TestComputeNilRng(t *testing.T) {
	// Should use default rng when nil is passed
	got := Compute("fixed", 5, 10, 0, nil)
	if got != 5 {
		t.Errorf("Compute with nil rng = %d, want 5", got)
	}
}

func TestComputeJitterVariation(t *testing.T) {
	// Test that jitter policies produce different values with different seeds
	rng1 := rand.New(rand.NewSource(1))
	rng2 := rand.New(rand.NewSource(2))

	val1 := Compute("exp_full_jitter", 5, 1000, 5, rng1)
	val2 := Compute("exp_full_jitter", 5, 1000, 5, rng2)

	// With different seeds, we should get different values (very high probability)
	// Run multiple times to ensure we see variation
	different := false
	for i := 0; i < 10; i++ {
		v1 := Compute("exp_full_jitter", 5, 1000, i, rng1)
		v2 := Compute("exp_full_jitter", 5, 1000, i, rng2)
		if v1 != v2 {
			different = true
			break
		}
	}

	if !different {
		t.Log("Warning: jitter did not produce different values (expected but not guaranteed)")
	}

	// At least verify they're in valid range
	if val1 < 0 || val1 > 160 {
		t.Errorf("val1 = %d, want between 0 and 160", val1)
	}
	if val2 < 0 || val2 > 160 {
		t.Errorf("val2 = %d, want between 0 and 160", val2)
	}
}

func TestMin(t *testing.T) {
	tests := []struct {
		a, b int
		want int
	}{
		{1, 2, 1},
		{2, 1, 1},
		{5, 5, 5},
		{-1, 1, -1},
		{0, 10, 0},
	}

	for _, tt := range tests {
		got := min(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("min(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestMax(t *testing.T) {
	tests := []struct {
		a, b int
		want int
	}{
		{1, 2, 2},
		{2, 1, 2},
		{5, 5, 5},
		{-1, 1, 1},
		{0, 10, 10},
	}

	for _, tt := range tests {
		got := max(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("max(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestComputeZeroMaxDelay(t *testing.T) {
	// Edge case: when maxDelay calculation results in 0 or negative
	rng := rand.New(rand.NewSource(42))

	// With zero base and max, it defaults base to 1, so result should be 0 or 1
	got := Compute("exp_full_jitter", 0, 0, 0, rng)
	if got < 0 || got > 1 {
		t.Errorf("Compute with zero base and max = %d, want between 0 and 1", got)
	}
}
