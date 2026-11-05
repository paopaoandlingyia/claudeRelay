package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/local/claude-relay/internal/store"
)

func TestReportedAvailabilityFollowsCurrentRoutingExclusions(t *testing.T) {
	for _, tc := range []struct {
		name           string
		reading        bool
		used           float64
		resetIn        time.Duration
		legacy         bool
		cooldownModel  string
		cooldownReason string
		disabled       bool
		wantAvailable  int
		wantIngress    bool
	}{
		{name: "unknown quota remains available", wantAvailable: 1, wantIngress: true},
		{name: "remaining quota stays available", reading: true, used: 99, resetIn: time.Hour, wantAvailable: 1, wantIngress: true},
		{name: "empty live window is unavailable without a 429", reading: true, used: 100, resetIn: time.Hour},
		{name: "reset window becomes available", reading: true, used: 100, resetIn: -time.Minute, wantAvailable: 1, wantIngress: true},
		{name: "legacy routing does not exclude sampled empty quota", reading: true, used: 100, resetIn: time.Hour, legacy: true, wantAvailable: 1, wantIngress: true},
		{name: "refresh failure remains unavailable", cooldownReason: "oauth_refresh_failed"},
		{name: "quota cooldown remains unavailable", cooldownReason: cooldownReasonFiveHourExhausted},
		{name: "model cooldown does not disable the entire ingress", cooldownModel: "claude-test", cooldownReason: cooldownReasonAmbiguous429, wantIngress: true},
		{name: "disabled account remains unavailable", disabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newTestServer(t, "http://127.0.0.1:1", 4096)
			server.cfg.AvailabilityAPIKey = "availability-key"
			account, found, err := server.store.AccountByAlias(t.Context(), "default")
			if err != nil || !found {
				t.Fatalf("account found=%v err=%v", found, err)
			}
			now := time.Now()
			if tc.reading {
				server.sampler.observe(account, fiveHourReading{
					resetsAt: strconv.FormatInt(now.Add(tc.resetIn).Unix(), 10), usedPercent: tc.used, observedAt: now.UnixMilli(),
				})
			}
			if tc.legacy {
				if _, err := server.routingPolicy.set(routingPolicyLegacy); err != nil {
					t.Fatal(err)
				}
			}
			if tc.cooldownReason != "" {
				if err := server.store.Cooldown(t.Context(), account.ID, tc.cooldownModel, now.Add(time.Hour), tc.cooldownReason); err != nil {
					t.Fatal(err)
				}
			}
			if tc.disabled {
				if _, err := server.store.SetAccountEnabled(t.Context(), account.Alias, false); err != nil {
					t.Fatal(err)
				}
			}

			var listed struct {
				Accounts []struct {
					Unavailable bool `json:"unavailable"`
				} `json:"accounts"`
			}
			response := adminRequest(t, server, http.MethodGet, "/admin/v1/accounts", "")
			if response.Code != http.StatusOK {
				t.Fatalf("account list status=%d", response.Code)
			}
			if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil {
				t.Fatal(err)
			}
			if len(listed.Accounts) != 1 || listed.Accounts[0].Unavailable != (tc.wantAvailable == 0) {
				t.Errorf("account list=%+v, want unavailable=%v", listed.Accounts, tc.wantAvailable == 0)
			}

			var overview overviewResponse
			response = adminRequest(t, server, http.MethodGet, "/admin/v1/overview", "")
			if err := json.Unmarshal(response.Body.Bytes(), &overview); err != nil {
				t.Fatal(err)
			}
			if overview.Accounts.Available != tc.wantAvailable {
				t.Errorf("available=%d, want %d", overview.Accounts.Available, tc.wantAvailable)
			}

			request := httptest.NewRequest(http.MethodGet, "/ops/v1/availability", nil)
			request.Header.Set("x-api-key", "availability-key")
			response = httptest.NewRecorder()
			server.routes().ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("availability status=%d, want unchanged HTTP 200", response.Code)
			}
			var availability availabilityResponse
			if err := json.Unmarshal(response.Body.Bytes(), &availability); err != nil {
				t.Fatal(err)
			}
			for ingress, available := range availability.Availability {
				if available != tc.wantIngress {
					t.Errorf("%s available=%v, want %v", ingress, available, tc.wantIngress)
				}
			}
		})
	}
}

func TestEmptyQuotaReportingPreservesMessagesErrorAndCountTokensAdmission(t *testing.T) {
	server := newTestServer(t, "http://127.0.0.1:1", 4096)
	account, _, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	server.sampler.observe(account, fiveHourReading{
		resetsAt: strconv.FormatInt(now.Add(time.Hour).Unix(), 10), usedPercent: 100, observedAt: now.UnixMilli(),
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("x-api-key", "downstream-key")
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), accountUnavailableMessage) {
		t.Fatalf("Messages error changed: status=%d body=%s", response.Code, response.Body.String())
	}
	selected, err := server.selector.selectAccount(t.Context(), requestRoute{
		CountTokens: true, AccountAccess: store.AccountAccessCompatibleOnly, Model: "claude-test",
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	selected.release()
	if selected.Account.ID != account.ID {
		t.Fatalf("count_tokens selected account=%d, want %d", selected.Account.ID, account.ID)
	}
}

func TestQuotaAvailabilityKeepsHealthyPoolsAndRecoversOnNewReading(t *testing.T) {
	server := newTestServer(t, "http://127.0.0.1:1", 4096)
	server.cfg.AvailabilityAPIKey = "availability-key"
	account, _, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	reset := strconv.FormatInt(now.Add(time.Hour).Unix(), 10)
	server.sampler.observe(account, fiveHourReading{resetsAt: reset, usedPercent: 100, observedAt: now.UnixMilli()})
	importTestAccount(t, server.store, "official", "official-token", "22222222-2222-4222-8222-222222222222", "b")
	if _, err := server.store.SetAccountPool(t.Context(), "official", store.AccountPoolOfficial); err != nil {
		t.Fatal(err)
	}
	for _, recovered := range []bool{false, true} {
		if recovered {
			server.sampler.observe(account, fiveHourReading{resetsAt: reset, usedPercent: 50, observedAt: now.UnixMilli() + 1})
		}
		request := httptest.NewRequest(http.MethodGet, "/ops/v1/availability", nil)
		request.Header.Set("x-api-key", "availability-key")
		response := httptest.NewRecorder()
		server.routes().ServeHTTP(response, request)
		var availability availabilityResponse
		if err := json.Unmarshal(response.Body.Bytes(), &availability); err != nil {
			t.Fatal(err)
		}
		if !availability.Availability[ingressOfficial] || availability.Availability[ingressCompatible] != recovered || availability.Availability[ingressExperimental] != recovered {
			t.Fatalf("recovered=%v availability=%v", recovered, availability.Availability)
		}
		views := decodeAccounts(t, adminRequest(t, server, http.MethodGet, "/admin/v1/accounts", ""))
		for _, view := range views {
			if view.Alias == account.Alias && view.Unavailable == recovered {
				t.Errorf("recovered=%v unavailable=%v", recovered, view.Unavailable)
			}
		}
	}
}
