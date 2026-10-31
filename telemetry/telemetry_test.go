package telemetry

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

func TestEnabledParsesEnv(t *testing.T) {
	t.Setenv("MCP_ROUTER_TRACING", "")
	if Enabled() {
		t.Fatal("empty env should disable tracing")
	}
	for _, value := range []string{"1", "true", "TRUE", "yes", "on"} {
		t.Setenv("MCP_ROUTER_TRACING", value)
		if !Enabled() {
			t.Fatalf("%q should enable tracing", value)
		}
	}
	t.Setenv("MCP_ROUTER_TRACING", "false")
	if Enabled() {
		t.Fatal("false should disable tracing")
	}
}

func TestSetupStdoutFlushesSpan(t *testing.T) {
	restoreTracingEnv(t)
	t.Setenv("OTEL_TRACES_EXPORTER", "stdout")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_SERVICE_NAME", "router-test")
	previous := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	var buf bytes.Buffer
	shutdown, err := setup(context.Background(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	_, span := otel.Tracer("telemetry.test").Start(context.Background(), "telemetry.probe")
	span.End()
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("telemetry.probe")) {
		t.Fatalf("stdout trace missing probe span: %s", buf.String())
	}
}

func TestSetupOTLPPostsTraces(t *testing.T) {
	restoreTracingEnv(t)
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/v1/traces" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	previous := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	shutdown, err := setup(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, span := otel.Tracer("telemetry.test").Start(context.Background(), "telemetry.otlp")
	span.End()
	_ = shutdown(context.Background())
	if hits.Load() == 0 {
		t.Fatal("expected an OTLP/HTTP export to the configured endpoint")
	}
}

func TestSetupRejectsUnknownExporter(t *testing.T) {
	restoreTracingEnv(t)
	t.Setenv("OTEL_TRACES_EXPORTER", "jaeger")
	if _, err := setup(context.Background(), nil); err == nil {
		t.Fatal("expected unknown exporter to fail")
	}
}

func TestSetupNoneExporter(t *testing.T) {
	restoreTracingEnv(t)
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	previous := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(previous) })
	shutdown, err := setup(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, span := otel.Tracer("telemetry.test").Start(context.Background(), "telemetry.none")
	span.End()
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if span.SpanContext().TraceID() == (trace.TraceID{}) {
		t.Fatal("expected a real trace id even when export is disabled")
	}
}

func restoreTracingEnv(t *testing.T) {
	t.Helper()
	t.Setenv("OTEL_TRACES_EXPORTER", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
}
