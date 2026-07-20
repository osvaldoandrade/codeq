// Package authclaims owns security-sensitive interpretation of validated token claims.
package authclaims

import (
	"errors"
	"regexp"
	"strings"

	"github.com/osvaldoandrade/codeq/pkg/auth"
)

var (
	// ErrTenantMissing means neither a tenant claim nor a fallback subject exists.
	ErrTenantMissing = errors.New("tenant claim is missing")
	// ErrTenantMalformed means a supplied tenant value is not a safe tenant label.
	ErrTenantMalformed = errors.New("tenant claim is malformed")
	// ErrTenantConflict means two supported claim aliases identify different tenants.
	ErrTenantConflict = errors.New("tenant claims conflict")
	tenantPattern     = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)
)

const (
	claimTID                 = "tid"
	claimTenantID            = "tenantId"
	claimTenantIDSnake       = "tenant_id"
	claimOrganizationID      = "organizationId"
	claimOrganizationIDSnake = "organization_id"
)

var tenantClaimNames = [...]string{claimTID, claimTenantID, claimTenantIDSnake, claimOrganizationID, claimOrganizationIDSnake}

// ResolveTenantID returns the one tenant asserted by a validated token.
// tid is canonical. Legacy aliases remain compatible only when every supplied
// alias has the same non-empty value. Subject fallback is reserved for tokens
// that do not contain any supported tenant claim.
func ResolveTenantID(claims *auth.Claims) (string, error) {
	if claims == nil {
		return "", ErrTenantMissing
	}
	if selected, found, err := resolveAliases(claims.Raw); found || err != nil {
		return selected, err
	}
	return validateFallbackSubject(claims.Subject)
}

func resolveAliases(raw map[string]interface{}) (string, bool, error) {
	selected := ""
	for _, name := range tenantClaimNames {
		value, present := raw[name]
		if !present {
			continue
		}
		text, ok := value.(string)
		if !ok {
			return "", true, ErrTenantMalformed
		}
		text = strings.TrimSpace(text)
		if !tenantPattern.MatchString(text) {
			return "", true, ErrTenantMalformed
		}
		if selected == "" {
			selected = text
			continue
		}
		if selected != text {
			return "", true, ErrTenantConflict
		}
	}
	if selected != "" {
		return selected, true, nil
	}
	return "", false, nil
}

func validateFallbackSubject(value string) (string, error) {
	subject := strings.TrimSpace(value)
	if subject == "" {
		return "", ErrTenantMissing
	}
	if !tenantPattern.MatchString(subject) {
		return "", ErrTenantMalformed
	}
	return subject, nil
}
