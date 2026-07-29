package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadValidatesUpstreamProxy(t *testing.T) {
	t.Setenv("CLAUDE_RELAY_API_KEY", "")
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"listen":"127.0.0.1:8317","api_key":"key","credentials_file":"credentials.json","upstream_base_url":"https://api.anthropic.com","upstream_proxy":"127.0.0.1:7890"}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load() accepted a proxy URL without a scheme")
	}
}
