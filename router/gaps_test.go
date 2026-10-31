package router

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestConfigDefaultsAndClamps(t *testing.T) {
	engine := New(NewRegistry(), nil, Config{})
	if engine.cfg.CandidateTopK != 5 || engine.cfg.DomainTopK != 2 || engine.cfg.BM25FastPath != 8 || engine.cfg.MinConfidence != 0.42 || engine.cfg.MinMargin != 0.06 {
		t.Fatalf("zero config should pick defaults, got %+v", engine.cfg)
	}
	if engine.cfg.RouteCacheTTL != 30*time.Second {
		t.Fatalf("default cache ttl = %s", engine.cfg.RouteCacheTTL)
	}
	if engine.cfg.TracingEnabled {
		t.Fatal("tracing should be off by default")
	}
	clamped := New(NewRegistry(), nil, Config{MinConfidence: -1, MinMargin: -1, RerankerWeight: -1, BM25FastPath: -1})
	if clamped.cfg.MinConfidence != 0 || clamped.cfg.MinMargin != 0 || clamped.cfg.RerankerWeight != 0 || clamped.cfg.BM25FastPath != 0 {
		t.Fatalf("negative thresholds should clamp to 0, got %+v", clamped.cfg)
	}
}

func TestRefreshEmptyRegistryAndRouteBeforeIndex(t *testing.T) {
	engine := New(NewRegistry(), nil, DefaultConfig())
	if err := engine.Refresh(context.Background()); err == nil {
		t.Fatal("expected refresh to fail on an empty registry")
	}
	if _, err := engine.Route(context.Background(), RouteRequest{Utterance: "weather"}); err == nil {
		t.Fatal("expected route to fail before Refresh")
	}
}

func TestRegistryRejectsInvalidUpdatesWithoutMutating(t *testing.T) {
	registry := NewRegistry()
	if err := registry.ReplaceServer("  ", nil); err == nil {
		t.Fatal("expected empty server name to fail")
	}
	if err := registry.ReplaceServer("calendar", []Tool{{Name: "  "}}); err == nil {
		t.Fatal("expected blank tool name to fail")
	}
	if err := registry.ReplaceServer("calendar", []Tool{{Name: "create", InputSchema: json.RawMessage(`{"type":"object"}`)}}); err != nil {
		t.Fatal(err)
	}
	before := registry.Snapshot()
	err := registry.ReplaceServer("calendar", []Tool{{Name: "create", ID: "calendar.create"}, {Name: "other", ID: "calendar.create"}})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate id error, got %v", err)
	}
	after := registry.Snapshot()
	if after.Version != before.Version || len(after.Tools) != 1 || after.Tools[0].Name != "create" {
		t.Fatalf("failed replace mutated the registry: %+v", after)
	}
	if _, ok := registry.Lookup("missing"); ok {
		t.Fatal("lookup of a missing tool should fail")
	}
	tool, ok := registry.Lookup("calendar.create")
	if !ok {
		t.Fatal("expected calendar.create")
	}
	tool.InputSchema[0] = 'X'
	again, _ := registry.Lookup("calendar.create")
	if len(again.InputSchema) == 0 || again.InputSchema[0] != '{' {
		t.Fatalf("lookup should return a cloned schema, got %s", again.InputSchema)
	}
	registry.RemoveServer("other")
	if registry.Snapshot().Version != before.Version {
		t.Fatal("removing an unknown server should not bump the version")
	}
	registry.RemoveServer("calendar")
	snap := registry.Snapshot()
	if snap.Version != before.Version+1 || len(snap.Tools) != 0 {
		t.Fatalf("unexpected snapshot after remove: %+v", snap)
	}
}

