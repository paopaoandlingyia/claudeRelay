package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestSignExistingCCHDefaultsOnAndCanBeDisabled(t *testing.T) {
	t.Setenv("CLAUDE_RELAY_API_KEY", "")
	for _, test := range []struct {
		name    string
		setting string
		want    bool
	}{
		{name: "default", setting: "", want: true},
		{name: "disabled", setting: `,"sign_existing_cch":false`, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			raw := fmt.Sprintf(`{"listen":"127.0.0.1:8317","api_key":"key","credentials_file":"credentials.json","upstream_base_url":"https://api.anthropic.com"%s}`, test.setting)
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.SignExistingCCH != test.want {
				t.Fatalf("SignExistingCCH = %v, want %v", cfg.SignExistingCCH, test.want)
			}
		})
	}
}

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
