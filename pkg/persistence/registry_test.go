package persistence

import (
	"testing"
)

func TestRegisterProvider(t *testing.T) {
	// Create a mock factory
	mockFactory := func(config PluginConfig) (PluginPersistence, error) {
		return nil, nil
	}

	// Register the provider
	RegisterProvider("test", mockFactory)

	// List providers should include our test provider
	providers := ListProviders()
	found := false
	for _, p := range providers {
		if p == "test" {
			found = true
			break
		}
	}

	if !found {
		t.Errorf("Expected to find 'test' provider in list, got: %v", providers)
	}
}

func TestNewPersistenceUnknownProvider(t *testing.T) {
	cfg := ProviderConfig{
		Type:   "unknown_provider",
		Config: []byte("{}"),
	}

	pluginCfg := PluginConfig{}

	_, err := NewPersistence(cfg, pluginCfg)
	if err == nil {
		t.Error("Expected error for unknown provider, got nil")
	}
}
