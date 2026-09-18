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

func BenchmarkLexicalIndex_Scores(b *testing.B) {
	// Setup a corpus of 100 tools
	docs := make([]string, 100)
	for i := 0; i < 100; i++ {
		docs[i] = fmt.Sprintf("Domain: domain-%d. Purpose: Tool %d handles calculations, weather forecasts, calendar events, search queries, database lookups, and notifications. Invoke when: the user asks for action %d.", i%5, i, i)
	}
	index := newLexicalIndex(docs)
	query := "check the weather forecast and calendar events for today"

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = index.scores(query)
	}
}

func TestPersistentEmbeddingCache(t *testing.T) {
	tempDir := t.TempDir()
	cacheFile := tempDir + "/embeddings.json"

	embedder := &mockEmbedder{vectorSize: 4}
	registry := NewRegistry()
	cfg := DefaultConfig()
	cfg.EmbeddingCachePath = cacheFile

	r1 := New(registry, embedder, cfg)
	tool := Tool{ID: "calc.add", Name: "add", Server: "calc", Domain: "math", Description: "Add two numbers together", InputSchema: json.RawMessage(`{}`)}
	if err := registry.ReplaceServer("calc", []Tool{tool}); err != nil {
		t.Fatal(err)
	}
	if err := r1.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	initialEmbeds := atomic.LoadInt64(&embedder.embeds)
	if initialEmbeds == 0 {
		t.Fatal("expected initial embeds to be greater than 0")
	}

	// Create a new router with the same cache file and registry snapshot
	atomic.StoreInt64(&embedder.embeds, 0)
	r2 := New(registry, embedder, cfg)
	if err := r2.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	subsequentEmbeds := atomic.LoadInt64(&embedder.embeds)
	if subsequentEmbeds != 0 {
		t.Fatalf("expected 0 embeds from r2 due to persistent cache, got %d", subsequentEmbeds)
	}
}

type failingEmbedder struct{}

func (f *failingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return nil, fmt.Errorf("simulated network timeout")
}

func TestFailOpenSemanticRouting(t *testing.T) {
	registry := NewRegistry()
	tool := Tool{
		ID:          "weather.forecast",
		Name:        "get_forecast",
		Server:      "weather",
		Domain:      "weather",
		Description: "Domain: weather. Purpose: Get weather forecast for city.",
		InputSchema: json.RawMessage(`{}`),
	}
	if err := registry.ReplaceServer("weather", []Tool{tool}); err != nil {
		t.Fatal(err)
	}

	// Build index initially with a working mock embedder
	embedder := &mockEmbedder{vectorSize: 4}
	r := New(registry, embedder, DefaultConfig())
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Now swap the embedder with a failing one
	r.embedder = &failingEmbedder{}

	// Routing should NOT return an error, but fail open to lexical BM25
	result, err := r.Route(context.Background(), RouteRequest{
		Utterance: "weather forecast",
		Final:     true,
	})
	if err != nil {
		t.Fatalf("expected fail-open to lexical, but got error: %v", err)
	}
	if result.Trace.UsedEmbeddings {
		t.Fatal("expected UsedEmbeddings to be false when embedder fails")
	}
	if result.Decision != DecisionSelected || len(result.Candidates) == 0 {
		t.Fatalf("expected tool selected via lexical fallback, got decision=%s", result.Decision)
	}
}

type failingReranker struct{}

func (f *failingReranker) Rerank(ctx context.Context, utterance, conversationContext string, tools []Tool) (map[string]float64, error) {
	return nil, fmt.Errorf("simulated LLM reranker failure")
}

func TestFailOpenReranker(t *testing.T) {
	registry := NewRegistry()
	tool := Tool{
		ID:          "weather.forecast",
		Name:        "get_forecast",
		Server:      "weather",
		Domain:      "weather",
		Description: "Domain: weather. Purpose: Get weather forecast for city.",
		InputSchema: json.RawMessage(`{}`),
	}
	if err := registry.ReplaceServer("weather", []Tool{tool}); err != nil {
		t.Fatal(err)
	}

	r := New(registry, nil, DefaultConfig(), WithReranker(&failingReranker{}))
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Routing should NOT return an error, but fail open to preliminary scores
	result, err := r.Route(context.Background(), RouteRequest{
		Utterance: "weather forecast",
		Final:     true,
	})
	if err != nil {
		t.Fatalf("expected fail-open to preliminary scores, but got error: %v", err)
	}
	if result.Trace.UsedReranker {
		t.Fatal("expected UsedReranker to be false when reranker fails")
	}
	if result.Decision != DecisionSelected {
		t.Fatalf("expected tool selected, got decision=%s", result.Decision)
	}
}


