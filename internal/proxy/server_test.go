package proxy

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/local/claude-relay/internal/claudeoauth"
	"github.com/local/claude-relay/internal/config"
	"github.com/local/claude-relay/internal/credential"
	"github.com/local/claude-relay/internal/store"
)

func TestForwardDecodesCompressedSSEBeforeUsageObservation(t *testing.T) {
	t.Parallel()
	const stream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-test\",\"usage\":{\"input_tokens\":11,\"output_tokens\":1}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept-Encoding"); got != "gzip" {
			t.Errorf("upstream Accept-Encoding = %q, want transport-managed gzip", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		compressed := gzip.NewWriter(w)
		_, _ = io.WriteString(compressed, stream)
		_ = compressed.Close()
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream.URL, 4096)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("x-api-key", "downstream-key")
	request.Header.Set("Accept-Encoding", "br, gzip")
	server.routes().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("downstream Content-Encoding = %q after transport decoding", got)
	}
	if got := recorder.Body.String(); got != stream {
		t.Fatalf("decoded response body = %q", got)
	}
	if err := server.accounting.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	account, found, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil || !found {
		t.Fatalf("account found=%v err=%v", found, err)
	}
	totals, err := server.store.UsageTotalsByModel(t.Context(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	usage := totals["claude-test"]
	if usage.Requests != 1 || usage.InputTokens != 11 || usage.OutputTokens != 7 {
		t.Fatalf("observed compressed usage = %#v", usage)
	}
}

func TestForwardPreservesBodyAndReplacesAuthentication(t *testing.T) {
	t.Parallel()
	requestBody := "{\n  \"model\": \"claude-test\",\n  \"system\": [{\"type\":\"text\",\"text\":\"x-anthropic-billing-header: cc_version=2.1.219.0a7; cc_entrypoint=claude-desktop; cch=abcde;\"}],\n  \"metadata\": {\"user_id\":\"official-client-value\"},\n  \"messages\": []\n}"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("beta") != "true" || r.URL.Query().Get("capture") != "1" {
			t.Errorf("query = %q", r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-access-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("x-api-key"); got != "" {
			t.Errorf("x-api-key leaked upstream: %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "claude-cli/2.1.215" {
			t.Errorf("User-Agent = %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != requestBody {
			t.Errorf("body changed:\n%s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: message_start\ndata: {}\n\n")
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream.URL, 1024)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages?capture=1", strings.NewReader(requestBody))
	request.Header.Set("x-api-key", "downstream-key")
	request.Header.Set("User-Agent", "claude-cli/2.1.215")
	request.Header.Set("anthropic-beta", "oauth-2025-04-20")
	server.routes().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := recorder.Body.String(); got != "event: message_start\ndata: {}\n\n" {
		t.Fatalf("response body = %q", got)
	}
	records := server.metrics.Recent(1)
	if len(records) != 1 {
		t.Fatalf("request records = %d, want 1", len(records))
	}
	if records[0].ClientClass != clientClassAmbiguous || records[0].RelayAction != "unchanged" {
		t.Errorf("client observation = class %q action %q", records[0].ClientClass, records[0].RelayAction)
	}
	if records[0].ClientEvidence == nil || records[0].ClientEvidence.StructuredMetadata {
		t.Errorf("client evidence = %#v", records[0].ClientEvidence)
	}
}

func TestOfficialIngressRejectsNonClaudeCodeShape(t *testing.T) {
	t.Parallel()
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 1024)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("x-api-key", "official-downstream-key")
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body = %s", recorder.Code, recorder.Body.String())
	}
	if upstreamCalls != 0 {
		t.Fatalf("official gate allowed %d upstream calls", upstreamCalls)
	}
	records := server.metrics.Recent(1)
	if len(records) != 1 || records[0].Ingress != store.AccountPoolOfficial ||
		records[0].ClientClass != clientClassCompatible || records[0].Account != "" {
		t.Fatalf("rejected request record = %#v", records)
	}
}

// The compatible ingress is fenced: an official-pool account must never serve a
// request that did not come through the official key.
func TestCompatibleIngressNeverSelectsOfficialPoolAccounts(t *testing.T) {
	t.Parallel()
	var authorizations []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	importTestAccount(t, server.store, "shared", "token-shared", "22222222-2222-4222-8222-222222222222", "b")
	if _, err := server.store.SetAccountPool(t.Context(), "default", store.AccountPoolOfficial); err != nil {
		t.Fatal(err)
	}

	for _, session := range []string{"one", "two", "three"} {
		request := httptest.NewRequest(http.MethodPost, "/v1/messages",
			strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"hi"}]}`))
		request.Header.Set("x-api-key", "downstream-key")
		request.Header.Set(claudeCodeSessionHeader, session)
		recorder := httptest.NewRecorder()
		server.routes().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("compatible status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	}
	if len(authorizations) != 3 {
		t.Fatalf("upstream calls = %#v", authorizations)
	}
	for _, authorization := range authorizations {
		if authorization != "Bearer token-shared" {
			t.Fatalf("compatible ingress reached a fenced account: %#v", authorizations)
		}
	}
	records := server.metrics.Recent(1)
	if len(records) != 1 || records[0].Ingress != store.AccountPoolCompatible {
		t.Fatalf("request record = %#v", records)
	}
}

// Pool permeability is one way, so the official ingress keeps the whole account
// set and does not need a dedicated official-pool account to be placed first.
func TestOfficialIngressDrawsFromTheCompatiblePool(t *testing.T) {
	t.Parallel()
	var authorizations []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	// The only account keeps its default compatible placement.
	server := newTestServer(t, upstream.URL, 4096)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("x-api-key", "official-downstream-key")
	request.Header.Set("User-Agent", "claude-cli/2.1.219")
	request.Header.Set(claudeCodeSessionHeader, "official-session")
	request.Header.Set("X-App", "cli")
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("official status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(authorizations) != 1 || authorizations[0] != "Bearer upstream-access-token" {
		t.Fatalf("official authorizations = %#v", authorizations)
	}
	records := server.metrics.Recent(1)
	if len(records) != 1 || records[0].Ingress != store.AccountPoolOfficial || records[0].Account != "default" {
		t.Fatalf("request record = %#v", records)
	}
}

// The account override is a selection hint, not a way around the fence.
func TestForcedAccountCannotEnterTheOfficialPoolFromCompatible(t *testing.T) {
	t.Parallel()
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	if _, err := server.store.SetAccountPool(t.Context(), "default", store.AccountPoolOfficial); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("x-api-key", "downstream-key")
	request.Header.Set(accountHeader, "default")
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable ||
		!strings.Contains(recorder.Body.String(), "cannot serve compatible traffic") {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if upstreamCalls != 0 {
		t.Fatalf("fenced override made %d upstream calls", upstreamCalls)
	}
}

// The reverse override is allowed, because official traffic may use any pool.
func TestForcedAccountFromOfficialIngressReachesTheCompatiblePool(t *testing.T) {
	t.Parallel()
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("x-api-key", "official-downstream-key")
	request.Header.Set("User-Agent", "claude-cli/2.1.219")
	request.Header.Set(claudeCodeSessionHeader, "official-session")
	request.Header.Set("X-App", "cli")
	request.Header.Set(accountHeader, "default")
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if upstreamCalls != 1 {
		t.Fatalf("official override made %d upstream calls", upstreamCalls)
	}
}

func TestFailedAlternateSelectionDoesNotDoubleCountAccount(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 1024)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("x-api-key", "downstream-key")
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)

	stat := server.metrics.AccountStats()["default"]
	if stat.Requests != 1 || stat.Failures != 1 {
		t.Fatalf("account stats = %#v, want one failed upstream attempt", stat)
	}
}

func TestForwardAddsStandardAnthropicHeadersWhenMissing(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Errorf("anthropic-version = %q", got)
		}
		if got := r.Header.Get("content-type"); got != "application/json" {
			t.Errorf("content-type = %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream.URL, 1024)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	request.Header.Del("content-type")
	request.Header.Set("x-api-key", "downstream-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestForwardPreservesClientAnthropicVersion(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("anthropic-version"); got != "client-version" {
			t.Errorf("anthropic-version = %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream.URL, 1024)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	request.Header.Set("x-api-key", "downstream-key")
	request.Header.Set("anthropic-version", "client-version")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestForwardTreatsUnknownBillingFieldsAsOpaque(t *testing.T) {
	t.Parallel()
	body := `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.215.574; cc_entrypoint=claude-desktop; cch=00000;"}],"messages":[]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		decoded := decodeBody(t, got)
		system := decoded["system"].([]any)
		if text := system[0].(map[string]any)["text"]; text != "x-anthropic-billing-header: cc_version=2.1.215.574; cc_entrypoint=claude-desktop; cch=00000;" {
			t.Fatalf("caller billing block changed: %q", text)
		}
		if _, ok := decoded["metadata"]; !ok {
			t.Fatal("minimum metadata was not added")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream.URL, 1024)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("x-api-key", "downstream-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestForwardAddsMinimumAttributionAndStableHeaderSession(t *testing.T) {
	t.Parallel()
	requestBody := `{"model":"claude-sonnet-5","system":"ordinary prompt","messages":[{"role":"user","content":"hello"}]}`
	var bodies [][]byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, got)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream.URL, 4096)
	for range 2 {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(requestBody))
		request.Header.Set("x-api-key", "downstream-key")
		request.Header.Set(claudeCodeSessionHeader, "chat-42")
		server.routes().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	}
	if len(bodies) != 2 {
		t.Fatalf("upstream requests = %d", len(bodies))
	}
	first := decodeBody(t, bodies[0])
	second := decodeBody(t, bodies[1])
	firstSystem := first["system"].([]any)
	billing := firstSystem[0].(map[string]any)["text"].(string)
	const expectedBilling = "x-anthropic-billing-header: cc_version=2.1.219.0a7; cc_entrypoint=claude-desktop-3p;"
	if billing != expectedBilling || strings.Contains(billing, "cch=") {
		t.Fatalf("billing attribution = %q", billing)
	}
	if got := firstSystem[1].(map[string]any)["text"]; got != "ordinary prompt" {
		t.Fatalf("original system = %#v", got)
	}
	firstUserID := decodeUserID(t, first)
	secondUserID := decodeUserID(t, second)
	if firstUserID != secondUserID {
		t.Fatalf("header-derived session was not stable: %#v != %#v", firstUserID, secondUserID)
	}
	if firstUserID.AccountUUID != "11111111-1111-4111-8111-111111111111" || firstUserID.DeviceID != strings.Repeat("a", 64) {
		t.Fatalf("metadata identity does not match credential: %#v", firstUserID)
	}
}

