package mcpclient

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pelletier/go-toml"
)

type ElicitationConfig struct {
	Enabled    bool `toml:"enabled"`
	AutoAccept bool `toml:"auto_accept"`
	Timeout    int  `toml:"timeout"`
}

func (c ElicitationConfig) Duration() time.Duration {
	if c.Timeout <= 0 {
		return 30 * time.Second
	}
	return time.Duration(c.Timeout) * time.Second
}

type FileConfig struct {
	Elicitation ElicitationConfig `toml:"elicitation"`
	Servers     []Server          `toml:"servers"`
}

func LoadConfig(configPath string) (FileConfig, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return FileConfig{}, fmt.Errorf("read MCP config: %w", err)
	}
	var config FileConfig
	if err := toml.Unmarshal(data, &config); err != nil {
		return FileConfig{}, fmt.Errorf("parse MCP config: %w", err)
	}
	if len(config.Servers) == 0 {
		return FileConfig{}, fmt.Errorf("MCP config contains no [[servers]] blocks")
	}
	seen := make(map[string]struct{}, len(config.Servers))
	for index := range config.Servers {
		server := &config.Servers[index]
		server.Name = strings.TrimSpace(server.Name)
		server.URL = strings.TrimSpace(server.URL)
		server.Command = strings.TrimSpace(server.Command)
		server.Transport = strings.TrimSpace(strings.ToLower(server.Transport))
		if server.Transport == "" {
			server.Transport = "streamable-http"
		}
		if server.Name == "" {
			return FileConfig{}, fmt.Errorf("server %d requires name", index+1)
		}
		switch server.Transport {
		case "streamable-http", "http":
			if server.URL == "" {
				return FileConfig{}, fmt.Errorf("server %q (%s) requires url", server.Name, server.Transport)
			}
		case "stdio":
			if server.Command == "" {
				return FileConfig{}, fmt.Errorf("server %q (stdio) requires command", server.Name)
			}
		default:
			return FileConfig{}, fmt.Errorf("server %q uses unsupported transport %q; use streamable-http or stdio", server.Name, server.Transport)
		}
		if _, duplicate := seen[server.Name]; duplicate {
			return FileConfig{}, fmt.Errorf("duplicate server name %q", server.Name)
		}
		seen[server.Name] = struct{}{}
	}
	return config, nil
}
