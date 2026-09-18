package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dynamicrouter "github.com/kavinbm16/mcp-dynamic-router"
	"github.com/kavinbm16/mcp-dynamic-router/mcpclient"
	"github.com/kavinbm16/mcp-dynamic-router/router"
)

func TestRouteEndpoint(t *testing.T) {
	handler := testAPI(t)
	defer handler.close()
	request := httptest.NewRequest(http.MethodPost, "/v1/route", bytes.NewBufferString(`{"utterance":"weather forecast Bengaluru","final":true}`))
	recorder := httptest.NewRecorder()
	handler.route(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var result router.RouteResult
	if err := json.NewDecoder(recorder.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Decision != router.DecisionSelected || result.Candidates[0].Tool.ID != "weather.forecast" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestStreamEndpointMaintainsStabilityAndClosesFinalSession(t *testing.T) {
	handler := testAPI(t)
	defer handler.close()
	first := postStream(t, handler, `{"session_id":"call-1","transcript":"weather forecast Bengaluru","confidence":0.9,"final":false}`)
	if first.Stable {
		t.Fatal("first prediction should not be stable")
	}
	second := postStream(t, handler, `{"session_id":"call-1","transcript":"weather forecast Bengaluru tomorrow","confidence":0.95,"final":false}`)
	if !second.Stable {
		t.Fatalf("second route should be stable: %+v", second)
	}
	_ = postStream(t, handler, `{"session_id":"call-1","transcript":"weather forecast Bengaluru tomorrow morning","confidence":0.98,"final":true}`)
	handler.mu.Lock()
	_, exists := handler.sessions["call-1"]
	handler.mu.Unlock()
	if exists {
		t.Fatal("final transcript should close the stream session")
	}
}

func postStream(t *testing.T, handler *api, body string) struct {
	Stable bool `json:"stable"`
} {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/stream", bytes.NewBufferString(body))
	recorder := httptest.NewRecorder()
	handler.stream(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Stable bool `json:"stable"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	return response
}

func testAPI(t *testing.T) *api {
	t.Helper()
	app := dynamicrouter.New(dynamicrouter.Options{})
	tool := router.Tool{ID: "weather.forecast", Server: "weather", Name: "forecast", Domain: "weather", ReadOnly: true, Description: "Domain: weather\nPurpose: Get a weather forecast.\nInvoke when: Call for weather, rain, or temperature.\nParameters: location is a city.\nLimitations: No historical data.\nExample: weather forecast Bengaluru tomorrow", InputSchema: json.RawMessage(`{"type":"object"}`)}
	if err := app.Registry.ReplaceServer("weather", []router.Tool{tool}); err != nil {
		t.Fatal(err)
	}
	if err := app.Router.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return newAPI(app)
}

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Result  any    `json:"result,omitempty"`
	Error   any    `json:"error,omitempty"`
}

func TestExecuteEndpoint(t *testing.T) {
	// Spin up mock MCP server
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var res jsonRPCResponse
		res.JSONRPC = "2.0"
		res.ID = req.ID

		switch req.Method {
		case "initialize":
			res.Result = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "mock-weather", "version": "1.0.0"},
			}
		case "notifications/initialized":
			w.WriteHeader(http.StatusNoContent)
			return
		case "tools/list":
			res.Result = map[string]any{
				"tools": []map[string]any{
					{
						"name":        "forecast",
						"description": "Domain: weather\nPurpose: Get a weather forecast.\nInvoke when: Call for weather.\nParameters:\n- location (string, required): city name.\nExample:\nUser: forecast Bengaluru",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"location": map[string]any{"type": "string"},
							},
							"required": []string{"location"},
						},
					},
				},
			}
		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			json.Unmarshal(req.Params, &params)
			if params.Name == "forecast" {
				loc, _ := params.Arguments["location"].(string)
				res.Result = map[string]any{
					"content": []map[string]any{
						{
							"type": "text",
							"text": fmt.Sprintf("The weather in %s is sunny", loc),
						},
					},
				}
			} else {
				res.Error = map[string]any{"code": -32601, "message": "method not found"}
			}
		default:
			res.Error = map[string]any{"code": -32601, "message": "method not found"}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(res)
	}))
	defer mockServer.Close()

	// Initialize dynamic router
	app := dynamicrouter.New(dynamicrouter.Options{})
	defer app.Close()

	// Connect to our mock server
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := app.MCP.Connect(ctx, mcpclient.Server{
		Name:      "weather",
		URL:       mockServer.URL,
		Transport: "streamable-http",
	})
	if err != nil {
		t.Fatalf("failed to connect mock server: %v", err)
	}

	// Build the routing index
	if err := app.Router.Refresh(ctx); err != nil {
		t.Fatalf("failed to refresh router: %v", err)
	}

	handler := newAPI(app)

	// 1. Test Successful Tool Execution via /v1/execute
	reqBody := `{"tool_id":"weather.forecast","arguments":{"location":"Bengaluru"}}`
	request := httptest.NewRequest(http.MethodPost, "/v1/execute", bytes.NewBufferString(reqBody))
	recorder := httptest.NewRecorder()
	handler.execute(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d. Body: %s", recorder.Code, recorder.Body.String())
	}

	var callRes map[string]any
	if err := json.NewDecoder(recorder.Body).Decode(&callRes); err != nil {
		t.Fatal(err)
	}
	content, ok := callRes["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("unexpected call response: %+v", callRes)
	}
	txtMap, ok := content[0].(map[string]any)
	if !ok || txtMap["text"] != "The weather in Bengaluru is sunny" {
		t.Fatalf("unexpected content text: %+v", content[0])
	}

	// 2. Test Argument Validation Failure
	reqBodyInvalid := `{"tool_id":"weather.forecast","arguments":{"location":123}}` // location must be string
	request = httptest.NewRequest(http.MethodPost, "/v1/execute", bytes.NewBufferString(reqBodyInvalid))
	recorder = httptest.NewRecorder()
	handler.execute(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for invalid args, got %d", recorder.Code)
	}
	var valErrResponse map[string]any
	json.NewDecoder(recorder.Body).Decode(&valErrResponse)
	if valErrResponse["error"] != "validation failed" {
		t.Fatalf("expected validation failed error, got: %+v", valErrResponse)
	}

	// 3. Test Tool Not Found
	reqBodyNotFound := `{"tool_id":"weather.missing","arguments":{}}`
	request = httptest.NewRequest(http.MethodPost, "/v1/execute", bytes.NewBufferString(reqBodyNotFound))
	recorder = httptest.NewRecorder()
	handler.execute(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found for missing tool, got %d", recorder.Code)
	}
}

func TestToolsEndpoint(t *testing.T) {
	handler := testAPI(t)
	defer handler.close()

	request := httptest.NewRequest(http.MethodGet, "/v1/tools", nil)
	recorder := httptest.NewRecorder()
	handler.tools(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", recorder.Code)
	}

	var res map[string]any
	if err := json.NewDecoder(recorder.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	tools, ok := res["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("expected 1 registered tool in tools endpoint, got %+v", res)
	}
}

func TestServersEndpoint(t *testing.T) {
	handler := testAPI(t)
	defer handler.close()

	request := httptest.NewRequest(http.MethodGet, "/v1/servers", nil)
	recorder := httptest.NewRecorder()
	handler.servers(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", recorder.Code)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	handler := testAPI(t)
	defer handler.close()

	// Execute one route request to generate metric records
	routeReq := httptest.NewRequest(http.MethodPost, "/v1/route", bytes.NewBufferString(`{"utterance":"weather forecast","final":true}`))
	routeRec := httptest.NewRecorder()
	handler.route(routeRec, routeReq)

	metricsReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsRec := httptest.NewRecorder()
	handler.metrics(metricsRec, metricsReq)

	if metricsRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", metricsRec.Code)
	}
	body := metricsRec.Body.String()
	if !bytes.Contains([]byte(body), []byte("mcp_router_requests_total")) {
		t.Fatalf("expected prometheus metrics format, got: %s", body)
	}
	if !bytes.Contains([]byte(body), []byte("mcp_router_decisions_total")) {
		t.Fatalf("expected decisions metrics in body, got: %s", body)
	}
}

func TestHealthEndpoint(t *testing.T) {
	handler := testAPI(t)
	defer handler.close()

	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	recorder := httptest.NewRecorder()
	handler.health(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", recorder.Code)
	}
	var res map[string]any
	if err := json.NewDecoder(recorder.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if res["status"] != "ok" || res["tools_count"] != float64(1) {
		t.Fatalf("unexpected health response: %+v", res)
	}
}

func TestCORSMiddleware(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := withCORS(mux)

	// Preflight request
	optionsReq := httptest.NewRequest(http.MethodOptions, "/healthz", nil)
	optionsRec := httptest.NewRecorder()
	handler.ServeHTTP(optionsRec, optionsReq)

	if optionsRec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 No Content for OPTIONS, got %d", optionsRec.Code)
	}
	if optionsRec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("expected CORS origin header, got %q", optionsRec.Header().Get("Access-Control-Allow-Origin"))
	}
}


