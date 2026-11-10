package auth

import (
	"encoding/json"
	"fmt"
	"sync"
)

// Scope lets the validators one process builds share state keyed by their
// configuration, such as one JWKS key-set cache per key-set URL for the
// producer and worker validators (ADR 0014). Only providers registered with
// RegisterScopedProvider use it. Create one with NewScope; it is safe for
// concurrent use.
type Scope struct {
	mu     sync.Mutex
	shared map[any]any
}

// NewScope returns an empty Scope.
func NewScope() *Scope {
	return &Scope{shared: make(map[any]any)}
}

// Shared returns the value stored under key, storing create() on first use.
// key must be comparable; providers use an unexported key type so no other
// provider can collide with it.
func (s *Scope) Shared(key any, create func() any) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.shared[key]; ok {
		return v
	}
	v := create()
	s.shared[key] = v
	return v
}

// ScopedValidatorFactory creates a validator that may share state through
// scope, which is never nil.
type ScopedValidatorFactory func(config json.RawMessage, scope *Scope) (Validator, error)

var scopedRegistry = make(map[string]ScopedValidatorFactory)

// RegisterScopedProvider registers the scope-aware factory of a provider
// type. The provider registers its plain factory with RegisterProvider too.
func RegisterScopedProvider(providerType string, factory ScopedValidatorFactory) {
	mu.Lock()
	defer mu.Unlock()
	scopedRegistry[providerType] = factory
}

// NewScopedValidator is NewValidator for a validator that shares state with
// the other validators built with the same scope. A nil scope, or a provider
// without a scoped factory, builds exactly what NewValidator builds.
func NewScopedValidator(scope *Scope, providerConfig ProviderConfig) (Validator, error) {
	if scope == nil {
		return NewValidator(providerConfig)
	}
	mu.RLock()
	scoped, ok := scopedRegistry[providerConfig.Type]
	_, plain := registry[providerConfig.Type]
	mu.RUnlock()
	if ok {
		return scoped(providerConfig.Config, scope)
	}
	if !plain {
		return nil, fmt.Errorf("unknown auth provider type: %s", providerConfig.Type)
	}
	return NewValidator(providerConfig)
}
