package authstatus

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/osvaldoandrade/codeq/pkg/auth"
)

func TestStreamMapsUnavailableAndRefusalToFixedStatuses(t *testing.T) {
	unavailable := fmt.Errorf("wrapped: %w", &auth.UnavailableError{RetryAfter: 7 * time.Second})
	if st := status.Convert(Stream(unavailable)); st.Code() != codes.Unavailable || st.Message() != msgUnavailable {
		t.Fatalf("unavailable mapped to %v %q", st.Code(), st.Message())
	}
	refused := fmt.Errorf("%w: dial tcp http://tikti.internal/jwks: secret detail", auth.ErrInvalidToken)
	st := status.Convert(Stream(refused))
	if st.Code() != codes.Unauthenticated || st.Message() != msgInvalidToken {
		t.Fatalf("refusal mapped to %v %q", st.Code(), st.Message())
	}
	if st := status.Convert(Stream(errors.New("anything"))); st.Code() != codes.Unauthenticated {
		t.Fatalf("unclassified error mapped to %v", st.Code())
	}
}

// Review P3-7: a stream client gets the same retry delay an HTTP client reads
// from Retry-After, as a google.rpc.RetryInfo status detail.
func TestStreamUnavailableCarriesRetryInfo(t *testing.T) {
	for _, tc := range []struct {
		retry time.Duration
		want  time.Duration
	}{
		{7 * time.Second, 7 * time.Second},
		{1500 * time.Millisecond, 2 * time.Second},
		{0, time.Second},
	} {
		st := status.Convert(Stream(&auth.UnavailableError{RetryAfter: tc.retry}))
		var got *errdetails.RetryInfo
		for _, d := range st.Details() {
			if info, ok := d.(*errdetails.RetryInfo); ok {
				got = info
			}
		}
		if got == nil || got.GetRetryDelay().AsDuration() != tc.want {
			t.Fatalf("retry %v: RetryInfo = %v, want %v", tc.retry, got, tc.want)
		}
	}
	if details := status.Convert(Stream(auth.ErrInvalidToken)).Details(); len(details) != 0 {
		t.Fatalf("a refusal carries details: %v", details)
	}
}
