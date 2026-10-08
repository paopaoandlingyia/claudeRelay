package store

import (
	"context"
	"fmt"
	"math"
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

func TestPromptLengthUsesAllInputCategoriesButNotOutput(t *testing.T) {
	for _, tc := range []struct {
		usage UsageCounters
		long  bool
	}{
		{UsageCounters{InputTokens: 1, OutputTokens: math.MaxInt64}, false},
		{UsageCounters{InputTokens: 1, CacheCreation5mTokens: 20_000, CacheCreation1hTokens: 30_000, CacheReadTokens: 49_998}, false},
		{UsageCounters{InputTokens: 1, CacheCreation5mTokens: 20_000, CacheCreation1hTokens: 30_000, CacheReadTokens: 49_999}, false},
		{UsageCounters{InputTokens: 1, CacheCreation5mTokens: 20_000, CacheCreation1hTokens: 30_000, CacheReadTokens: 50_000}, true},
		{UsageCounters{InputTokens: math.MaxInt64, CacheReadTokens: math.MaxInt64}, true},
	} {
		if got := tc.usage.HasLongContext(); got != tc.long {
			t.Fatalf("long context for %+v=%v want %v", tc.usage, got, tc.long)
		}
	}
}

func TestFiveFiveUpgradeBackfillsOnlyReconciledHoursAndPreservesPrices(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(fmt.Sprintf("operator_price_%t", custom), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "relay.db")
			database, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			account := importTestAccount(t, database, "context", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
			low := UsageCounters{InputTokens: 1, OutputTokens: 1000, CacheCreation5mTokens: 20000, CacheCreation1hTokens: 30000, CacheReadTokens: 49999, Requests: 1}
			high := low
			high.CacheReadTokens++
			total := low
			total.Add(high)
			for _, hour := range []int64{3600, 7200, 10800} {
				if err := database.AddUsageBuckets(t.Context(), []UsageBucket{{BucketStart: hour, AccountID: account.ID, Model: "claude-haiku-5-5", Counters: total}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := database.AddFiveHourEvents(t.Context(), []FiveHourEvent{
				{EventKey: "low", AccountID: account.ID, Kind: FiveHourEventMessages, ObservedAt: 3601000, CompletedAt: 3602000, Model: "claude-haiku-5-5", UsedPercent: -1, Usage: low, UsageSeen: true, Complete: true},
				{EventKey: "high", AccountID: account.ID, Kind: FiveHourEventMessages, ObservedAt: 3601001, CompletedAt: 3602001, Model: "claude-haiku-5-5", UsedPercent: -1, Usage: high, UsageSeen: true, Complete: true},
				// The third hour lacks a request and must remain unclassified.
				{EventKey: "partial", AccountID: account.ID, Kind: FiveHourEventMessages, ObservedAt: 10801000, CompletedAt: 10802000, Model: "claude-haiku-5-5", UsedPercent: -1, Usage: low, UsageSeen: true, Complete: true},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := database.db.Exec(`DELETE FROM model_prices WHERE model_pattern IN ('claude-sonnet-5-5*','claude-haiku-5-5*')`); err != nil {
				t.Fatal(err)
			}
			if custom {
				for _, pattern := range []string{"claude-sonnet-5-5*", "claude-haiku-5-5*"} {
					if _, err := database.SaveModelPrice(t.Context(), ModelPrice{ModelPattern: pattern, EffectiveFrom: 1, CacheReadUSDPerMTok: .08, Source: "operator override"}); err != nil {
						t.Fatal(err)
					}
				}
			}
			// Reproduce the released v13 layout, not just an older version number.
			for _, column := range []string{"long_input_tokens", "long_output_tokens", "long_cache_creation_5m_tokens", "long_cache_creation_1h_tokens", "long_cache_read_tokens", "context_known_requests"} {
				if _, err := database.db.Exec(`ALTER TABLE usage_hourly DROP COLUMN ` + column); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := database.db.Exec(`PRAGMA user_version=13`); err != nil {
				t.Fatal(err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				database, err = Open(path)
				if err != nil {
					t.Fatal(err)
				}
				buckets, err := database.UsageBuckets(t.Context(), 0)
				if err != nil || len(buckets) != 3 {
					t.Fatalf("buckets=%+v err=%v", buckets, err)
				}
				for i, b := range buckets {
					if b.Counters != total {
						t.Fatalf("raw hourly counters changed: %+v", b)
					}
					if i == 0 {
						if b.ContextKnownRequests != 2 || b.LongContextCounters.InputTokens != high.InputTokens ||
							b.LongContextCounters.OutputTokens != high.OutputTokens || b.LongContextCounters.CacheReadTokens != high.CacheReadTokens ||
							b.LongContextCounters.CacheCreation5mTokens != high.CacheCreation5mTokens || b.LongContextCounters.CacheCreation1hTokens != high.CacheCreation1hTokens {
							t.Fatalf("reconciled prompt sizes not restored: %+v", b)
						}
					} else if b.ContextKnownRequests != 0 || b.LongContextCounters != (UsageCounters{}) {
						t.Fatalf("invented prompt sizes: %+v", b)
					}
				}
				prices, err := database.ModelPrices(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				seen := 0
				for _, p := range prices {
					if p.ModelPattern != "claude-sonnet-5-5*" && p.ModelPattern != "claude-haiku-5-5*" {
						continue
					}
					seen++
					want := .1
					if p.ModelPattern == "claude-haiku-5-5*" {
						want = .01
					}
					if custom {
						want = .08
					}
					if p.CacheReadUSDPerMTok != want || custom && p.Source != "operator override" {
						t.Fatalf("upgraded price=%+v want read %g", p, want)
					}
				}
				if seen != 2 {
					t.Fatalf("default prices missing or duplicated: %d", seen)
				}
				if err := database.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
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

func TestOpusFiveFiveHasItsPublishedPrice(t *testing.T) {
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
		if price.ModelPattern != "claude-opus-5-5*" || price.EffectiveFrom != 1 {
			continue
		}
		if price.InputUSDPerMTok != 4 || price.OutputUSDPerMTok != 20 ||
			price.CacheCreation5mUSDPerMTok != 5 || price.CacheCreation1hUSDPerMTok != 8 ||
			price.CacheReadUSDPerMTok != .2 {
			t.Fatalf("Opus 5.5 price = %+v", price)
		}
		return
	}
	t.Fatal("Opus 5.5 default price is missing")
}
