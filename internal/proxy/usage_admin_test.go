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
