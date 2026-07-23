// Package multi composes a bounded list of authentication providers. It is
// intended for migrations where short-lived JWKS tokens and one local static
// credential must coexist without weakening either validator.
package multi

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/osvaldoandrade/codeq/pkg/auth"
)

const maxProviders = 4

type config struct {
	Providers []auth.ProviderConfig `json:"providers"`
}

type validator struct {
	providers []auth.Validator
}

func init() {
	auth.RegisterProvider("multi", newValidatorFromJSON)
}

func newValidatorFromJSON(raw json.RawMessage) (auth.Validator, error) {
	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("multi auth: invalid config: %w", err)
	}
	if len(cfg.Providers) < 2 || len(cfg.Providers) > maxProviders {
		return nil, fmt.Errorf("multi auth: providers must contain between 2 and %d entries", maxProviders)
	}
	result := &validator{providers: make([]auth.Validator, 0, len(cfg.Providers))}
	for index, provider := range cfg.Providers {
		provider.Type = strings.TrimSpace(provider.Type)
		if provider.Type == "" || provider.Type == "multi" {
			return nil, fmt.Errorf("multi auth: provider %d has an invalid type", index)
		}
		nested, err := auth.NewValidator(provider)
		if err != nil {
			return nil, fmt.Errorf("multi auth: configure provider %d: %w", index, err)
		}
		result.providers = append(result.providers, nested)
	}
	return result, nil
}

func (v *validator) Validate(token string) (*auth.Claims, error) {
	var validationErrors []error
	for _, provider := range v.providers {
		claims, err := provider.Validate(token)
		if err == nil {
			return claims, nil
		}
		validationErrors = append(validationErrors, err)
	}
	return nil, fmt.Errorf("multi auth: token rejected by every provider: %w", errors.Join(validationErrors...))
}
