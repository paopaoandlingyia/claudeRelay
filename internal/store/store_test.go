package store

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/local/claude-relay/internal/credential"
)

func TestImportBindingAndCooldown(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "relay.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	account, err := database.ImportAccount(context.Background(), "Primary", credential.Credential{
		Type:        "claude",
		AccessToken: "secret",
		AccountUUID: "11111111-1111-4111-8111-111111111111",
		DeviceID:    strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Bind(context.Background(), "route", account.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	bound, found, err := database.BoundAccount(context.Background(), "route", time.Now())
	if err != nil || !found || bound.ID != account.ID {
		t.Fatalf("bound account = %#v found=%v err=%v", bound, found, err)
	}
	if err := database.Cooldown(context.Background(), account.ID, "model", time.Now().Add(time.Minute), "test"); err != nil {
		t.Fatal(err)
	}
	accounts, err := database.Accounts(context.Background(), "model", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 0 {
		t.Fatalf("cooling account remained schedulable: %#v", accounts)
	}
	mode, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && mode.Mode().Perm()&0o077 != 0 {
		t.Fatalf("database permissions = %o", mode.Mode().Perm())
	}
}

func TestImportSameAliasUpdatesCredentialWithoutChangingID(t *testing.T) {
	t.Parallel()
	database, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	base := credential.Credential{Type: "claude", AccessToken: "one", AccountUUID: "11111111-1111-4111-8111-111111111111", DeviceID: strings.Repeat("a", 64)}
	first, err := database.ImportAccount(context.Background(), "account", base)
	if err != nil {
		t.Fatal(err)
	}
	base.AccessToken = "two"
	second, err := database.ImportAccount(context.Background(), "ACCOUNT", base)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || second.AccessToken != "two" {
		t.Fatalf("updated account = %#v, first = %#v", second, first)
	}
}
