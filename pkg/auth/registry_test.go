package auth

import (
	"encoding/json"
	"errors"
	"testing"
)

type mockValidator struct{}

func (m *mockValidator) Validate(token string) (*Claims, error) {
	if token == "valid" {
		return &Claims{Subject: "test-user"}, nil
	}
	return nil, errors.New("invalid token")
}

func TestRegistry(t *testing.T) {
	// Register a mock provider
	RegisterProvider("mock", func(config json.RawMessage) (Validator, error) {
		return &mockValidator{}, nil
	})

	// Check provider is listed
	providers := ListProviders()
	found := false
	for _, p := range providers {
		if p == "mock" {
			found = true
			break
		}
	}
	if !found {
		t.Error("mock provider not found in registry")
	}

	// Create validator from config
	cfg := ProviderConfig{
		Type:   "mock",
		Config: json.RawMessage(`{}`),
	}
	validator, err := NewValidator(cfg)
	if err != nil {
		t.Fatalf("failed to create validator: %v", err)
	}

	// Test validator
	claims, err := validator.Validate("valid")
	if err != nil {
		t.Fatalf("expected valid token: %v", err)
	}
	if claims.Subject != "test-user" {
		t.Errorf("expected subject 'test-user', got '%s'", claims.Subject)
	}

	// Test invalid token
	_, err = validator.Validate("invalid")
	if err == nil {
		t.Error("expected error for invalid token")
	}
}

func TestRegistryUnknownProvider(t *testing.T) {
	cfg := ProviderConfig{
		Type:   "unknown",
		Config: json.RawMessage(`{}`),
	}
	_, err := NewValidator(cfg)
	if err == nil {
		t.Error("expected error for unknown provider")
	}
}
