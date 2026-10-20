package controllers

import (
	"context"
	"errors"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

func createExpiryContext(ctx context.Context, value string) (context.Context, error) {
	if value == "" {
		return ctx, nil
	}
	deadline, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || deadline.IsZero() {
		return nil, errors.New("invalid 'expiresAt' (use RFC3339Nano)")
	}
	return domain.WithTaskExpiry(ctx, deadline), nil
}
