package dynamicrouter

import (
	"context"
	"strings"
	"time"

	"github.com/kavinbm16/mcp-dynamic-router/mcpclient"
	"github.com/kavinbm16/mcp-dynamic-router/router"
	"github.com/kavinbm16/mcp-dynamic-router/streamrag"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Options struct {
	MCPConfigPath      string
	RouterConfig       router.Config
	Embedder           router.Embedder
	Reranker           router.Reranker
	ElicitationHandler mcpclient.ElicitationHandler
}

// App is the plug-and-play facade. It owns discovery, the routing index,
// Stream RAG sessions, and MCP connections.
type App struct {
	Registry *router.Registry
	Router   *router.Router
	MCP      *mcpclient.Client
	config   string
}

func New(options Options) *App {
	config := options.RouterConfig
	registry := router.NewRegistry()
	routerOptions := []router.Option{}
	if options.Reranker != nil {
		routerOptions = append(routerOptions, router.WithReranker(options.Reranker))
	}
	engine := router.New(registry, options.Embedder, config, routerOptions...)
	clientOptions := []mcpclient.Option{}
	if options.ElicitationHandler != nil {
		clientOptions = append(clientOptions, mcpclient.WithElicitationHandler(options.ElicitationHandler))
	}
	client := mcpclient.New(registry, engine.Refresh, clientOptions...)
	configPath := strings.TrimSpace(options.MCPConfigPath)
	if configPath == "" {
		configPath = "mcp.toml"
	}
	return &App{Registry: registry, Router: engine, MCP: client, config: configPath}
}

func (a *App) Start(ctx context.Context) (mcpclient.ConnectReport, error) {
	return a.MCP.ConnectFile(ctx, a.config)
}

func (a *App) Route(ctx context.Context, request router.RouteRequest) (router.RouteResult, error) {
	return a.Router.Route(ctx, request)
}

func (a *App) Call(ctx context.Context, tool router.Tool, arguments map[string]any) (*mcp.CallToolResult, error) {
	return a.MCP.Call(ctx, tool, arguments)
}

func (a *App) NewStream(options streamrag.Options, hooks streamrag.Hooks) *streamrag.Session {
	return streamrag.New(a.Router, options, hooks)
}

func (a *App) Close() error { return a.MCP.Close() }

// ==========================================
// Fluent Builder Pattern Implementations
// ==========================================

// AppBuilder provides a fluent builder interface for App.
type AppBuilder struct {
	mcpConfigPath      string
	routerConfig       router.Config
	embedder           router.Embedder
	reranker           router.Reranker
	elicitationHandler mcpclient.ElicitationHandler
}

// NewBuilder initializes a new AppBuilder with DefaultConfig.
func NewBuilder() *AppBuilder {
	return &AppBuilder{
		routerConfig: router.DefaultConfig(),
	}
}

func (b *AppBuilder) WithMCPConfigPath(path string) *AppBuilder {
	b.mcpConfigPath = path
	return b
}

func (b *AppBuilder) WithEmbedder(embedder router.Embedder) *AppBuilder {
	b.embedder = embedder
	return b
}

func (b *AppBuilder) WithReranker(reranker router.Reranker) *AppBuilder {
	b.reranker = reranker
	return b
}

func (b *AppBuilder) WithElicitationHandler(handler mcpclient.ElicitationHandler) *AppBuilder {
	b.elicitationHandler = handler
	return b
}

func (b *AppBuilder) WithBM25FastPath(threshold float64) *AppBuilder {
	b.routerConfig.BM25FastPath = threshold
	return b
}

func (b *AppBuilder) WithRouteCacheTTL(ttl time.Duration) *AppBuilder {
	b.routerConfig.RouteCacheTTL = ttl
	return b
}

func (b *AppBuilder) WithMinConfidence(confidence float64) *AppBuilder {
	b.routerConfig.MinConfidence = confidence
	return b
}

func (b *AppBuilder) WithMinMargin(margin float64) *AppBuilder {
	b.routerConfig.MinMargin = margin
	return b
}

func (b *AppBuilder) WithRerankerWeight(weight float64) *AppBuilder {
	b.routerConfig.RerankerWeight = weight
	return b
}

func (b *AppBuilder) WithLexicalWeight(weight float64) *AppBuilder {
	b.routerConfig.LexicalWeight = weight
	return b
}

func (b *AppBuilder) WithSemanticWeight(weight float64) *AppBuilder {
	b.routerConfig.SemanticWeight = weight
	return b
}

func (b *AppBuilder) WithMaxDomainTools(n int) *AppBuilder {
	b.routerConfig.MaxDomainTools = n
	return b
}

func (b *AppBuilder) Build() *App {
	return New(Options{
		MCPConfigPath:      b.mcpConfigPath,
		RouterConfig:       b.routerConfig,
		Embedder:           b.embedder,
		Reranker:           b.reranker,
		ElicitationHandler: b.elicitationHandler,
	})
}

// StreamBuilder provides a fluent builder interface for streamrag.Session.
type StreamBuilder struct {
	router  *router.Router
	options streamrag.Options
	hooks   streamrag.Hooks
}

// NewStreamBuilder creates a StreamBuilder utilizing the current App's routing engine.
func (a *App) NewStreamBuilder() *StreamBuilder {
	return &StreamBuilder{
		router:  a.Router,
		options: streamrag.DefaultOptions(),
	}
}

func (b *StreamBuilder) WithMinPartialCharacters(n int) *StreamBuilder {
	b.options.MinPartialCharacters = n
	return b
}

func (b *StreamBuilder) WithStableUpdates(n int) *StreamBuilder {
	b.options.StableUpdates = n
	return b
}

func (b *StreamBuilder) WithMinASRConfidence(confidence float64) *StreamBuilder {
	b.options.MinASRConfidence = confidence
	return b
}

func (b *StreamBuilder) WithPrefetchReadOnly(prefetch bool) *StreamBuilder {
	b.options.PrefetchReadOnly = prefetch
	return b
}

func (b *StreamBuilder) OnPrediction(fn func(streamrag.Prediction)) *StreamBuilder {
	b.hooks.OnPrediction = fn
	return b
}

func (b *StreamBuilder) OnPrefetch(fn func(context.Context, router.Tool) error) *StreamBuilder {
	b.hooks.OnPrefetch = fn
	return b
}

func (b *StreamBuilder) OnCommit(fn func(streamrag.Prediction)) *StreamBuilder {
	b.hooks.OnCommit = fn
	return b
}

func (b *StreamBuilder) OnError(fn func(error)) *StreamBuilder {
	b.hooks.OnError = fn
	return b
}

func (b *StreamBuilder) Build() *streamrag.Session {
	return streamrag.New(b.router, b.options, b.hooks)
}
