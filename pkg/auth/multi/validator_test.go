package multi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/osvaldoandrade/codeq/pkg/auth"
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
