package credential

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Credential struct {
	Type         string         `json:"type"`
	AccessToken  string         `json:"access_token"`
	RefreshToken string         `json:"refresh_token,omitempty"`
	ExpiresAt    string         `json:"expired,omitempty"`
	Email        string         `json:"email,omitempty"`
	AccountUUID  string         `json:"account_uuid,omitempty"`
	Extra        map[string]any `json:"extra,omitempty"`
}

func Load(path string) (Credential, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Credential{}, fmt.Errorf("read credentials: %w", err)
	}
	var cred Credential
	if err := json.Unmarshal(raw, &cred); err != nil {
		return Credential{}, fmt.Errorf("decode credentials: %w", err)
	}
	if err := cred.validate(); err != nil {
		return Credential{}, err
	}
	return cred, nil
}

func Import(sourcePath, destinationPath string) (Credential, error) {
	raw, err := os.ReadFile(sourcePath)
	if err != nil {
		return Credential{}, fmt.Errorf("read source credentials: %w", err)
	}

	var source map[string]any
	if err := json.Unmarshal(raw, &source); err != nil {
		return Credential{}, fmt.Errorf("decode source credentials: %w", err)
	}
	cred := Credential{
		Type:         stringValue(source, "type"),
		AccessToken:  stringValue(source, "access_token"),
		RefreshToken: stringValue(source, "refresh_token"),
		ExpiresAt:    firstStringValue(source, "expired", "expires_at", "expiry"),
		Email:        stringValue(source, "email"),
		AccountUUID:  firstStringValue(source, "account_uuid", "organization_uuid"),
		Extra:        make(map[string]any),
	}
	if cred.Type == "" {
		cred.Type = "claude"
	}
	for key, value := range source {
		switch key {
		case "type", "access_token", "refresh_token", "expired", "expires_at", "expiry", "email", "account_uuid", "organization_uuid":
		default:
			cred.Extra[key] = value
		}
	}
	if len(cred.Extra) == 0 {
		cred.Extra = nil
	}
	if err := cred.validate(); err != nil {
		return Credential{}, err
	}
	if err := save(destinationPath, cred); err != nil {
		return Credential{}, err
	}
	return cred, nil
}

func (cred Credential) IsExpired(now time.Time) bool {
	if strings.TrimSpace(cred.ExpiresAt) == "" {
		return false
	}
	expires, err := time.Parse(time.RFC3339, cred.ExpiresAt)
	return err == nil && !expires.After(now)
}

func (cred Credential) validate() error {
	if strings.TrimSpace(cred.Type) != "claude" {
		return fmt.Errorf("credentials type must be claude")
	}
	if strings.TrimSpace(cred.AccessToken) == "" {
		return fmt.Errorf("credentials access_token is required")
	}
	if cred.ExpiresAt != "" {
		if _, err := time.Parse(time.RFC3339, cred.ExpiresAt); err != nil {
			return fmt.Errorf("credentials expired must use RFC3339: %w", err)
		}
	}
	return nil
}

func save(path string, cred Credential) error {
	raw, err := json.MarshalIndent(cred, "", "  ")
	if err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}
	raw = append(raw, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create credential directory: %w", err)
	}
	temporary, err := os.CreateTemp(dir, ".credentials-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary credential file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protect temporary credential file: %w", err)
	}
	if _, err := temporary.Write(raw); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary credential file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary credential file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary credential file: %w", err)
	}
	if err := replaceFile(temporaryPath, path); err != nil {
		return fmt.Errorf("replace credential file: %w", err)
	}
	return nil
}

func replaceFile(temporaryPath, destinationPath string) error {
	if err := os.Rename(temporaryPath, destinationPath); err == nil {
		return nil
	}
	if _, err := os.Stat(destinationPath); err != nil {
		return err
	}

	dir := filepath.Dir(destinationPath)
	backup, err := os.CreateTemp(dir, ".credentials-backup-*.tmp")
	if err != nil {
		return err
	}
	backupPath := backup.Name()
	if err := backup.Close(); err != nil {
		_ = os.Remove(backupPath)
		return err
	}
	if err := os.Remove(backupPath); err != nil {
		return err
	}
	if err := os.Rename(destinationPath, backupPath); err != nil {
		return err
	}

	replaced := false
	defer func() {
		if replaced {
			_ = os.Remove(backupPath)
			return
		}
		_ = os.Rename(backupPath, destinationPath)
	}()
	if err := os.Rename(temporaryPath, destinationPath); err != nil {
		return err
	}
	replaced = true
	return nil
}

func stringValue(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return strings.TrimSpace(value)
}

func firstStringValue(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringValue(values, key); value != "" {
			return value
		}
	}
	return ""
}
