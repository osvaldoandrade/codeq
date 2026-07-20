package safeint

import (
	"math"
	"testing"
)

func TestInt32SaturatesAtWireBounds(t *testing.T) {
	tests := []struct {
		name  string
		input int
		want  int32
	}{
		{name: "in range", input: 42, want: 42},
		{name: "upper bound", input: math.MaxInt32, want: math.MaxInt32},
		{name: "lower bound", input: math.MinInt32, want: math.MinInt32},
	}
	if math.MaxInt > math.MaxInt32 {
		tests = append(tests,
			struct {
				name  string
				input int
				want  int32
			}{name: "above range", input: math.MaxInt, want: math.MaxInt32},
			struct {
				name  string
				input int
				want  int32
			}{name: "below range", input: math.MinInt, want: math.MinInt32},
		)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Int32(test.input); got != test.want {
				t.Fatalf("Int32(%d) = %d, want %d", test.input, got, test.want)
			}
		})
	}
}
