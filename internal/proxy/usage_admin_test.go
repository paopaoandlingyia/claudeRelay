package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/local/claude-relay/internal/store"
)

func TestRelayUsageFlowsIntoDashboardWithoutChangingSSE(t *testing.T) {
	raw := "event: message_start\n" +
		`data: {"type":"message_start","message":{"model":"claude-sonnet-5","usage":{"input_tokens":100,"cache_read_input_tokens":50}}}` + "\n\n" +
		"event: message_delta\n" + `data: {"type":"message_delta","usage":{"output_tokens":20}}` + "\n\n" +
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
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
	if dashboard.Totals.CostUSD <= 0 || len(dashboard.ByModel) != 1 {
		t.Fatalf("dashboard=%+v", dashboard)
	}
}

func TestMatchingPricePrefersExactAndLatestVersion(t *testing.T) {
	prices := []store.ModelPrice{
		{ModelPattern: "claude-*", EffectiveFrom: 1, InputUSDPerMTok: 1},
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
}

func TestFiveHourEstimateRequiresSameResetWindow(t *testing.T) {
	prices := []store.ModelPrice{{ModelPattern: "m", EffectiveFrom: 1, InputUSDPerMTok: 2}}
	snapshots := []store.SubscriptionUsageSnapshot{
		{AccountID: 1, Account: "a", ObservedAt: time.Unix(20, 0).UnixMilli(), ResetsAt: "same", UsedPercent: 30, Totals: map[string]store.UsageCounters{"m": {InputTokens: 2_000_000}}},
		{AccountID: 1, Account: "a", ObservedAt: time.Unix(10, 0).UnixMilli(), ResetsAt: "same", UsedPercent: 10, Totals: map[string]store.UsageCounters{"m": {InputTokens: 1_000_000}}},
	}
	estimates := buildFiveHourEstimates(snapshots, prices)
	if len(estimates) != 1 || estimates[0].ObservedCostUSD != 2 || estimates[0].FullWindowUSD != 10 {
		t.Fatalf("estimates=%+v", estimates)
	}
}

// Every step here moves utilization by one percent, the finest movement the
// upstream reports. Pairing neighbours would extrapolate each step on its own;
// anchoring measures the whole span. Newest first, matching the store ordering.
func TestFiveHourEstimateAnchorsEachWindowToItsEarliestReading(t *testing.T) {
	prices := []store.ModelPrice{{ModelPattern: "m", EffectiveFrom: 1, InputUSDPerMTok: 1}}
	reading := func(at int64, window string, usedPercent float64, tokens int64) store.SubscriptionUsageSnapshot {
		return store.SubscriptionUsageSnapshot{AccountID: 1, Account: "a", ObservedAt: time.Unix(at, 0).UnixMilli(),
			ResetsAt: window, UsedPercent: usedPercent, Totals: map[string]store.UsageCounters{"m": {InputTokens: tokens}}}
	}
	estimates := buildFiveHourEstimates([]store.SubscriptionUsageSnapshot{
		reading(60, "b", 6, 5_000_000),
		reading(50, "b", 4, 4_000_000),
		reading(40, "a", 12, 3_000_000),
		reading(30, "a", 11, 2_000_000),
		reading(20, "a", 10, 1_000_000),
		reading(10, "", 9, 500_000),
	}, prices)
	if len(estimates) != 2 {
		t.Fatalf("want one row per identified window, got %d: %+v", len(estimates), estimates)
	}
	// Window a spans three readings: 10 to 12 percent over $1 to $3 of relay usage.
	if got := estimates[0]; got.UsedPercentDelta != 2 || got.ObservedCostUSD != 2 || got.FullWindowUSD != 100 {
		t.Fatalf("window a = %+v", got)
	}
	// The reading opening window b must not pair with the last of window a, whose
	// utilization was higher.
	if got := estimates[1]; got.UsedPercentDelta != 2 || got.ObservedCostUSD != 1 || got.FullWindowUSD != 50 {
		t.Fatalf("window b = %+v", got)
	}
}

func TestFiveHourEstimateIgnoresWindowsThatCannotShowMovement(t *testing.T) {
	prices := []store.ModelPrice{{ModelPattern: "m", EffectiveFrom: 1, InputUSDPerMTok: 1}}
	totals := map[string]store.UsageCounters{"m": {InputTokens: 1_000_000}}
	snapshots := []store.SubscriptionUsageSnapshot{
		// One reading has nothing to be compared against, and utilization that
		// never moved leaves no denominator.
		{AccountID: 1, Account: "a", ObservedAt: time.Unix(30, 0).UnixMilli(), ResetsAt: "lonely", UsedPercent: 9, Totals: totals},
		{AccountID: 1, Account: "a", ObservedAt: time.Unix(20, 0).UnixMilli(), ResetsAt: "flat", UsedPercent: 5, Totals: totals},
		{AccountID: 1, Account: "a", ObservedAt: time.Unix(10, 0).UnixMilli(), ResetsAt: "flat", UsedPercent: 5, Totals: totals},
	}
	if estimates := buildFiveHourEstimates(snapshots, prices); len(estimates) != 0 {
		t.Fatalf("estimates=%+v", estimates)
	}
}
