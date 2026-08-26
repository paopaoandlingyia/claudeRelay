package proxy

import (
	"context"
	"encoding/json"
	"io"
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
	if dashboard.Totals.CostUSD <= 0 || len(dashboard.ByModel) != 1 {
		t.Fatalf("dashboard=%+v", dashboard)
	}
	if len(dashboard.FiveHourCurrent) != 1 || dashboard.FiveHourCurrent[0].EventCount != 1 ||
		dashboard.FiveHourCurrent[0].MaxUsedPercent != 31 || dashboard.FiveHourCurrent[0].ObservedCostUSD <= 0 {
		t.Fatalf("current five-hour window=%+v", dashboard.FiveHourCurrent)
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

func TestValueFiveHourWindowsUsesObservedModelTotalsWithoutExtrapolation(t *testing.T) {
	windows := []store.FiveHourWindow{{
		Account: "a", ResetsAt: "1786386600", FirstObservedAt: 10_000, LastObservedAt: 20_000,
		FirstUsedPercent: 10, LastUsedPercent: 100, MaxUsedPercent: 100, ExhaustedAt: 20_000,
		EventCount: 3, MissingUsageCount: 1, IncompleteCount: 1,
		ByModel: map[string]store.UsageCounters{
			"sonnet":  {InputTokens: 1_000_000, Requests: 2},
			"unknown": {OutputTokens: 100, Requests: 1},
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
}
