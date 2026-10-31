package streamrag

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kavinbm16/mcp-dynamic-router/router"
)

func TestPartialPredictionsBecomeStableAndPrefetchReadOnlyTool(t *testing.T) {
	engine := testRouter(t)
	prefetched := make(chan string, 1)
	session := New(engine, DefaultOptions(), Hooks{OnPrefetch: func(_ context.Context, tool router.Tool) error {
		prefetched <- tool.ID
		return nil
	}})
	defer session.Close()

	first, err := session.Update(context.Background(), Event{Transcript: "weather forecast Bengaluru", Confidence: .9})
	if err != nil {
		t.Fatal(err)
	}
	if first.Stable {
		t.Fatal("first prediction should not be stable")
	}
	second, err := session.Update(context.Background(), Event{Transcript: "weather forecast Bengaluru tomorrow", Confidence: .92})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Stable {
		t.Fatalf("second matching prediction should be stable: %+v", second)
	}
	select {
	case toolID := <-prefetched:
		if toolID != "weather.forecast" {
			t.Fatalf("unexpected prefetch: %s", toolID)
		}
	case <-time.After(time.Second):
		t.Fatal("expected read-only prefetch")
	}
}

func TestFinalTranscriptAlwaysRoutesAndCommits(t *testing.T) {
	committed := make(chan Prediction, 1)
	session := New(testRouter(t), DefaultOptions(), Hooks{OnCommit: func(prediction Prediction) { committed <- prediction }})
	defer session.Close()
	prediction, err := session.Update(context.Background(), Event{Transcript: "weather", Final: true, Confidence: .1})
	if err != nil {
		t.Fatal(err)
	}
	if !prediction.Triggered || !prediction.Final {
		t.Fatalf("unexpected prediction: %+v", prediction)
	}
	select {
	case <-committed:
	case <-time.After(time.Second):
		t.Fatal("expected commit callback")
	}
}

func TestLowConfidencePartialIsIgnored(t *testing.T) {
	session := New(testRouter(t), DefaultOptions(), Hooks{})
	defer session.Close()
	prediction, err := session.Update(context.Background(), Event{Transcript: "weather forecast Bengaluru", Confidence: 0.2})
	if err != nil {
		t.Fatal(err)
	}
	if prediction.Triggered || prediction.Reason == "" {
		t.Fatalf("low-confidence partial should be ignored: %+v", prediction)
	}
}

func TestClosedSessionRejectsUpdates(t *testing.T) {
	session := New(testRouter(t), DefaultOptions(), Hooks{})
	session.Close()
	if _, err := session.Update(context.Background(), Event{Transcript: "weather forecast Bengaluru", Final: true}); err == nil {
		t.Fatal("expected a closed session to reject updates")
	}
}

func TestShortPartialIsIgnored(t *testing.T) {
	session := New(testRouter(t), DefaultOptions(), Hooks{})
	defer session.Close()
	prediction, err := session.Update(context.Background(), Event{Transcript: "weath", Confidence: .9})
	if err != nil {
		t.Fatal(err)
	}
	if prediction.Triggered {
		t.Fatalf("short partial should be ignored: %+v", prediction)
	}
}

func testRouter(t *testing.T) *router.Router {
	t.Helper()
	registry := router.NewRegistry()
	tool := router.Tool{ID: "weather.forecast", Server: "weather", Name: "forecast", Domain: "weather", ReadOnly: true, Description: "Domain: weather\nPurpose: Get a weather forecast.\nInvoke when: Call for weather, rain, or temperature.\nParameters: location is a city.\nLimitations: No historical data.\nExample: weather forecast Bengaluru tomorrow", InputSchema: json.RawMessage(`{"type":"object"}`)}
	if err := registry.ReplaceServer("weather", []router.Tool{tool}); err != nil {
		t.Fatal(err)
	}
	engine := router.New(registry, nil, router.DefaultConfig())
	if err := engine.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return engine
}
