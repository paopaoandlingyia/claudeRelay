package claudeoauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestPKCEExchangeAndRotatingRefresh(t *testing.T) {
	t.Parallel()
	var grants []string
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		grant, _ := body["grant_type"].(string)
		grants = append(grants, grant)
		w.Header().Set("Content-Type", "application/json")
		if grant == "authorization_code" {
			if body["code_verifier"] == "" || body["state"] == "" || body["code"] != "auth-code" {
				t.Errorf("exchange body = %#v", body)
			}
			_, _ = w.Write([]byte(`{"access_token":"access-1","refresh_token":"refresh-1","expires_in":28800,"token_type":"bearer","scope":"user:inference","account":{"uuid":"11111111-1111-4111-8111-111111111111","email_address":"user@example.com"}}`))
			return
		}
		if body["refresh_token"] != "refresh-1" {
			t.Errorf("refresh body = %#v", body)
		}
		_, _ = w.Write([]byte(`{"access_token":"access-2","refresh_token":"refresh-2","expires_in":28800}`))
	}))
	defer tokenServer.Close()

	client := NewForTest(tokenServer.Client(), "https://example.test/authorize", tokenServer.URL, "https://example.test/callback")
	started, err := client.Start("primary")
	if err != nil {
		t.Fatal(err)
	}
	authURL, err := url.Parse(started.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	state := authURL.Query().Get("state")
	if state == "" || authURL.Query().Get("code_challenge") == "" {
		t.Fatalf("authorization URL = %s", started.AuthorizationURL)
	}
	alias, cred, err := client.Exchange(context.Background(), started.SessionID, "auth-code#"+state)
	if err != nil {
		t.Fatal(err)
	}
	if alias != "primary" || cred.AccessToken != "access-1" || cred.RefreshToken != "refresh-1" || cred.AccountUUID == "" || cred.DeviceID == "" {
		t.Fatalf("exchange result alias=%q credential=%#v", alias, cred)
	}
	refreshed, err := client.Refresh(context.Background(), cred.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.AccessToken != "access-2" || refreshed.RefreshToken != "refresh-2" {
		t.Fatalf("refresh result = %#v", refreshed)
	}
	if strings.Join(grants, ",") != "authorization_code,refresh_token" {
		t.Fatalf("grants = %#v", grants)
	}
}

func TestExchangeRejectsMismatchedStateBeforeTokenRequest(t *testing.T) {
	t.Parallel()
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client := NewForTest(server.Client(), "https://example.test/authorize", server.URL, "https://example.test/callback")
	started, err := client.Start("primary")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.Exchange(context.Background(), started.SessionID, "code#wrong-state"); err == nil {
		t.Fatal("Exchange accepted mismatched state")
	}
	if called {
		t.Fatal("token endpoint was called for mismatched state")
	}
}
