package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	dynamicrouter "github.com/kavinbm16/mcp-dynamic-router"
	"github.com/kavinbm16/mcp-dynamic-router/embedding"
	"github.com/kavinbm16/mcp-dynamic-router/router"
	"github.com/kavinbm16/mcp-dynamic-router/selector"
	"github.com/kavinbm16/mcp-dynamic-router/streamrag"
)

type streamRequest struct {
	SessionID  string  `json:"session_id"`
	Transcript string  `json:"transcript"`
	Context    string  `json:"context,omitempty"`
	Final      bool    `json:"final"`
	Confidence float64 `json:"confidence,omitempty"`
}

type executeRequest struct {
	ToolID    string         `json:"tool_id"`
	Arguments map[string]any `json:"arguments"`
}

type streamEntry struct {
	session  *streamrag.Session
	lastSeen time.Time
}

type api struct {
	app       *dynamicrouter.App
	mu        sync.Mutex
	sessions  map[string]*streamEntry
	metricsMu sync.Mutex
	requests  map[string]int64
	decisions map[string]int64
}

func (a *api) recordRequest(endpoint string) {
	a.metricsMu.Lock()
	a.requests[endpoint]++
	a.metricsMu.Unlock()
}

func (a *api) recordDecision(decision router.Decision) {
	a.metricsMu.Lock()
	a.decisions[string(decision)]++
	a.metricsMu.Unlock()
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func newAPI(app *dynamicrouter.App) *api {
	return &api{
		app:       app,
		sessions:  make(map[string]*streamEntry),
		requests:  make(map[string]int64),
		decisions: make(map[string]int64),
	}
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8090", "HTTP listen address")
	mcpConfig := flag.String("mcp", "mcp.toml", "path to mcp.toml")
	ollamaURL := flag.String("ollama", "", "optional Ollama base URL")
	embeddingModel := flag.String("embedding-model", "", "optional Ollama embedding model")
	selectorModel := flag.String("selector-model", "", "optional Ollama tool-selector model")
	flag.Parse()

	options := dynamicrouter.Options{MCPConfigPath: *mcpConfig, RouterConfig: router.DefaultConfig()}
	if *embeddingModel != "" {
		options.Embedder = &embedding.Ollama{BaseURL: *ollamaURL, Model: *embeddingModel}
	}
	if *selectorModel != "" {
		options.Reranker = &selector.Ollama{BaseURL: *ollamaURL, Model: *selectorModel}
	}
	app := dynamicrouter.New(options)
	defer app.Close()

	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 30*time.Second)
	report, err := app.Start(startupCtx)
	cancelStartup()
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("MCP servers connected=%v failed=%v", report.Connected, report.Failed)

	handler := newAPI(app)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handler.health)
	mux.HandleFunc("GET /metrics", handler.metrics)
	mux.HandleFunc("GET /v1/tools", handler.tools)
	mux.HandleFunc("GET /v1/servers", handler.servers)
	mux.HandleFunc("POST /v1/route", handler.route)
	mux.HandleFunc("POST /v1/stream", handler.stream)
	mux.HandleFunc("POST /v1/execute", handler.execute)

	server := &http.Server{
		Addr:              *listen,
		Handler:           withCORS(mux),
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		log.Printf("routerd listening on http://%s", *listen)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("HTTP server failed: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	handler.close()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP shutdown: %v", err)
	}
}

func (a *api) health(response http.ResponseWriter, _ *http.Request) {
	a.recordRequest("/healthz")
	snap := a.app.Registry.Snapshot()
	servers := a.app.ConnectedServers()
	writeJSON(response, http.StatusOK, map[string]any{
		"status":            "ok",
		"registry_version":  snap.Version,
		"tools_count":       len(snap.Tools),
		"connected_servers": servers,
	})
}

func (a *api) tools(response http.ResponseWriter, _ *http.Request) {
	a.recordRequest("/v1/tools")
	snap := a.app.Registry.Snapshot()
	writeJSON(response, http.StatusOK, map[string]any{
		"version": snap.Version,
		"count":   len(snap.Tools),
		"tools":   snap.Tools,
	})
}

func (a *api) servers(response http.ResponseWriter, _ *http.Request) {
	a.recordRequest("/v1/servers")
	servers := a.app.ConnectedServers()
	writeJSON(response, http.StatusOK, map[string]any{
		"count":   len(servers),
		"servers": servers,
	})
}

