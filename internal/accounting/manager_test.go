package accounting

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/local/claude-relay/internal/credential"
	"github.com/local/claude-relay/internal/store"
)

func TestManagerRetriesFailedObservationsWithoutLosingIngressUsage(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	account, err := database.ImportAccount(t.Context(), "retry", credential.Credential{
		Type: "claude", AccessToken: "test-token", AccountUUID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
		DeviceID: strings.Repeat("c", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, time.October, 7, 12, 0, 0, 0, time.UTC)
	reset := strconv.FormatInt(at.Add(4*time.Hour).Unix(), 10)
	manager := NewManager(database)
	manager.Record(account.ID, "claude-opus-5-5", "compatible", at, Usage{
		Seen: true, Complete: true, InputTokens: 10, OutputTokens: 20,
		CacheCreation5mTokens: 30, CacheCreation1hTokens: 40, CacheReadTokens: 50,
	}, FiveHourContext{
		EventKey: "retry-1", ResetsAt: reset, ObservedAt: at, CompletedAt: at.Add(time.Second),
		Status: 200, UsedPercent: 20,
	})
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := manager.Flush(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("failed flush error=%v, want context cancellation", err)
	}
	// New traffic joins the restored batch before the next flush.
	manager.Record(account.ID, "claude-opus-5-5", "compatible", at.Add(time.Second), Usage{
		Seen: true, Complete: true, InputTokens: 1, OutputTokens: 2,
		CacheCreation5mTokens: 3, CacheCreation1hTokens: 4, CacheReadTokens: 5,
	}, FiveHourContext{
		EventKey: "retry-2", ResetsAt: reset, ObservedAt: at.Add(time.Second), CompletedAt: at.Add(2 * time.Second),
		Status: 200, UsedPercent: 21,
	})
	for range 2 {
		if err := manager.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	want := store.UsageCounters{
		InputTokens: 11, OutputTokens: 22, CacheCreation5mTokens: 33,
		CacheCreation1hTokens: 44, CacheReadTokens: 55, Requests: 2,
	}
	buckets, err := database.UsageBuckets(t.Context(), 0)
	if err != nil || len(buckets) != 1 || buckets[0].Counters != want {
		t.Fatalf("account usage=%+v err=%v, want %+v", buckets, err, want)
	}
	ingress, err := database.UsageIngressBuckets(t.Context(), 0)
	if err != nil || len(ingress) != 1 || ingress[0].Ingress != "compatible" || ingress[0].Counters != want {
		t.Fatalf("ingress usage=%+v err=%v, want %+v", ingress, err, want)
	}
	events, err := database.AllFiveHourEvents(t.Context())
	if err != nil || len(events) != 2 {
		t.Fatalf("events=%+v err=%v, want two requests without duplicates", events, err)
	}
}

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
	manager.Record(account.ID, "claude-test", "compatible", now, Usage{}, FiveHourContext{
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
	manager.Record(account.ID, "claude-test", "compatible", now, Usage{Seen: true, Complete: true, InputTokens: 10}, FiveHourContext{
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
	ingressBuckets, err := database.UsageIngressBuckets(context.Background(), 0)
	if err != nil || len(ingressBuckets) != 1 || ingressBuckets[0].Ingress != "compatible" || ingressBuckets[0].Counters.InputTokens != 10 {
		t.Fatalf("ingress hourly buckets=%+v err=%v", ingressBuckets, err)
	}
}

func TestManagerPersistsRefusalWithoutUsage(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	account, err := database.ImportAccount(context.Background(), "refusal", credential.Credential{
		Type: "claude", AccessToken: "secret", AccountUUID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		DeviceID: strings.Repeat("b", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	manager := NewManager(database)
	manager.RecordRefusal(account.ID, now, Refusal{Seen: true, Category: "cyber"})
	if err := manager.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	summaries, err := database.RecentAccountRefusals(context.Background(), now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got := summaries[account.ID]; got.Count24h != 1 || got.LastCategory != "cyber" {
		t.Fatalf("summary=%+v", got)
	}
}
