package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"
	identitymw "github.com/osvaldoandrade/codeq/internal/identitymw"
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

// extractTenantID extracts tenant ID from JWT claims
func extractTenantID(claims *auth.Claims) string {
	if claims == nil || claims.Raw == nil {
		return ""
	}

	// Try multiple common claim names for tenant ID
	tenantID := ""
	if v, ok := claims.Raw["tenantId"].(string); ok {
		tenantID = strings.TrimSpace(v)
	} else if v, ok := claims.Raw["tenant_id"].(string); ok {
		tenantID = strings.TrimSpace(v)
	} else if v, ok := claims.Raw["organizationId"].(string); ok {
		tenantID = strings.TrimSpace(v)
	} else if v, ok := claims.Raw["organization_id"].(string); ok {
		tenantID = strings.TrimSpace(v)
	}

	// Fall back to using subject as tenant for single-tenant scenarios
	if tenantID == "" {
		tenantID = strings.TrimSpace(claims.Subject)
	}

	return tenantID
}
