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

func TestQuotaAwareRoutingOverridesAutomaticAffinityAndLegacyRestoresIt(t *testing.T) {
	server := newTestServer(t, "https://upstream.invalid", 1<<20)
	importTestAccount(t, server.store, "secondary", "token-secondary", "22222222-2222-4222-8222-222222222222", "b")

	primary, found, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil || !found {
		t.Fatalf("primary found=%v err=%v", found, err)
	}
	secondary, found, err := server.store.AccountByAlias(t.Context(), "secondary")
	if err != nil || !found {
		t.Fatalf("secondary found=%v err=%v", found, err)
	}
	now := time.Now()
	server.sampler.observe(primary, fiveHourReading{
		resetsAt: strconv.FormatInt(now.Add(5*time.Minute).Unix(), 10), usedPercent: 10, observedAt: now.UnixMilli(),
	})
	server.sampler.observe(secondary, fiveHourReading{
		resetsAt: strconv.FormatInt(now.Add(4*time.Hour).Unix(), 10), usedPercent: 95, observedAt: now.UnixMilli(),
	})
	route := requestRoute{
		ConversationKey: "session:test", SelectionKey: "prefix:test", StickyTTL: time.Hour,
		AccountUUID: secondary.AccountUUID, Ingress: ingressCompatible, AccountAccess: store.AccountAccessCompatibleOnly, Model: "claude-test",
	}
	if err := server.store.Bind(t.Context(), route.ConversationKey, secondary.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := server.routingPolicy.set(routingPolicyQuotaAware); err != nil {
		t.Fatal(err)
	}

	selected, err := server.selector.selectAccount(t.Context(), route, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Account.ID != primary.ID || selected.Source != "quota_aware" {
		t.Fatalf("quota-aware selection = account %q source %q, want default/quota_aware", selected.Account.Alias, selected.Source)
	}
	if !selected.QuotaPriority.known || selected.QuotaPriority.remaining != 90 {
		t.Fatalf("quota priority = %+v", selected.QuotaPriority)
	}
	selected.release()
	selected.releaseSession()

	if _, err := server.routingPolicy.set(routingPolicyLegacy); err != nil {
		t.Fatal(err)
	}
	selected, err = server.selector.selectAccount(t.Context(), route, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer selected.release()
	defer selected.releaseSession()
	if selected.Account.ID != secondary.ID || selected.Source != "account_uuid" {
		t.Fatalf("legacy selection = account %q source %q, want secondary/account_uuid", selected.Account.Alias, selected.Source)
	}
}

func TestQuotaAwareRoutingSamplesUnknownAccountsFirst(t *testing.T) {
	server := newTestServer(t, "https://upstream.invalid", 1<<20)
	importTestAccount(t, server.store, "unknown", "token-unknown", "22222222-2222-4222-8222-222222222222", "b")
	primary, _, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	server.sampler.observe(primary, fiveHourReading{
		resetsAt: strconv.FormatInt(now.Add(time.Minute).Unix(), 10), usedPercent: 0, observedAt: now.UnixMilli(),
	})
	if _, err := server.routingPolicy.set(routingPolicyQuotaAware); err != nil {
		t.Fatal(err)
	}

	selected, err := server.selector.selectAccount(t.Context(), requestRoute{
		SelectionKey: "prefix:test", Ingress: ingressCompatible, AccountAccess: store.AccountAccessCompatibleOnly, Model: "claude-test",
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer selected.release()
	defer selected.releaseSession()
	if selected.Account.Alias != "unknown" || selected.Source != "quota_probe" {
		t.Fatalf("selection = account %q source %q, want unknown/quota_probe", selected.Account.Alias, selected.Source)
	}
}

func TestQuotaAwareRoutingSkipsKnownEmptyWindow(t *testing.T) {
	server := newTestServer(t, "https://upstream.invalid", 1<<20)
	importTestAccount(t, server.store, "available", "token-available", "22222222-2222-4222-8222-222222222222", "b")
	empty, _, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil {
		t.Fatal(err)
	}
	available, _, err := server.store.AccountByAlias(t.Context(), "available")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	server.sampler.observe(empty, fiveHourReading{
		resetsAt: strconv.FormatInt(now.Add(time.Minute).Unix(), 10), usedPercent: 100, observedAt: now.UnixMilli(),
	})
	server.sampler.observe(available, fiveHourReading{
		resetsAt: strconv.FormatInt(now.Add(4*time.Hour).Unix(), 10), usedPercent: 90, observedAt: now.UnixMilli(),
	})
	if _, err := server.routingPolicy.set(routingPolicyQuotaAware); err != nil {
		t.Fatal(err)
	}

	selected, err := server.selector.selectAccount(t.Context(), requestRoute{
		SelectionKey: "prefix:test", Ingress: ingressCompatible, AccountAccess: store.AccountAccessCompatibleOnly, Model: "claude-test",
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer selected.release()
	defer selected.releaseSession()
	if selected.Account.ID != available.ID {
		t.Fatalf("selected %q, want available account", selected.Account.Alias)
	}
}

func TestQuotaAwareSuccessDoesNotPersistAffinityAndRollbackDoes(t *testing.T) {
	reset := time.Now().Add(time.Hour).Unix()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(fiveHourResetHeader, strconv.FormatInt(reset, 10))
		w.Header().Set(fiveHourUtilizationHeader, "0.25")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 1<<20)
	body := `{"model":"claude-test","system":[{"type":"text","text":"shared","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hello"}]}`
	route, err := deriveRequestRoute([]byte(body), nil, compatibleIngress, "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := server.routingPolicy.set(routingPolicyQuotaAware); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("x-api-key", "downstream-key")
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("quota-aware response status = %d body=%s", response.Code, response.Body.String())
	}
	if _, found, err := server.store.BoundAccount(t.Context(), route.ConversationKey, store.AccountAccessCompatibleOnly, time.Now()); err != nil || found {
		t.Fatalf("quota-aware binding found=%v err=%v", found, err)
	}

	if _, err := server.routingPolicy.set(routingPolicyLegacy); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("x-api-key", "downstream-key")
	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("legacy response status = %d body=%s", response.Code, response.Body.String())
	}
	if _, found, err := server.store.BoundAccount(t.Context(), route.ConversationKey, store.AccountAccessCompatibleOnly, time.Now()); err != nil || !found {
		t.Fatalf("legacy binding found=%v err=%v", found, err)
	}
}

func TestRoutingPolicyAdminEndpointAndOverview(t *testing.T) {
	server := newTestServer(t, "https://upstream.invalid", 1<<20)
	decodeOverview := func() overviewResponse {
		recorder := adminRequest(t, server, http.MethodGet, "/admin/v1/overview", "")
		var overview overviewResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &overview); err != nil {
			t.Fatal(err)
		}
		return overview
	}
	if got := decodeOverview().RoutingPolicy; got != routingPolicyLegacy {
		t.Fatalf("initial policy = %q", got)
	}

	recorder := adminRequest(t, server, http.MethodPost, "/admin/v1/routing/policy", `{"policy":"quota_aware"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("toggle status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := decodeOverview().RoutingPolicy; got != routingPolicyQuotaAware {
		t.Fatalf("enabled policy = %q", got)
	}

	recorder = adminRequest(t, server, http.MethodPost, "/admin/v1/routing/policy", `{"policy":"typo"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid policy status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := decodeOverview().RoutingPolicy; got != routingPolicyQuotaAware {
		t.Fatalf("invalid update changed policy to %q", got)
	}
}
