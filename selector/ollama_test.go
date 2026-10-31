package selector

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kavinbm16/mcp-dynamic-router/router"
)

func TestOllamaRerank(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("path = %s", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "weather.get_forecast") || !strings.Contains(string(body), "umbrella") {
			t.Fatalf("prompt missing tool or utterance: %s", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]string{"content": `{"tool_id":"weather.get_forecast"}`},
		})
	}))
	t.Cleanup(server.Close)

	selector := &Ollama{BaseURL: server.URL + "/", Model: "llama3.2", Client: server.Client()}
	scores, err := selector.Rerank(context.Background(), "umbrella", "", []router.Tool{{
		ID: "weather.get_forecast", Description: "forecast", InputSchema: json.RawMessage(`{"type":"object"}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if scores["weather.get_forecast"] != 1 {
		t.Fatalf("scores = %#v", scores)
	}
}

func TestOllamaRerankNone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]string{"content": `{"tool_id":"none"}`},
		})
	}))
	t.Cleanup(server.Close)
	selector := &Ollama{BaseURL: server.URL, Model: "llama3.2", Client: server.Client()}
	scores, err := selector.Rerank(context.Background(), "hello", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 0 {
		t.Fatalf("none should yield no scores, got %#v", scores)
	}
}

func TestOllamaRerankRequiresModel(t *testing.T) {
	if _, err := (&Ollama{}).Rerank(context.Background(), "hello", "", nil); err == nil {
		t.Fatal("expected missing model to fail")
	}
}

func TestOllamaRerankHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	selector := &Ollama{BaseURL: server.URL, Model: "llama3.2", Client: server.Client()}
	_, err := selector.Rerank(context.Background(), "hello", "", nil)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("expected status error, got %v", err)
	}
}
