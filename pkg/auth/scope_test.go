package auth

import (
	"encoding/json"
	"testing"
)

type scopeTestValidator struct {
	scope *Scope
}

func (scopeTestValidator) Validate(string) (*Claims, error) { return &Claims{}, nil }

func TestScopeSharedCreatesOnce(t *testing.T) {
	s := NewScope()
	created := 0
	create := func() any { created++; return &created }
	first := s.Shared("k", create)
	if again := s.Shared("k", create); again != first || created != 1 {
		t.Fatalf("Shared created %d values for one key", created)
	}
	if other := s.Shared("other", create); other == nil || created != 2 {
		t.Fatalf("a second key reused the first value (created=%d)", created)
	}
}

func TestNewScopedValidatorRoutesByProvider(t *testing.T) {
	RegisterProvider("scope-test-plain", func(json.RawMessage) (Validator, error) { return scopeTestValidator{}, nil })
	RegisterProvider("scope-test-scoped", func(json.RawMessage) (Validator, error) { return scopeTestValidator{}, nil })
	RegisterScopedProvider("scope-test-scoped", func(_ json.RawMessage, s *Scope) (Validator, error) {
		return scopeTestValidator{scope: s}, nil
	})
	scope := NewScope()

	v, err := NewScopedValidator(scope, ProviderConfig{Type: "scope-test-scoped"})
	if err != nil || v.(scopeTestValidator).scope != scope {
		t.Fatalf("scoped provider not given the scope: %#v %v", v, err)
	}
	if v, err := NewScopedValidator(nil, ProviderConfig{Type: "scope-test-scoped"}); err != nil || v.(scopeTestValidator).scope != nil {
		t.Fatalf("a nil scope must build what NewValidator builds: %#v %v", v, err)
	}
	if _, err := NewScopedValidator(scope, ProviderConfig{Type: "scope-test-plain"}); err != nil {
		t.Fatalf("a provider without a scoped factory: %v", err)
	}
	if _, err := NewScopedValidator(scope, ProviderConfig{Type: "scope-test-missing"}); err == nil {
		t.Fatal("an unknown provider was accepted")
	}
}
