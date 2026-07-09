package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/kavinbm16/mcp-dynamic-router/router"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Server struct {
	Name       string       `toml:"name"`
	URL        string       `toml:"url"`
	Transport  string       `toml:"transport"`
	HTTPClient *http.Client `toml:"-"`
}

type ElicitationHandler func(context.Context, string, *mcp.ElicitRequest) (*mcp.ElicitResult, error)

type Option func(*Client)

func WithElicitationHandler(handler ElicitationHandler) Option {
	return func(client *Client) { client.elicitationHandler = handler }
}

func WithClientInfo(name, version string) Option {
	return func(client *Client) {
		client.clientName = name
		client.clientVersion = version
	}
}

type ConnectReport struct {
	Connected []string          `json:"connected"`
	Failed    map[string]string `json:"failed,omitempty"`
}

type session struct {
	mu      sync.Mutex
	server  Server
	session *mcp.ClientSession
}

// Client manages active connections to MCP servers, mirrors their tools to the
// registry, and watches the config file for hot-reloads.
type Client struct {
	registry           *router.Registry
	onToolsChanged     func(context.Context) error
	mu                 sync.RWMutex
	sessions           map[string]*session
	elicitation        ElicitationConfig
	elicitationHandler ElicitationHandler
	configPath         string             // Path to the mcp.toml file
	lastModTime        time.Time          // Last recorded modification time of the config
	watcherCancel      context.CancelFunc // Cancels the background file watcher goroutine
	watcherWait        sync.WaitGroup     // Coordinates watcher shutdown
	onReload           func()             // Test hook called after checkConfigChanges reloads config
	clientName         string             // Name sent in mcp.ClientOptions
	clientVersion      string             // Version sent in mcp.ClientOptions
}

func New(registry *router.Registry, onToolsChanged func(context.Context) error, options ...Option) *Client {
	client := &Client{registry: registry, onToolsChanged: onToolsChanged, sessions: make(map[string]*session), clientName: "mcp-dynamic-router", clientVersion: "0.1.0"}
	for _, option := range options {
		option(client)
	}
	return client
}

// ConnectFile loads mcp.toml, connects every configured server, and starts the
// background config-file watcher. Individual server failures are returned in the
// report so healthy servers remain usable. The watcher is always started so that
// a corrected config is picked up automatically even if the initial connect fails.
func (c *Client) ConnectFile(ctx context.Context, configPath string) (ConnectReport, error) {
	report, err := c.connectServers(ctx, configPath)
	c.startWatcher(configPath)
	return report, err
}


// connectServers loads the config file, dials every server, and calls
// onToolsChanged. It intentionally does NOT start or restart the watcher —
// callers that are already inside the watcher goroutine must use this method
// to avoid spawning a second concurrent watcher.
func (c *Client) connectServers(ctx context.Context, configPath string) (ConnectReport, error) {
	config, err := LoadConfig(configPath)
	if err != nil {
		return ConnectReport{}, err
	}
	c.elicitation = config.Elicitation
	report := ConnectReport{Failed: make(map[string]string)}
	type connectResult struct {
		name string
		err  error
	}
	results := make(chan connectResult, len(config.Servers))
	var wait sync.WaitGroup
	for _, server := range config.Servers {
		wait.Add(1)
		go func(server Server) {
			defer wait.Done()
			results <- connectResult{name: server.Name, err: c.Connect(ctx, server)}
		}(server)
	}
	wait.Wait()
	close(results)
	for result := range results {
		if result.err != nil {
			report.Failed[result.name] = result.err.Error()
		} else {
			report.Connected = append(report.Connected, result.name)
		}
	}
	sort.Strings(report.Connected)
	if len(report.Connected) == 0 {
		return report, fmt.Errorf("could not connect any configured MCP server")
	}
	if c.onToolsChanged != nil {
		if err := c.onToolsChanged(ctx); err != nil {
			return report, fmt.Errorf("build routing index: %w", err)
		}
	}
	return report, nil
}

func (c *Client) Connect(ctx context.Context, server Server) error {
	if server.Name == "" || server.URL == "" {
		return fmt.Errorf("server name and URL are required")
	}
	if server.Transport == "" {
		server.Transport = "streamable-http"
	}
	if server.Transport != "streamable-http" && server.Transport != "http" {
		return fmt.Errorf("server %q uses unsupported transport %q; use streamable-http", server.Name, server.Transport)
	}
	holder := &session{server: server}
	client := mcp.NewClient(&mcp.Implementation{Name: c.clientName, Version: c.clientVersion}, &mcp.ClientOptions{
		KeepAlive:          30 * time.Second,
		ElicitationHandler: c.resolveElicitation(server.Name),
		ToolListChangedHandler: func(_ context.Context, _ *mcp.ToolListChangedRequest) {
			go c.refreshAfterNotification(server.Name)
		},
	})
	transport := &mcp.StreamableClientTransport{Endpoint: server.URL, HTTPClient: server.HTTPClient}
	connected, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return fmt.Errorf("connect %s: %w", server.Name, err)
	}
	holder.session = connected

	c.mu.Lock()
	previous := c.sessions[server.Name]
	c.sessions[server.Name] = holder
	c.mu.Unlock()
	if previous != nil && previous.session != nil {
		_ = previous.session.Close()
	}
	if err := c.SyncServer(ctx, server.Name); err != nil {
		_ = connected.Close()
		c.mu.Lock()
		if c.sessions[server.Name] == holder {
			delete(c.sessions, server.Name)
		}
		c.mu.Unlock()
		return err
	}
	return nil
}

