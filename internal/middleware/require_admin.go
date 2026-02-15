package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	identitymw "github.com/osvaldoandrade/codeq/internal/identitymw"
)

func RequireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if v, ok := c.Get("userClaims"); ok {
			if claims, ok := v.(*auth.Claims); ok && claims != nil {
				if claims.HasScope(adminScope) {
					c.Next()
					return
				}
				if role, ok := claims.Raw["role"].(string); ok && strings.EqualFold(role, "ADMIN") {
					c.Next()
					return
				}
			}
		}
		v, _ := c.Get("userRole")
		if role, _ := v.(string); strings.EqualFold(role, "ADMIN") {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized. Admin only"})
	}
}
