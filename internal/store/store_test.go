package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/local/claude-relay/internal/credential"
)

func TestSchemaV3MigratesExistingAccountsToCompatiblePool(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "relay.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`
		CREATE TABLE accounts (
			id INTEGER PRIMARY KEY,
			alias TEXT NOT NULL COLLATE NOCASE UNIQUE,
			type TEXT NOT NULL,
			access_token TEXT NOT NULL,
			refresh_token TEXT NOT NULL DEFAULT '',
			expires_at TEXT NOT NULL DEFAULT '',
			email TEXT NOT NULL DEFAULT '',
			account_uuid TEXT NOT NULL,
			device_id TEXT NOT NULL,
			extra_json TEXT NOT NULL DEFAULT '{}',
			enabled INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			last_refresh_at INTEGER NOT NULL DEFAULT 0
		);
		INSERT INTO accounts (
			alias,type,access_token,account_uuid,device_id,enabled,created_at,updated_at
		) VALUES ('legacy','claude','secret','11111111-1111-4111-8111-111111111111','device',1,1,1);
		CREATE TABLE account_cooldowns (
			account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			model TEXT NOT NULL DEFAULT '',
			until_at INTEGER NOT NULL,
			reason TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(account_id, model)
		);
		INSERT INTO account_cooldowns(account_id,model,until_at,reason)
			VALUES (1,'claude-test',4102444800,'legacy cooldown');
		PRAGMA user_version=3;
	`)
	if err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	account, found, err := database.AccountByAlias(context.Background(), "legacy")
	if err != nil || !found {
		t.Fatalf("legacy account lookup: found=%v err=%v", found, err)
	}
	if account.Pool != AccountPoolCompatible || !account.Enabled {
		t.Fatalf("migrated account = %#v", account)
	}
	var version int
	if err := database.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("schema version = %d, want %d", version, schemaVersion)
	}
	var observedAt int64
	if err := database.db.QueryRow(`SELECT observed_at FROM account_cooldowns WHERE account_id=1 AND model='claude-test'`).Scan(&observedAt); err != nil {
		t.Fatalf("read migrated cooldown: %v", err)
	}
	if observedAt != 0 {
		t.Fatalf("migrated cooldown observed_at = %d, want 0", observedAt)
	}
}

func TestSchemaV7RemovesOnlySeededSonnetFiveIncrease(t *testing.T) {
	for _, test := range []struct {
		name      string
		source    string
		wantCount int
	}{
		{name: "seeded price", source: "Anthropic API pricing", wantCount: 0},
		{name: "operator price", source: "operator override", wantCount: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "relay.db")
			database, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = database.db.Exec(`INSERT INTO model_prices(model_pattern,effective_from,
				input_usd_per_mtok,output_usd_per_mtok,cache_creation_5m_usd_per_mtok,
				cache_creation_1h_usd_per_mtok,cache_read_usd_per_mtok,source,created_at)
				VALUES('claude-sonnet-5*',1788192000,3,15,3.75,6,0.3,?,1)`, test.source)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.db.Exec(`PRAGMA user_version=6`); err != nil {
				t.Fatal(err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}

			database, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			var count int
			if err := database.db.QueryRow(`SELECT COUNT(*) FROM model_prices WHERE
				model_pattern='claude-sonnet-5*' AND effective_from=1788192000`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != test.wantCount {
				t.Fatalf("future Sonnet 5 price count = %d, want %d", count, test.wantCount)
			}
		})
	}
}

// TestConnectionPragmas pins the per-connection settings to the DSN. Running
// them as one-off statements at startup would leave any additional pooled
// connection without foreign_keys, silently disabling ON DELETE CASCADE.
func TestConnectionPragmas(t *testing.T) {
	t.Parallel()
	database, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, want := range []struct {
		pragma string
		value  string
	}{
		{"journal_mode", "wal"},
		{"synchronous", "1"},
		{"foreign_keys", "1"},
		{"busy_timeout", "5000"},
	} {
		var got string
		if err := database.db.QueryRow(`PRAGMA ` + want.pragma).Scan(&got); err != nil {
			t.Fatalf("read pragma %s: %v", want.pragma, err)
		}
		if !strings.EqualFold(got, want.value) {
			t.Fatalf("pragma %s = %q, want %q", want.pragma, got, want.value)
		}
	}
}

// TestExpiredBindingsAreIgnoredBeforePruning covers the rate-limited sweep: an
// expired row may still be present, so correctness has to come from the
// expires_at filter rather than from the delete.
func TestExpiredBindingsAreIgnoredBeforePruning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := newTestStore(t)
	account := importTestAccount(t, database, "primary", "11111111-1111-4111-8111-111111111111")
	if _, err := database.SetAccountEnabled(ctx, account.Alias, true); err != nil {
		t.Fatal(err)
	}
	// The first Bind consumes the prune interval, so the expired row written by
	// the second one is guaranteed to still be in the table.
	if err := database.Bind(ctx, "session:warm", account.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := database.Bind(ctx, "prefix:affinity", account.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := database.Bind(ctx, "prefix:stale", account.ID, -time.Hour); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_bindings WHERE route_key='prefix:stale'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("stale binding rows = %d, want the unpruned row to remain", rows)
	}
	if _, found, err := database.BoundAccount(ctx, "prefix:stale", AccountPoolCompatible, time.Now()); err != nil || found {
		t.Fatalf("expired binding was routable: found=%v err=%v", found, err)
	}
	counts, err := database.SessionBindingCounts(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if counts[account.ID] != 2 {
		t.Fatalf("live binding count = %d, want 2", counts[account.ID])
	}
	active, err := database.ActiveSessionCounts(ctx, time.Now(), time.Now().Add(-5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if active[account.ID] != 1 {
		t.Fatalf("active session count = %d, want 1", active[account.ID])
	}
	active, err = database.ActiveSessionCounts(ctx, time.Now(), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if active[account.ID] != 0 {
		t.Fatalf("future-cutoff active session count = %d, want 0", active[account.ID])
	}
}

func TestBindingKeepsLongerAffinityOnlyOnTheSameAccount(t *testing.T) {
	t.Parallel()
	database := newTestStore(t)
	first := importTestAccount(t, database, "first", "11111111-1111-4111-8111-111111111111")
	second := importTestAccount(t, database, "second", "22222222-2222-4222-8222-222222222222")
	for _, account := range []Account{first, second} {
		if _, err := database.SetAccountEnabled(t.Context(), account.Alias, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.Bind(t.Context(), "prefix:cache", first.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := database.Bind(t.Context(), "prefix:cache", first.ID, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	bound, found, err := database.BoundAccount(t.Context(), "prefix:cache", AccountPoolCompatible, time.Now().Add(30*time.Minute))
	if err != nil || !found || bound.ID != first.ID {
		t.Fatalf("short refresh reduced same-account affinity: bound=%#v found=%v err=%v", bound, found, err)
	}
	if err := database.Bind(t.Context(), "prefix:cache", second.ID, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, found, err := database.BoundAccount(t.Context(), "prefix:cache", AccountPoolCompatible, time.Now().Add(30*time.Minute)); err != nil || found {
		t.Fatalf("account switch inherited old affinity: found=%v err=%v", found, err)
	}
	bound, found, err = database.BoundAccount(t.Context(), "prefix:cache", AccountPoolCompatible, time.Now())
	if err != nil || !found || bound.ID != second.ID {
		t.Fatalf("new account binding unavailable: bound=%#v found=%v err=%v", bound, found, err)
	}
}

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
	bound, found, err := database.BoundAccount(context.Background(), "route", AccountPoolCompatible, time.Now())
	if err != nil || !found || bound.ID != account.ID {
		t.Fatalf("bound account = %#v found=%v err=%v", bound, found, err)
	}
	if err := database.Cooldown(context.Background(), account.ID, "model", time.Now().Add(time.Minute), "test"); err != nil {
		t.Fatal(err)
	}
	accounts, err := database.Accounts(context.Background(), AccountPoolCompatible, "model", time.Now())
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
	accounts, err := database.Accounts(ctx, AccountPoolCompatible, "claude-opus-5", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Errorf("routable accounts = %d, want 1 after clearing the cooldown", len(accounts))
	}
}

func TestCooldownDoesNotShortenAnExistingDecision(t *testing.T) {
	t.Parallel()
	database := newTestStore(t)
	account := importTestAccount(t, database, "primary", "11111111-1111-4111-8111-111111111111")
	now := time.Now()
	longUntil := now.Add(time.Hour).Truncate(time.Second)
	if err := database.Cooldown(t.Context(), account.ID, "claude-test", longUntil, "hard_window"); err != nil {
		t.Fatal(err)
	}
	if err := database.Cooldown(t.Context(), account.ID, "claude-test", now.Add(5*time.Second), "transient"); err != nil {
		t.Fatal(err)
	}
	cooldowns, err := database.ActiveCooldowns(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(cooldowns) != 1 || cooldowns[0].UntilAt != longUntil.Unix() || cooldowns[0].Reason != "hard_window" {
		t.Fatalf("cooldown after shorter decision = %#v", cooldowns)
	}
	cleared, err := database.ClearCooldownMatchesObservedBefore(t.Context(), account.ID,
		[]CooldownMatch{{Model: "claude-test", Reason: "transient"}}, time.Now())
	if err != nil || cleared != 0 {
		t.Fatalf("mismatched reason cleared=%v err=%v", cleared, err)
	}
	cleared, err = database.ClearCooldownMatchesObservedBefore(t.Context(), account.ID,
		[]CooldownMatch{{Model: "claude-test", Reason: "hard_window"}}, time.Now())
	if err != nil || cleared != 1 {
		t.Fatalf("matching reason cleared=%v err=%v", cleared, err)
	}
}

func TestOlderObservationCannotClearNewerCooldown(t *testing.T) {
	t.Parallel()
	database := newTestStore(t)
	account := importTestAccount(t, database, "primary", "11111111-1111-4111-8111-111111111111")
	older := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	newer := older.Add(time.Second)
	if err := database.CooldownObservedAt(t.Context(), account.ID, "claude-test", newer.Add(time.Hour), "hard_window", newer); err != nil {
		t.Fatal(err)
	}
	match := []CooldownMatch{{Model: "claude-test", Reason: "hard_window"}}
	cleared, err := database.ClearCooldownMatchesObservedBefore(t.Context(), account.ID, match, older)
	if err != nil || cleared != 0 {
		t.Fatalf("older observation cleared=%v err=%v", cleared, err)
	}
	cleared, err = database.ClearCooldownMatchesObservedBefore(t.Context(), account.ID, match, newer)
	if err != nil || cleared != 1 {
		t.Fatalf("matching observation cleared=%v err=%v", cleared, err)
	}
}

// A fresh account is shared, so both ingresses see it. Moving it to the official
// pool fences it off from the compatible ingress and drops its bindings.
func TestSetAccountPoolClearsBindingsAndFencesCompatibleTraffic(t *testing.T) {
	t.Parallel()
	database := newTestStore(t)
	ctx := context.Background()
	account := importTestAccount(t, database, "primary", "11111111-1111-4111-8111-111111111111")
	if account.Pool != AccountPoolCompatible {
		t.Fatalf("fresh account pool = %q", account.Pool)
	}
	if _, err := database.SetAccountEnabled(ctx, account.Alias, true); err != nil {
		t.Fatal(err)
	}
	for _, ingress := range []string{AccountPoolCompatible, AccountPoolOfficial} {
		shared, err := database.Accounts(ctx, ingress, "claude-test", time.Now())
		if err != nil || len(shared) != 1 {
			t.Fatalf("%s ingress accounts = %#v err=%v", ingress, shared, err)
		}
	}
	if err := database.Bind(ctx, "route", account.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	moved, err := database.SetAccountPool(ctx, account.Alias, AccountPoolOfficial)
	if err != nil {
		t.Fatal(err)
	}
	if moved.Pool != AccountPoolOfficial || !moved.Enabled {
		t.Fatalf("moved account = %#v", moved)
	}
	if _, found, err := database.BoundAccount(ctx, "route", AccountPoolOfficial, time.Now()); err != nil || found {
		t.Fatalf("old binding survived pool move: found=%v err=%v", found, err)
	}
	compatible, err := database.Accounts(ctx, AccountPoolCompatible, "claude-test", time.Now())
	if err != nil || len(compatible) != 0 {
		t.Fatalf("compatible accounts = %#v err=%v", compatible, err)
	}
	official, err := database.Accounts(ctx, AccountPoolOfficial, "claude-test", time.Now())
	if err != nil || len(official) != 1 || official[0].ID != account.ID {
		t.Fatalf("official accounts = %#v err=%v", official, err)
	}
	if _, err := database.SetAccountPool(ctx, account.Alias, "unknown"); err == nil {
		t.Fatal("invalid account pool was accepted")
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
