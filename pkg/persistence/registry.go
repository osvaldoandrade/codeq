package persistence

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// ProviderConfig contains provider-specific configuration
type ProviderConfig struct {
	Type   string          `yaml:"type" json:"type"`
	Config json.RawMessage `yaml:"config" json:"config"`
}

// PluginConfig provides initialization parameters to persistence plugins
type PluginConfig struct {
	// Config contains plugin-specific configuration
	Config json.RawMessage
	
	// Timezone for task scheduling
	Timezone *time.Location
	
	// BackoffPolicy for retry logic
	BackoffPolicy      string
	BackoffBaseSeconds int
	BackoffMaxSeconds  int
}

// PluginFactory creates persistence plugins from configuration
type PluginFactory func(config PluginConfig) (PluginPersistence, error)

var (
	registry = make(map[string]PluginFactory)
	mu       sync.RWMutex
)

// RegisterProvider registers a persistence plugin factory for a provider type
func RegisterProvider(providerType string, factory PluginFactory) {
	mu.Lock()
	defer mu.Unlock()
	registry[providerType] = factory
}

// NewPersistence creates a persistence plugin from provider configuration
func NewPersistence(providerConfig ProviderConfig, pluginConfig PluginConfig) (PluginPersistence, error) {
	mu.RLock()
	factory, ok := registry[providerConfig.Type]
	mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("unknown persistence provider type: %s", providerConfig.Type)
	}

	// Merge the provider config into the plugin config
	pluginConfig.Config = providerConfig.Config
	
	return factory(pluginConfig)
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
