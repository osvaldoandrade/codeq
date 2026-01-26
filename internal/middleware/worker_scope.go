package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func RequireWorkerScope(scope string) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := GetWorkerClaims(c)
		if !ok || claims == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing worker claims"})
			return
		}
		if !scopeHas(claims.Scope, scope) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "missing scope"})
			return
		}
		c.Next()
	}
}

func scopeHas(scopes string, want string) bool {
	for _, s := range strings.Fields(scopes) {
		if s == want {
			return true
		}
	}
	return false
}
