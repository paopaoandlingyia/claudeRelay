package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/local/claude-relay/internal/metrics"
	"github.com/local/claude-relay/internal/store"
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

func TestListAccountsReportsCooldownAndRoutingActivity(t *testing.T) {
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
	if err := server.store.Bind(t.Context(), "session:route-key", account.ID, time.Hour); err != nil {
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
	if view.ActiveSessions != 1 {
		t.Errorf("active_sessions = %d, want 1", view.ActiveSessions)
	}
	release := server.load.reserve(account.ID)
	recorder = adminRequest(t, server, http.MethodGet, "/admin/v1/accounts", "")
	release()
	if got := decodeAccounts(t, recorder)[0].InFlight; got != 1 {
		t.Errorf("in_flight = %d, want 1", got)
	}
	if view.CreatedAt == 0 {
		t.Error("created_at was not reported")
	}
	if view.Pool != store.AccountPoolCompatible {
		t.Errorf("pool = %q, want compatible", view.Pool)
	}
}

func TestListAccountsUpdatesFiveHourWindowFromResponseHeaders(t *testing.T) {
	t.Parallel()
	reset := time.Now().Add(4 * time.Hour).Truncate(time.Second)
	headersSent := make(chan struct{})
	releaseBody := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseBody) }) }

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected upstream path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set(fiveHourResetHeader, strconv.FormatInt(reset.Unix(), 10))
		w.Header().Set(fiveHourUtilizationHeader, "0.31")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(headersSent)
		<-releaseBody
		_, _ = io.WriteString(w,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-test\",\"usage\":{\"input_tokens\":1}}}\n\n"+
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()
	defer release()
	server := newTestServer(t, upstream.URL, 4096)

	before := decodeAccounts(t, adminRequest(t, server, http.MethodGet, "/admin/v1/accounts", ""))
	if len(before) != 1 || before[0].FiveHourWindow != nil {
		t.Fatalf("account invented a five-hour window before a response: %+v", before)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("x-api-key", "downstream-key")
	done := make(chan struct{})
	go func() {
		server.routes().ServeHTTP(recorder, request)
		close(done)
	}()
	<-headersSent

	var window *accountUsageWindow
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		accounts := decodeAccounts(t, adminRequest(t, server, http.MethodGet, "/admin/v1/accounts", ""))
		if len(accounts) == 1 && accounts[0].FiveHourWindow != nil {
			window = accounts[0].FiveHourWindow
			break
		}
		time.Sleep(time.Millisecond)
	}
	if window == nil {
		t.Fatal("response headers did not update the account window while the response body was still streaming")
	}
	if window.ID != "five_hour" || window.UsedPercent != 31 || window.RemainingPercent != 69 ||
		window.ResetsAt != reset.UTC().Format(time.RFC3339) || window.ObservedAt == 0 {
		t.Fatalf("response-derived window = %+v", window)
	}

	release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("relay response did not finish after the upstream body was released")
	}
}

func TestSetAccountPoolClearsStickySessions(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "https://upstream.invalid", 1024)
	account, found, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil || !found {
		t.Fatalf("account lookup failed: found=%v err=%v", found, err)
	}
	if err := server.store.Bind(t.Context(), "route-key", account.ID, time.Hour); err != nil {
		t.Fatal(err)
	}

	recorder := adminRequest(t, server, http.MethodPost, "/admin/v1/accounts/default/pool", `{"pool":"official"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var view accountView
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Pool != store.AccountPoolOfficial {
		t.Fatalf("pool = %q, want official", view.Pool)
	}
	counts, err := server.store.SessionBindingCounts(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != 0 {
		t.Fatalf("sticky sessions survived pool move: %v", counts)
	}
	if recorder := adminRequest(t, server, http.MethodPost, "/admin/v1/accounts/default/pool", `{"pool":"other"}`); recorder.Code != http.StatusBadRequest {
		t.Errorf("invalid pool status = %d, want 400", recorder.Code)
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
	existing := `{"alias":"default","credential":"{\"type\":\"claude\",\"access_token\":\"replacement\"}"}`
	if recorder := adminRequest(t, server, http.MethodPost, "/admin/v1/accounts/import", existing); recorder.Code != http.StatusConflict {
		t.Errorf("implicit replacement status = %d, want 409", recorder.Code)
	}
	explicit := `{"alias":"default","credential":"{\"type\":\"claude\",\"access_token\":\"replacement\"}","replace":true}`
	if recorder := adminRequest(t, server, http.MethodPost, "/admin/v1/accounts/import", explicit); recorder.Code != http.StatusCreated {
		t.Errorf("explicit replacement status = %d, want 201, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestAdminResponsesAreNotCacheableAndCannotBeFramed(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "https://upstream.invalid", 1024)
	recorder := adminRequest(t, server, http.MethodGet, "/admin/v1/overview", "")
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := recorder.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
	if got := recorder.Header().Get("Content-Security-Policy"); !strings.Contains(got, "frame-ancestors 'none'") {
		t.Errorf("Content-Security-Policy = %q", got)
	}
}

func TestOverviewAvailabilityAccountsForRefreshableExpiry(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "https://upstream.invalid", 1024)
	account, _, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.store.UpdateTokens(t.Context(), account.ID, "expired", "refresh", "2020-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	decode := func() overviewResponse {
		recorder := adminRequest(t, server, http.MethodGet, "/admin/v1/overview", "")
		var overview overviewResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &overview); err != nil {
			t.Fatal(err)
		}
		return overview
	}
	overview := decode()
	if overview.Accounts.Available != 1 || overview.Accounts.Expired != 1 {
		t.Errorf("refreshable totals = %#v", overview.Accounts)
	}
	server.cfg.AutoRefresh = false
	overview = decode()
	if overview.Accounts.Available != 0 {
		t.Errorf("available with refresh disabled = %d, want 0", overview.Accounts.Available)
	}
}

func TestForcedRefreshRechecksEnabledState(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "https://upstream.invalid", 1024)
	account, _, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.store.UpdateTokens(t.Context(), account.ID, "access", "refresh", time.Now().Add(time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if _, err := server.store.SetAccountEnabled(t.Context(), account.Alias, false); err != nil {
		t.Fatal(err)
	}
	if _, err := server.tokens.refreshNow(t.Context(), account); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("refreshNow error = %v, want disabled-account rejection", err)
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
	if overview.OfficialAPIKey != "official-downstream-key" {
		t.Errorf("official_api_key = %q, want the configured official key", overview.OfficialAPIKey)
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
			Account               string                  `json:"account"`
			Model                 string                  `json:"model"`
			Status                int                     `json:"status"`
			Selection             string                  `json:"selection"`
			ClientClass           string                  `json:"client_class"`
			ClassificationVersion int                     `json:"classification_version"`
			RelayAction           string                  `json:"relay_action"`
			ClientEvidence        *metrics.ClientEvidence `json:"client_evidence"`
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
	if record.ClientClass != clientClassCompatible || record.ClassificationVersion != clientClassificationVersion ||
		record.RelayAction != "minimal_attribution" || record.ClientEvidence == nil {
		t.Errorf("request observation = %#v", record)
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
