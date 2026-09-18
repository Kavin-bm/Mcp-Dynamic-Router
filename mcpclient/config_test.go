package mcpclient

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "mcp.toml")
	contents := `[elicitation]
enabled = true
auto_accept = false
timeout = 30

[[servers]]
name = "weather-service"
url = "http://localhost:23202/mcp"
transport = "streamable-http"

[[servers]]
name = "wellness-service"
url = "http://localhost:23002/mcp"
`
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Servers) != 2 || config.Servers[1].Transport != "streamable-http" {
		t.Fatalf("unexpected config: %+v", config)
	}
	if !config.Elicitation.Enabled || config.Elicitation.Duration().Seconds() != 30 {
		t.Fatalf("unexpected elicitation config: %+v", config.Elicitation)
	}
}

func TestLoadConfigRejectsSSE(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "mcp.toml")
	if err := os.WriteFile(configPath, []byte("[[servers]]\nname='old'\nurl='http://localhost/mcp'\ntransport='sse'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(configPath); err == nil {
		t.Fatal("expected deprecated SSE transport to be rejected")
	}
}

func TestLoadConfigStdio(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "mcp.toml")
	content := `
[[servers]]
name = "local-tool"
transport = "stdio"
command = "echo"
args = ["hello", "world"]
`
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("unexpected error loading stdio config: %v", err)
	}
	if len(cfg.Servers) != 1 || cfg.Servers[0].Transport != "stdio" || cfg.Servers[0].Command != "echo" {
		t.Fatalf("unexpected stdio server parsed: %+v", cfg.Servers[0])
	}
	if len(cfg.Servers[0].Args) != 2 || cfg.Servers[0].Args[0] != "hello" {
		t.Fatalf("unexpected stdio args: %+v", cfg.Servers[0].Args)
	}
}

func TestLoadConfigStdioRequiresCommand(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "mcp.toml")
	content := `
[[servers]]
name = "bad-stdio"
transport = "stdio"
`
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(configPath); err == nil {
		t.Fatal("expected stdio server without command to fail validation")
	}
}
