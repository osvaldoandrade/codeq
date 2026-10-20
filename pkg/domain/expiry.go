package domain

import (
	"context"
	"errors"
	"time"
)

// ErrTaskExpired is intentionally also a not-found error on lease/read paths.
// Creation maps it to a stable gone response without returning retained metadata.
var ErrTaskExpired = errors.New("not-found: task_expired")

// ErrAbsoluteExpiryCapacity refuses new acceptance without blocking cleanup.
var ErrAbsoluteExpiryCapacity = errors.New("absolute_expiry_capacity: metadata capacity is full; review the configured per-shard budget and cleanup health")

type requestedExpiryKey struct{}

// WithTaskExpiry carries caller intent, never authorization. Storage must verify
// the authenticated tenant's topic policy before accepting an absolute deadline.
func WithTaskExpiry(ctx context.Context, expiresAt time.Time) context.Context {
	return context.WithValue(ctx, requestedExpiryKey{}, expiresAt.UTC())
}

// RequestedTaskExpiry returns the ingress deadline; zero means it was omitted.
func RequestedTaskExpiry(ctx context.Context) time.Time {
	value, _ := ctx.Value(requestedExpiryKey{}).(time.Time)
	return value
}