func (c *Client) SyncServer(ctx context.Context, serverName string) error {
	holder, err := c.get(serverName)
	if err != nil {
		return err
	}
	holder.mu.Lock()
	defer holder.mu.Unlock()
	response, err := holder.session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return fmt.Errorf("list tools from %s: %w", serverName, err)
	}
	tools := make([]router.Tool, 0, len(response.Tools))
	for _, source := range response.Tools {
		schema, err := json.Marshal(source.InputSchema)
		if err != nil {
			return fmt.Errorf("marshal schema for %s.%s: %w", serverName, source.Name, err)
		}
		readOnly := source.Annotations != nil && source.Annotations.ReadOnlyHint
		tools = append(tools, router.Tool{Server: serverName, Name: source.Name, Description: source.Description, InputSchema: schema, ReadOnly: readOnly})
	}
	return c.registry.ReplaceServer(serverName, tools)
}

func (c *Client) resolveElicitation(serverName string) func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
	if !c.elicitation.Enabled {
		return nil
	}
	return func(ctx context.Context, request *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		timeoutCtx, cancel := context.WithTimeout(ctx, c.elicitation.Duration())
		defer cancel()
		if c.elicitationHandler != nil {
			return c.elicitationHandler(timeoutCtx, serverName, request)
		}
		if c.elicitation.AutoAccept {
			return &mcp.ElicitResult{Action: "accept"}, nil
		}
		return &mcp.ElicitResult{Action: "decline"}, nil
	}
}

func (c *Client) Call(ctx context.Context, tool router.Tool, arguments map[string]any) (*mcp.CallToolResult, error) {
	holder, err := c.get(tool.Server)
	if err != nil {
		return nil, err
	}
	holder.mu.Lock()
	defer holder.mu.Unlock()
	result, err := holder.session.CallTool(ctx, &mcp.CallToolParams{Name: tool.Name, Arguments: arguments})
	if err != nil {
		return nil, fmt.Errorf("call %s: %w", tool.ID, err)
	}
	return result, nil
}

// Close terminates all active server connections, cancels the background config
// file watcher, and waits for its goroutine to safely exit.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.watcherCancel != nil {
		c.watcherCancel()
	}
	c.mu.Unlock()
	c.watcherWait.Wait()

	return c.closeSessions()
}

// closeSessions closes active connections to all servers without stopping the config watcher.
func (c *Client) closeSessions() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var first error
	for name, holder := range c.sessions {
		log.Printf("[MCP Client] Closing connection session for server %q", name)
		if err := holder.session.Close(); err != nil && first == nil {
			first = fmt.Errorf("close %s: %w", name, err)
		}
	}
	c.sessions = make(map[string]*session)
	return first
}

// startWatcher registers the config file path and starts a background polling ticker
// to detect configuration modifications and trigger hot-reloads.
func (c *Client) startWatcher(configPath string) {
	c.mu.Lock()
	if c.watcherCancel != nil {
		c.watcherCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.watcherCancel = cancel
	c.configPath = configPath
	if info, err := os.Stat(configPath); err == nil {
		c.lastModTime = info.ModTime()
	}
	c.mu.Unlock()

	c.watcherWait.Add(1)
	go func() {
		defer c.watcherWait.Done()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.checkConfigChanges(ctx)
			}
		}
	}()
}

// checkConfigChanges stats the configuration file, comparing its mod-time to our cached value.
// If changed, it shuts down all connections and triggers a full client reconnect.
func (c *Client) checkConfigChanges(ctx context.Context) {
	c.mu.Lock()
	path := c.configPath
	lastMod := c.lastModTime
	c.mu.Unlock()

	if path == "" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return // Ignore stat errors (e.g. temporary write lock)
	}
	if !info.ModTime().After(lastMod) {
		return
	}

	c.mu.Lock()
	c.lastModTime = info.ModTime()
	c.mu.Unlock()

	log.Printf("[MCP Watcher] Config file %s modified; performing full hot-reload...", path)
	_ = c.closeSessions()
	// Use connectServers instead of ConnectFile to avoid spawning a second watcher
	// goroutine from inside the existing one. This goroutine IS the watcher.
	_, reloadErr := c.connectServers(ctx, path)
	if reloadErr != nil {
		log.Printf("[MCP Watcher] Reload failed for config %s: %v", path, reloadErr)
	} else {
		log.Printf("[MCP Watcher] Reload succeeded. All connections refreshed.")
	}

	c.mu.Lock()
	onReloadHook := c.onReload
	c.mu.Unlock()
	if onReloadHook != nil {
		onReloadHook()
	}
}

func (c *Client) get(serverName string) (*session, error) {
	c.mu.RLock()
	holder := c.sessions[serverName]
	c.mu.RUnlock()
	if holder == nil {
		return nil, fmt.Errorf("MCP server %q is not connected", serverName)
	}
	return holder, nil
}

func (c *Client) refreshAfterNotification(serverName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := c.SyncServer(ctx, serverName); err != nil {
		log.Printf("[MCP Client] Failed to sync server %q after tool list change: %v", serverName, err)
		return
	}
	if c.onToolsChanged != nil {
		if err := c.onToolsChanged(ctx); err != nil {
			log.Printf("[MCP Client] Failed to update tools after notification for %q: %v", serverName, err)
		}
	}
}
