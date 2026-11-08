package authstatus

import (
	"errors"
	"fmt"
	"testing"
	"time"

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
