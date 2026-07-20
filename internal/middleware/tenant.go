package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/osvaldoandrade/codeq/internal/authclaims"
	"github.com/osvaldoandrade/codeq/pkg/auth"
)

const (
	tenantClaimsErrorKey     = "error"
	tenantClaimsErrorMessage = "invalid tenant claims"
)

// GetTenantID extracts tenant ID from the request context
func GetTenantID(c *gin.Context) string {
	if v, ok := c.Get("tenantID"); ok {
		if tenantID, ok := v.(string); ok {
			return tenantID
		}
	}
	return ""
}

// extractTenantID keeps the middleware call site narrow while delegating the
// canonical security policy to authclaims.
func extractTenantID(claims *auth.Claims) (string, error) {
	return authclaims.ResolveTenantID(claims)
}
