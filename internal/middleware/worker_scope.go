package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func RequireWorkerScope(scope string) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := GetWorkerClaims(c)
		if !ok || claims == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing worker claims"})
			return
		}
		if !claims.HasScope(scope) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "missing scope"})
			return
		}
		c.Next()
	}
}
