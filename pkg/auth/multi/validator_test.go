package multi

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/auth"
	_ "github.com/osvaldoandrade/codeq/pkg/auth/jwks"
	_ "github.com/osvaldoandrade/codeq/pkg/auth/static"
)

func TestMultiValidatorAcceptsAnyConfiguredProvider(t *testing.T) {
	raw := json.RawMessage(`{"providers":[
		{"type":"static","config":{"token":"first","subject":"one"}},
		{"type":"static","config":{"token":"second","subject":"two"}}
	]}`)
	instance, err := newValidatorFromJSON(raw)
	if err != nil {
		t.Fatalf("newValidatorFromJSON() error = %v", err)
	}
	for token, subject := range map[string]string{"first": "one", "second": "two"} {
		claims, err := instance.Validate(token)
		if err != nil || claims.Subject != subject {
			t.Fatalf("Validate(%q) = %#v, %v", token, claims, err)
		}
	}
	if _, err := instance.Validate("unknown"); err == nil || strings.Contains(err.Error(), "unknown") {
		t.Fatalf("rejected token error must not echo token: %v", err)
	}
}

func TestMultiValidatorRejectsUnsafeComposition(t *testing.T) {
	tests := []json.RawMessage{
		json.RawMessage(`{"providers":[]}`),
		json.RawMessage(`{"providers":[{"type":"static","config":{"token":"one"}}]}`),
		json.RawMessage(`{"providers":[{"type":"multi","config":{}},{"type":"static","config":{"token":"one"}}]}`),
	}
	for _, raw := range tests {
		if _, err := newValidatorFromJSON(raw); err == nil {
			t.Fatalf("newValidatorFromJSON(%s) error = nil", raw)
		}
	}
}

func TestProviderRegistration(t *testing.T) {
	raw := json.RawMessage(`{"providers":[
		{"type":"static","config":{"token":"first","subject":"one"}},
		{"type":"static","config":{"token":"second","subject":"two"}}
	]}`)
	if _, err := auth.NewValidator(auth.ProviderConfig{Type: "multi", Config: raw}); err != nil {
		t.Fatalf("registered multi provider error = %v", err)
	}
}

type ctxKey struct{}

// recordingValidator reports the context value it was called with.
type recordingValidator struct{ seen *string }

func (r recordingValidator) Validate(string) (*auth.Claims, error) {
	return nil, auth.ErrInvalidToken
}

func (r recordingValidator) ValidateContext(ctx context.Context, _ string) (*auth.Claims, error) {
	*r.seen, _ = ctx.Value(ctxKey{}).(string)
	return nil, auth.ErrInvalidToken
}

func TestMultiValidatorPassesContextToChildren(t *testing.T) {
	var seen string
	instance := &validator{providers: []auth.Validator{recordingValidator{seen: &seen}, recordingValidator{seen: &seen}}}
	ctx := context.WithValue(context.Background(), ctxKey{}, "request")
	if _, err := auth.ValidateContext(ctx, instance, "token"); err == nil {
		t.Fatal("refused by every child but accepted")
	}
	if seen != "request" {
		t.Fatalf("child saw context value %q, want %q", seen, "request")
	}
}

// A multi validator built in a scope passes it to its children, so a JWKS
// child shares its key-set cache with the process's other validators of that
// URL (ADR 0014): one fetch serves both children here.
func TestScopedMultiSharesChildKeySetCache(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var fetches atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "kid": "k1", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(srv.Close)
	child := func(aud string) string {
		return `{"type":"jwks","config":{"jwksUrl":"` + srv.URL + `","issuer":"iss","audience":"` + aud + `"}}`
	}
	raw := json.RawMessage(`{"providers":[` + child("first") + `,` + child("second") + `]}`)
	v, err := auth.NewScopedValidator(auth.NewScope(), auth.ProviderConfig{Type: "multi", Config: raw})
	if err != nil {
		t.Fatalf("NewScopedValidator: %v", err)
	}
	now := time.Now().Unix()
	token := signRS256(t, key, "k1", map[string]any{"iss": "iss", "aud": "second", "sub": "svc", "exp": now + 3600, "iat": now - 60})
	if _, err := auth.ValidateContext(context.Background(), v, token); err != nil {
		t.Fatalf("token for the second child refused: %v", err)
	}
	if got := fetches.Load(); got != 1 {
		t.Fatalf("children of one scope fetched %d times, want 1", got)
	}
}

func signRS256(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	input := enc(map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}) + "." + enc(claims)
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}
