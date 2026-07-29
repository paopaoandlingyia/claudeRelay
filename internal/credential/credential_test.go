package credential

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestImportCLIProxyCredential(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.json")
	destination := filepath.Join(dir, "data", "credentials.json")
	raw := `{
  "type": "claude",
  "access_token": "access-secret",
  "refresh_token": "refresh-secret",
  "expired": "2030-01-02T03:04:05Z",
  "email": "user@example.com",
  "account_uuid": "11111111-1111-4111-8111-111111111111",
  "custom": "retained"
}`
	if err := os.WriteFile(source, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	imported, err := Import(source, destination)
	if err != nil {
		t.Fatalf("Import() error = %v", err)
	}
	if imported.AccessToken != "access-secret" || imported.RefreshToken != "refresh-secret" {
		t.Fatal("tokens were not imported")
	}
	if imported.AccountUUID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("AccountUUID = %q", imported.AccountUUID)
	}
	if len(imported.DeviceID) != 64 {
		t.Fatalf("DeviceID = %q", imported.DeviceID)
	}
	if imported.Extra["custom"] != "retained" {
		t.Fatalf("custom extra = %#v", imported.Extra["custom"])
	}

	saved, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Credential
	if err := json.Unmarshal(saved, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.AccessToken != imported.AccessToken {
		t.Fatal("saved credential differs from imported credential")
	}
	if decoded.DeviceID != imported.DeviceID {
		t.Fatal("saved device identity differs from imported credential")
	}

	if _, err := Import(source, destination); err != nil {
		t.Fatalf("second Import() error = %v", err)
	}
}

func TestLoadGeneratesIdentityOnce(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte(`{"type":"claude","access_token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !isUUID(first.AccountUUID) || !isHex(first.DeviceID, 64) {
		t.Fatalf("generated identity has an invalid shape: account=%q device=%q", first.AccountUUID, first.DeviceID)
	}
	if first.AccountUUID != second.AccountUUID || first.DeviceID != second.DeviceID {
		t.Fatal("generated identity was not stable across loads")
	}
}

func TestImportRejectsNonClaudeCredential(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.json")
	if err := os.WriteFile(source, []byte(`{"type":"codex","access_token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(source, filepath.Join(dir, "destination.json")); err == nil {
		t.Fatal("Import() succeeded for a non-Claude credential")
	}
}
