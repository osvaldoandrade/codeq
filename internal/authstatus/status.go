// Package authstatus maps token validation failures to fixed gRPC statuses.
package authstatus

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/osvaldoandrade/codeq/pkg/auth"
)

// Fixed stream handshake messages; validator error text is never echoed.
const (
	msgInvalidToken = "invalid token"
	msgUnavailable  = "authentication temporarily unavailable"
)

// Stream returns codes.Unavailable when the token could not be evaluated
// (no usable key set, ADR 0014) and codes.Unauthenticated otherwise.
func Stream(err error) error {
	if _, unavailable := auth.RetryAfter(err); unavailable {
		return status.Error(codes.Unavailable, msgUnavailable)
	}
	return status.Error(codes.Unauthenticated, msgInvalidToken)
}
