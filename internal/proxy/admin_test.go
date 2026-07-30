package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/local/claude-relay/internal/metrics"
)

func adminRequest(t *testing.T, server *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("x-api-key", "admin-key")
	if body != "" {
		request.Header.Set("content-type", "application/json")
	}
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)
	return recorder
}

func decodeAccounts(t *testing.T, recorder *httptest.ResponseRecorder) []accountView {
	t.Helper()
	var payload struct {
		Accounts []accountView `json:"accounts"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode accounts: %v (%s)", err, recorder.Body.String())
	}
	return payload.Accounts
}

func TestListAccountsReportsCooldownAndStickySessions(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "https://upstream.invalid", 1024)
	account, found, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil || !found {
		t.Fatalf("account lookup failed: found = %v, err = %v", found, err)
	}
	until := time.Now().Add(90 * time.Second)
	if err := server.store.Cooldown(t.Context(), account.ID, "claude-opus-5", until, "Too Many Requests"); err != nil {
		t.Fatal(err)
	}
	if err := server.store.Bind(t.Context(), "route-key", account.ID, time.Hour); err != nil {
		t.Fatal(err)
	}

	recorder := adminRequest(t, server, http.MethodGet, "/admin/v1/accounts", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	accounts := decodeAccounts(t, recorder)
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}
	view := accounts[0]
	if view.Cooldown == nil {
		t.Fatal("cooldown was not reported, so an unusable account would look available")
	}
	if view.Cooldown.Model != "claude-opus-5" || view.Cooldown.Reason != "Too Many Requests" {
		t.Errorf("cooldown = %#v", view.Cooldown)
	}
	if want := until.Unix() * 1000; view.Cooldown.UntilAt != want {
		t.Errorf("cooldown.until_at = %d, want %d epoch milliseconds", view.Cooldown.UntilAt, want)
	}
	if view.StickySessions != 1 {
		t.Errorf("sticky_sessions = %d, want 1", view.StickySessions)
	}
	if view.CreatedAt == 0 {
		t.Error("created_at was not reported")
	}
}

func TestDeleteAccountAlsoDropsItsMetrics(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "https://upstream.invalid", 1024)
	server.metrics.Record(metricsEventFor("default"))

	recorder := adminRequest(t, server, http.MethodDelete, "/admin/v1/accounts/default", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if _, present := server.metrics.AccountStats()["default"]; present {
		t.Error("deleted account still reports traffic statistics")
	}
	if recorder := adminRequest(t, server, http.MethodDelete, "/admin/v1/accounts/default", ""); recorder.Code != http.StatusNotFound {
		t.Errorf("second delete status = %d, want 404", recorder.Code)
	}
}

func TestRenameAccountRejectsExistingAlias(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "https://upstream.invalid", 1024)
	importTestAccount(t, server.store, "other", "token", "22222222-2222-4222-8222-222222222222", "b")

	recorder := adminRequest(t, server, http.MethodPost, "/admin/v1/accounts/default/rename", `{"alias":"other"}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body = %s", recorder.Code, recorder.Body.String())
	}
	recorder = adminRequest(t, server, http.MethodPost, "/admin/v1/accounts/default/rename", `{"alias":"renamed"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if _, found, err := server.store.AccountByAlias(t.Context(), "renamed"); err != nil || !found {
		t.Errorf("renamed account missing: found = %v, err = %v", found, err)
	}
}

func TestRefreshRefusesDisabledAccountAndGlobalStop(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "https://upstream.invalid", 1024)
	if _, err := server.store.SetAccountEnabled(t.Context(), "default", false); err != nil {
		t.Fatal(err)
	}
	recorder := adminRequest(t, server, http.MethodPost, "/admin/v1/accounts/default/refresh", "")
	if recorder.Code != http.StatusConflict {
		t.Fatalf("disabled account status = %d, want 409", recorder.Code)
	}

	if _, err := server.store.SetAccountEnabled(t.Context(), "default", true); err != nil {
		t.Fatal(err)
	}
	server.cfg.AutoRefresh = false
	recorder = adminRequest(t, server, http.MethodPost, "/admin/v1/accounts/default/refresh", "")
	if recorder.Code != http.StatusConflict {
		t.Fatalf("global stop status = %d, want 409", recorder.Code)
	}
}

func TestCheckReportsExpiredTokenWithoutContactingUpstream(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("expired account should not reach upstream")
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 1024)
	account, _, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.store.UpdateTokens(t.Context(), account.ID, "token", "refresh", "2020-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}

	recorder := adminRequest(t, server, http.MethodPost, "/admin/v1/accounts/default/check", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var result checkResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.OK || !strings.Contains(result.Detail, "expired") {
		t.Fatalf("check result = %#v", result)
	}
}

func TestImportAccountFromConsoleArrivesDisabled(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "https://upstream.invalid", 1024)
	body := `{"alias":"imported","credential":"{\"type\":\"claude\",\"access_token\":\"abc\",\"refresh_token\":\"def\"}"}`

	recorder := adminRequest(t, server, http.MethodPost, "/admin/v1/accounts/import", body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	account, found, err := server.store.AccountByAlias(t.Context(), "imported")
	if err != nil || !found {
		t.Fatalf("imported account missing: found = %v, err = %v", found, err)
	}
	if account.Enabled {
		t.Error("console import enabled the account, bypassing explicit activation")
	}
	if recorder := adminRequest(t, server, http.MethodPost, "/admin/v1/accounts/import", `{"alias":"bad","credential":"not json"}`); recorder.Code != http.StatusBadRequest {
		t.Errorf("invalid credential status = %d, want 400", recorder.Code)
	}
}

func TestOverviewAndRequestsExposeRelayActivity(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg"}`))
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 1<<20)

	relay := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`))
	relay.Header.Set("x-api-key", "downstream-key")
	server.routes().ServeHTTP(httptest.NewRecorder(), relay)

	recorder := adminRequest(t, server, http.MethodGet, "/admin/v1/overview", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("overview status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var overview overviewResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &overview); err != nil {
		t.Fatal(err)
	}
	if overview.RelayAPIKey != "downstream-key" {
		t.Errorf("relay_api_key = %q, want the configured relay key", overview.RelayAPIKey)
	}
	if overview.Accounts.Total != 1 || overview.Accounts.Enabled != 1 {
		t.Errorf("account totals = %#v", overview.Accounts)
	}
	if overview.Requests.Requests != 1 || overview.Requests.Failures != 0 {
		t.Errorf("request summary = %#v", overview.Requests)
	}

	recorder = adminRequest(t, server, http.MethodGet, "/admin/v1/requests?limit=10", "")
	var listing struct {
		Requests []struct {
			Account   string `json:"account"`
			Model     string `json:"model"`
			Status    int    `json:"status"`
			Selection string `json:"selection"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(listing.Requests))
	}
	record := listing.Requests[0]
	if record.Account != "default" || record.Model != "claude-opus-5" || record.Status != http.StatusOK || record.Selection == "" {
		t.Errorf("request record = %#v", record)
	}
}

func TestAdminEndpointsRejectTheRelayKey(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "https://upstream.invalid", 1024)
	request := httptest.NewRequest(http.MethodGet, "/admin/v1/overview", nil)
	request.Header.Set("x-api-key", "downstream-key")
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
}

func metricsEventFor(alias string) metrics.Event {
	return metrics.Event{RequestID: "test", Account: alias, Status: http.StatusOK}
}
