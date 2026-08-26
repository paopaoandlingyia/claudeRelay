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
