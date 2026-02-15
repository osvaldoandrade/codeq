package auth

import (
	"encoding/json"
	"fmt"
	"sync"
)

// ProviderConfig contains provider-specific configuration
type ProviderConfig struct {
	Type   string          `yaml:"type" json:"type"`
	Config json.RawMessage `yaml:"config" json:"config"`
}

// ValidatorFactory creates validators from configuration
type ValidatorFactory func(config json.RawMessage) (Validator, error)

var (
	registry = make(map[string]ValidatorFactory)
	mu       sync.RWMutex
)

// RegisterProvider registers a validator factory for a provider type
func RegisterProvider(providerType string, factory ValidatorFactory) {
	mu.Lock()
	defer mu.Unlock()
	registry[providerType] = factory
}

// NewValidator creates a validator from provider configuration
func NewValidator(providerConfig ProviderConfig) (Validator, error) {
	mu.RLock()
	factory, ok := registry[providerConfig.Type]
	mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("unknown auth provider type: %s", providerConfig.Type)
	}

	return factory(providerConfig.Config)
}

// ListProviders returns registered provider types
func ListProviders() []string {
	mu.RLock()
	defer mu.RUnlock()

	providers := make([]string, 0, len(registry))
	for name := range registry {
		providers = append(providers, name)
	}
	return providers
}
