package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestRefusalBucketsAggregateAndSummarizeRecentActivity(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	account := importTestAccount(t, database, "refusal", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	now := time.Now().UTC().Truncate(time.Minute)
	recentBucket := now.Truncate(time.Hour).Unix()
	old := now.Add(-25 * time.Hour)
	buckets := []RefusalBucket{
		{BucketStart: recentBucket, AccountID: account.ID, Category: "cyber", Count: 1, LastObservedAt: now.Add(-time.Minute).UnixMilli()},
		{BucketStart: recentBucket, AccountID: account.ID, Category: "cyber", Count: 2, LastObservedAt: now.UnixMilli()},
		{BucketStart: recentBucket, AccountID: account.ID, Category: "unknown", Count: 1, LastObservedAt: now.Add(-2 * time.Minute).UnixMilli()},
		{BucketStart: old.Truncate(time.Hour).Unix(), AccountID: account.ID, Category: "cyber", Count: 10, LastObservedAt: old.UnixMilli()},
	}
	if err := database.AddRefusalBuckets(ctx, buckets); err != nil {
		t.Fatal(err)
	}
	summaries, err := database.RecentAccountRefusals(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got := summaries[account.ID]
	if got.Count24h != 4 || got.LastCategory != "cyber" || got.LastAt != now.UnixMilli() {
		t.Fatalf("summary=%+v", got)
	}
	var storedRows int
	if err := database.db.QueryRow(`SELECT COUNT(*) FROM refusal_hourly`).Scan(&storedRows); err != nil {
		t.Fatal(err)
	}
	if storedRows != 2 {
		t.Fatalf("stored refusal rows=%d, want 2 recent category rows", storedRows)
	}
}

func TestSchemaV10MigratesRefusalObservationTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`DROP TABLE refusal_hourly; PRAGMA user_version=10`); err != nil {
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
	var version, tableCount int
	if err := database.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='refusal_hourly'`).Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if version != 11 || tableCount != 1 {
		t.Fatalf("version=%d refusal_hourly tables=%d", version, tableCount)
	}
}
