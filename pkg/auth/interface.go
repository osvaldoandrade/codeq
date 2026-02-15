package auth

import (
	"time"
)

// Claims represents authentication token claims
type Claims struct {
	Subject    string
	Email      string
	Issuer     string
	Audience   []string
	ExpiresAt  time.Time
	IssuedAt   time.Time
	Scopes     []string
	EventTypes []string
	Raw        map[string]interface{}
}

// HasScope checks if the claims contain a specific scope
func (c *Claims) HasScope(scope string) bool {
	if c == nil {
		return false
	}
	for _, s := range c.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// Validator validates authentication tokens
type Validator interface {
	Validate(token string) (*Claims, error)
}

// ValidatorFactory creates a validator instance
type ValidatorFactory interface {
	NewValidator(config Config) (Validator, error)
}

// Config contains validator configuration
type Config struct {
	JwksURL     string
	Issuer      string
	Audience    string
	ClockSkew   time.Duration
	HTTPTimeout time.Duration
}
