package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/local/claude-relay/internal/claudeoauth"
	"github.com/local/claude-relay/internal/store"
)

func TestOAuthReauthorization(t *testing.T) {
	for _, tc := range []struct {
		name              string
		enabled           bool
		uuid              string
		tokenStatus, want int
		removed           bool
	}{
		{"enabled", true, "11111111-1111-4111-8111-111111111111", 200, 200, false},
		{"disabled", false, "11111111-1111-4111-8111-111111111111", 200, 200, false},
		{"wrong account", true, "22222222-2222-4222-8222-222222222222", 200, 409, false},
		{"exchange failure", true, "", 401, 400, false},
		{"removed during exchange", true, "11111111-1111-4111-8111-111111111111", 200, 409, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newTestServer(t, "https://upstream.invalid", 1024)
			called := false
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				if tc.removed {
					if _, err := server.store.DeleteAccount(t.Context(), "renamed"); err != nil {
						t.Error(err)
					}
				}
				if r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" {
					t.Error("saved proxy authentication was not used")
				}
				if r.URL.Host != "token.invalid" {
					t.Errorf("unexpected token destination: %s", r.URL)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.tokenStatus)
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new-access", "refresh_token": "new-refresh", "expires_in": 28800, "account": map[string]string{"uuid": tc.uuid}})
			}))
			defer proxy.Close()
			proxyURL := strings.Replace(proxy.URL, "http://", "http://user:pass@", 1)
			if _, err := server.store.SetAccountProxy(t.Context(), "default", "custom", proxyURL); err != nil {
				t.Fatal(err)
			}
			if _, err := server.store.SetAccountEnabled(t.Context(), "default", tc.enabled); err != nil {
				t.Fatal(err)
			}
			if _, err := server.store.SetAccountPool(t.Context(), "default", store.AccountPoolOfficial); err != nil {
				t.Fatal(err)
			}
			before, _, err := server.store.AccountByAlias(t.Context(), "default")
			if err != nil {
				t.Fatal(err)
			}
			transport := &accountTransport{accounts: make(map[int64]accountTransportEntry)}
			defer func() {
				for _, entry := range transport.accounts {
					entry.transport.CloseIdleConnections()
				}
			}()
			server.oauth = claudeoauth.NewForTest(&http.Client{Transport: transport}, "https://authorize.invalid", "http://token.invalid/token", "https://callback.invalid")
			started := adminRequest(t, server, "POST", "/admin/v1/oauth/claude/start", `{"alias":"default","reauthorize":true}`)
			if started.Code != 200 {
				t.Fatalf("start: %d %s", started.Code, started.Body.String())
			}
			if strings.Contains(started.Body.String(), "pass") {
				t.Fatal("proxy credentials exposed")
			}
			var session claudeoauth.StartResult
			if err := json.Unmarshal(started.Body.Bytes(), &session); err != nil {
				t.Fatal(err)
			}
			pending, err := server.oauth.PendingSession(session.SessionID)
			if err != nil || pending.AccountID != before.ID {
				t.Fatalf("session was not bound to account: %v", err)
			}
			// A rename while the browser is open must still update the original account ID.
			if _, err := server.store.RenameAccount(t.Context(), "default", "renamed"); err != nil {
				t.Fatal(err)
			}
			before.Alias = "renamed"
			body, _ := json.Marshal(map[string]string{"session_id": session.SessionID, "code": "code#" + pending.State})
			result := adminRequest(t, server, "POST", "/admin/v1/oauth/claude/exchange", string(body))
			if result.Code != tc.want {
				t.Fatalf("exchange: %d %s", result.Code, result.Body.String())
			}
			if !called {
				t.Fatal("saved proxy was not used")
			}
			after, found, err := server.store.AccountByID(t.Context(), before.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.removed {
				if found {
					t.Fatal("reauthorization recreated a removed account")
				}
				return
			}
			if tc.want == 200 {
				if after.AccessToken != "new-access" || after.RefreshToken != "new-refresh" || after.ExpiresAt == before.ExpiresAt || after.LastRefreshAt == 0 {
					t.Fatal("tokens were not updated")
				}
				after.AccessToken, after.RefreshToken, after.ExpiresAt = before.AccessToken, before.RefreshToken, before.ExpiresAt
				after.LastRefreshAt = before.LastRefreshAt
			}
			after.UpdatedAt = before.UpdatedAt
			if !reflect.DeepEqual(before, after) {
				t.Fatal("reauthorization changed unrelated account fields or replaced credentials on failure")
			}
		})
	}
}

func TestOAuthReauthorizationRejectsMissingAccountAndProxyOverride(t *testing.T) {
	server := newTestServer(t, "https://upstream.invalid", 1024)
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"alias":"missing","reauthorize":true}`, 404},
		{`{"alias":"default","reauthorize":true,"proxy_mode":"direct"}`, 400},
	} {
		result := adminRequest(t, server, "POST", "/admin/v1/oauth/claude/start", tc.body)
		if result.Code != tc.want {
			t.Fatalf("start: %d %s", result.Code, result.Body.String())
		}
	}
}
