package store

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestFiveHourEventsAreIdempotentAndAggregatedByWindowAndModel(t *testing.T) {
	database := newTestStore(t)
	account := importTestAccount(t, database, "usage", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	ctx := context.Background()
	reset := time.Now().Add(4 * time.Hour).Truncate(time.Second)
	events := []FiveHourEvent{
		{EventKey: "later", AccountID: account.ID, ResetsAt: strconv.FormatInt(reset.Unix(), 10), Kind: FiveHourEventMessages,
			ObservedAt: 2000, CompletedAt: 2500, Model: "sonnet", Status: 200, UsedPercent: 20,
			Usage: UsageCounters{InputTokens: 20, Requests: 1}, UsageSeen: true, Complete: true},
		{EventKey: "earlier", AccountID: account.ID, ResetsAt: strconv.FormatInt(reset.Unix(), 10), Kind: FiveHourEventMessages,
			ObservedAt: 1000, CompletedAt: 1500, Model: "sonnet", Status: 200, UsedPercent: 10,
			Usage: UsageCounters{InputTokens: 10, Requests: 1}, UsageSeen: false, Complete: false},
	}
	// Insert out of observation order, then repeat the batch. Event keys make
	// retries safe without multiplying token totals.
	if err := database.AddFiveHourEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	if err := database.AddFiveHourEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	windows, err := database.FiveHourWindows(ctx, false, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 1 {
		t.Fatalf("windows=%+v", windows)
	}
	window := windows[0]
	if window.FirstObservedAt != 1000 || window.LastObservedAt != 2000 || window.FirstUsedPercent != 10 ||
		window.LastUsedPercent != 20 || window.EventCount != 2 || window.MissingUsageCount != 1 ||
		window.IncompleteCount != 1 || window.ByModel["sonnet"].InputTokens != 30 {
		t.Fatalf("window=%+v", window)
	}
	if err := database.AddFiveHourEvents(ctx, []FiveHourEvent{{
		EventKey: "oauth-only", AccountID: account.ID, ResetsAt: strconv.FormatInt(reset.Add(time.Hour).Unix(), 10),
		Kind: FiveHourEventOAuth, ObservedAt: 3000, CompletedAt: 3000, UsedPercent: 30, Complete: true,
	}}); err != nil {
		t.Fatal(err)
	}
	windows, err = database.FiveHourWindows(ctx, false, 0, 10)
	if err != nil || len(windows) != 1 {
		t.Fatalf("OAuth-only reading appeared as a relayed current window: windows=%+v err=%v", windows, err)
	}
}

func TestMarkFiveHourExhaustedCreatesWindowWithoutUtilizationReading(t *testing.T) {
	database := newTestStore(t)
	account := importTestAccount(t, database, "usage", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	now := time.Now()
	reset := now.Add(4 * time.Hour).Unix()
	event := FiveHourEvent{
		EventKey: "exhaustion:request", AccountID: account.ID, ResetsAt: strconv.FormatInt(reset, 10),
		ObservedAt: now.UnixMilli(), CompletedAt: now.UnixMilli(), Status: 429, UsedPercent: -1,
	}
	if err := database.MarkFiveHourExhausted(context.Background(), event, "explicit"); err != nil {
		t.Fatal(err)
	}
	windows, err := database.FiveHourWindows(context.Background(), true, now.UnixMilli(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 1 || windows[0].ExhaustedAt != now.UnixMilli() || windows[0].ExhaustionReason != "explicit" {
		t.Fatalf("exhausted windows=%+v", windows)
	}
}

func TestFiveHourEventsMergeAdjacentResetIdentities(t *testing.T) {
	database := newTestStore(t)
	account := importTestAccount(t, database, "jitter", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	ctx := context.Background()
	reset := time.Now().Add(4 * time.Hour).Truncate(time.Second)
	if err := database.AddFiveHourEvents(ctx, []FiveHourEvent{{
		EventKey: "oauth-jitter", AccountID: account.ID, ResetsAt: strconv.FormatInt(reset.Add(-time.Second).Unix(), 10),
		Kind: FiveHourEventOAuth, ObservedAt: 1000, CompletedAt: 1000, UsedPercent: 0, Complete: true,
	}, {
		EventKey: "message-exact", AccountID: account.ID, ResetsAt: strconv.FormatInt(reset.Unix(), 10),
		Kind: FiveHourEventMessages, ObservedAt: 2000, CompletedAt: 2500, Model: "opus", Status: 200,
		UsedPercent: 100, Usage: UsageCounters{OutputTokens: 20, Requests: 1}, UsageSeen: true, Complete: true,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := database.MarkFiveHourExhausted(ctx, FiveHourEvent{
		EventKey: "exhaustion-exact", AccountID: account.ID, ResetsAt: strconv.FormatInt(reset.Unix(), 10),
		ObservedAt: 3000, CompletedAt: 3000, Status: 429, UsedPercent: 100,
	}, "explicit"); err != nil {
		t.Fatal(err)
	}
	windows, err := database.FiveHourWindows(ctx, true, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 1 || windows[0].EventCount != 1 || windows[0].ByModel["opus"].OutputTokens != 20 {
		t.Fatalf("adjacent resets produced split windows: %+v", windows)
	}
	events, err := database.AllFiveHourEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.ResetsAt != windows[0].ResetsAt {
			t.Fatalf("event %q reset=%q, window reset=%q", event.EventKey, event.ResetsAt, windows[0].ResetsAt)
		}
	}
}

func TestSchemaNineMergesExistingAdjacentFiveHourWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	account := importTestAccount(t, database, "migration", "cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	reset := time.Now().Add(4 * time.Hour).Truncate(time.Second).Unix()
	if _, err := database.db.Exec(`INSERT INTO five_hour_windows(account_id,resets_at,first_observed_at,last_observed_at,
		first_used_percent,last_used_percent,max_used_percent,exhausted_at,exhaustion_reason) VALUES
		(?,?,?,?,?,?,?,?,?),(?,?,?,?,?,?,?,?,?)`,
		account.ID, strconv.FormatInt(reset-1, 10), 1000, 1500, 0, 10, 10, 0, "",
		account.ID, strconv.FormatInt(reset, 10), 2000, 3000, 20, 100, 100, 3000, "explicit"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`INSERT INTO five_hour_events(event_key,account_id,resets_at,kind,observed_at,
		completed_at,model,status,used_percent,input_tokens,usage_seen,complete) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		"migration-message", account.ID, strconv.FormatInt(reset, 10), FiveHourEventMessages,
		2000, 2500, "opus", 200, 100, 30, true, true); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`PRAGMA user_version=8`); err != nil {
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
	windows, err := database.AllFiveHourWindows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 1 {
		t.Fatalf("migration left adjacent windows: %+v", windows)
	}
	window := windows[0]
	if window.ResetsAt != strconv.FormatInt(reset, 10) || window.FirstObservedAt != 1000 ||
		window.LastObservedAt != 3000 || window.FirstUsedPercent != 0 || window.LastUsedPercent != 100 ||
		window.MaxUsedPercent != 100 || window.ExhaustedAt != 3000 || window.ExhaustionReason != "explicit" {
		t.Fatalf("merged window=%+v", window)
	}
}

func TestSchemaEightDropsLegacyFiveHourSnapshots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`CREATE TABLE subscription_usage_snapshots(id INTEGER PRIMARY KEY, marker TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`INSERT INTO subscription_usage_snapshots(marker) VALUES('legacy')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`PRAGMA user_version=7`); err != nil {
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
	if err := database.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='subscription_usage_snapshots'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("legacy five-hour snapshot table survived the v8 migration")
	}
}
