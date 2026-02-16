package tracing

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"google.golang.org/grpc/credentials"
)

type Config struct {
	Enabled     bool
	ServiceName string

	OTLPEndpoint string
	OTLPInsecure bool

	SampleRatio float64
}

func Setup(ctx context.Context, cfg Config, logger *slog.Logger) (func(context.Context) error, error) {
	if logger == nil {
		logger = slog.Default()
	}

	if !cfg.Enabled {
		// Still set the propagator to ensure consistent header propagation if a
		// downstream component is instrumented separately.
		otel.SetTextMapPropagator(defaultPropagator())
		return func(context.Context) error { return nil }, nil
	}

	serviceName := strings.TrimSpace(cfg.ServiceName)
	if serviceName == "" {
		serviceName = strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME"))
	}
	if serviceName == "" {
		serviceName = "codeq"
	}

	endpoint := strings.TrimSpace(cfg.OTLPEndpoint)
	if endpoint == "" {
		endpoint = strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	}
	if endpoint == "" {
		endpoint = "localhost:4317"
	}
	endpoint = sanitizeEndpoint(endpoint)

	insecure := cfg.OTLPInsecure
	if v := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_INSECURE")); v != "" {
		insecure = parseBool(v)
	}

	sampleRatio := cfg.SampleRatio
	if sampleRatio <= 0 || sampleRatio > 1 {
		sampleRatio = 1
	}

	expOpts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(endpoint),
	}
	if insecure {
		expOpts = append(expOpts, otlptracegrpc.WithInsecure())
	} else {
		expOpts = append(expOpts, otlptracegrpc.WithTLSCredentials(credentials.NewClientTLSFromCert(nil, "")))
	}

	exp, err := otlptracegrpc.New(ctx, expOpts...)
	if err != nil {
		// Fail open to avoid making tracing a hard dependency at runtime.
		logger.Warn("otel exporter init failed; tracing disabled", "err", err)
		otel.SetTextMapPropagator(defaultPropagator())
		return func(context.Context) error { return nil }, nil
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(serviceName),
	))
	if err != nil {
		// Resource failures shouldn't prevent tracing.
		logger.Warn("otel resource init failed; using default", "err", err)
		res = resource.Default()
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRatio))),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(defaultPropagator())

	return tp.Shutdown, nil
}

func defaultPropagator() propagation.TextMapPropagator {
	return propagation.TraceContext{}
}

// webhookPropagator returns a propagator that only includes TraceContext,
// excluding Baggage to prevent unintentional propagation of sensitive data
// to third-party webhook endpoints.
func webhookPropagator() propagation.TextMapPropagator {
	return propagation.TraceContext{}
}

func sanitizeEndpoint(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}
	// OTEL_EXPORTER_OTLP_ENDPOINT is often configured as a URL. The gRPC exporter expects host:port.
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			return u.Host
		}
	}
	return strings.TrimSuffix(raw, "/")
}

func parseBool(v string) bool {
	v = strings.TrimSpace(strings.ToLower(v))
	return v == "true" || v == "1" || v == "yes" || v == "y" || v == "on"
}

// TraceContextStrings returns the W3C trace context strings for the current span in ctx.
func TraceContextStrings(ctx context.Context) (traceParent string, traceState string) {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return carrier.Get("traceparent"), carrier.Get("tracestate")
}

// ContextWithRemoteParent builds a context with a remote parent span context using W3C trace context strings.
func ContextWithRemoteParent(ctx context.Context, traceParent string, traceState string) context.Context {
	traceParent = strings.TrimSpace(traceParent)
	traceState = strings.TrimSpace(traceState)
	if traceParent == "" && traceState == "" {
		return ctx
	}
	carrier := propagation.MapCarrier{}
	if traceParent != "" {
		carrier.Set("traceparent", traceParent)
	}
	if traceState != "" {
		carrier.Set("tracestate", traceState)
	}
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}

// InjectHeaders injects W3C trace context headers into the provided http.Header.
// Only traceparent and tracestate headers are injected; baggage is explicitly
// excluded to prevent unintentional propagation of sensitive data to third-party
// webhook endpoints.
func InjectHeaders(ctx context.Context, h http.Header) {
	if h == nil {
		return
	}
	webhookPropagator().Inject(ctx, propagation.HeaderCarrier(h))
}

func ParseSampleRatio(v string) float64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0
	}
	return f
}
