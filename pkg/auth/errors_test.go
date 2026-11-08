package auth

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestRetryAfterClassifiesWrappedAndJoinedUnavailability(t *testing.T) {
	refused := fmt.Errorf("%w: bad signature", ErrInvalidToken)
	if _, ok := RetryAfter(refused); ok {
		t.Fatal("refusal classified unavailable")
	}
	if _, ok := RetryAfter(nil); ok {
		t.Fatal("nil classified unavailable")
	}
	joined := fmt.Errorf("multi: %w", errors.Join(refused, &UnavailableError{RetryAfter: 9 * time.Second}))
	if d, ok := RetryAfter(joined); !ok || d != 9*time.Second {
		t.Fatalf("joined unavailability: %v %v", d, ok)
	}
	if d, ok := RetryAfter(&UnavailableError{}); !ok || d != time.Second {
		t.Fatalf("zero retry not floored: %v %v", d, ok)
	}
	if d, ok := RetryAfter(fmt.Errorf("x: %w", ErrUnavailable)); !ok || d != time.Second {
		t.Fatalf("bare sentinel: %v %v", d, ok)
	}
	if !errors.Is(&UnavailableError{}, ErrUnavailable) || errors.Is(&UnavailableError{}, ErrInvalidToken) {
		t.Fatal("UnavailableError identity wrong")
	}
	if (&UnavailableError{}).Error() != ErrUnavailable.Error() {
		t.Fatal("UnavailableError message not fixed")
	}
}

func TestPreferUnavailable(t *testing.T) {
	refused := fmt.Errorf("%w: unknown kid", ErrInvalidToken)
	down := &UnavailableError{RetryAfter: time.Second}
	if got := PreferUnavailable(nil, refused, down); !errors.Is(got, down) {
		t.Fatalf("got %v, want unavailability", got)
	}
	if got := PreferUnavailable(nil, refused, errors.New("other")); !errors.Is(got, refused) {
		t.Fatalf("got %v, want first error", got)
	}
	if got := PreferUnavailable(nil, nil); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}
