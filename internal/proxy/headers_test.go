package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestCopyRequestHeadersBrowserPolicy(t *testing.T) {
	t.Parallel()
	source := http.Header{
		"Origin": {"chrome-extension://translator"},
		"Anthropic-Dangerous-Direct-Browser-Access": {"true"},
		"Authorization":            {"Bearer client-token"},
		"Content-Type":             {"application/json"},
		"Anthropic-Version":        {"2023-06-01"},
		"Anthropic-Beta":           {"oauth-2025-04-20", "messages-2023-12-15"},
		"User-Agent":               {"claude-cli/2.1.215"},
		"X-Claude-Code-Session-Id": {"client-session"},
		"X-App":                    {"cli"},
	}
	original := source.Clone()
	for _, policy := range []ingressPolicy{compatibleIngress, experimentalIngress, officialIngress} {
		t.Run(policy.Name, func(t *testing.T) {
			want := source.Clone()
			if policy.Name != ingressOfficial {
				want.Del("Origin")
				want.Del("anthropic-dangerous-direct-browser-access")
			}
			destination := make(http.Header)
			copyRequestHeaders(destination, source, policy)
			if !reflect.DeepEqual(destination, want) {
				t.Errorf("upstream headers = %#v, want %#v", destination, want)
			}
			if !reflect.DeepEqual(source, original) {
				t.Errorf("incoming headers changed: got %#v, want %#v", source, original)
			}
		})
	}
}

func TestCopyResponseHeadersPreservesCORSHeaders(t *testing.T) {
	t.Parallel()
	source := http.Header{
		"Content-Type":                  {"application/json"},
		"Access-Control-Allow-Origin":   {"chrome-extension://translator"},
		"Access-Control-Allow-Headers":  {"content-type, x-api-key"},
		"Access-Control-Allow-Methods":  {"POST, OPTIONS"},
		"Access-Control-Expose-Headers": {"x-request-id"},
	}
	destination := make(http.Header)

	copyResponseHeaders(destination, source)

	if !reflect.DeepEqual(destination, source) {
		t.Errorf("downstream headers = %#v, want %#v", destination, source)
	}
}

func TestForwardStripsBrowserOnlyHeaders(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name         string
		path         string
		stream       bool
		contentType  string
		responseBody string
	}{
		{
			name:         "messages JSON",
			path:         "/v1/messages",
			contentType:  "application/json",
			responseBody: `{"type":"message","model":"claude-test","content":[{"type":"text","text":"hello"}],"usage":{"input_tokens":3,"output_tokens":1}}`,
		},
		{
			name:        "messages SSE",
			path:        "/v1/messages",
			stream:      true,
			contentType: "text/event-stream",
			responseBody: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-test\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		},
		{
			name:         "count tokens",
			path:         "/v1/messages/count_tokens",
			contentType:  "application/json",
			responseBody: `{"input_tokens":3}`,
		},
	} {
		for _, key := range []string{"downstream-key", "experimental-downstream-key"} {
			t.Run(test.name+"/"+key, func(t *testing.T) {
				t.Parallel()
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Origin") != "" || r.Header.Get("anthropic-dangerous-direct-browser-access") != "" {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusUnauthorized)
						_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"CORS requests are not allowed for this Organization because of its settings."}}`)
						return
					}
					for header, want := range map[string]string{
						"Authorization":     "Bearer upstream-access-token",
						"x-api-key":         "",
						"anthropic-version": "2023-06-01",
						"anthropic-beta":    "messages-2023-12-15",
						"User-Agent":        "NextAI-Translator/0.6.19",
					} {
						if got := r.Header.Get(header); got != want {
							t.Errorf("upstream %s = %q, want %q", header, got, want)
						}
					}
					if r.URL.Path != test.path {
						t.Errorf("upstream path = %q, want %q", r.URL.Path, test.path)
					}
					w.Header().Set("Content-Type", test.contentType)
					w.Header().Set("Access-Control-Allow-Origin", "chrome-extension://translator")
					_, _ = io.WriteString(w, test.responseBody)
				}))
				defer upstream.Close()
				server := newTestServer(t, upstream.URL, 4096)
				body := `{"model":"claude-test","messages":[{"role":"user","content":"hello"}]`
				if test.stream {
					body += `,"stream":true`
				}
				request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(body+`}`))
				request.Header.Set("x-api-key", key)
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("anthropic-version", "2023-06-01")
				request.Header.Set("anthropic-beta", "messages-2023-12-15")
				request.Header.Set("User-Agent", "NextAI-Translator/0.6.19")
				request.Header.Set("Origin", "chrome-extension://translator")
				request.Header.Set("anthropic-dangerous-direct-browser-access", "true")
				recorder := httptest.NewRecorder()

				server.routes().ServeHTTP(recorder, request)

				if recorder.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
				}
				if got := recorder.Body.String(); got != test.responseBody {
					t.Errorf("response body = %q, want %q", got, test.responseBody)
				}
				if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "chrome-extension://translator" {
					t.Errorf("downstream CORS header = %q", got)
				}
			})
		}
	}
}

func TestOfficialForwardPreservesBrowserHeadersAndUpstreamRejection(t *testing.T) {
	t.Parallel()
	const rejection = `{"type":"error","error":{"type":"authentication_error","message":"CORS requests are not allowed for this Organization because of its settings."}}`
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			called := false
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				if r.Header.Get("Origin") != "chrome-extension://translator" || r.Header.Get("anthropic-dangerous-direct-browser-access") != "true" {
					t.Error("official ingress stripped browser headers")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, rejection)
			}))
			defer upstream.Close()
			server := newTestServer(t, upstream.URL, 4096)
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(officialClaudeCodeTestBody()))
			setOfficialClaudeCodeTestHeaders(request)
			request.Header.Set("x-api-key", "official-downstream-key")
			request.Header.Set("Origin", "chrome-extension://translator")
			request.Header.Set("anthropic-dangerous-direct-browser-access", "true")
			recorder := httptest.NewRecorder()
			server.routes().ServeHTTP(recorder, request)
			if !called || recorder.Code != http.StatusUnauthorized || recorder.Body.String() != rejection {
				t.Fatalf("upstream rejection not preserved: called=%v status=%d body=%s", called, recorder.Code, recorder.Body.String())
			}
		})
	}
}
