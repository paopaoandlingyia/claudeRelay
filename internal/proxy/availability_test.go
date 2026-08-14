package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/local/claude-relay/internal/store"
)

func TestAvailabilityUsesAuthenticatedIngressPool(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "http://127.0.0.1:1", 4096)

	if _, err := server.store.SetAccountEnabled(t.Context(), "default", false); err != nil {
		t.Fatal(err)
	}
	importTestAccount(t, server.store, "official-only", "official-token", "22222222-2222-4222-8222-222222222222", "b")
	if _, err := server.store.SetAccountPool(t.Context(), "official-only", store.AccountPoolOfficial); err != nil {
		t.Fatal(err)
	}

	assertStatus := func(key, want string) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/availability", nil)
		request.Header.Set("x-api-key", key)
		recorder := httptest.NewRecorder()
		server.routes().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		var response availabilityResponse
		if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
			t.Fatal(err)
		}
		if response.Status != want || response.ObservedAt == 0 || response.TTLSeconds != availabilityTTLSeconds {
			t.Fatalf("availability = %#v, want status %q", response, want)
		}
	}

	assertStatus("downstream-key", "unavailable")
	assertStatus("official-downstream-key", "available")
}

func TestAvailabilityRequiresRelayAuthentication(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "http://127.0.0.1:1", 4096)
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/availability", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
}
