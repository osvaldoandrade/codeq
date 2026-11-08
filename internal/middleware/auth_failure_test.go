package middleware

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/osvaldoandrade/codeq/pkg/auth"
	"github.com/osvaldoandrade/codeq/pkg/config"
)

// errLeakyRefusal mimics a validator error carrying internals that must never
// reach a response body.
var errLeakyRefusal = fmt.Errorf("%w: failed to fetch JWKS: dial tcp http://tikti.codecloud-identity.svc/v1/.well-known/jwks.json: invalid issuer: https://issuer.internal", auth.ErrInvalidToken)

const (
	bearerX            = "Bearer x"
	testWorkerAudience = "codeq-worker-test"
)

var leakMarkers = []string{"tikti", "well-known", "iss" + "uer", "dial"}

type errValidator struct{ err error }

func (v errValidator) Validate(string) (*auth.Claims, error) { return nil, v.err }

func unavailable(retry time.Duration) error {
	return fmt.Errorf("multi auth: %w", errors.Join(errLeakyRefusal, &auth.UnavailableError{RetryAfter: retry}))
}

func runAuth(t *testing.T, handler gin.HandlerFunc, header string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/codeq/tasks", nil)
	if header != "" {
		c.Request.Header.Set("Authorization", header)
	}
	handler(c)
	return rec
}

func errorBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return body["error"]
}

func TestAuthFailuresUseFixedBodiesAndDistinctStatuses(t *testing.T) {
	cfg := &config.Config{Env: "prod", AllowProducerAsWorker: true, WorkerAudience: testWorkerAudience}
	refused := errValidator{err: errLeakyRefusal}
	down := errValidator{err: unavailable(12300 * time.Millisecond)}

	cases := []struct {
		name       string
		handler    gin.HandlerFunc
		header     string
		wantStatus int
		wantBody   string
		wantRetry  string
	}{
		{"producer refusal", AuthMiddleware(refused, cfg), bearerX, http.StatusUnauthorized, msgInvalidToken, ""},
		{"producer missing header", AuthMiddleware(refused, cfg), "", http.StatusUnauthorized, errMissingAuthorization.Error(), ""},
		{"producer bad scheme", AuthMiddleware(refused, cfg), "Basic x", http.StatusUnauthorized, errInvalidAuthorization.Error(), ""},
		{"producer unavailable", AuthMiddleware(down, cfg), bearerX, http.StatusServiceUnavailable, msgAuthUnavailable, "13"},
		{"worker refusal", WorkerAuthMiddleware(refused, refused, cfg), bearerX, http.StatusUnauthorized, msgInvalidToken, ""},
		{"worker unavailable", WorkerAuthMiddleware(down, refused, cfg), bearerX, http.StatusServiceUnavailable, msgAuthUnavailable, "13"},
		{"worker promotion unavailable", WorkerAuthMiddleware(refused, down, cfg), bearerX, http.StatusServiceUnavailable, msgAuthUnavailable, "13"},
		{"any refusal", AnyAuthMiddleware(refused, refused, cfg), bearerX, http.StatusUnauthorized, msgInvalidToken, ""},
		{"any producer unavailable", AnyAuthMiddleware(refused, down, cfg), bearerX, http.StatusServiceUnavailable, msgAuthUnavailable, "13"},
		{"any worker unavailable", AnyAuthMiddleware(down, nil, cfg), bearerX, http.StatusServiceUnavailable, msgAuthUnavailable, "13"},
		{"any without validators", AnyAuthMiddleware(nil, nil, cfg), bearerX, http.StatusUnauthorized, msgInvalidToken, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := runAuth(t, tc.handler, tc.header)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status %d, want %d", rec.Code, tc.wantStatus)
			}
			if got := errorBody(t, rec); got != tc.wantBody {
				t.Fatalf("body %q, want %q", got, tc.wantBody)
			}
			if got := rec.Header().Get("Retry-After"); got != tc.wantRetry {
				t.Fatalf("Retry-After %q, want %q", got, tc.wantRetry)
			}
			for _, leak := range leakMarkers {
				if strings.Contains(strings.ToLower(rec.Body.String()), leak) {
					t.Fatalf("body leaks %q: %s", leak, rec.Body.String())
				}
			}
		})
	}
}

func TestWorkerShapeFailureKeepsFixedMessage(t *testing.T) {
	shapeless := fakeValidator{"x": &auth.Claims{Subject: "w", Scopes: []string{workerClaimScope}}}
	rec := runAuth(t, WorkerAuthMiddleware(shapeless, nil, &config.Config{}), bearerX)
	if rec.Code != http.StatusUnauthorized || errorBody(t, rec) != errMissingEventTypes.Error() {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}