func TestDecideThresholds(t *testing.T) {
	cfg := DefaultConfig()
	if got := decide(nil, cfg); got.Decision != DecisionNoTool {
		t.Fatalf("empty shortlist: %+v", got)
	}
	low := decide([]Candidate{{Tool: Tool{ID: "a"}, Score: 0.1}, {Tool: Tool{ID: "b"}, Score: 0.01}}, cfg)
	if low.Decision != DecisionNoTool {
		t.Fatalf("low confidence: %+v", low)
	}
	close := decide([]Candidate{{Tool: Tool{ID: "a"}, Score: 0.80}, {Tool: Tool{ID: "b"}, Score: 0.79}}, cfg)
	if close.Decision != DecisionClarify || math.Abs(close.Margin-0.01) > 1e-9 {
		t.Fatalf("tight margin: %+v", close)
	}
	clear := decide([]Candidate{{Tool: Tool{ID: "a"}, Score: 0.90}, {Tool: Tool{ID: "b"}, Score: 0.40}}, cfg)
	if clear.Decision != DecisionSelected || math.Abs(clear.Margin-0.50) > 1e-9 {
		t.Fatalf("clear winner: %+v", clear)
	}
	only := decide([]Candidate{{Tool: Tool{ID: "a"}, Score: 0.90}}, cfg)
	if only.Decision != DecisionSelected || only.Margin != 0.90 {
		t.Fatalf("single candidate margin should be the top score, got %+v", only)
	}
}

func TestValidateArgumentsSchemaEdges(t *testing.T) {
	tool := Tool{InputSchema: json.RawMessage(`not-json`)}
	if errors := ValidateArguments(tool, map[string]any{}); len(errors) != 1 {
		t.Fatalf("invalid schema: %v", errors)
	}
	tool = Tool{InputSchema: json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"},"flags":{"type":"array"},"ok":{"type":"boolean"}},"required":["count"],"additionalProperties":false}`)}
	missing := ValidateArguments(tool, map[string]any{"extra": true})
	if len(missing) != 2 {
		t.Fatalf("expected required and additionalProperties errors, got %v", missing)
	}
	bad := ValidateArguments(tool, map[string]any{"count": 1.5, "flags": "nope", "ok": "yes"})
	if len(bad) != 3 {
		t.Fatalf("expected three type errors, got %v", bad)
	}
	ok := ValidateArguments(tool, map[string]any{"count": float64(3), "flags": []any{"a"}, "ok": true})
	if len(ok) != 0 {
		t.Fatalf("expected valid arguments, got %v", ok)
	}
}

func TestExtractDomainFallbacks(t *testing.T) {
	if got := ExtractDomain(Tool{Domain: " Weather "}); got != "weather" {
		t.Fatalf("domain field: %q", got)
	}
	if got := ExtractDomain(Tool{Description: "Domain: Calendar\nPurpose: Create an event."}); got != "calendar" {
		t.Fatalf("description domain: %q", got)
	}
	if got := ExtractDomain(Tool{Server: "Files"}); got != "files" {
		t.Fatalf("server fallback: %q", got)
	}
	if got := ExtractDomain(Tool{}); got != "uncategorized" {
		t.Fatalf("empty fallback: %q", got)
	}
}

func TestRerankerCanChangeTheWinner(t *testing.T) {
	registry := NewRegistry()
	tools := weatherAndCalendar()
	if err := registry.ReplaceServer("weather", tools[:1]); err != nil {
		t.Fatal(err)
	}
	if err := registry.ReplaceServer("calendar", tools[1:]); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.BM25FastPath = -1
	cfg.DomainTopK = 2
	cfg.RouteCacheTTL = -1
	engine := New(registry, nil, cfg, WithReranker(staticReranker{scores: map[string]float64{
		"weather.get_forecast":  0,
		"calendar.create_event": 1,
	}}))
	if err := engine.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := engine.Route(context.Background(), RouteRequest{Utterance: "Will I need an umbrella in Bengaluru today?"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Trace.UsedReranker {
		t.Fatal("expected the reranker to run")
	}
	if len(result.Candidates) == 0 || result.Candidates[0].Tool.ID != "calendar.create_event" {
		t.Fatalf("expected reranker to promote calendar, got %+v", result.Candidates)
	}
}