func TestForwardPreservesExistingMetadataWithoutCCH(t *testing.T) {
	t.Parallel()
	body := `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=custom; cc_entrypoint=cli;"}],"metadata":{"user_id":"caller-owned"},"messages":[]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != body {
			t.Fatalf("already-attributed body changed: %s", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("x-api-key", "downstream-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestAttributionTransformIsIdempotent(t *testing.T) {
	t.Parallel()
	cred := credential.Credential{
		AccountUUID: "11111111-1111-4111-8111-111111111111",
		DeviceID:    strings.Repeat("b", 64),
	}
	headers := http.Header{"X-Session-Id": []string{"stable-session"}}
	body := []byte(`{"system":[{"type":"text","text":"keep me"}],"messages":[]}`)
	first, changed, err := addSubscriptionAttribution(body, headers, cred, true, "")
	if err != nil || !changed {
		t.Fatalf("first transform: changed=%v err=%v", changed, err)
	}
	second, changed, err := addSubscriptionAttribution(first, headers, cred, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if changed || string(second) != string(first) {
		t.Fatalf("second transform was not idempotent: changed=%v\n%s\n%s", changed, first, second)
	}
}

func TestCountTokensAddsBillingWithoutMetadata(t *testing.T) {
	t.Parallel()
	requestBody := `{"model":"claude-haiku-4-5-20251001","messages":[{"role":"user","content":"hello"}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		body := decodeBody(t, got)
		if _, exists := body["metadata"]; exists {
			t.Fatalf("count_tokens request contains unsupported metadata: %s", got)
		}
		system := body["system"].([]any)
		if got := system[0].(map[string]any)["text"]; got != observedBillingAttribution {
			t.Fatalf("billing attribution = %#v", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"input_tokens":41}`)
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream.URL, 4096)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(requestBody))
	request.Header.Set("x-api-key", "downstream-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func decodeBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func decodeUserID(t *testing.T, body map[string]any) attributionUserID {
	t.Helper()
	metadata := body["metadata"].(map[string]any)
	var value attributionUserID
	if err := json.Unmarshal([]byte(metadata["user_id"].(string)), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestAuthenticationRejectsWrongKey(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "http://127.0.0.1:1", 1024)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	request.Header.Set("x-api-key", "wrong-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestRelayAndAdminAuthenticationAreSeparated(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "http://127.0.0.1:1", 1024)

	adminWithRelayKey := httptest.NewRecorder()
	adminRequest := httptest.NewRequest(http.MethodGet, "/admin/v1/accounts", nil)
	adminRequest.Header.Set("x-api-key", "downstream-key")
	server.routes().ServeHTTP(adminWithRelayKey, adminRequest)
	if adminWithRelayKey.Code != http.StatusUnauthorized {
		t.Fatalf("relay key on admin endpoint status = %d", adminWithRelayKey.Code)
	}

	relayWithAdminKey := httptest.NewRecorder()
	relayRequest := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	relayRequest.Header.Set("x-api-key", "admin-key")
	server.routes().ServeHTTP(relayWithAdminKey, relayRequest)
	if relayWithAdminKey.Code != http.StatusUnauthorized {
		t.Fatalf("admin key on relay endpoint status = %d", relayWithAdminKey.Code)
	}

	adminWithAdminKey := httptest.NewRecorder()
	authorizedAdminRequest := httptest.NewRequest(http.MethodGet, "/admin/v1/accounts", nil)
	authorizedAdminRequest.Header.Set("x-api-key", "admin-key")
	server.routes().ServeHTTP(adminWithAdminKey, authorizedAdminRequest)
	if adminWithAdminKey.Code != http.StatusOK {
		t.Fatalf("admin key on admin endpoint status = %d", adminWithAdminKey.Code)
	}
}

func TestWebUIIsPublicButManagementAPIStillRequiresAuthentication(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "http://127.0.0.1:1", 4096)

	page := httptest.NewRecorder()
	server.routes().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Claude Relay") {
		t.Fatalf("Web UI status = %d body = %s", page.Code, page.Body.String())
	}
	if got := page.Header().Get("Content-Security-Policy"); got == "" {
		t.Fatal("Web UI omitted Content-Security-Policy")
	}

	asset := httptest.NewRecorder()
	server.routes().ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	if asset.Code != http.StatusOK || !strings.Contains(asset.Body.String(), "claudeRelayAdminKey") {
		t.Fatalf("Web UI asset status = %d", asset.Code)
	}

	admin := httptest.NewRecorder()
	server.routes().ServeHTTP(admin, httptest.NewRequest(http.MethodGet, "/admin/v1/accounts", nil))
	if admin.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated management status = %d", admin.Code)
	}
}

func TestRequestBodyLimit(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "http://127.0.0.1:1", 4)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"too":"large"}`))
	request.Header.Set("x-api-key", "downstream-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestForcedAccountHeaderSelectsAliasAndIsNotForwarded(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token-secondary" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get(accountHeader); got != "" {
			t.Errorf("private routing header leaked upstream: %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	importTestAccount(t, server.store, "secondary", "token-secondary", "22222222-2222-4222-8222-222222222222", "b")

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("x-api-key", "downstream-key")
	request.Header.Set(accountHeader, "secondary")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get(accountHeader); got != "secondary" {
		t.Fatalf("selected account response header = %q", got)
	}
}

func TestStickySessionKeepsSelectedAccount(t *testing.T) {
	t.Parallel()
	var authorizations []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	importTestAccount(t, server.store, "secondary", "token-secondary", "22222222-2222-4222-8222-222222222222", "b")
	body := `{"model":"claude-test","messages":[{"role":"user","content":"hello"}]}`
	for range 2 {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		request.Header.Set("x-api-key", "downstream-key")
		request.Header.Set("X-Claude-Session-Id", "conversation-1")
		server.routes().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	}
	if len(authorizations) != 2 || authorizations[0] != authorizations[1] {
		t.Fatalf("sticky session accounts = %#v", authorizations)
	}
}

func TestStickyOverloadReturns429WithoutSwitchingAccount(t *testing.T) {
	t.Parallel()
	var authorizations []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	server.selector.maxInflightPerAcct = 1
	importTestAccount(t, server.store, "secondary", "token-secondary", "22222222-2222-4222-8222-222222222222", "b")
	body := `{"model":"claude-test","messages":[{"role":"user","content":"sticky overload"}]}`
	headers := http.Header{"X-Claude-Session-Id": []string{"sticky-overload"}}
	route, err := deriveRequestRoute([]byte(body), headers, store.AccountPoolCompatible)
	if err != nil {
		t.Fatal(err)
	}
	primary, found, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil || !found {
		t.Fatalf("primary account = %#v found=%v err=%v", primary, found, err)
	}
	if err := server.store.Bind(t.Context(), route.ConversationKey, primary.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	holdPrimary := server.load.reserve(primary.ID)

	send := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		request.Header.Set("x-api-key", "downstream-key")
		request.Header.Set("X-Claude-Session-Id", "sticky-overload")
		server.routes().ServeHTTP(recorder, request)
		return recorder
	}
	first := send()
	if first.Code != http.StatusTooManyRequests || len(authorizations) != 0 {
		t.Fatalf("sticky overload status=%d authorizations=%#v body=%s", first.Code, authorizations, first.Body.String())
	}
	bound, found, err := server.store.BoundAccount(t.Context(), route.ConversationKey, store.AccountPoolCompatible, time.Now())
	if err != nil || !found || bound.ID != primary.ID {
		t.Fatalf("temporary fallback changed sticky binding: account=%#v found=%v err=%v", bound, found, err)
	}
	holdPrimary()

	second := send()
	if second.Code != http.StatusOK || len(authorizations) != 1 || authorizations[0] != "Bearer upstream-access-token" {
		t.Fatalf("sticky recovery status=%d authorizations=%#v body=%s", second.Code, authorizations, second.Body.String())
	}
	records := server.metrics.Recent(2)
	if len(records) != 2 || records[0].Selection != "sticky" || records[1].Status != http.StatusTooManyRequests || records[1].Account != "" {
		t.Fatalf("routing records = %#v", records)
	}
}

func TestUnboundRequestUsesLeastLoadedAccount(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token-secondary" {
			t.Errorf("least-loaded account authorization = %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	importTestAccount(t, server.store, "secondary", "token-secondary", "22222222-2222-4222-8222-222222222222", "b")
	primary, found, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil || !found {
		t.Fatalf("primary account = %#v found=%v err=%v", primary, found, err)
	}
	holdPrimary := server.load.reserve(primary.ID)
	defer holdPrimary()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"unbound load"}]}`))
	request.Header.Set("x-api-key", "downstream-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	records := server.metrics.Recent(1)
	if len(records) != 1 || records[0].Selection != "load_balance" || records[0].Account != "secondary" {
		t.Fatalf("load-balanced record = %#v", records)
	}
}

func TestForcedAccountDoesNotBypassWhenOverloaded(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-access-token" {
			t.Errorf("forced account authorization = %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	server.selector.maxInflightPerAcct = 1
	importTestAccount(t, server.store, "secondary", "token-secondary", "22222222-2222-4222-8222-222222222222", "b")
	primary, found, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil || !found {
		t.Fatalf("primary account = %#v found=%v err=%v", primary, found, err)
	}
	holdPrimary := server.load.reserve(primary.ID)
	defer holdPrimary()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"forced overload"}]}`))
	request.Header.Set("x-api-key", "downstream-key")
	request.Header.Set(accountHeader, "default")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestCooldownPolicyRequiresExplicitWindowExhaustion(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)

	if got, ok := retryAfterHeader("1.5", now); !ok || got != 1500*time.Millisecond {
		t.Errorf("fractional Retry-After = %v, %v", got, ok)
	}
	date := now.Add(45 * time.Second).Format(http.TimeFormat)
	if got, ok := retryAfterHeader(date, now); !ok || got != 45*time.Second {
		t.Errorf("date Retry-After = %v, %v", got, ok)
	}

	headers := http.Header{}
	headers.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(now.Add(5*time.Minute).Unix(), 10))
	headers.Set("anthropic-ratelimit-unified-5h-utilization", "0.31")
	decision, ok := cooldownDecisionForResponse(headers, http.StatusTooManyRequests, "claude-test", now)
	if !ok || decision.reason != cooldownReasonAmbiguous429 || decision.model != "claude-test" || !decision.until.Equal(now.Add(ambiguous429Cooldown)) {
		t.Fatalf("ambiguous 429 decision = %#v ok=%v", decision, ok)
	}
	headers.Set("anthropic-ratelimit-unified-5h-status", "rejected")
	decision, ok = cooldownDecisionForResponse(headers, http.StatusTooManyRequests, "claude-test", now)
	if !ok || decision.reason != cooldownReasonFiveHourExhausted || decision.model != "" || !decision.until.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("exhausted 5h decision = %#v ok=%v", decision, ok)
	}
	if _, ok := cooldownDecisionForResponse(headers, 529, "claude-test", now); ok {
		t.Fatal("529 persisted an account cooldown")
	}
	if _, ok := cooldownDecisionForResponse(headers, http.StatusInternalServerError, "claude-test", now); ok {
		t.Fatal("500 persisted an account cooldown")
	}
}

