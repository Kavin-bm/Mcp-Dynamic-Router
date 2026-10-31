# Contributing to MCP Dynamic Router

Thank you for your interest in contributing to the MCP Dynamic Router project! We welcome contributions of all forms, including bug fixes, design improvements, documentation, and new integration adapters.

---

## How to Contribute

1. **Fork the Repository** and clone your fork locally.
2. **Create a Feature Branch** off the `main` branch.
3. **Write Code and Tests** — please ensure your changes are fully covered by unit tests.
4. **Run Verification Commands:**
   ```bash
   go build ./...
   go vet ./...
   go test -race ./...
   go run ./cmd/router-audit
   go run ./cmd/router-eval -strict
   ```
5. **Submit a Pull Request (PR)** detailing the changes, design decisions, and benchmarks.

---

##  Contribution Areas & Low-Hanging Fruits

We have flagged the following high-priority areas for new contributors to pick up:

### 1. Middleware & Telemetry
* **Routing traces:** OpenTelemetry spans for each routing stage are optional and off by default. See the README for `routerd -tracing` and `MCP_ROUTER_TRACING`. A follow-up is to continue those traces across downstream tool execution.
* **Prometheus histograms:** `/metrics` already exposes request and decision counters. Latency and margin histograms are still open.

### 2. Provider Adapters
* **Python Framework Wrappers:** Write production-ready client wrappers for LiveKit Agents and Pipecat pipelines.
* **JS/TS SDK Integration:** Build a lightweight TypeScript wrapper mirroring tool execution requests.

### 3. Routing Strategies
* **Custom LLM Selector Adapters:** Add built-in selector modules for OpenAI, Anthropic, or Hugging Face.
* **Advanced Lexical Search:** Explore alternative lexical algorithms (e.g. TF-IDF adjustments).

---

## 🏷️ Placeholder "Good First Issues"

If you are looking for a place to start, consider these issues:

### 🛠️ Good First Issue #1: Support Custom Auth Headers in `mcp.toml`
* **Description:** Allow specifying authentication tokens and custom headers for HTTP/SSE servers inside `mcp.toml`.
* **Where to Edit:** [mcpclient/config.go](mcpclient/config.go) and [mcpclient/client.go](mcpclient/client.go).
* **Tags:** `good first issue`, `configuration`

### 📊 Good First Issue #2: Implement Prometheus Metrics Registry
* **Description:** Add middleware to track routing decision latencies and cache hit metrics.
* **Where to Edit:** `router/router.go` and `cmd/routerd/main.go`.
* **Tags:** `good first issue`, `telemetry`

### 📁 Good First Issue #3: Create a Local SQLite Embedding Cache
* **Description:** Replace the in-memory embedding cache with a SQLite cache to persist pre-computed tool vectors across sidecar restarts.
* **Where to Edit:** `embedding/` package.
* **Tags:** `good first issue`, `caching`, `database`
