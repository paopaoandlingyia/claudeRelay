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

func TestFiveHourEstimateBreaksObservedValueDownByModel(t *testing.T) {
	prices := []store.ModelPrice{
		{ModelPattern: "sonnet", EffectiveFrom: 1, InputUSDPerMTok: 2},
		{ModelPattern: "opus", EffectiveFrom: 1, OutputUSDPerMTok: 10},
	}
	snapshots := []store.SubscriptionUsageSnapshot{
		{AccountID: 1, Account: "a", ObservedAt: time.Unix(20, 0).UnixMilli(), ResetsAt: "same", UsedPercent: 30,
			Totals: map[string]store.UsageCounters{
				"sonnet":  {InputTokens: 2_000_000, Requests: 4},
				"opus":    {OutputTokens: 500_000, Requests: 2},
				"unknown": {InputTokens: 100, Requests: 1},
			}},
		{AccountID: 1, Account: "a", ObservedAt: time.Unix(10, 0).UnixMilli(), ResetsAt: "same", UsedPercent: 10,
			Totals: map[string]store.UsageCounters{
				"sonnet": {InputTokens: 1_000_000, Requests: 1},
			}},
	}
	estimates := buildFiveHourEstimates(snapshots, prices)
	if len(estimates) != 1 {
		t.Fatalf("estimates=%+v", estimates)
	}
	got := estimates[0]
	if got.ObservedCostUSD != 7 || got.FullWindowUSD != 35 || !got.Unpriced || len(got.ByModel) != 3 {
		t.Fatalf("estimate=%+v", got)
	}
	if got.ByModel[0].Model != "opus" || got.ByModel[0].CostUSD != 5 || got.ByModel[0].Usage.Requests != 2 {
		t.Fatalf("first model=%+v", got.ByModel[0])
	}
	if got.ByModel[1].Model != "sonnet" || got.ByModel[1].CostUSD != 2 || got.ByModel[1].Usage.Requests != 3 {
		t.Fatalf("second model=%+v", got.ByModel[1])
	}
	if got.ByModel[2].Model != "unknown" || !got.ByModel[2].Unpriced {
		t.Fatalf("unpriced model=%+v", got.ByModel[2])
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

func TestLatestFiveHourEstimatePerAccountDropsHistoricalRows(t *testing.T) {
	estimates := []fiveHourEstimate{
		{Account: "a", To: 10, FullWindowUSD: 10},
		{Account: "b", To: 40, FullWindowUSD: 40},
		{Account: "a", To: 30, FullWindowUSD: 30},
		// Selection must use the observation time, not whichever row happened
		// to appear last in the input.
		{Account: "b", To: 20, FullWindowUSD: 20},
	}

	got := latestFiveHourEstimatePerAccount(estimates)
	if len(got) != 2 {
		t.Fatalf("want one row per account, got %d: %+v", len(got), got)
	}
	if got[0].Account != "a" || got[0].To != 30 || got[0].FullWindowUSD != 30 {
		t.Fatalf("first latest estimate = %+v", got[0])
	}
	if got[1].Account != "b" || got[1].To != 40 || got[1].FullWindowUSD != 40 {
		t.Fatalf("second latest estimate = %+v", got[1])
	}
}

// Captured from the live dashboard on 2026-08-19. The page rendered 48 rows
// because it included every measurable historical reset window for each of the
// six accounts.
func TestLatestFiveHourEstimatePerAccountOnLiveDashboardSample(t *testing.T) {
	observed := func(day, hour, minute, second int) int64 {
		return time.Date(2026, time.August, day, hour, minute, second, 0, time.UTC).UnixMilli()
	}
	row := func(account string, day, hour, minute, second int, fullWindowUSD float64) fiveHourEstimate {
		return fiveHourEstimate{Account: account, To: observed(day, hour, minute, second), FullWindowUSD: fullWindowUSD}
	}
	estimates := []fiveHourEstimate{
		row("QIuLin", 18, 23, 58, 30, 0.0952),
		row("gaen.v", 18, 23, 58, 33, 46.99),
		row("devin.k", 18, 23, 22, 39, 14.45),
		row("X.F", 18, 23, 58, 33, 57.01),
		row("ambe", 18, 23, 58, 27, 55.74),
		row("QIuLin", 18, 20, 7, 55, 59.63),
		row("entha", 18, 21, 38, 24, 86.08),
		row("devin.k", 18, 19, 48, 2, 77.80),
		row("gaen.v", 18, 18, 40, 36, 78.46),
		row("X.F", 18, 16, 39, 18, 74.86),
		row("ambe", 18, 15, 37, 39, 53.55),
		row("QIuLin", 18, 15, 1, 8, 52.25),
		row("entha", 18, 14, 7, 2, 74.56),
		row("devin.k", 18, 14, 47, 28, 51.66),
		row("gaen.v", 18, 15, 13, 42, 64.64),
		row("X.F", 18, 13, 54, 55, 55.09),
		row("ambe", 18, 13, 42, 12, 69.68),
		row("QIuLin", 18, 12, 52, 3, 75.84),
		row("entha", 18, 11, 40, 31, 85.08),
		row("devin.k", 18, 11, 57, 1, 64.82),
		row("gaen.v", 18, 11, 43, 56, 77.87),
		row("X.F", 18, 10, 27, 33, 87.54),
		row("ambe", 18, 7, 11, 50, 0.0075),
		row("devin.k", 18, 5, 22, 1, 21.65),
		row("QIuLin", 18, 6, 0, 9, 20.72),
		row("entha", 18, 5, 30, 17, 15.75),
		row("X.F", 18, 1, 7, 50, 69.79),
		row("gaen.v", 17, 23, 46, 57, 74.51),
		row("devin.k", 17, 23, 21, 14, 57.86),
		row("entha", 18, 0, 15, 0, 94.19),
		row("QIuLin", 17, 23, 51, 54, 17.15),
		row("ambe", 17, 23, 21, 13, 54.77),
		row("X.F", 17, 17, 30, 43, 87.33),
		row("gaen.v", 17, 17, 30, 41, 92.47),
		row("devin.k", 17, 19, 8, 51, 54.68),
		row("QIuLin", 17, 17, 30, 40, 96.23),
		row("entha", 17, 19, 8, 52, 77.17),
		row("ambe", 17, 17, 30, 36, 78.80),
		row("X.F", 17, 15, 50, 2, 94.25),
		row("gaen.v", 17, 12, 33, 9, 66.06),
		row("devin.k", 17, 12, 41, 47, 49.94),
		row("QIuLin", 17, 12, 38, 50, 31.84),
		row("entha", 17, 12, 11, 51, 38.96),
		row("ambe", 17, 12, 41, 45, 44.81),
		row("X.F", 17, 10, 17, 42, 47.44),
		row("gaen.v", 17, 7, 55, 1, 2.71),
		row("devin.k", 17, 9, 28, 40, 100.59),
		row("QIuLin", 17, 4, 32, 15, 52.73),
	}

	got := latestFiveHourEstimatePerAccount(estimates)
	want := []fiveHourEstimate{
		row("entha", 18, 21, 38, 24, 86.08),
		row("devin.k", 18, 23, 22, 39, 14.45),
		row("ambe", 18, 23, 58, 27, 55.74),
		row("QIuLin", 18, 23, 58, 30, 0.0952),
		row("X.F", 18, 23, 58, 33, 57.01),
		row("gaen.v", 18, 23, 58, 33, 46.99),
	}
	if len(got) != len(want) {
		t.Fatalf("live rows: want %d accounts after filtering 48 rows, got %d: %+v", len(want), len(got), got)
	}
	for index := range want {
		if got[index].Account != want[index].Account || got[index].To != want[index].To || got[index].FullWindowUSD != want[index].FullWindowUSD {
			t.Fatalf("live row %d = %+v, want %+v", index, got[index], want[index])
		}
	}
}

// These reset pairs came from live readings of eight windows. In every pair the
// lower value split a real window into a short overlapping phantom window.
func TestFiveHourEstimateMergesObservedResetDrift(t *testing.T) {
	prices := []store.ModelPrice{{ModelPattern: "m", EffectiveFrom: 1, InputUSDPerMTok: 1}}
	for _, test := range []struct {
		account string
		reset   string
		stray   string
	}{
		{account: "gaen.v", reset: "1786676400", stray: "1786676399"},
		{account: "devin.k", reset: "1786681200", stray: "1786681199"},
		{account: "QIuLin", reset: "1786686600", stray: "1786686599"},
		{account: "ambe", reset: "1786688400", stray: "1786688399"},
		{account: "gaen.v", reset: "1786694400", stray: "1786694399"},
		{account: "devin.k", reset: "1786699200", stray: "1786699199"},
		{account: "QIuLin", reset: "1786704600", stray: "1786704599"},
		{account: "ambe", reset: "1786706400", stray: "1786706399"},
	} {
		t.Run(test.account+"/"+test.reset, func(t *testing.T) {
			reading := func(at int64, reset string, usedPercent float64, tokens int64) store.SubscriptionUsageSnapshot {
				return store.SubscriptionUsageSnapshot{AccountID: 1, Account: test.account,
					ObservedAt: time.Unix(at, 0).UnixMilli(), ResetsAt: reset, UsedPercent: usedPercent,
					Totals: map[string]store.UsageCounters{"m": {InputTokens: tokens}}}
			}
			// Which identity the window's earliest reading happens to carry is a
			// property of the data, and the merged row reports the genuine one
			// either way.
			for _, anchor := range []struct{ name, resetsAt string }{
				{name: "anchored on the genuine reading", resetsAt: test.reset},
				{name: "anchored on the drifted reading", resetsAt: test.stray},
			} {
				t.Run(anchor.name, func(t *testing.T) {
					estimates := buildFiveHourEstimates([]store.SubscriptionUsageSnapshot{
						reading(40, test.reset, 78, 4_000_000),
						reading(30, test.stray, 78, 3_000_000),
						reading(20, test.stray, 64, 2_000_000),
						reading(10, anchor.resetsAt, 10, 1_000_000),
					}, prices)
					if len(estimates) != 1 {
						t.Fatalf("want one row for the real window, got %d: %+v", len(estimates), estimates)
					}
					got := estimates[0]
					if got.From != time.Unix(10, 0).UnixMilli() || got.To != time.Unix(40, 0).UnixMilli() ||
						got.ResetsAt != test.reset || got.UsedPercentDelta != 68 || got.ObservedCostUSD != 3 {
						t.Fatalf("merged window = %+v", got)
					}
				})
			}
		})
	}
}

// Two readings of the same whole percent differ by about 1e-15 once the 0..1
// fraction has been multiplied out. That difference is greater than zero, so a
// window that never moved would otherwise divide a real cost by it and put a
// figure of the order of 1e16 on the dashboard. Readings already stored and
// every reading the manual refresh path writes still carry their fraction, so
// rounding at the sampler does not reach them.
func TestFiveHourEstimateRejectsAnEpsilonDenominator(t *testing.T) {
	prices := []store.ModelPrice{{ModelPattern: "m", EffectiveFrom: 1, InputUSDPerMTok: 1}}
	lower, upper := 0.29*100, 0.29000000000000004*100
	if upper-lower <= 0 {
		t.Fatalf("the two readings no longer differ: %v and %v", lower, upper)
	}
	snapshots := []store.SubscriptionUsageSnapshot{
		{AccountID: 1, Account: "a", ObservedAt: time.Unix(20, 0).UnixMilli(), ResetsAt: "1786676400",
			UsedPercent: upper, Totals: map[string]store.UsageCounters{"m": {InputTokens: 2_000_000}}},
		{AccountID: 1, Account: "a", ObservedAt: time.Unix(10, 0).UnixMilli(), ResetsAt: "1786676400",
			UsedPercent: lower, Totals: map[string]store.UsageCounters{"m": {InputTokens: 1_000_000}}},
	}
	if estimates := buildFiveHourEstimates(snapshots, prices); len(estimates) != 0 {
		t.Fatalf("estimates=%+v", estimates)
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
