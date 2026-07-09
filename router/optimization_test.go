package router

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type mockEmbedder struct {
	calls      int64
	embeds     int64
	vectorSize int
}

func (m *mockEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	atomic.AddInt64(&m.calls, 1)
	atomic.AddInt64(&m.embeds, int64(len(texts)))
	time.Sleep(50 * time.Millisecond)

	results := make([][]float32, len(texts))
	for idx := range texts {
		vec := make([]float32, m.vectorSize)
		// Return some dummy vector values
		for i := 0; i < m.vectorSize; i++ {
			vec[i] = 1.0 / float32(m.vectorSize)
		}
		results[idx] = vec
	}
	return results, nil
}

func TestIncrementalEmbeddingCache(t *testing.T) {
	embedder := &mockEmbedder{vectorSize: 1536}
	registry := NewRegistry()
	router := New(registry, embedder, DefaultConfig())

	// Add 2 initial tools
	tools := []Tool{
		{ID: "weather.get", Name: "get_weather", Server: "weather", Domain: "weather", Description: "Get current weather for location", InputSchema: json.RawMessage(`{}`)},
		{ID: "calendar.get", Name: "get_events", Server: "calendar", Domain: "calendar", Description: "Get calendar events", InputSchema: json.RawMessage(`{}`)},
	}
	if err := registry.ReplaceServer("weather", tools[:1]); err != nil {
		t.Fatal(err)
	}
	if err := registry.ReplaceServer("calendar", tools[1:]); err != nil {
		t.Fatal(err)
	}

	// 1st Refresh: Cache is empty, must embed both tools and domains (2 tools, 2 domains)
	if err := router.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	initialEmbeds := atomic.LoadInt64(&embedder.embeds)
	if initialEmbeds != 4 { // 2 tools + 2 domains
		t.Fatalf("expected 4 initial embeds, got %d", initialEmbeds)
	}

	// Add a 3rd tool
	extraTool := Tool{ID: "weather.forecast", Name: "get_forecast", Server: "weather", Domain: "weather", Description: "Get weather forecast", InputSchema: json.RawMessage(`{}`)}
	// Replacing "weather" updates the registry
	if err := registry.ReplaceServer("weather", []Tool{tools[0], extraTool}); err != nil {
		t.Fatal(err)
	}

	// Reset embeds counter
	atomic.StoreInt64(&embedder.embeds, 0)

	// 2nd Refresh:
	// - Tools "weather.get" and "calendar.get" are cached.
	// - "weather.forecast" is new (miss).
	// - Domain "calendar" is cached (no changes to calendar tools).
	// - Domain "weather" changed, so it's a miss.
	// We expect only 2 text embeds: 1 tool ("weather.forecast") and 1 domain ("weather").
	if err := router.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	subsequentEmbeds := atomic.LoadInt64(&embedder.embeds)
	if subsequentEmbeds != 2 {
		t.Fatalf("expected 2 incremental embeds (1 tool + 1 domain), got %d", subsequentEmbeds)
	}
}

func TestSingleflightCollapsing(t *testing.T) {
	embedder := &mockEmbedder{vectorSize: 1536}
	registry := NewRegistry()
	router := New(registry, embedder, DefaultConfig())

	tool := Tool{ID: "weather.get", Name: "get_weather", Server: "weather", Domain: "weather", Description: "Get weather", InputSchema: json.RawMessage(`{}`)}
	if err := registry.ReplaceServer("weather", []Tool{tool}); err != nil {
		t.Fatal(err)
	}
	if err := router.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Reset calls counter
	atomic.StoreInt64(&embedder.calls, 0)

	// Issue 10 concurrent route requests for the same query.
	var wg sync.WaitGroup
	concurrency := 10
	errorsChan := make(chan error, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := router.Route(context.Background(), RouteRequest{
				Utterance: "Is it raining in Bengaluru?",
				Final:     true,
			})
			if err != nil {
				errorsChan <- err
			}
		}()
	}

	wg.Wait()
	close(errorsChan)

	for err := range errorsChan {
		t.Error(err)
	}

	calls := atomic.LoadInt64(&embedder.calls)
	// We expect the calls to be collapsed to 1 or very few (due to concurrency scheduling), but definitely less than 10.
	// Usually 1 since they run exactly concurrently.
	if calls > 3 {
		t.Errorf("expected singleflight request collapsing to restrict calls, got %d calls", calls)
	}
	fmt.Printf("Singleflight calls collapsed to: %d/%d\n", calls, concurrency)
}

func TestConcurrentRefreshCollapsesDuplicateEmbedCalls(t *testing.T) {
	embedder := &mockEmbedder{vectorSize: 4}
	registry := NewRegistry()
	router := New(registry, embedder, DefaultConfig())

	tool := Tool{
		ID: "weather.get", Name: "get_weather", Server: "weather",
		Domain: "weather", Description: "Get weather forecast for a city",
		InputSchema: json.RawMessage(`{}`),
	}
	if err := registry.ReplaceServer("weather", []Tool{tool}); err != nil {
		t.Fatal(err)
	}

	// Fire N concurrent Refresh calls against the same registry version.
	// The singleflight key is "refresh-v<version>", so all calls for the
	// same version must collapse to a single embedder invocation.
	const concurrency = 10
	var wg sync.WaitGroup
	errs := make(chan error, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := router.Refresh(context.Background()); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	calls := atomic.LoadInt64(&embedder.calls)
	// With singleflight on the registry version, at most 1 call should reach
	// the embedder (all 10 goroutines share the same version key).
	// Allow 2 as a tolerance for scheduler timing.
	if calls > 2 {
		t.Errorf("expected concurrent Refresh to collapse embed calls via singleflight, got %d embed calls for %d goroutines", calls, concurrency)
	}
	t.Logf("embed calls collapsed from %d goroutines to %d embedder call(s)", concurrency, calls)
}

