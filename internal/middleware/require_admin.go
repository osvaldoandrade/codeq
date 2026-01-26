package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func RequireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		v, _ := c.Get("userRole")
		if role, _ := v.(string); role != "ADMIN" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized. Admin only"})
			return
		}
		c.Next()
	}
}
