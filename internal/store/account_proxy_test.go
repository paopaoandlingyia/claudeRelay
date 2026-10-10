package store

import (
	"path/filepath"
	"testing"

	"github.com/local/claude-relay/internal/credential"
)

func TestAccountProxyPersistsAndReimportPreservesExit(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "relay.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	cred := credential.Credential{Type: "claude", AccessToken: "token", AccountUUID: "11111111-1111-4111-8111-111111111111", DeviceID: "device"}
	account, err := database.ImportAccount(t.Context(), "new", cred)
	if err != nil {
		t.Fatal(err)
	}
	if account.ProxyMode != "direct" {
		t.Fatalf("new account mode=%s", account.ProxyMode)
	}
	address := "http://user:p%40ss%23word@proxy.invalid:12323"
	if _, err := database.SetAccountProxy(t.Context(), "new", "custom", address); err != nil {
		t.Fatal(err)
	}
	account, err = database.ImportAccount(t.Context(), "renamed", cred)
	if err != nil {
		t.Fatal(err)
	}
	if account.ProxyMode != "custom" || account.ProxyURL != address {
		t.Fatal("reimport changed exit")
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	account, found, err := database.AccountByAlias(t.Context(), "renamed")
	if err != nil || !found || account.ProxyURL != address || account.ProxyMode != "custom" {
		t.Fatal("exit lost after reopen")
	}
	for _, input := range []struct{ mode, address string }{{"invalid", ""}, {"custom", ""}, {"custom", "socks5://proxy.invalid:12324"}, {"custom", "http://user:pass@proxy.invalid/path"}, {"direct", address}, {"global", address}} {
		if _, err := database.SetAccountProxy(t.Context(), "renamed", input.mode, input.address); err == nil {
			t.Fatalf("invalid proxy accepted: %s", input.mode)
		}
	}
}