func (a *api) metrics(response http.ResponseWriter, _ *http.Request) {
	a.recordRequest("/metrics")
	a.metricsMu.Lock()
	reqs := make(map[string]int64, len(a.requests))
	for k, v := range a.requests {
		reqs[k] = v
	}
	decs := make(map[string]int64, len(a.decisions))
	for k, v := range a.decisions {
		decs[k] = v
	}
	a.metricsMu.Unlock()

	a.mu.Lock()
	activeSessions := len(a.sessions)
	a.mu.Unlock()

	snap := a.app.Registry.Snapshot()
	connectedServers := len(a.app.ConnectedServers())

	var b strings.Builder
	b.WriteString("# HELP mcp_router_requests_total Total number of HTTP requests processed\n")
	b.WriteString("# TYPE mcp_router_requests_total counter\n")
	endpoints := []string{"/healthz", "/metrics", "/v1/route", "/v1/stream", "/v1/execute", "/v1/tools", "/v1/servers"}
	for _, ep := range endpoints {
		fmt.Fprintf(&b, "mcp_router_requests_total{endpoint=%q} %d\n", ep, reqs[ep])
	}
	b.WriteString("\n# HELP mcp_router_decisions_total Routing decisions made by the router\n")
	b.WriteString("# TYPE mcp_router_decisions_total counter\n")
	for _, dec := range []string{"selected", "clarify", "no_tool"} {
		fmt.Fprintf(&b, "mcp_router_decisions_total{decision=%q} %d\n", dec, decs[dec])
	}
	b.WriteString("\n# HELP mcp_router_active_sessions Number of active stream RAG sessions\n")
	b.WriteString("# TYPE mcp_router_active_sessions gauge\n")
	fmt.Fprintf(&b, "mcp_router_active_sessions %d\n", activeSessions)

	b.WriteString("\n# HELP mcp_router_registered_tools Total tools loaded in registry\n")
	b.WriteString("# TYPE mcp_router_registered_tools gauge\n")
	fmt.Fprintf(&b, "mcp_router_registered_tools %d\n", len(snap.Tools))

	b.WriteString("\n# HELP mcp_router_connected_servers Total active downstream MCP server connections\n")
	b.WriteString("# TYPE mcp_router_connected_servers gauge\n")
	fmt.Fprintf(&b, "mcp_router_connected_servers %d\n", connectedServers)

	response.Header().Set("Content-Type", "text/plain; version=0.0.4")
	response.WriteHeader(http.StatusOK)
	response.Write([]byte(b.String()))
}

func (a *api) route(response http.ResponseWriter, request *http.Request) {
	a.recordRequest("/v1/route")
	var input router.RouteRequest
	if err := decodeJSON(response, request, &input); err != nil {
		return
	}
	result, err := a.app.Route(request.Context(), input)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	a.recordDecision(result.Decision)
	writeJSON(response, http.StatusOK, result)
}

func (a *api) stream(response http.ResponseWriter, request *http.Request) {
	a.recordRequest("/v1/stream")
	var input streamRequest
	if err := decodeJSON(response, request, &input); err != nil {
		return
	}
	if input.SessionID == "" {
		writeError(response, http.StatusBadRequest, fmt.Errorf("session_id is required"))
		return
	}
	session := a.getSession(input.SessionID)
	prediction, err := session.Update(request.Context(), streamrag.Event{Transcript: input.Transcript, Context: input.Context, Final: input.Final, Confidence: input.Confidence})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			writeError(response, http.StatusConflict, fmt.Errorf("superseded by a newer transcript"))
		} else {
			writeError(response, http.StatusInternalServerError, err)
		}
		return
	}
	if prediction.Triggered {
		a.recordDecision(prediction.Result.Decision)
	}
	if input.Final {
		a.deleteSession(input.SessionID)
	}
	writeJSON(response, http.StatusOK, prediction)
}

func (a *api) execute(response http.ResponseWriter, request *http.Request) {
	a.recordRequest("/v1/execute")
	var input executeRequest
	if err := decodeJSON(response, request, &input); err != nil {
		return
	}
	if input.ToolID == "" {
		writeError(response, http.StatusBadRequest, fmt.Errorf("tool_id is required"))
		return
	}

	tool, found := a.app.Registry.Lookup(input.ToolID)
	if !found {
		writeError(response, http.StatusNotFound, fmt.Errorf("tool %q not found", input.ToolID))
		return
	}

	// Validate arguments against JSON Schema
	if validationErrors := router.ValidateArguments(tool, input.Arguments); len(validationErrors) > 0 {
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(response).Encode(map[string]any{
			"error":             "validation failed",
			"validation_errors": validationErrors,
		})
		return
	}

	result, err := a.app.Call(request.Context(), tool, input.Arguments)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (a *api) getSession(id string) *streamrag.Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for sessionID, entry := range a.sessions {
		if now.Sub(entry.lastSeen) > 2*time.Minute {
			entry.session.Close()
			delete(a.sessions, sessionID)
		}
	}
	if entry := a.sessions[id]; entry != nil {
		entry.lastSeen = now
		return entry.session
	}
	session := a.app.NewStream(streamrag.DefaultOptions(), streamrag.Hooks{})
	a.sessions[id] = &streamEntry{session: session, lastSeen: now}
	return session
}

func (a *api) deleteSession(id string) {
	a.mu.Lock()
	entry := a.sessions[id]
	delete(a.sessions, id)
	a.mu.Unlock()
	if entry != nil {
		entry.session.Close()
	}
}

func (a *api) close() {
	a.mu.Lock()
	sessions := a.sessions
	a.sessions = make(map[string]*streamEntry)
	a.mu.Unlock()
	for _, entry := range sessions {
		entry.session.Close()
	}
}

func decodeJSON(response http.ResponseWriter, request *http.Request, target any) error {
	request.Body = http.MaxBytesReader(response, request.Body, 1<<20)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(response, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
		return err
	}
	return nil
}

func writeError(response http.ResponseWriter, status int, err error) {
	writeJSON(response, status, map[string]string{"error": err.Error()})
}

func writeJSON(response http.ResponseWriter, status int, payload any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	if err := json.NewEncoder(response).Encode(payload); err != nil {
		log.Printf("write JSON response: %v", err)
	}
}
