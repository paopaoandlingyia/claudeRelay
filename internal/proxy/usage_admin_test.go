package proxy

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/local/claude-relay/internal/store"
)

func TestRelayUsageFlowsIntoDashboardAndFiveHourWindowWithoutChangingSSE(t *testing.T) {
	raw := "event: message_start\n" +
		`data: {"type":"message_start","message":{"model":"claude-sonnet-5","usage":{"input_tokens":100,"cache_read_input_tokens":50}}}` + "\n\n" +
		"event: message_delta\n" + `data: {"type":"message_delta","usage":{"output_tokens":20}}` + "\n\n" +
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"
	reset := time.Now().Add(4 * time.Hour).Truncate(time.Second)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set(fiveHourResetHeader, strconv.FormatInt(reset.Unix(), 10))
		w.Header().Set(fiveHourUtilizationHeader, "0.31")
		_, _ = io.WriteString(w, raw)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	request.Header.Set("x-api-key", "downstream-key")
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != raw {
		t.Fatalf("relay status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if err := server.accounting.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	buckets, err := server.store.UsageBuckets(context.Background(), 0)
	if err != nil || len(buckets) != 1 {
		t.Fatalf("buckets=%#v err=%v", buckets, err)
	}
	usage := buckets[0].Counters
	if usage.InputTokens != 100 || usage.OutputTokens != 20 || usage.CacheReadTokens != 50 || usage.Requests != 1 || usage.Incomplete != 0 {
		t.Fatalf("usage=%+v", usage)
	}

	admin := adminRequest(t, server, http.MethodGet, "/admin/v1/usage?from=0", "")
	if admin.Code != http.StatusOK {
		t.Fatalf("dashboard status=%d body=%s", admin.Code, admin.Body.String())
	}
	var dashboard usageDashboardResponse
	if err := json.Unmarshal(admin.Body.Bytes(), &dashboard); err != nil {
		t.Fatal(err)
	}
	if dashboard.Totals.CostUSD <= 0 || len(dashboard.ByModel) != 1 || len(dashboard.ByIngress) != 1 ||
		dashboard.ByIngress[0].Ingress != ingressCompatible || dashboard.ByIngress[0].Usage.CacheReadTokens != 50 {
		t.Fatalf("dashboard=%+v", dashboard)
	}
	if len(dashboard.FiveHourCurrent) != 1 || dashboard.FiveHourCurrent[0].EventCount != 1 ||
		dashboard.FiveHourCurrent[0].MaxUsedPercent != 31 || dashboard.FiveHourCurrent[0].ObservedCostUSD <= 0 {
		t.Fatalf("current five-hour window=%+v", dashboard.FiveHourCurrent)
	}
	if dashboard.Totals.APIValueByTypeUSD == nil || dashboard.ByModel[0].APIValueByTypeUSD == nil ||
		dashboard.FiveHourCurrent[0].APIValueByTypeUSD.CacheRead <= 0 ||
		!dashboard.FiveHourCurrent[0].DataQuality.PartialStart {
		t.Fatalf("missing API value breakdown or partial-start flag: %+v", dashboard)
	}
}

func TestMatchingPricePrefersExactAndLatestVersion(t *testing.T) {
	prices := []store.ModelPrice{
		{ModelPattern: "claude-*", EffectiveFrom: 1, InputUSDPerMTok: 1},
		{ModelPattern: "claude-opus-5*", EffectiveFrom: 1, InputUSDPerMTok: 5},
		{ModelPattern: "claude-opus-5-5*", EffectiveFrom: 1, InputUSDPerMTok: 4},
		{ModelPattern: "claude-sonnet-5*", EffectiveFrom: 1, InputUSDPerMTok: 2},
		{ModelPattern: "claude-sonnet-5", EffectiveFrom: 1, InputUSDPerMTok: 3},
		{ModelPattern: "claude-sonnet-5", EffectiveFrom: 20, InputUSDPerMTok: 4},
	}
	price, ok := matchingPrice(prices, "claude-sonnet-5", 10)
	if !ok || price.InputUSDPerMTok != 3 {
		t.Fatalf("price at 10=%+v ok=%v", price, ok)
	}
	price, ok = matchingPrice(prices, "claude-sonnet-5", 30)
	if !ok || price.InputUSDPerMTok != 4 {
		t.Fatalf("price at 30=%+v ok=%v", price, ok)
	}
	price, ok = matchingPrice(prices, "claude-opus-5-5-20260923", 30)
	if !ok || price.InputUSDPerMTok != 4 {
		t.Fatalf("Opus 5.5 price=%+v ok=%v", price, ok)
	}
}

func TestValueFiveHourWindowsUsesObservedModelTotalsWithoutExtrapolation(t *testing.T) {
	windows := []store.FiveHourWindow{{
		Account: "a", ResetsAt: "1786386600", FirstObservedAt: 10_000, LastObservedAt: 20_000,
		FirstUsedPercent: 10, LastUsedPercent: 100, MaxUsedPercent: 100, ExhaustedAt: 20_000,
		EventCount: 3, MissingUsageCount: 1, IncompleteCount: 1,
		ByModel: map[string]store.UsageCounters{
			"sonnet":  {InputTokens: 1_000_000, Requests: 2},
			"unknown": {OutputTokens: 100, Requests: 1},
		},
		PriceBuckets: []store.FiveHourPriceBucket{
			{ObservedAtSeconds: 10, Model: "sonnet", Counters: store.UsageCounters{InputTokens: 1_000_000, Requests: 2}},
			{ObservedAtSeconds: 20, Model: "unknown", Counters: store.UsageCounters{OutputTokens: 100, Requests: 1}},
		},
	}}
	prices := []store.ModelPrice{{ModelPattern: "sonnet", EffectiveFrom: 1, InputUSDPerMTok: 2}}
	values := valueFiveHourWindows(windows, prices)
	if len(values) != 1 {
		t.Fatalf("values=%+v", values)
	}
	value := values[0]
	if value.ObservedCostUSD != 2 || !value.Unpriced || value.EventCount != 3 ||
		value.MissingUsageCount != 1 || len(value.ByModel) != 2 {
		t.Fatalf("window value=%+v", value)
	}
	if value.APIValueByTypeUSD.Input != 2 || value.ObservedUsedPercentDelta == nil ||
		*value.ObservedUsedPercentDelta != 90 || value.APIValueUSDPerUsedPercent != nil ||
		!value.DataQuality.MissingUsage || !value.DataQuality.IncompleteUsage ||
		!value.DataQuality.Unpriced || !value.DataQuality.PartialStart || !value.DataQuality.MixedModels {
		t.Fatalf("partial data produced an efficiency estimate or lost its quality flags: %+v", value)
	}
}

func TestFiveHourValuePreservesRequestTimePricesAndQuality(t *testing.T) {
	server := newTestServer(t, "http://unused.invalid", 4096)
	account, found, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil || !found {
		t.Fatalf("account found=%v err=%v", found, err)
	}
	usage := store.UsageCounters{
		InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheCreation5mTokens: 1_000_000,
		CacheCreation1hTokens: 1_000_000, CacheReadTokens: 1_000_000, Requests: 1,
	}
	if err := server.store.AddFiveHourEvents(t.Context(), []store.FiveHourEvent{
		{EventKey: "before-price-change", AccountID: account.ID, ResetsAt: "2000000000",
			Kind: store.FiveHourEventMessages, ObservedAt: 19_999, CompletedAt: 20_100,
			Model: "claude-opus-5-5", Status: 200, UsedPercent: 0, Usage: usage, UsageSeen: true, Complete: true},
		{EventKey: "at-price-change", AccountID: account.ID, ResetsAt: "2000000000",
			Kind: store.FiveHourEventMessages, ObservedAt: 20_000, CompletedAt: 20_200,
			Model: "claude-opus-5-5", Status: 200, UsedPercent: 25, Usage: usage, UsageSeen: true, Complete: true},
	}); err != nil {
		t.Fatal(err)
	}
	windows, err := server.store.FiveHourWindows(t.Context(), false, 0, 10)
	if err != nil || len(windows) != 1 {
		t.Fatalf("windows=%+v err=%v", windows, err)
	}
	prices := []store.ModelPrice{
		{ModelPattern: "claude-opus-5*", EffectiveFrom: 1, InputUSDPerMTok: 99},
		{ModelPattern: "claude-opus-5-5*", EffectiveFrom: 1, InputUSDPerMTok: 4, OutputUSDPerMTok: 20,
			CacheCreation5mUSDPerMTok: 5, CacheCreation1hUSDPerMTok: 8, CacheReadUSDPerMTok: .4},
		{ModelPattern: "claude-opus-5-5*", EffectiveFrom: 20, InputUSDPerMTok: 4, OutputUSDPerMTok: 20,
			CacheCreation5mUSDPerMTok: 5, CacheCreation1hUSDPerMTok: 8, CacheReadUSDPerMTok: .2},
	}
	value := valueFiveHourWindows(windows, prices)[0]
	wantByType := usageAPIValue{Input: 8, Output: 40, CacheCreation5m: 10, CacheCreation1h: 16, CacheRead: .6}
	if math.Abs(value.ObservedCostUSD-74.6) > 1e-10 ||
		value.APIValueByTypeUSD.Input != wantByType.Input || value.APIValueByTypeUSD.Output != wantByType.Output ||
		value.APIValueByTypeUSD.CacheCreation5m != wantByType.CacheCreation5m ||
		value.APIValueByTypeUSD.CacheCreation1h != wantByType.CacheCreation1h ||
		math.Abs(value.APIValueByTypeUSD.CacheRead-wantByType.CacheRead) > 1e-10 {
		t.Fatalf("historical API value=%+v, want 74.6 USD and %+v", value, wantByType)
	}
	if value.ObservedUsedPercentDelta == nil || *value.ObservedUsedPercentDelta != 25 ||
		value.APIValueUSDPerUsedPercent == nil || math.Abs(*value.APIValueUSDPerUsedPercent-74.6/25) > 1e-10 ||
		value.DataQuality != (fiveHourDataQuality{}) || len(value.ByModel) != 1 ||
		value.ByModel[0].Usage.InputTokens != 2_000_000 || value.ByModel[0].APIValueByTypeUSD == nil ||
		math.Abs(value.ByModel[0].APIValueByTypeUSD.CacheRead-.6) > 1e-10 {
		t.Fatalf("non-exhausted window was extrapolated or model totals changed: %+v", value)
	}
	for _, tc := range []struct {
		name                string
		missing, incomplete int64
		first, maximum      float64
		unpriced            bool
		quality             fiveHourDataQuality
		wantDelta           *float64
	}{
		{name: "missing usage", missing: 1, maximum: 25, quality: fiveHourDataQuality{MissingUsage: true}, wantDelta: new(float64(25))},
		{name: "incomplete usage", incomplete: 1, maximum: 25, quality: fiveHourDataQuality{IncompleteUsage: true}, wantDelta: new(float64(25))},
		{name: "missing price", unpriced: true, maximum: 25, quality: fiveHourDataQuality{Unpriced: true}, wantDelta: new(float64(25))},
		{name: "partial start", first: 10, maximum: 25, quality: fiveHourDataQuality{PartialStart: true}, wantDelta: new(float64(15))},
		{name: "unknown quota", first: -1, maximum: -1, quality: fiveHourDataQuality{MissingQuota: true}},
		{name: "zero delta", wantDelta: new(float64(0))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			window := windows[0]
			window.MissingUsageCount, window.IncompleteCount = tc.missing, tc.incomplete
			window.FirstUsedPercent, window.MaxUsedPercent = tc.first, tc.maximum
			matchingPrices := prices
			if tc.unpriced {
				matchingPrices = nil
			}
			got := valueFiveHourWindows([]store.FiveHourWindow{window}, matchingPrices)[0]
			if got.APIValueUSDPerUsedPercent != nil || got.DataQuality != tc.quality {
				t.Fatalf("misleading ratio or missing quality flag: %+v", got)
			}
			if tc.wantDelta == nil {
				if got.ObservedUsedPercentDelta != nil {
					t.Fatalf("invented utilization for an unknown quota: %+v", got)
				}
			} else if got.ObservedUsedPercentDelta == nil || *got.ObservedUsedPercentDelta != *tc.wantDelta {
				t.Fatalf("delta=%v, want %v", got.ObservedUsedPercentDelta, *tc.wantDelta)
			}
		})
	}
}
