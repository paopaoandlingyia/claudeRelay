package proxy

import (
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
	requestBody := "{\n  \"model\": \"claude-test\",\n  \"messages\": []\n}"
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

func TestForwardSignsExistingBillingBlock(t *testing.T) {
	t.Parallel()
	body := `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.215.574; cc_entrypoint=claude-desktop; cch=00000;"}],"messages":[]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) == body || strings.Contains(string(got), "cch=00000;") {
			t.Fatalf("body was not signed: %s", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream.URL, 1024)
	server.cfg.SignExistingCCH = true
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("x-api-key", "downstream-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
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
	}, credential.Credential{Type: "claude", AccessToken: "upstream-access-token"})
	if err != nil {
		t.Fatal(err)
	}
	return server
}