func TestRetryableResponseSwitchesAccountOnce(t *testing.T) {
	t.Parallel()
	var firstAuthorization string
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			firstAuthorization = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if got := r.Header.Get("Authorization"); got == firstAuthorization {
			t.Errorf("retry reused first account: %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	importTestAccount(t, server.store, "secondary", "token-secondary", "22222222-2222-4222-8222-222222222222", "b")
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"retry me"}]}`))
	request.Header.Set("x-api-key", "downstream-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || calls != 2 {
		t.Fatalf("status = %d calls = %d body = %s", recorder.Code, calls, recorder.Body.String())
	}
}

func TestGenericUpstreamFailuresFailOverWithoutCoolingAccounts(t *testing.T) {
	t.Parallel()
	statuses := []int{529, http.StatusInternalServerError}
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(statuses[calls])
		calls++
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	importTestAccount(t, server.store, "secondary", "token-secondary", "22222222-2222-4222-8222-222222222222", "b")

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"retry me"}]}`))
	request.Header.Set("x-api-key", "downstream-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusInternalServerError || calls != 2 {
		t.Fatalf("status=%d calls=%d body=%s", recorder.Code, calls, recorder.Body.String())
	}
	cooldowns, err := server.store.ActiveCooldowns(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(cooldowns) != 0 {
		t.Fatalf("generic upstream failures persisted cooldowns: %#v", cooldowns)
	}
}

