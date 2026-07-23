package multi

import (
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
