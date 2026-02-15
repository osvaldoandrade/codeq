package static

import (
	"encoding/json"
	"testing"
)

func TestStaticValidator(t *testing.T) {
	raw := json.RawMessage(`{"token":"t-1","subject":"s-1","email":"e@local","scopes":["codeq:claim"],"eventTypes":["*"],"raw":{"role":"ADMIN"}}`)
	v, err := NewValidatorFromJSON(raw)
	if err != nil {
		t.Fatalf("NewValidatorFromJSON: %v", err)
	}

	claims, err := v.Validate("t-1")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if claims.Subject != "s-1" {
		t.Fatalf("expected subject s-1, got %q", claims.Subject)
	}
	if claims.Email != "e@local" {
		t.Fatalf("expected email e@local, got %q", claims.Email)
	}
	if !claims.HasScope("codeq:claim") {
		t.Fatalf("expected scope present")
	}
	if len(claims.EventTypes) != 1 || claims.EventTypes[0] != "*" {
		t.Fatalf("expected wildcard eventTypes, got %v", claims.EventTypes)
	}

	if _, err := v.Validate("wrong"); err == nil {
		t.Fatalf("expected validation error for wrong token")
	}
}

func TestStaticValidator_StringConfig(t *testing.T) {
	raw := json.RawMessage(`"t-2"`)
	v, err := NewValidatorFromJSON(raw)
	if err != nil {
		t.Fatalf("NewValidatorFromJSON: %v", err)
	}
	if _, err := v.Validate("t-2"); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}
