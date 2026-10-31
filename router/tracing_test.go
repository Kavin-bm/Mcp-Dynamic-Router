package router

import (
	"context"
	"math"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestTracingDisabledRecordsNoSpans(t *testing.T) {
	t.Setenv("MCP_ROUTER_TRACING", "")
	rec := installSpanRecorder(t)
	engine := lexicalEngine(t, DefaultConfig(), weatherAndCalendar()...)
	if _, err := engine.Route(context.Background(), RouteRequest{Utterance: "Will I need an umbrella in Bengaluru today?", Final: true}); err != nil {
		t.Fatal(err)
	}
	if ended := rec.Ended(); len(ended) != 0 {
		t.Fatalf("tracing is off by default, recorded %d spans", len(ended))
	}
}

func TestTracingEnvEnablesSpans(t *testing.T) {
	t.Setenv("MCP_ROUTER_TRACING", "yes")
	rec := installSpanRecorder(t)
	cfg := DefaultConfig()
	cfg.TracingEnabled = false
	cfg.RouteCacheTTL = -1
	engine := lexicalEngine(t, cfg, weatherAndCalendar()...)
	if _, err := engine.Route(context.Background(), RouteRequest{Utterance: "Will I need an umbrella in Bengaluru today?", Final: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := spansByName(rec)[spanRoute]; !ok {
		t.Fatal("MCP_ROUTER_TRACING=yes should emit a router.route span without Config.TracingEnabled")
	}
}

func TestTracingDoesNotChangeRouteResult(t *testing.T) {
	t.Setenv("MCP_ROUTER_TRACING", "")
	installSpanRecorder(t)
	tools := weatherAndCalendar()
	offCfg := DefaultConfig()
	offCfg.RouteCacheTTL = -1
	onCfg := offCfg
	onCfg.TracingEnabled = true
	off := lexicalEngine(t, offCfg, tools...)
	on := lexicalEngine(t, onCfg, tools...)
	queries := []RouteRequest{
		{Utterance: "Will I need an umbrella in Bengaluru today?", Final: true},
		{Utterance: "Tell me a bedtime story", Final: true},
		{Utterance: "check the weather and also schedule a meeting", Final: true},
	}
	for _, query := range queries {
		without, err := off.Route(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		with, err := on.Route(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		if !sameDecision(without, with) {
			t.Fatalf("tracing changed %q\n off=%+v\n on=%+v", query.Utterance, without, with)
		}
	}
}

func TestTracingStagesOnFastPathMiss(t *testing.T) {
	t.Setenv("MCP_ROUTER_TRACING", "")
	rec := installSpanRecorder(t)
	cfg := DefaultConfig()
	cfg.TracingEnabled = true
	cfg.RouteCacheTTL = -1
	cfg.BM25FastPath = 1e9
	cfg.DomainTopK = 2
	engine := lexicalEngine(t, cfg, weatherAndCalendar()...)
	result, err := engine.Route(context.Background(), RouteRequest{Utterance: "weather forecast calendar event", Final: true})
	if err != nil {
		t.Fatal(err)
	}
	found := spansByName(rec)
	root := requireSpan(t, found, spanRoute)
	for _, name := range []string{spanCache, spanFastPath, spanDomain, spanSelect, spanDecide} {
		child := requireSpan(t, found, name)
		if child.Parent().SpanID() != root.SpanContext().SpanID() {
			t.Fatalf("%s is not a child of %s", name, spanRoute)
		}
	}
	if _, ok := found[spanEmbed]; ok {
		t.Fatal("lexical router should not emit router.embed_query")
	}
	fast := found[spanFastPath][0]
	taken, ok := boolAttr(fast, attrFastPath)
	if !ok || taken {
		t.Fatalf("fast-path span taken=%v ok=%v", taken, ok)
	}
	assertScoreAttrs(t, root, result)
	assertScoreAttrs(t, found[spanSelect][0], result)
	assertScoreAttrs(t, found[spanDecide][0], result)
	if len(result.Candidates) < 2 {
		t.Fatalf("expected two candidates so margin is the gap between them, got %+v", result.Candidates)
	}
	gap := result.Candidates[0].Score - result.Candidates[1].Score
	if math.Abs(result.Margin-gap) > 1e-9 {
		t.Fatalf("margin %.4f is not top-second gap %.4f", result.Margin, gap)
	}
}

func TestTracingFastPathSkipsLaterStages(t *testing.T) {
	t.Setenv("MCP_ROUTER_TRACING", "")
	rec := installSpanRecorder(t)
	cfg := DefaultConfig()
	cfg.TracingEnabled = true
	cfg.RouteCacheTTL = -1
	cfg.BM25FastPath = 0.1
	engine := lexicalEngine(t, cfg, weatherTool())
	result, err := engine.Route(context.Background(), RouteRequest{Utterance: "get weather forecast"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != DecisionSelected || len(result.Candidates) != 1 {
		t.Fatalf("expected a fast-path selection, got %+v", result)
	}
	found := spansByName(rec)
	fast := requireSpan(t, found, spanFastPath)
	taken, ok := boolAttr(fast, attrFastPath)
	if !ok || !taken {
		t.Fatalf("expected fast-path span to be taken, ok=%v taken=%v", ok, taken)
	}
	for _, name := range []string{spanDomain, spanSelect, spanEmbed, spanRerank} {
		if _, ok := found[name]; ok {
			t.Fatalf("fast-path should not emit %s", name)
		}
	}
	requireSpan(t, found, spanDecide)
	assertScoreAttrs(t, requireSpan(t, found, spanRoute), result)
}

func TestTracingCacheHitSkipsStages(t *testing.T) {
	t.Setenv("MCP_ROUTER_TRACING", "")
	rec := installSpanRecorder(t)
	cfg := DefaultConfig()
	cfg.TracingEnabled = true
	engine := lexicalEngine(t, cfg, weatherTool())
	if _, err := engine.Route(context.Background(), RouteRequest{Utterance: "get weather forecast"}); err != nil {
		t.Fatal(err)
	}
	rec.Reset()
	result, err := engine.Route(context.Background(), RouteRequest{Utterance: "get weather forecast"})
	if err != nil {
		t.Fatal(err)
	}
	found := spansByName(rec)
	cache := requireSpan(t, found, spanCache)
	hit, ok := boolAttr(cache, attrCacheHit)
	if !ok || !hit {
		t.Fatalf("expected cache hit, ok=%v hit=%v", ok, hit)
	}
	for _, name := range []string{spanFastPath, spanDomain, spanSelect, spanDecide} {
		if _, ok := found[name]; ok {
			t.Fatalf("cache hit should not re-run %s", name)
		}
	}
	assertScoreAttrs(t, requireSpan(t, found, spanRoute), result)
}

func TestTracingMultiIntentFragments(t *testing.T) {
	t.Setenv("MCP_ROUTER_TRACING", "")
	rec := installSpanRecorder(t)
	cfg := DefaultConfig()
	cfg.TracingEnabled = true
	cfg.RouteCacheTTL = -1
	engine := lexicalEngine(t, cfg, weatherAndCalendar()...)
	if _, err := engine.Route(context.Background(), RouteRequest{Utterance: "check the weather and also schedule a meeting", Final: true}); err != nil {
		t.Fatal(err)
	}
	found := spansByName(rec)
	root := requireSpan(t, found, spanRoute)
	split := requireSpan(t, found, spanIntent)
	if split.Parent().SpanID() != root.SpanContext().SpanID() {
		t.Fatal("intent split span should be a child of router.route")
	}
	fragments := found[spanFragment]
	if len(fragments) != 2 {
		t.Fatalf("expected 2 fragment spans, got %d", len(fragments))
	}
	for _, fragment := range fragments {
		if fragment.Parent().SpanID() != root.SpanContext().SpanID() {
			t.Fatal("fragment span should be a child of router.route")
		}
	}
	requireSpan(t, found, spanMerge)
}

func TestTracingEmbedAndRerankSpans(t *testing.T) {
	t.Setenv("MCP_ROUTER_TRACING", "")
	rec := installSpanRecorder(t)
	cfg := DefaultConfig()
	cfg.TracingEnabled = true
	cfg.RouteCacheTTL = -1
	cfg.BM25FastPath = -1
	cfg.DomainTopK = 2
	registry := NewRegistry()
	if err := registry.ReplaceServer("weather", []Tool{weatherTool()}); err != nil {
		t.Fatal(err)
	}
	engine := New(registry, &staticEmbedder{size: 4}, cfg, WithReranker(staticReranker{scores: map[string]float64{
		"weather.get_forecast": 1,
	}}))
	if err := engine.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Route(context.Background(), RouteRequest{Utterance: "umbrella forecast Bengaluru"}); err != nil {
		t.Fatal(err)
	}
	found := spansByName(rec)
	domain := requireSpan(t, found, spanDomain)
	embed := requireSpan(t, found, spanEmbed)
	rerank := requireSpan(t, found, spanRerank)
	selectSpan := requireSpan(t, found, spanSelect)
	if embed.Parent().SpanID() != domain.SpanContext().SpanID() {
		t.Fatal("embed span should be a child of domain filtering")
	}
	if rerank.Parent().SpanID() != selectSpan.SpanContext().SpanID() {
		t.Fatal("rerank span should be a child of tool selection")
	}
}

func TestTracingRecordsRouteError(t *testing.T) {
	t.Setenv("MCP_ROUTER_TRACING", "")
	rec := installSpanRecorder(t)
	cfg := DefaultConfig()
	cfg.TracingEnabled = true
	engine := New(NewRegistry(), nil, cfg)
	_, err := engine.Route(context.Background(), RouteRequest{Utterance: "weather"})
	if err == nil {
		t.Fatal("expected an error before Refresh")
	}
	root := requireSpan(t, spansByName(rec), spanRoute)
	if root.Status().Code != codes.Error {
		t.Fatalf("expected error status, got %s", root.Status().Code)
	}
}

func installSpanRecorder(t *testing.T) *tracetest.SpanRecorder {
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

func lexicalEngine(t *testing.T, cfg Config, tools ...Tool) *Router {
	t.Helper()
	registry := NewRegistry()
	byServer := map[string][]Tool{}
	for _, tool := range tools {
		byServer[tool.Server] = append(byServer[tool.Server], tool)
	}
	for server, serverTools := range byServer {
		if err := registry.ReplaceServer(server, serverTools); err != nil {
			t.Fatal(err)
		}
	}
	engine := New(registry, nil, cfg)
	if err := engine.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return engine
}

func weatherAndCalendar() []Tool {
	return []Tool{
		weatherTool(),
		{ID: "calendar.create_event", Server: "calendar", Domain: "calendar", Name: "create_event", Description: structured("calendar", "Create a calendar event", "the user asks to schedule a meeting", "title and start time", "Does not find weather", "Schedule lunch tomorrow at noon"), InputSchema: []byte(`{"type":"object"}`)},
	}
}

type staticEmbedder struct{ size int }

func (s staticEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	vectors := make([][]float32, len(texts))
	for i := range texts {
		vectors[i] = make([]float32, s.size)
		vectors[i][0] = 1
	}
	return vectors, nil
}

type staticReranker struct{ scores map[string]float64 }

func (s staticReranker) Rerank(context.Context, string, string, []Tool) (map[string]float64, error) {
	return s.scores, nil
}

func spansByName(rec *tracetest.SpanRecorder) map[string][]sdktrace.ReadOnlySpan {
	found := map[string][]sdktrace.ReadOnlySpan{}
	for _, span := range rec.Ended() {
		found[span.Name()] = append(found[span.Name()], span)
	}
	return found
}

func requireSpan(t *testing.T, found map[string][]sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	spans := found[name]
	if len(spans) == 0 {
		t.Fatalf("missing span %s (have %v)", name, spanNames(found))
	}
	return spans[0]
}

func spanNames(found map[string][]sdktrace.ReadOnlySpan) []string {
	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	return names
}

func assertScoreAttrs(t *testing.T, span sdktrace.ReadOnlySpan, result RouteResult) {
	t.Helper()
	count, ok := intAttr(span, attrCandidateCount)
	if !ok || count != len(result.Candidates) {
		t.Fatalf("%s candidate_count=%d ok=%v, result has %d", span.Name(), count, ok, len(result.Candidates))
	}
	top, ok := floatAttr(span, attrTopScore)
	if !ok || math.Abs(top-result.Confidence) > 1e-9 {
		t.Fatalf("%s top_score=%v ok=%v, confidence=%v", span.Name(), top, ok, result.Confidence)
	}
	margin, ok := floatAttr(span, attrMargin)
	if !ok || math.Abs(margin-result.Margin) > 1e-9 {
		t.Fatalf("%s margin=%v ok=%v, result margin=%v", span.Name(), margin, ok, result.Margin)
	}
	if len(result.Candidates) > 1 {
		second, ok := floatAttr(span, attrSecondScore)
		if !ok || math.Abs(second-result.Candidates[1].Score) > 1e-9 {
			t.Fatalf("%s second_score=%v ok=%v, candidate=%v", span.Name(), second, ok, result.Candidates[1].Score)
		}
	}
	decision, ok := stringAttr(span, attrDecision)
	if !ok || decision != string(result.Decision) {
		t.Fatalf("%s decision=%q ok=%v, result=%s", span.Name(), decision, ok, result.Decision)
	}
}

func sameDecision(left, right RouteResult) bool {
	if left.Decision != right.Decision || left.Confidence != right.Confidence || left.Margin != right.Margin || len(left.Candidates) != len(right.Candidates) {
		return false
	}
	for i := range left.Candidates {
		if left.Candidates[i].Tool.ID != right.Candidates[i].Tool.ID || left.Candidates[i].Score != right.Candidates[i].Score || left.Candidates[i].Rank != right.Candidates[i].Rank {
			return false
		}
	}
	return true
}

func intAttr(span sdktrace.ReadOnlySpan, key string) (int, bool) {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return int(kv.Value.AsInt64()), true
		}
	}
	return 0, false
}

func floatAttr(span sdktrace.ReadOnlySpan, key string) (float64, bool) {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsFloat64(), true
		}
	}
	return 0, false
}

func boolAttr(span sdktrace.ReadOnlySpan, key string) (bool, bool) {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsBool(), true
		}
	}
	return false, false
}

func stringAttr(span sdktrace.ReadOnlySpan, key string) (string, bool) {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsString(), true
		}
	}
	return "", false
}
