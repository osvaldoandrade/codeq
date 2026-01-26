package middleware

import (
	"github.com/gin-gonic/gin"
	"log/slog"
)

func LoggerMiddleware(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("logger", logger)
		c.Next()
	}
}
