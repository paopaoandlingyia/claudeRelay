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

func TestAutoRefreshDefaultsOnAndCanBeDisabled(t *testing.T) {
	t.Setenv("CLAUDE_RELAY_AUTO_REFRESH_ENABLED", "")
	path := filepath.Join(t.TempDir(), "config.json")
	base := `{"listen":"127.0.0.1:8567","api_key":"key","database_file":"relay.db","upstream_base_url":"https://api.anthropic.com"}`
	if err := os.WriteFile(path, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AutoRefresh {
		t.Fatal("automatic refresh did not default to enabled")
	}
	disabled := `{"listen":"127.0.0.1:8567","api_key":"key","database_file":"relay.db","upstream_base_url":"https://api.anthropic.com","auto_refresh_enabled":false}`
	if err := os.WriteFile(path, []byte(disabled), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AutoRefresh {
		t.Fatal("explicit automatic refresh disable was ignored")
	}
}
