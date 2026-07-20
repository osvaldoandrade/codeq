// Package authclaims owns security-sensitive interpretation of validated token claims.
package authclaims

import (
	"errors"
	"regexp"
	"strings"

	"github.com/osvaldoandrade/codeq/pkg/auth"
)

var (
	ErrTenantMissing   = errors.New("tenant claim is missing")
	ErrTenantMalformed = errors.New("tenant claim is malformed")
	ErrTenantConflict  = errors.New("tenant claims conflict")
	tenantPattern      = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)
)

var tenantClaimNames = [...]string{"tid", "tenantId", "tenant_id", "organizationId", "organization_id"}

// ResolveTenantID returns the one tenant asserted by a validated token.
// tid is canonical. Legacy aliases remain compatible only when every supplied
// alias has the same non-empty value. Subject fallback is reserved for tokens
// that do not contain any supported tenant claim.
func ResolveTenantID(claims *auth.Claims) (string, error) {
	if claims == nil {
		return "", ErrTenantMissing
	}

	selected := ""
	for _, name := range tenantClaimNames {
		raw, present := claims.Raw[name]
		if !present {
			continue
		}
		value, ok := raw.(string)
		if !ok {
			return "", ErrTenantMalformed
		}
		value = strings.TrimSpace(value)
		if !tenantPattern.MatchString(value) {
			return "", ErrTenantMalformed
		}
		if selected == "" {
			selected = value
			continue
		}
		if selected != value {
			return "", ErrTenantConflict
		}
	}
	if selected != "" {
		return selected, nil
	}

	subject := strings.TrimSpace(claims.Subject)
	if subject == "" {
		return "", ErrTenantMissing
	}
	if !tenantPattern.MatchString(subject) {
		return "", ErrTenantMalformed
	}
	return subject, nil
}
