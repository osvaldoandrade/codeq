// Package auth defines token validation contracts shared by the HTTP and
// gRPC transports and the pluggable validator providers.
package auth

import (
	"errors"
	"time"
)

// ErrInvalidToken classifies a token that was evaluated and refused: bad
// signature, unknown key after a successful key refresh, wrong issuer or
// audience, expired, missing a required claim, or malformed. Transports map
// it to 401 / Unauthenticated with a generic message.
var ErrInvalidToken = errors.New("invalid token")

// ErrUnavailable classifies a token that could not be evaluated because the
// verifier has no usable key material (never fetched, or older than its
// bounded maximum age). It is never an acceptance: transports map it to
// 503 + Retry-After / Unavailable, distinct from 401 (ADR 0014).
var ErrUnavailable = errors.New("token verification temporarily unavailable")

// UnavailableError carries the advisory retry delay of an ErrUnavailable.
type UnavailableError struct {
	RetryAfter time.Duration
}

func (e *UnavailableError) Error() string { return ErrUnavailable.Error() }

// Is makes errors.Is(err, ErrUnavailable) hold for every UnavailableError.
func (e *UnavailableError) Is(target error) bool { return target == ErrUnavailable }

// minRetryAfter is the smallest advertised retry delay.
const minRetryAfter = time.Second

// RetryAfter reports whether err (or any error it wraps or joins) is an
// unavailability and returns its advisory retry delay, at least one second.
func RetryAfter(err error) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	var unavailable *UnavailableError
	if errors.As(err, &unavailable) {
		return max(unavailable.RetryAfter, minRetryAfter), true
	}
	if errors.Is(err, ErrUnavailable) {
		return minRetryAfter, true
	}
	return 0, false
}

// PreferUnavailable returns the first unavailability among errs, otherwise
// the first non-nil error. A caller that tried several validators uses it so
// that "could not verify" is not reported as "verified and refused".
func PreferUnavailable(errs ...error) error {
	var first error
	for _, err := range errs {
		if err == nil {
			continue
		}
		if _, ok := RetryAfter(err); ok {
			return err
		}
		if first == nil {
			first = err
		}
	}
	return first
}
