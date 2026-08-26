package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/local/claude-relay/internal/store"
)

func TestAvailabilityReportsIngressRoutingWithoutAccountDetails(t *testing.T) {
	server := newTestServer(t, "http://127.0.0.1:1", 4096)
	server.cfg.AvailabilityAPIKey = "availability-key"

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/ops/v1/availability", nil)
	request.Header.Set("Authorization", "Bearer availability-key")
	server.routes().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("availability status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response availabilityResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Availability[store.AccountPoolCompatible] || !response.Availability[store.AccountPoolOfficial] {
		t.Fatalf("availability=%v, want both ingresses available through the compatible pool", response.Availability)
	}
	if strings.Contains(recorder.Body.String(), "default") || strings.Contains(recorder.Body.String(), "upstream-access-token") {
		t.Fatalf("availability response exposed account details: %s", recorder.Body.String())
	}

	if _, err := server.store.SetAccountPool(t.Context(), "default", store.AccountPoolOfficial); err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/ops/v1/availability", nil)
	request.Header.Set("Authorization", "Bearer availability-key")
	server.routes().ServeHTTP(recorder, request)
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Availability[store.AccountPoolCompatible] || !response.Availability[store.AccountPoolOfficial] {
		t.Fatalf("availability=%v, want official ingress alone to reach the official pool", response.Availability)
	}
}

func TestAvailabilityIgnoresModelScoped429CooldownButHonorsAccountExclusion(t *testing.T) {
	server := newTestServer(t, "http://127.0.0.1:1", 4096)
	server.cfg.AvailabilityAPIKey = "availability-key"
	account, found, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil || !found {
		t.Fatalf("default account found=%v err=%v", found, err)
	}

	if err := server.store.Cooldown(t.Context(), account.ID, "claude-test", time.Now().Add(time.Minute), cooldownReasonRetryAfter429); err != nil {
		t.Fatal(err)
	}
	read := func() availabilityResponse {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/ops/v1/availability", nil)
		request.Header.Set("x-api-key", "availability-key")
		server.routes().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("availability status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		var response availabilityResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	if response := read(); !response.Availability[store.AccountPoolCompatible] {
		t.Fatalf("model-scoped 429 made ingress unavailable: %v", response.Availability)
	}

	if err := server.store.Cooldown(t.Context(), account.ID, "", time.Now().Add(time.Minute), "oauth_refresh_failed"); err != nil {
		t.Fatal(err)
	}
	if response := read(); response.Availability[store.AccountPoolCompatible] || response.Availability[store.AccountPoolOfficial] {
		t.Fatalf("account-wide exclusion left ingress available: %v", response.Availability)
	}
}

func TestAvailabilityRequiresDedicatedKey(t *testing.T) {
	server := newTestServer(t, "http://127.0.0.1:1", 4096)
	server.cfg.AvailabilityAPIKey = "availability-key"

	for _, key := range []string{"", "admin-key", "downstream-key"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/ops/v1/availability", nil)
		request.Header.Set("x-api-key", key)
		server.routes().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("key %q status=%d, want 401", key, recorder.Code)
		}
	}
}