func TestAmbiguous429FailsOverWithShortModelCooldown(t *testing.T) {
	t.Parallel()
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	importTestAccount(t, server.store, "secondary", "token-secondary", "22222222-2222-4222-8222-222222222222", "b")

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"retry me"}]}`))
	request.Header.Set("x-api-key", "downstream-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || calls != 2 {
		t.Fatalf("status=%d calls=%d body=%s", recorder.Code, calls, recorder.Body.String())
	}
	cooldowns, err := server.store.ActiveCooldowns(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(cooldowns) != 1 || cooldowns[0].Model != "claude-test" || cooldowns[0].Reason != cooldownReasonAmbiguous429 {
		t.Fatalf("ambiguous 429 cooldowns = %#v", cooldowns)
	}
	remaining := time.Until(time.Unix(cooldowns[0].UntilAt, 0))
	if remaining <= 0 || remaining > ambiguous429Cooldown {
		t.Fatalf("ambiguous 429 remaining cooldown = %v", remaining)
	}
}

func TestExplicitFiveHourExhaustionCoolsAccountAcrossModels(t *testing.T) {
	t.Parallel()
	var calls int
	var firstAuthorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			firstAuthorization = r.Header.Get("Authorization")
			w.Header().Set("anthropic-ratelimit-unified-5h-status", "rejected")
			w.Header().Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	importTestAccount(t, server.store, "secondary", "token-secondary", "22222222-2222-4222-8222-222222222222", "b")

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"retry me"}]}`))
	request.Header.Set("x-api-key", "downstream-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || calls != 2 {
		t.Fatalf("status=%d calls=%d body=%s", recorder.Code, calls, recorder.Body.String())
	}
	alias := "secondary"
	if firstAuthorization == "Bearer upstream-access-token" {
		alias = "default"
	}
	account, found, err := server.store.AccountByAlias(t.Context(), alias)
	if err != nil || !found {
		t.Fatalf("first account %q found=%v err=%v", alias, found, err)
	}
	cooling, err := server.store.IsCooling(t.Context(), account.ID, "claude-other-model", time.Now())
	if err != nil || !cooling {
		t.Fatalf("cross-model cooling=%v err=%v", cooling, err)
	}
}

