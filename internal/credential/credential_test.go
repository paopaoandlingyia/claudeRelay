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
  "account_uuid": "account-1",
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
	if imported.AccountUUID != "account-1" {
		t.Fatalf("AccountUUID = %q", imported.AccountUUID)
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

	if _, err := Import(source, destination); err != nil {
		t.Fatalf("second Import() error = %v", err)
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
