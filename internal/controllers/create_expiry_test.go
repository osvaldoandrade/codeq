package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

func FuzzCreateExpiryContext(f *testing.F) {
	for _, value := range []string{"", "2026-10-19T12:30:59.123456789Z", "2026-10-19T12:30:59-03:00", "bad", "0001-01-01T00:00:00Z", "2026-99-19T12:30:59Z"} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		if len(value) > 4096 {
			t.Skip()
		}
		ctx, err := createExpiryContext(context.Background(), value)
		if err == nil {
			actual := domain.RequestedTaskExpiry(ctx)
			if value == "" {
				if !actual.IsZero() {
					t.Fatal("absent expiry invented")
				}
				return
			}
			expected, e := time.Parse(time.RFC3339Nano, value)
			if e != nil || expected.IsZero() || !actual.Equal(expected) {
				t.Fatalf("invalid accepted expiry %q", value)
			}
		}
	})
}
