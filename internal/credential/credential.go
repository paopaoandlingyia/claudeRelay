package credential

import (
	"crypto/rand"
	"encoding/hex"
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
	DeviceID     string         `json:"device_id,omitempty"`
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
	changed, err := cred.ensureIdentity()
	if err != nil {
		return Credential{}, err
	}
	if changed {
		if err := save(path, cred); err != nil {
			return Credential{}, fmt.Errorf("persist generated credential identity: %w", err)
		}
	}
	return cred, nil
}

func Import(sourcePath, destinationPath string) (Credential, error) {
	cred, err := ReadImport(sourcePath)
	if err != nil {
		return Credential{}, err
	}
	if err := save(destinationPath, cred); err != nil {
		return Credential{}, err
	}
	return cred, nil
}

// ReadImport parses a CLIProxyAPI-compatible Claude credential without
// persisting it. Runtime stores use this when importing into their own database.
func ReadImport(sourcePath string) (Credential, error) {
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
		DeviceID:     stringValue(source, "device_id"),
		Extra:        make(map[string]any),
	}
	if cred.Type == "" {
		cred.Type = "claude"
	}
	for key, value := range source {
		switch key {
		case "type", "access_token", "refresh_token", "expired", "expires_at", "expiry", "email", "account_uuid", "organization_uuid", "device_id":
		default:
			cred.Extra[key] = value
		}
	}
	if len(cred.Extra) == 0 {
		cred.Extra = nil
	}
	return Prepare(cred)
}

// Prepare validates a credential and fills stable attribution identity fields.
func Prepare(cred Credential) (Credential, error) {
	if err := cred.validate(); err != nil {
		return Credential{}, err
	}
	if _, err := cred.ensureIdentity(); err != nil {
		return Credential{}, err
	}
	return cred, nil
}

func (cred *Credential) ensureIdentity() (bool, error) {
	changed := false
	if !isUUID(cred.AccountUUID) {
		value, err := randomUUID()
		if err != nil {
			return false, fmt.Errorf("generate account identity: %w", err)
		}
		cred.AccountUUID = value
		changed = true
	}
	if !isHex(cred.DeviceID, 64) {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return false, fmt.Errorf("generate device identity: %w", err)
		}
		cred.DeviceID = hex.EncodeToString(raw)
		changed = true
	} else {
		normalized := strings.ToLower(cred.DeviceID)
		changed = changed || normalized != cred.DeviceID
		cred.DeviceID = normalized
	}
	return changed, nil
}

func randomUUID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return formatUUID(raw), nil
}

func formatUUID(raw []byte) string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

func isUUID(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	return isHex(strings.ReplaceAll(value, "-", ""), 32)
}

func isHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
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
