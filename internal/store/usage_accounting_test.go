package store

import (
	"context"
	"path/filepath"
	"testing"
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

func TestSonnetFivePermanentPriceHasNoFutureIncrease(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	prices, err := database.ModelPrices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, price := range prices {
		if price.ModelPattern == "claude-sonnet-5*" && price.EffectiveFrom > 1 {
			t.Fatalf("unexpected future Sonnet 5 price: %+v", price)
		}
	}
}
