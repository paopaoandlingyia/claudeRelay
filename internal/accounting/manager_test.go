package accounting

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/local/claude-relay/internal/credential"
	"github.com/local/claude-relay/internal/store"
)

func TestManagerPersistsFiveHourEventEvenWhenUsageIsMissing(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	account, err := database.ImportAccount(context.Background(), "usage", credential.Credential{
		Type: "claude", AccessToken: "secret", AccountUUID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		DeviceID: strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	manager := NewManager(database)
	manager.Record(account.ID, "claude-test", now, Usage{}, FiveHourContext{
		EventKey: "request-1", ResetsAt: strconv.FormatInt(time.Now().Add(4*time.Hour).Unix(), 10),
		ObservedAt: now, CompletedAt: now.Add(time.Second), Status: 200, UsedPercent: 20,
	})
	if err := manager.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, err := database.AllFiveHourEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].UsageSeen || events[0].Complete || events[0].Model != "claude-test" {
		t.Fatalf("events=%+v", events)
	}
	buckets, err := database.UsageBuckets(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 0 {
		t.Fatalf("missing usage unexpectedly entered hourly accounting: %+v", buckets)
	}
}

func TestClearFiveHourObservationsDropsPendingEventsOnly(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	account, err := database.ImportAccount(context.Background(), "usage", credential.Credential{
		Type: "claude", AccessToken: "secret", AccountUUID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		DeviceID: strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	manager := NewManager(database)
	manager.Record(account.ID, "claude-test", now, Usage{Seen: true, Complete: true, InputTokens: 10}, FiveHourContext{
		EventKey: "request-1", ObservedAt: now, CompletedAt: now, UsedPercent: -1,
	})
	if err := manager.ClearFiveHourObservations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, err := database.AllFiveHourEvents(context.Background())
	if err != nil || len(events) != 0 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	buckets, err := database.UsageBuckets(context.Background(), 0)
	if err != nil || len(buckets) != 1 || buckets[0].Counters.InputTokens != 10 {
		t.Fatalf("hourly buckets=%+v err=%v", buckets, err)
	}
}
