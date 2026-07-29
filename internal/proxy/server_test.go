package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/local/claude-relay/internal/config"
	"github.com/local/claude-relay/internal/credential"
)

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

func TestForwardPreservesExistingCCHByteForByte(t *testing.T) {
	t.Parallel()
	body := `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.215.574; cc_entrypoint=claude-desktop; cch=00000;"}],"messages":[]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != body {
			t.Fatalf("signed client body changed:\n%s", got)
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
		request.Header.Set("X-Claude-Session-Id", "chat-42")
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
	if billing != observedBillingAttribution || strings.Contains(billing, "cch=") {
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
	if firstUserID.AccountUUID != server.credential.AccountUUID || firstUserID.DeviceID != server.credential.DeviceID {
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
	first, changed, err := addSubscriptionAttribution(body, headers, cred, true)
	if err != nil || !changed {
		t.Fatalf("first transform: changed=%v err=%v", changed, err)
	}
	second, changed, err := addSubscriptionAttribution(first, headers, cred, true)
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

func newTestServer(t *testing.T, upstreamURL string, maxRequestBytes int64) *Server {
	t.Helper()
	server, err := NewServer(config.Config{
		Listen:          "127.0.0.1:0",
		APIKey:          "downstream-key",
		CredentialsFile: "unused.json",
		UpstreamBaseURL: upstreamURL,
		MaxRequestBytes: maxRequestBytes,
	}, credential.Credential{
		Type:        "claude",
		AccessToken: "upstream-access-token",
		AccountUUID: "11111111-1111-4111-8111-111111111111",
		DeviceID:    strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	return server
}
