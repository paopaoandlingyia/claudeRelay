package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestUsageBucketsAggregateAndPricesAreVersioned(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	account := importTestAccount(t, database, "usage", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	bucket := UsageBucket{BucketStart: 3600, AccountID: account.ID, Model: "claude-sonnet-5", Counters: UsageCounters{InputTokens: 10, Requests: 1}}
	if err := database.AddUsageBuckets(ctx, []UsageBucket{bucket, bucket}); err != nil {
		t.Fatal(err)
	}
	buckets, err := database.UsageBuckets(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 || buckets[0].Counters.InputTokens != 20 || buckets[0].Counters.Requests != 2 {
		t.Fatalf("buckets=%#v", buckets)
	}
	prices, err := database.ModelPrices(ctx)
	if err != nil || len(prices) == 0 {
		t.Fatalf("default prices=%d err=%v", len(prices), err)
	}
	saved, err := database.SaveModelPrice(ctx, ModelPrice{ModelPattern: "claude-new*", EffectiveFrom: 10, InputUSDPerMTok: 1, OutputUSDPerMTok: 2})
	if err != nil || saved.ID == 0 {
		t.Fatalf("saved=%#v err=%v", saved, err)
	}
}

// An anchored estimate measures from a window's first reading, so that reading
// has to survive however many later ones sampling adds. Returning the newest N
// rows would drop it and quietly shrink the span being measured.
func TestSubscriptionUsageSnapshotsKeepEachWindowsFirstReading(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	account := importTestAccount(t, database, "usage", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	base := time.Now().Add(-time.Hour).UnixMilli()
	for step := range 300 {
		if err := database.CaptureSubscriptionUsageSnapshot(ctx, account.ID, base+int64(step)*1000, "1786386600", float64(step)); err != nil {
			t.Fatal(err)
		}
	}
	snapshots, err := database.SubscriptionUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 2 {
		t.Fatalf("want the first and last reading of the window, got %d", len(snapshots))
	}
	// Newest first, matching the ordering the estimator walks backwards.
	if snapshots[0].UsedPercent != 299 || snapshots[1].UsedPercent != 0 {
		t.Fatalf("want the span 0..299, got %v..%v", snapshots[1].UsedPercent, snapshots[0].UsedPercent)
	}
}

// Sampling follows relayed traffic, so this table gains rows for as long as the
// relay runs and needs a horizon of its own.
func TestSubscriptionUsageSnapshotsArePrunedPastTheRetentionHorizon(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	account := importTestAccount(t, database, "usage", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	now := time.Now()
	stale := now.Add(-subscriptionSnapshotRetention - time.Hour).UnixMilli()
	for _, observedAt := range []int64{stale, now.UnixMilli()} {
		if err := database.CaptureSubscriptionUsageSnapshot(ctx, account.ID, observedAt, "1786386600", 10); err != nil {
			t.Fatal(err)
		}
		// The first capture claimed the sweep, so let the next one run.
		database.lastSnapshotPrune.Store(0)
	}
	snapshots, err := database.SubscriptionUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || snapshots[0].ObservedAt == stale {
		t.Fatalf("want only the reading inside the horizon, got %+v", snapshots)
	}
}
