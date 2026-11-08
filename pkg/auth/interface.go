package auth

import (
	"context"
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

// ContextValidator is implemented by validators that may wait on a remote
// dependency (for example a JWKS refresh) and stop waiting when the request
// context ends. A wait abandoned this way is never an acceptance.
type ContextValidator interface {
	Validator
	ValidateContext(ctx context.Context, token string) (*Claims, error)
}

// ValidateContext validates token with v, honoring ctx when v implements
// ContextValidator; other validators do not wait and ignore ctx.
func ValidateContext(ctx context.Context, v Validator, token string) (*Claims, error) {
	if cv, ok := v.(ContextValidator); ok {
		return cv.ValidateContext(ctx, token)
	}
	return v.Validate(token)
}
