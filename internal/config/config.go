package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
)

const defaultMaxRequestBytes int64 = 32 << 20

type Config struct {
	Listen          string `json:"listen"`
	APIKey          string `json:"api_key"`
	DatabaseFile    string `json:"database_file"`
	CredentialsFile string `json:"credentials_file"`
	UpstreamBaseURL string `json:"upstream_base_url"`
	UpstreamProxy   string `json:"upstream_proxy"`
	MaxRequestBytes int64  `json:"max_request_bytes"`
	AutoRefresh     bool   `json:"auto_refresh_enabled"`
}

func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	cfg := Config{AutoRefresh: true}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	applyEnvironment(&cfg)
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyEnvironment(cfg *Config) {
	if value := strings.TrimSpace(os.Getenv("CLAUDE_RELAY_LISTEN")); value != "" {
		cfg.Listen = value
	}
	if value := strings.TrimSpace(os.Getenv("CLAUDE_RELAY_API_KEY")); value != "" {
		cfg.APIKey = value
	}
	if value := strings.TrimSpace(os.Getenv("CLAUDE_RELAY_CREDENTIALS_FILE")); value != "" {
		cfg.CredentialsFile = value
	}
	if value := strings.TrimSpace(os.Getenv("CLAUDE_RELAY_DATABASE_FILE")); value != "" {
		cfg.DatabaseFile = value
	}
	if value := strings.TrimSpace(os.Getenv("CLAUDE_RELAY_UPSTREAM_BASE_URL")); value != "" {
		cfg.UpstreamBaseURL = value
	}
	if value := strings.TrimSpace(os.Getenv("CLAUDE_RELAY_AUTO_REFRESH_ENABLED")); value != "" {
		cfg.AutoRefresh = strings.EqualFold(value, "true") || value == "1"
	}
}

func (cfg *Config) validate() error {
	cfg.Listen = strings.TrimSpace(cfg.Listen)
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	cfg.DatabaseFile = strings.TrimSpace(cfg.DatabaseFile)
	cfg.CredentialsFile = strings.TrimSpace(cfg.CredentialsFile)
	cfg.UpstreamBaseURL = strings.TrimRight(strings.TrimSpace(cfg.UpstreamBaseURL), "/")
	cfg.UpstreamProxy = strings.TrimSpace(cfg.UpstreamProxy)
	if cfg.MaxRequestBytes == 0 {
		cfg.MaxRequestBytes = defaultMaxRequestBytes
	}

	if cfg.Listen == "" {
		return fmt.Errorf("config listen is required")
	}
	if cfg.APIKey == "" {
		return fmt.Errorf("config api_key or CLAUDE_RELAY_API_KEY is required")
	}
	if cfg.DatabaseFile == "" {
		cfg.DatabaseFile = "data/claude-relay.db"
	}
	if cfg.MaxRequestBytes < 1 {
		return fmt.Errorf("config max_request_bytes must be positive")
	}
	parsed, err := url.Parse(cfg.UpstreamBaseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("config upstream_base_url must be an absolute HTTP(S) URL")
	}
	if cfg.UpstreamProxy != "" {
		proxyURL, proxyErr := url.Parse(cfg.UpstreamProxy)
		if proxyErr != nil || proxyURL.Host == "" || (proxyURL.Scheme != "http" && proxyURL.Scheme != "https") {
			return fmt.Errorf("config upstream_proxy must be an absolute HTTP(S) URL")
		}
	}
	return nil
}
