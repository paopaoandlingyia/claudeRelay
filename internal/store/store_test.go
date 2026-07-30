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
	if account.Enabled {
		t.Fatal("newly imported account was enabled")
	}
	account, err = database.SetAccountEnabled(context.Background(), account.Alias, true)
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
	if _, err := database.SetAccountEnabled(context.Background(), first.Alias, true); err != nil {
		t.Fatal(err)
	}
	base.AccessToken = "two"
	second, err := database.ImportAccount(context.Background(), "ACCOUNT", base)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || second.AccessToken != "two" || second.Enabled {
		t.Fatalf("updated account = %#v, first = %#v", second, first)
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	database, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func importTestAccount(t *testing.T, database *Store, alias, uuid string) Account {
	t.Helper()
	account, err := database.ImportAccount(context.Background(), alias, credential.Credential{
		Type:        "claude",
		AccessToken: "secret",
		AccountUUID: uuid,
		DeviceID:    strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	return account
}

func TestDeleteAccountRemovesBindingsAndCooldowns(t *testing.T) {
	t.Parallel()
	database := newTestStore(t)
	ctx := context.Background()
	account := importTestAccount(t, database, "primary", "11111111-1111-4111-8111-111111111111")
	if _, err := database.SetAccountEnabled(ctx, account.Alias, true); err != nil {
		t.Fatal(err)
	}
	if err := database.Bind(ctx, "route", account.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := database.Cooldown(ctx, account.ID, "", time.Now().Add(time.Hour), "rate_limit"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.DeleteAccount(ctx, account.Alias); err != nil {
		t.Fatal(err)
	}
	if _, found, err := database.AccountByAlias(ctx, account.Alias); err != nil || found {
		t.Fatalf("account still present, found = %v, err = %v", found, err)
	}
	counts, err := database.SessionBindingCounts(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != 0 {
		t.Errorf("session bindings survived deletion: %v", counts)
	}
	cooldowns, err := database.ActiveCooldowns(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(cooldowns) != 0 {
		t.Errorf("cooldowns survived deletion: %v", cooldowns)
	}
	if _, err := database.DeleteAccount(ctx, account.Alias); err == nil {
		t.Error("deleting a missing account returned no error")
	}
}

func TestRenameAccountKeepsIdentityAndRejectsCollisions(t *testing.T) {
	t.Parallel()
	database := newTestStore(t)
	ctx := context.Background()
	first := importTestAccount(t, database, "primary", "11111111-1111-4111-8111-111111111111")
	importTestAccount(t, database, "secondary", "22222222-2222-4222-8222-222222222222")
	if _, err := database.SetAccountEnabled(ctx, first.Alias, true); err != nil {
		t.Fatal(err)
	}

	renamed, err := database.RenameAccount(ctx, "primary", "work")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.ID != first.ID || renamed.Alias != "work" || !renamed.Enabled {
		t.Fatalf("renamed account = %#v, want same id and enabled state as %#v", renamed, first)
	}
	if _, err := database.RenameAccount(ctx, "work", "secondary"); err == nil {
		t.Error("renaming onto an existing alias returned no error")
	}
	if _, err := database.RenameAccount(ctx, "work", "not a valid alias"); err == nil {
		t.Error("invalid alias was accepted")
	}
}

func TestClearCooldownsRestoresRouting(t *testing.T) {
	t.Parallel()
	database := newTestStore(t)
	ctx := context.Background()
	account := importTestAccount(t, database, "primary", "11111111-1111-4111-8111-111111111111")
	if _, err := database.SetAccountEnabled(ctx, account.Alias, true); err != nil {
		t.Fatal(err)
	}
	if err := database.Cooldown(ctx, account.ID, "claude-opus-5", time.Now().Add(time.Hour), "rate_limit"); err != nil {
		t.Fatal(err)
	}
	cooldowns, err := database.ActiveCooldowns(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(cooldowns) != 1 || cooldowns[0].Model != "claude-opus-5" || cooldowns[0].Reason != "rate_limit" {
		t.Fatalf("active cooldowns = %#v", cooldowns)
	}
	removed, err := database.ClearCooldowns(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("cleared %d cooldowns, want 1", removed)
	}
	accounts, err := database.Accounts(ctx, "claude-opus-5", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Errorf("routable accounts = %d, want 1 after clearing the cooldown", len(accounts))
	}
}

func TestUpdateTokensRecordsRefreshTime(t *testing.T) {
	t.Parallel()
	database := newTestStore(t)
	ctx := context.Background()
	account := importTestAccount(t, database, "primary", "11111111-1111-4111-8111-111111111111")
	if account.LastRefreshAt != 0 {
		t.Fatalf("LastRefreshAt = %d on a fresh import, want 0", account.LastRefreshAt)
	}
	updated, err := database.UpdateTokens(ctx, account.ID, "new-access", "new-refresh", "2026-08-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastRefreshAt == 0 {
		t.Error("LastRefreshAt was not recorded after a token rotation")
	}
}
