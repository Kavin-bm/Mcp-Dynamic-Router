package dynamicrouter_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	dynamicrouter "github.com/kavinbm16/mcp-dynamic-router"
	"github.com/kavinbm16/mcp-dynamic-router/router"
)

func TestBuilderWithoutTracingEmitsNoSpans(t *testing.T) {
	t.Setenv("MCP_ROUTER_TRACING", "")
	rec := installRecorder(t)
	app := dynamicrouter.NewBuilder().Build()
	if err := app.Registry.ReplaceServer("weather", []router.Tool{forecastTool()}); err != nil {
		t.Fatal(err)
	}
	if err := app.Router.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Route(context.Background(), router.RouteRequest{Utterance: "weather forecast Bengaluru", Final: true}); err != nil {
		t.Fatal(err)
	}
	if ended := rec.Ended(); len(ended) != 0 {
		t.Fatalf("default builder should not emit spans, got %d", len(ended))
	}
}

func TestBuilderWithTracingEmitsRouteSpan(t *testing.T) {
	t.Setenv("MCP_ROUTER_TRACING", "")
	rec := installRecorder(t)
	app := dynamicrouter.NewBuilder().WithTracing(true).Build()
	if err := app.Registry.ReplaceServer("weather", []router.Tool{forecastTool()}); err != nil {
		t.Fatal(err)
	}
	if err := app.Router.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := app.Route(context.Background(), router.RouteRequest{Utterance: "weather forecast Bengaluru", Final: true})
	if err != nil {
		t.Fatal(err)
	}
	var root sdktrace.ReadOnlySpan
	for _, span := range rec.Ended() {
		if span.Name() == "router.route" {
			root = span
		}
	}
	if root == nil {
		t.Fatal("WithTracing(true) should emit router.route")
	}
	var count int
	var sawCount bool
	for _, kv := range root.Attributes() {
		if string(kv.Key) == "router.candidate_count" {
			count = int(kv.Value.AsInt64())
			sawCount = true
		}
	}
	if !sawCount || count != len(result.Candidates) {
		t.Fatalf("candidate_count attr=%d ok=%v result=%d", count, sawCount, len(result.Candidates))
	}
}

func forecastTool() router.Tool {
	return router.Tool{
		ID: "weather.forecast", Server: "weather", Name: "forecast", Domain: "weather",
		Description: "Domain: weather\nPurpose: Get a weather forecast.\nInvoke when: Call for weather, rain, or temperature.\nParameters: location is a city.\nLimitations: No historical data.\nExample: weather forecast Bengaluru tomorrow",
		InputSchema: []byte(`{"type":"object"}`),
	}
}

func installRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = provider.Shutdown(context.Background())
	})
	return rec
}
