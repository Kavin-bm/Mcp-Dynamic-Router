package router

import (
	"context"
	"encoding/json"
	"testing"
)

func TestAuditToolAcceptsStructuredDescription(t *testing.T) {
	tool := weatherTool()
	report := AuditTool(tool, DefaultDescriptionPolicy())
	if report.Score != 100 || len(report.Issues) != 0 {
		t.Fatalf("expected perfect audit, got score=%d issues=%v", report.Score, report.Issues)
	}
}

func TestAuditToolFindsMissingSectionsAndParameterDocs(t *testing.T) {
	tool := Tool{ID: "calendar.create", Name: "create_event", Description: "Creates an event.", InputSchema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}}}`)}
	report := AuditTool(tool, DefaultDescriptionPolicy())
	if report.Score >= 70 {
		t.Fatalf("expected a poor score, got %d", report.Score)
	}
	if len(report.Issues) < 6 {
		t.Fatalf("expected multiple actionable issues, got %v", report.Issues)
	}
}

func TestRegistryReplaceServerIsAtomic(t *testing.T) {
	registry := NewRegistry()
	if err := registry.ReplaceServer("one", []Tool{{Name: "first"}, {Name: "second"}}); err != nil {
		t.Fatal(err)
	}
	if err := registry.ReplaceServer("one", []Tool{{Name: "third"}}); err != nil {
		t.Fatal(err)
	}
	snapshot := registry.Snapshot()
	if snapshot.Version != 2 || len(snapshot.Tools) != 1 || snapshot.Tools[0].ID != "one.third" {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
}

func TestRouterUsesDomainThenSelectsTool(t *testing.T) {
	registry := NewRegistry()
	tools := []Tool{
		weatherTool(),
		{ID: "calendar.create_event", Server: "calendar", Domain: "calendar", Name: "create_event", Description: structured("calendar", "Create a calendar event", "the user asks to schedule a meeting", "title and start time", "Does not find weather", "Schedule lunch tomorrow at noon"), InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	if err := registry.ReplaceServer("weather", tools[:1]); err != nil {
		t.Fatal(err)
	}
	if err := registry.ReplaceServer("calendar", tools[1:]); err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig()
	config.DomainTopK = 1
	router := New(registry, nil, config)
	if err := router.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := router.Route(context.Background(), RouteRequest{Utterance: "Will I need an umbrella in Bengaluru today?", Final: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != DecisionSelected || result.Candidates[0].Tool.ID != "weather.get_forecast" {
		t.Fatalf("unexpected route: %+v", result)
	}
	if len(result.Trace.Domains) != 1 || result.Trace.Domains[0] != "weather" {
		t.Fatalf("unexpected domains: %v", result.Trace.Domains)
	}
}

func TestRouterAbstainsForUnrelatedQuery(t *testing.T) {
	registry := NewRegistry()
	if err := registry.ReplaceServer("weather", []Tool{weatherTool()}); err != nil {
		t.Fatal(err)
	}
	router := New(registry, nil, DefaultConfig())
	if err := router.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := router.Route(context.Background(), RouteRequest{Utterance: "Tell me a bedtime story"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != DecisionNoTool {
		t.Fatalf("expected no_tool, got %+v", result)
	}
}

func TestRouterHandlesMultiIntentQueries(t *testing.T) {
	registry := NewRegistry()
	tools := []Tool{
		weatherTool(),
		{ID: "calendar.create_event", Server: "calendar", Domain: "calendar", Name: "create_event", Description: structured("calendar", "Create a calendar event", "the user asks to schedule a meeting", "title and start time", "Does not find weather", "Schedule lunch tomorrow at noon"), InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	if err := registry.ReplaceServer("weather", tools[:1]); err != nil {
		t.Fatal(err)
	}
	if err := registry.ReplaceServer("calendar", tools[1:]); err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig()
	config.DomainTopK = 2
	router := New(registry, nil, config)
	if err := router.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Compound query
	query := "Will I need an umbrella in Bengaluru today? And also schedule lunch tomorrow at noon"
	result, err := router.Route(context.Background(), RouteRequest{Utterance: query, Final: true})
	if err != nil {
		t.Fatal(err)
	}

	if result.Decision != DecisionSelected {
		t.Fatalf("expected selected decision, got %+v", result)
	}

	if len(result.Candidates) != 2 {
		t.Fatalf("expected 2 candidates (one for weather, one for calendar), got %d", len(result.Candidates))
	}

	// Weather candidate should be one of them and calendar should be the other
	hasWeather := false
	hasCalendar := false
	for _, c := range result.Candidates {
		if c.Tool.ID == "weather.get_forecast" {
			hasWeather = true
		}
		if c.Tool.ID == "calendar.create_event" {
			hasCalendar = true
		}
	}

	if !hasWeather || !hasCalendar {
		t.Fatalf("expected both weather and calendar tools to be resolved, got weather=%v calendar=%v", hasWeather, hasCalendar)
	}
}

func TestValidateArguments(t *testing.T) {
	tool := weatherTool()
	errors := ValidateArguments(tool, map[string]any{"location": 42, "units": "kelvin"})
	if len(errors) != 2 {
		t.Fatalf("expected two errors, got %v", errors)
	}
	if errors[0].Field != "location" || errors[1].Field != "units" {
		t.Fatalf("unexpected errors: %v", errors)
	}
	if errors := ValidateArguments(tool, map[string]any{"location": "Bengaluru", "units": "metric"}); len(errors) != 0 {
		t.Fatalf("unexpected errors: %v", errors)
	}
}

type trackingEmbedder struct {
	called bool
}

func (m *trackingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	m.called = true
	res := make([][]float32, len(texts))
	for i := range texts {
		res[i] = make([]float32, 2)
	}
	return res, nil
}

func TestRouterBM25FastPath(t *testing.T) {
	registry := NewRegistry()
	tool := Tool{
		ID:          "weather.get_forecast",
		Server:      "weather",
		Domain:      "weather",
		Name:        "get_forecast",
		Description: "get weather forecast",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
	if err := registry.ReplaceServer("weather", []Tool{tool}); err != nil {
		t.Fatal(err)
	}

	embedder := &trackingEmbedder{}
	config := DefaultConfig()
	config.BM25FastPath = 0.1 // Set very low so any query matching vocab triggers fast-path
	router := New(registry, embedder, config)
	if err := router.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Reset called flag since Refresh invokes the embedder to build the index
	embedder.called = false

	result, err := router.Route(context.Background(), RouteRequest{
		Utterance: "get weather forecast",
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("Decision: %s, Candidates: %+v, Reason: %s", result.Decision, result.Candidates, result.Reason)

	if result.Decision != DecisionSelected || result.Candidates[0].Tool.ID != "weather.get_forecast" {
		t.Fatalf("unexpected result: %+v", result)
	}

	if embedder.called {
		t.Fatal("expected embedder to NOT be called (fast-path should bypass semantic search)")
	}
}

func TestRouterRouteCaching(t *testing.T) {
	registry := NewRegistry()
	tool := Tool{
		ID:          "weather.get_forecast",
		Server:      "weather",
		Domain:      "weather",
		Name:        "get_forecast",
		Description: "get weather forecast",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
	if err := registry.ReplaceServer("weather", []Tool{tool}); err != nil {
		t.Fatal(err)
	}

	router := New(registry, nil, DefaultConfig())
	if err := router.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	// First query
	res1, err := router.Route(context.Background(), RouteRequest{Utterance: "get weather forecast"})
	if err != nil {
		t.Fatal(err)
	}

	// Modify the registry and invalidate compilation index,
	// verifying that we get the cached result.
	router.registry.RemoveServer("weather")
	router.index = nil // Invalidate to force crash if cache misses

	res2, err := router.Route(context.Background(), RouteRequest{Utterance: "get weather forecast"})
	if err != nil {
		t.Fatal(err)
	}

	if res2.Decision != res1.Decision || len(res2.Candidates) != len(res1.Candidates) {
		t.Fatalf("cached result mismatch: %+v vs %+v", res2, res1)
	}
}

func weatherTool() Tool {
	return Tool{ID: "weather.get_forecast", Server: "weather", Domain: "weather", Name: "get_forecast", Description: structured("weather", "Get the current weather and short-term forecast for a location", "the user asks about temperature, rain, or forecast", "location is a city; units is metric or imperial", "Does not provide historical climate data", "Will I need an umbrella in Bengaluru today?"), InputSchema: json.RawMessage(`{"type":"object","properties":{"location":{"type":"string","description":"City or place name"},"units":{"type":"string","description":"Unit system","enum":["metric","imperial"]}},"required":["location"],"additionalProperties":false}`), ReadOnly: true}
}

func structured(domain, purpose, invoke, parameters, limitations, example string) string {
	return "Domain: " + domain + "\nPurpose: " + purpose + ".\nInvoke when: Call this tool when " + invoke + ".\nParameters: " + parameters + ".\nLimitations: " + limitations + ".\nExample: User: " + example + " Arguments: {}"
}

func TestStemmingTokenization(t *testing.T) {
	tokens := tokenize("scheduling calendar meetings")
	expected := []string{"schedul", "calendar", "meeting"}
	if len(tokens) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, tokens)
	}
	for i := range tokens {
		if tokens[i] != expected[i] {
			t.Errorf("at index %d: expected %s, got %s", i, expected[i], tokens[i])
		}
	}
}

func TestSplitIntents(t *testing.T) {
	cases := []struct {
		name     string
		query    string
		expected []string
	}{
		{
			name:     "normal two-intent split",
			query:    "check the weather and also schedule a meeting",
			expected: []string{"check the weather", "schedule a meeting"},
		},
		{
			name:     "numeric continuation is NOT split",
			query:    "add 2 and also 3 to the list",
			expected: []string{"add 2 and also 3 to the list"},
		},
		{
			name:     "both separators numeric-guarded — no split fallback",
			query:    "add 2 and also 3 and also 4",
			expected: []string{"add 2 and also 3 and also 4"},
		},
		{
			name:  "leading 'and also' — 'also' matches as standalone separator",
			query: "and also buy milk and also send email",
			// The regex treats " also " as a valid separator on its own (via the
			// |also| branch), so "and also buy milk" is split into ["and", "buy milk"].
			// The second " and also " then yields "send email". Result: 3 fragments.
			expected: []string{"and", "buy milk", "send email"},
		},
		{
			name:  "first match numeric-guarded, second match valid",
			query: "add 2 and also 3 and also buy milk",
			// first separator skipped (numeric after), second consumed
			expected: []string{"add 2 and also 3", "buy milk"},
		},
		{
			name:     "no separator — returns input unchanged",
			query:    "get the weather forecast",
			expected: []string{"get the weather forecast"},
		},
		{
			name:     "three intents",
			query:    "check weather then book a table and also send a recap email",
			expected: []string{"check weather", "book a table", "send a recap email"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitIntents(tc.query)
			if len(got) != len(tc.expected) {
				t.Fatalf("len mismatch: expected %v, got %v", tc.expected, got)
			}
			for i := range got {
				if got[i] != tc.expected[i] {
					t.Errorf("fragment[%d]: expected %q, got %q", i, tc.expected[i], got[i])
				}
			}
		})
	}
}
