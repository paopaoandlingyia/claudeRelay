package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestIDMiddlewarePreservesValidClientID(t *testing.T) {
	t.Parallel()
	var contextID string
	handler := withRequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contextID = requestIDFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.Header.Set(requestIDHeader, "client-request_42")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if got := recorder.Header().Get(requestIDHeader); got != "client-request_42" {
		t.Fatalf("response request ID = %q", got)
	}
	if contextID != "client-request_42" {
		t.Fatalf("context request ID = %q", contextID)
	}
}

func TestRequestIDMiddlewareReplacesUnsafeClientID(t *testing.T) {
	t.Parallel()
	handler := withRequestID(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.Header.Set(requestIDHeader, "unsafe value")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	got := recorder.Header().Get(requestIDHeader)
	if got == "" || got == "unsafe value" || strings.Contains(got, " ") {
		t.Fatalf("generated request ID = %q", got)
	}
}

func TestRelayRequestIDIsReturnedButNotForwardedUpstream(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get(requestIDHeader); got != "" {
			t.Errorf("relay request ID leaked upstream: %q", got)
		}
		w.Header().Set(requestIDHeader, "upstream-value")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","messages":[]}`))
	request.Header.Set("x-api-key", "downstream-key")
	request.Header.Set(requestIDHeader, "caller-trace-7")
	recorder := httptest.NewRecorder()

	server.routes().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get(requestIDHeader); got != "caller-trace-7" {
		t.Fatalf("response request ID = %q", got)
	}
}
