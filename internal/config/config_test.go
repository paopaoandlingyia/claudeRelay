package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadValidatesUpstreamProxy(t *testing.T) {
	t.Setenv("CLAUDE_RELAY_API_KEY", "")
	t.Setenv("CLAUDE_RELAY_ADMIN_API_KEY", "")
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"listen":"127.0.0.1:8317","relay_api_key":"relay-key","admin_api_key":"admin-key","credentials_file":"credentials.json","upstream_base_url":"https://api.anthropic.com","upstream_proxy":"127.0.0.1:7890"}`
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
	base := `{"listen":"127.0.0.1:8567","relay_api_key":"relay-key","admin_api_key":"admin-key","database_file":"relay.db","upstream_base_url":"https://api.anthropic.com"}`
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
	disabled := `{"listen":"127.0.0.1:8567","relay_api_key":"relay-key","admin_api_key":"admin-key","database_file":"relay.db","upstream_base_url":"https://api.anthropic.com","auto_refresh_enabled":false}`
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

func TestContainerEnvironmentOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"listen":"127.0.0.1:8567","relay_api_key":"file-relay-key","admin_api_key":"file-admin-key","database_file":"relay.db","upstream_base_url":"https://api.anthropic.com"}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_RELAY_LISTEN", "0.0.0.0:8567")
	t.Setenv("CLAUDE_RELAY_API_KEY", "environment-key")
	t.Setenv("CLAUDE_RELAY_ADMIN_API_KEY", "environment-admin-key")
	t.Setenv("CLAUDE_RELAY_DATABASE_FILE", "/data/claude-relay.db")
	t.Setenv("CLAUDE_RELAY_UPSTREAM_PROXY", "http://proxy:7890")
	t.Setenv("CLAUDE_RELAY_MAX_REQUEST_BYTES", "1048576")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "0.0.0.0:8567" || cfg.RelayAPIKey != "environment-key" || cfg.AdminAPIKey != "environment-admin-key" {
		t.Fatalf("server environment overrides = listen %q relay key %q admin key %q", cfg.Listen, cfg.RelayAPIKey, cfg.AdminAPIKey)
	}
	if cfg.DatabaseFile != "/data/claude-relay.db" || cfg.UpstreamProxy != "http://proxy:7890" {
		t.Fatalf("runtime environment overrides = database %q proxy %q", cfg.DatabaseFile, cfg.UpstreamProxy)
	}
	if cfg.MaxRequestBytes != 1048576 {
		t.Fatalf("max request bytes = %d", cfg.MaxRequestBytes)
	}
}

func TestInvalidEnvironmentRequestLimitFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"listen":"127.0.0.1:8567","relay_api_key":"relay-key","admin_api_key":"admin-key","database_file":"relay.db","upstream_base_url":"https://api.anthropic.com"}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_RELAY_MAX_REQUEST_BYTES", "not-a-number")
	if _, err := Load(path); err == nil {
		t.Fatal("Load() accepted an invalid environment request limit")
	}
}

func TestRelayAndAdminKeysMustDiffer(t *testing.T) {
	t.Setenv("CLAUDE_RELAY_API_KEY", "same-key")
	t.Setenv("CLAUDE_RELAY_ADMIN_API_KEY", "same-key")
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"listen":"127.0.0.1:8567","database_file":"relay.db","upstream_base_url":"https://api.anthropic.com"}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load() accepted identical relay and admin API keys")
	}
}