func TestUnknownBillingFieldsDoNotOverrideForcedAccount(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token-secondary" {
			t.Errorf("Authorization = %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	importTestAccount(t, server.store, "secondary", "token-secondary", "22222222-2222-4222-8222-222222222222", "b")
	body := `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=x; cc_entrypoint=cli; cch=abcde;"}],"metadata":{"user_id":"{\"account_uuid\":\"11111111-1111-4111-8111-111111111111\",\"session_id\":\"s\"}"},"messages":[]}`
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("x-api-key", "downstream-key")
	request.Header.Set(accountHeader, "secondary")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestRoutingPrefixIgnoresContentAfterCacheBreakpoint(t *testing.T) {
	t.Parallel()
	headers := http.Header{}
	first, err := deriveRequestRoute([]byte(`{"model":"m","system":[{"type":"text","text":"stable","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"one"}]}`), headers, store.AccountPoolCompatible)
	if err != nil {
		t.Fatal(err)
	}
	second, err := deriveRequestRoute([]byte(`{"model":"m","system":[{"type":"text","text":"stable","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"different tail"}]}`), headers, store.AccountPoolCompatible)
	if err != nil {
		t.Fatal(err)
	}
	if first.SelectionKey != second.SelectionKey {
		t.Fatalf("cache prefix keys differ: %q != %q", first.SelectionKey, second.SelectionKey)
	}
}

func TestAdminAccountLifecycleDoesNotExposeTokens(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "http://127.0.0.1:1", 4096)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/admin/v1/accounts", nil)
	request.Header.Set("x-api-key", "admin-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "upstream-access-token") || strings.Contains(recorder.Body.String(), "refresh_token\":\"") {
		t.Fatalf("account response exposed token material: %s", recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/admin/v1/accounts/default/disable", nil)
	request.Header.Set("x-api-key", "admin-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"enabled":false`) {
		t.Fatalf("disable status = %d body = %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m","messages":[]}`))
	request.Header.Set("x-api-key", "downstream-key")
	request.Header.Set(accountHeader, "default")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled forced account status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestOAuthManagementFlowImportsDisabledAccount(t *testing.T) {
	t.Parallel()
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"oauth-access","refresh_token":"oauth-refresh","expires_in":28800,"account":{"uuid":"33333333-3333-4333-8333-333333333333","email_address":"oauth@example.com"}}`)
	}))
	defer tokenServer.Close()
	server := newTestServer(t, "http://127.0.0.1:1", 4096)
	oauthClient := claudeoauth.NewForTest(tokenServer.Client(), "https://example.test/authorize", tokenServer.URL, "https://example.test/callback")
	server.oauth = oauthClient
	server.tokens.oauth = oauthClient

	startRecorder := httptest.NewRecorder()
	startRequest := httptest.NewRequest(http.MethodPost, "/admin/v1/oauth/claude/start", strings.NewReader(`{"alias":"oauth-account"}`))
	startRequest.Header.Set("x-api-key", "admin-key")
	server.routes().ServeHTTP(startRecorder, startRequest)
	if startRecorder.Code != http.StatusOK {
		t.Fatalf("start status = %d body = %s", startRecorder.Code, startRecorder.Body.String())
	}
	var started struct {
		AuthorizationURL string `json:"authorization_url"`
		SessionID        string `json:"session_id"`
	}
	if err := json.Unmarshal(startRecorder.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	parsedURL, err := url.Parse(started.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	state := parsedURL.Query().Get("state")
	exchangeBody, _ := json.Marshal(map[string]string{"session_id": started.SessionID, "code": "auth-code#" + state})
	exchangeRecorder := httptest.NewRecorder()
	exchangeRequest := httptest.NewRequest(http.MethodPost, "/admin/v1/oauth/claude/exchange", strings.NewReader(string(exchangeBody)))
	exchangeRequest.Header.Set("x-api-key", "admin-key")
	server.routes().ServeHTTP(exchangeRecorder, exchangeRequest)
	if exchangeRecorder.Code != http.StatusCreated || !strings.Contains(exchangeRecorder.Body.String(), `"enabled":false`) {
		t.Fatalf("exchange status = %d body = %s", exchangeRecorder.Code, exchangeRecorder.Body.String())
	}
	account, found, err := server.store.AccountByAlias(t.Context(), "oauth-account")
	if err != nil || !found || account.Enabled || account.RefreshToken != "oauth-refresh" {
		t.Fatalf("OAuth account = %#v found=%v err=%v", account, found, err)
	}
}

func TestTokenManagerPersistsRotatedRefreshToken(t *testing.T) {
	t.Parallel()
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":28800}`)
	}))
	defer tokenServer.Close()
	server := newTestServer(t, "http://127.0.0.1:1", 4096)
	account, err := server.store.ImportAccount(t.Context(), "refreshing", credential.Credential{
		Type:         "claude",
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Minute).Format(time.RFC3339),
		AccountUUID:  "44444444-4444-4444-8444-444444444444",
		DeviceID:     strings.Repeat("d", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	account, err = server.store.SetAccountEnabled(t.Context(), account.Alias, true)
	if err != nil {
		t.Fatal(err)
	}
	oauthClient := claudeoauth.NewForTest(tokenServer.Client(), "https://example.test/authorize", tokenServer.URL, "https://example.test/callback")
	manager := tokenManager{store: server.store, oauth: oauthClient, autoRefresh: true}
	updated, err := manager.ensureFresh(t.Context(), account)
	if err != nil {
		t.Fatal(err)
	}
	if updated.AccessToken != "new-access" || updated.RefreshToken != "new-refresh" {
		t.Fatalf("updated account tokens were not rotated")
	}
	persisted, _, err := server.store.AccountByID(t.Context(), account.ID)
	if err != nil || persisted.RefreshToken != "new-refresh" {
		t.Fatalf("persisted account refresh token was not rotated: err=%v", err)
	}
}

func newTestServer(t *testing.T, upstreamURL string, maxRequestBytes int64) *Server {
	t.Helper()
	database, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	_, err = database.ImportAccount(t.Context(), "default", credential.Credential{
		Type:        "claude",
		AccessToken: "upstream-access-token",
		AccountUUID: "11111111-1111-4111-8111-111111111111",
		DeviceID:    strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.SetAccountEnabled(t.Context(), "default", true); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(config.Config{
		Listen:          "127.0.0.1:0",
		RelayAPIKey:     "downstream-key",
		OfficialAPIKey:  "official-downstream-key",
		AdminAPIKey:     "admin-key",
		CredentialsFile: "unused.json",
		UpstreamBaseURL: upstreamURL,
		MaxRequestBytes: maxRequestBytes,
		RequestLogSize:  50,
		AutoRefresh:     true,
	}, database)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func importTestAccount(t *testing.T, database *store.Store, alias, token, accountUUID, deviceByte string) {
	t.Helper()
	_, err := database.ImportAccount(t.Context(), alias, credential.Credential{
		Type:        "claude",
		AccessToken: token,
		AccountUUID: accountUUID,
		DeviceID:    strings.Repeat(deviceByte, 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.SetAccountEnabled(t.Context(), alias, true); err != nil {
		t.Fatal(err)
	}
}
