// Package authstatus maps token validation failures to fixed gRPC statuses.
package authstatus

import (
	"math"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/osvaldoandrade/codeq/pkg/auth"
)

// Fixed stream handshake messages; validator error text is never echoed.
const (
	msgInvalidToken = "invalid token"
	msgUnavailable  = "authentication temporarily unavailable"
)

// Stream returns codes.Unavailable when the token could not be evaluated
// (no usable key set, ADR 0014) and codes.Unauthenticated otherwise. The
// Unavailable status carries a google.rpc.RetryInfo detail whose retry_delay
// is the HTTP Retry-After value: the advisory delay rounded up to whole
// seconds.
func Stream(err error) error {
	retry, unavailable := auth.RetryAfter(err)
	if !unavailable {
		return status.Error(codes.Unauthenticated, msgInvalidToken)
	}
	st := status.New(codes.Unavailable, msgUnavailable)
	seconds := time.Duration(math.Ceil(retry.Seconds())) * time.Second
	if detailed, derr := st.WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(seconds)}); derr == nil {
		st = detailed
	}
	return st.Err()
}
