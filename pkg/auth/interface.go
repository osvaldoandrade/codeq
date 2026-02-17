package auth

import (
	"slices"
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
	return slices.Contains(c.Scopes, scope)
}

// Validator validates authentication tokens
type Validator interface {
	Validate(token string) (*Claims, error)
}
