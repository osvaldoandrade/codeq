package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// TracingMiddleware extracts W3C trace context from incoming requests and starts a span
// for the HTTP handler chain. This avoids depending on otelgin, while still enabling
// distributed tracing correlation.
func TracingMiddleware(serviceName string) gin.HandlerFunc {
	if strings.TrimSpace(serviceName) == "" {
		serviceName = "codeq"
	}
	tracer := otel.Tracer(serviceName + "/http")

	return func(c *gin.Context) {
		ctx := otel.GetTextMapPropagator().Extract(c.Request.Context(), propagation.HeaderCarrier(c.Request.Header))
		name := "HTTP " + c.Request.Method + " " + c.Request.URL.Path
		ctx, span := tracer.Start(ctx, name,
			trace.WithAttributes(
				attribute.String("http.method", c.Request.Method),
				attribute.String("http.path", c.Request.URL.Path),
				attribute.String("http.host", c.Request.Host),
			),
		)
		c.Request = c.Request.WithContext(ctx)

		c.Next()

		status := c.Writer.Status()
		route := c.FullPath()
		if route != "" {
			span.SetName("HTTP " + c.Request.Method + " " + route)
			span.SetAttributes(attribute.String("http.route", route))
		}
		span.SetAttributes(attribute.Int("http.status_code", status))
		if status >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, http.StatusText(status))
		}
		span.End()
	}
}
