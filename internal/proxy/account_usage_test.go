package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/local/claude-relay/internal/credential"
	"github.com/local/claude-relay/internal/store"
)

func TestAccountUsageCacheKeyDoesNotFollowReusableDatabaseID(t *testing.T) {
	first := store.Account{ID: 1, Credential: credential.Credential{AccountUUID: "11111111-1111-4111-8111-111111111111"}}
	second := store.Account{ID: 1, Credential: credential.Credential{AccountUUID: "22222222-2222-4222-8222-222222222222"}}
	if accountUsageCacheKey(first) == accountUsageCacheKey(second) {
		t.Fatal("different account identities shared a usage cache key")
	}
}

func TestAccountUsageReadsOnlyReturnedWindowsAndCaches(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	usageCalls := 0
	profileCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-access-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("anthropic-beta"); got != "oauth-2025-04-20" {
			t.Errorf("anthropic-beta = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/oauth/usage":
			usageCalls++
			_, _ = w.Write([]byte(`{
				"five_hour":{"utilization":11.25,"resets_at":"2026-07-31T20:00:00Z"},
				"seven_day":null,
				"extra_usage":{"is_enabled":true,"monthly_limit":2000,"used_credits":325,"utilization":16.25}
			}`))
		case "/api/oauth/profile":
			profileCalls++
			_, _ = w.Write([]byte(`{"account":{"has_claude_max":false,"has_claude_pro":true}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)

	decode := func(recorder *httptest.ResponseRecorder) accountUsageView {
		t.Helper()
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		var view accountUsageView
		if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		return view
	}

	first := decode(adminRequest(t, server, http.MethodGet, "/admin/v1/accounts/default/usage", ""))
	if first.Cached || first.PlanType != "pro" || len(first.Windows) != 1 {
		t.Fatalf("first usage = %#v", first)
	}
	window := first.Windows[0]
	if window.ID != "five_hour" || window.UsedPercent != 11.25 || window.RemainingPercent != 88.75 {
		t.Fatalf("five-hour window = %#v", window)
	}
	if first.ExtraUsage == nil || !first.ExtraUsage.Enabled || first.ExtraUsage.UsedCreditsCents != 325 {
		t.Fatalf("extra usage = %#v", first.ExtraUsage)
	}
	second := decode(adminRequest(t, server, http.MethodGet, "/admin/v1/accounts/default/usage", ""))
	if !second.Cached || second.FetchedAt != first.FetchedAt {
		t.Fatalf("cached usage = %#v", second)
	}
	forced := decode(adminRequest(t, server, http.MethodPost, "/admin/v1/accounts/default/usage/refresh", ""))
	if forced.Cached {
		t.Fatal("forced refresh was reported as cached")
	}
	mu.Lock()
	defer mu.Unlock()
	if usageCalls != 2 || profileCalls != 2 {
		t.Fatalf("upstream calls = usage %d profile %d, want 2 each", usageCalls, profileCalls)
	}
}

func TestAccountUsageSurvivesUnavailableOptionalProfile(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/oauth/profile" {
			http.Error(w, `{"error":{"message":"profile unavailable"}}`, http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":0,"resets_at":null}}`))
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	recorder := adminRequest(t, server, http.MethodGet, "/admin/v1/accounts/default/usage", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var view accountUsageView
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.PlanType != "" || len(view.Windows) != 1 || view.Windows[0].RemainingPercent != 100 {
		t.Fatalf("usage = %#v", view)
	}
}

func TestResolveClaudePlan(t *testing.T) {
	t.Parallel()
	flag := func(v bool) *bool { return &v }
	org := func(orgType, status string) rawProfilePayload {
		var profile rawProfilePayload
		profile.Account.HasClaudeMax = flag(false)
		profile.Account.HasClaudePro = flag(false)
		profile.Organization.OrganizationType = orgType
		profile.Organization.SubscriptionStatus = status
		return profile
	}
	account := func(max, pro bool) rawProfilePayload {
		var profile rawProfilePayload
		profile.Account.HasClaudeMax = flag(max)
		profile.Account.HasClaudePro = flag(pro)
		return profile
	}

	cases := []struct {
		name    string
		profile rawProfilePayload
		want    string
	}{
		{"max", account(true, false), "max"},
		{"pro", account(false, true), "pro"},
		{"free", account(false, false), "free"},
		{"team", org("claude_team", "active"), "team"},
		// Organization accounts leave the Max/Pro flags false, so an org plan
		// must win over the free check rather than being reported as free.
		{"enterprise", org("claude_enterprise", "active"), "enterprise"},
		{"unknown org type passes through", org("claude_something_new", "active"), "something_new"},
		{"org type without claude prefix", org("Partner", "active"), "partner"},
		{"inactive org falls back to account flags", org("claude_enterprise", "canceled"), "free"},
		{"no signal at all", rawProfilePayload{}, ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveClaudePlan(testCase.profile); got != testCase.want {
				t.Fatalf("resolveClaudePlan = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestAccountUsageDoesNotRefreshDisabledExpiredAccount(t *testing.T) {
	t.Parallel()
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		upstreamCalls++
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	account, err := server.store.ImportAccount(t.Context(), "disabled", credential.Credential{
		Type:         "claude",
		AccessToken:  "expired-access",
		RefreshToken: "must-not-rotate",
		ExpiresAt:    time.Now().Add(-time.Hour).Format(time.RFC3339),
		AccountUUID:  "33333333-3333-4333-8333-333333333333",
		DeviceID:     "device",
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := adminRequest(t, server, http.MethodGet, "/admin/v1/accounts/disabled/usage", "")
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if upstreamCalls != 0 {
		t.Fatalf("disabled expired usage made %d upstream calls", upstreamCalls)
	}
	persisted, _, err := server.store.AccountByID(t.Context(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.AccessToken != "expired-access" || persisted.RefreshToken != "must-not-rotate" || persisted.LastRefreshAt != 0 {
		t.Fatalf("disabled account tokens changed: %#v", persisted)
	}
}
