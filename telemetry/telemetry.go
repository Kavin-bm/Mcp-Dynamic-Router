// Package telemetry installs an OpenTelemetry tracer provider for the router.
// Tracing is off unless the process opts in. The router records spans only
// when Config.TracingEnabled is set or MCP_ROUTER_TRACING is 1, true, yes,
// or on; this package exports those spans.
package telemetry

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Enabled reports whether MCP_ROUTER_TRACING requests traces.
// Accepted values: 1, true, yes, on (case-insensitive). Keep this list in
// sync with router.envTracingEnabled.
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MCP_ROUTER_TRACING"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// Setup installs a global TracerProvider and W3C trace-context propagator.
// The exporter is chosen from the environment:
//
//   - OTEL_TRACES_EXPORTER=none disables export
//   - OTEL_TRACES_EXPORTER=otlp, or an OTEL_EXPORTER_OTLP_ENDPOINT /
//     OTEL_EXPORTER_OTLP_TRACES_ENDPOINT value, uses OTLP/HTTP
//   - otherwise spans are written to stdout as JSON, which is the local
//     default when routerd is started with -tracing and no collector
//
// The returned function flushes and shuts the provider down.
func Setup(ctx context.Context) (func(context.Context) error, error) {
	return setup(ctx, nil)
}

func setup(ctx context.Context, stdout io.Writer) (func(context.Context) error, error) {
	exporter, err := newExporter(ctx, stdout)
	if err != nil {
		return nil, err
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(serviceResource()),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return provider.Shutdown, nil
}

func serviceResource() *resource.Resource {
	name := strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME"))
	if name == "" {
		name = "mcp-dynamic-router"
	}
	res, err := resource.New(context.Background(),
		resource.WithAttributes(attribute.String("service.name", name)),
	)
	if err != nil {
		return resource.Default()
	}
	return res
}

func newExporter(ctx context.Context, stdout io.Writer) (sdktrace.SpanExporter, error) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_TRACES_EXPORTER"))) {
	case "none":
		return noopExporter{}, nil
	case "otlp":
		return otlptracehttp.New(ctx)
	case "stdout":
		return stdoutExporter(stdout)
	case "":
		if strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) != "" || strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")) != "" {
			return otlptracehttp.New(ctx)
		}
		return stdoutExporter(stdout)
	default:
		return nil, fmt.Errorf("unsupported OTEL_TRACES_EXPORTER %q (use otlp, stdout, or none)", strings.TrimSpace(os.Getenv("OTEL_TRACES_EXPORTER")))
	}
}

func stdoutExporter(stdout io.Writer) (sdktrace.SpanExporter, error) {
	options := []stdouttrace.Option{}
	if stdout != nil {
		options = append(options, stdouttrace.WithWriter(stdout))
	}
	return stdouttrace.New(options...)
}

// noopExporter drops spans. It is used when OTEL_TRACES_EXPORTER=none so a
// TracerProvider still exists and the router can create spans without export.
type noopExporter struct{}

func (noopExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }
func (noopExporter) Shutdown(context.Context) error                             { return nil }
