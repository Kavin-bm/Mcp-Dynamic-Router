package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOllamaEmbed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "nomic-embed-text" || len(body.Input) != 1 || body.Input[0] != "forecast" {
			t.Fatalf("body = %+v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{0.25, 0.75}}})
	}))
	t.Cleanup(server.Close)

	embedder := &Ollama{BaseURL: server.URL + "/", Model: "nomic-embed-text", Client: server.Client()}
	vectors, err := embedder.Embed(context.Background(), []string{"forecast"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 1 || len(vectors[0]) != 2 || vectors[0][1] != 0.75 {
		t.Fatalf("vectors = %#v", vectors)
	}
}

func TestOllamaEmbedRequiresModel(t *testing.T) {
	if _, err := (&Ollama{}).Embed(context.Background(), []string{"forecast"}); err == nil {
		t.Fatal("expected missing model to fail")
	}
}

func TestOllamaEmbedHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	t.Cleanup(server.Close)
	embedder := &Ollama{BaseURL: server.URL, Model: "nomic-embed-text", Client: server.Client()}
	_, err := embedder.Embed(context.Background(), []string{"forecast"})
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("expected status error, got %v", err)
	}
}
