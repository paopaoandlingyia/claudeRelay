package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/local/claude-relay/internal/claudeoauth"
	"github.com/local/claude-relay/internal/store"
)

func TestAccountExitRoutingAndFailure(t *testing.T) {
	var directCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directCalls.Add(1)
		io.WriteString(w, "direct")
	}))
	defer upstream.Close()
	makeProxy := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" {
				t.Errorf("proxy authentication missing")
			}
			io.WriteString(w, name)
		}))
	}
	first, second := makeProxy("first"), makeProxy("second")
	defer first.Close()
	defer second.Close()
	authURL := func(address string) string {
		u, _ := url.Parse(address)
		u.User = url.UserPassword("user", "pass")
		return u.String()
	}
	global := http.DefaultTransport.(*http.Transport).Clone()
	globalProxy, _ := url.Parse(authURL(first.URL))
	global.Proxy = http.ProxyURL(globalProxy)
	exits := &accountTransport{global: global, globalURL: globalProxy.String(), accounts: make(map[int64]accountTransportEntry)}
	defer exits.CloseIdleConnections()
	client := &http.Client{Transport: exits}
	// The default transport reads these variables; explicit direct must bypass them.
	t.Setenv("HTTP_PROXY", first.URL)
	t.Setenv("HTTPS_PROXY", first.URL)
	t.Setenv("NO_PROXY", "")
	call := func(account store.Account) (string, error) {
		req, _ := http.NewRequestWithContext(withAccountExit(t.Context(), account), "GET", upstream.URL, nil)
		response, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		return string(body), err
	}
	for _, test := range []struct {
		account store.Account
		want    string
	}{
		{store.Account{ID: 1, ProxyMode: "direct"}, "direct"},
		{store.Account{ID: 2, ProxyMode: "global"}, "first"},
		{store.Account{ID: 3, ProxyMode: "custom", ProxyURL: authURL(first.URL)}, "first"},
		{store.Account{ID: 4, ProxyMode: "custom", ProxyURL: authURL(second.URL)}, "second"},
		{store.Account{ID: 3, ProxyMode: "custom", ProxyURL: authURL(second.URL)}, "second"},
	} {
		got, err := call(test.account)
		if err != nil || got != test.want {
			t.Fatalf("mode=%s ID=%d got=%q err=%v", test.account.ProxyMode, test.account.ID, got, err)
		}
	}
	first.Close()
	for _, account := range []store.Account{{ID: 3, ProxyMode: "custom", ProxyURL: authURL(first.URL)}, {ID: 2, ProxyMode: "global"}} {
		if _, err := call(account); err == nil {
			t.Fatal("dead proxy silently succeeded")
		}
	}
	if directCalls.Load() != 1 {
		t.Fatalf("failed proxy reached direct upstream: %d calls", directCalls.Load())
	}
	if _, err := call(store.Account{ID: 5, ProxyMode: "invalid"}); err == nil {
		t.Fatal("invalid exit accepted")
	}
}

func TestAccountProxyHTTPSConnect(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy authentication leaked to upstream")
		}
		io.WriteString(w, "secure upstream")
	}))
	defer upstream.Close()
	var connects atomic.Int32
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" {
			io.WriteString(w, "warmup")
			return
		}
		connects.Add(1)
		if r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" {
			t.Error("CONNECT authentication missing")
		}
		target, err := net.Dial("tcp", r.Host)
		if err != nil {
			t.Error(err)
			w.WriteHeader(502)
			return
		}
		connection, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			target.Close()
			t.Error(err)
			return
		}
		defer connection.Close()
		defer target.Close()
		buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		buffer.Flush()
		done := make(chan struct{})
		go func() { io.Copy(target, buffer); target.Close(); close(done) }()
		io.Copy(connection, target)
		connection.Close()
		<-done
	}))
	defer proxyServer.Close()
	proxyURL, _ := url.Parse(proxyServer.URL)
	proxyURL.User = url.UserPassword("user", "pass")
	account := store.Account{ID: 1, ProxyMode: "custom", ProxyURL: proxyURL.String()}
	exits := &accountTransport{global: http.DefaultTransport.(*http.Transport).Clone(), accounts: make(map[int64]accountTransportEntry)}
	defer exits.CloseIdleConnections()
	client := &http.Client{Transport: exits}
	warmup, _ := http.NewRequestWithContext(withAccountExit(t.Context(), account), "GET", "http://warmup.invalid", nil)
	response, err := client.Do(warmup)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	// Trust this test server's certificate; production retains Go's system trust roots.
	exits.accounts[1].transport.TLSClientConfig = upstream.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	request, _ := http.NewRequestWithContext(withAccountExit(t.Context(), account), "GET", upstream.URL, nil)
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "secure upstream" || connects.Load() != 1 {
		t.Fatalf("HTTPS proxy body=%q err=%v CONNECTs=%d", body, err, connects.Load())
	}
}

func TestAccountProxyAdminAndAllRequestPaths(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	paths := map[string]int{}
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" {
			t.Errorf("proxy authentication missing")
		}
		mu.Lock()
		paths[r.URL.Path]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			io.WriteString(w, `{"access_token":"fresh","refresh_token":"refresh","expires_in":3600,"account":{"uuid":"33333333-3333-4333-8333-333333333333"}}`)
		case "/api/oauth/usage":
			io.WriteString(w, `{"five_hour":{"utilization":10}}`)
		case "/api/oauth/profile":
			io.WriteString(w, `{"account":{"has_claude_pro":true}}`)
		default:
			io.WriteString(w, `{"input_tokens":1,"usage":{"input_tokens":1,"output_tokens":1}}`)
		}
	}))
	defer proxyServer.Close()
	server := newTestServer(t, "http://upstream.invalid", 4096)
	defer server.exits.CloseIdleConnections()
	proxyURL, _ := url.Parse(proxyServer.URL)
	proxyURL.User = url.UserPassword("user", "pass")
	body, _ := json.Marshal(map[string]string{"mode": "custom", "url": proxyURL.String()})
	response := adminRequest(t, server, "POST", "/admin/v1/accounts/default/proxy", string(body))
	if response.Code != 200 || strings.Contains(response.Body.String(), "pass") || strings.Contains(response.Body.String(), "user") {
		t.Fatalf("proxy response status=%d body=%s", response.Code, response.Body.String())
	}
	response = adminRequest(t, server, "POST", "/admin/v1/accounts/default/proxy", `{"mode":"custom"}`)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	account, _, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil {
		t.Fatal(err)
	}
	if account.ProxyURL != proxyURL.String() {
		t.Fatal("omitted URL did not preserve authentication")
	}
	request := httptest.NewRequest("POST", "/v1/messages", nil)
	result, err := server.doUpstream(request, []byte(`{}`), account)
	if err != nil {
		t.Fatal(err)
	}
	result.Body.Close()
	server.oauth = claudeoauth.NewForTest(&http.Client{Transport: server.exits}, "https://authorize.invalid", "http://token.invalid/token", "https://callback.invalid")
	server.tokens.oauth = server.oauth
	if _, err := server.store.UpdateTokens(t.Context(), account.ID, "upstream-access-token", "refresh", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := server.tokens.refreshNow(t.Context(), account); err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct{ method, path string }{{"POST", "/admin/v1/accounts/default/check"}, {"POST", "/admin/v1/accounts/default/usage/refresh"}} {
		response = adminRequest(t, server, route.method, route.path, "")
		if response.Code != 200 {
			t.Fatal(response.Body.String())
		}
	}
	startBody, _ := json.Marshal(map[string]string{"alias": "new-oauth", "proxy_mode": "custom", "proxy_url": proxyURL.String()})
	response = adminRequest(t, server, "POST", "/admin/v1/oauth/claude/start", string(startBody))
	var start claudeoauth.StartResult
	if err := json.Unmarshal(response.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}
	response = adminRequest(t, server, "POST", "/admin/v1/oauth/claude/exchange", fmt.Sprintf(`{"session_id":%q,"code":"code"}`, start.SessionID))
	if response.Code != 201 {
		t.Fatalf("OAuth exchange status=%d body=%s", response.Code, response.Body.String())
	}
	imported, found, err := server.store.AccountByAlias(t.Context(), "new-oauth")
	if err != nil || !found || imported.ProxyURL != account.ProxyURL {
		t.Fatal("OAuth exit was not persisted")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens", "/api/oauth/usage", "/api/oauth/profile", "/token"} {
		if paths[path] == 0 {
			t.Fatalf("%s bypassed account proxy", path)
		}
	}
	if paths["/token"] != 2 {
		t.Fatalf("token endpoint calls=%d want refresh and exchange", paths["/token"])
	}
}

func TestProxyErrorsRedactCredentialsAndKeepCause(t *testing.T) {
	original := fmt.Errorf("connection to http://user:secret@proxy.invalid:12323 failed: secret")
	err := redactProxyError(original, "http://user:secret@proxy.invalid:12323")
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "user") {
		t.Fatal("proxy credentials leaked")
	}
	if err.(*proxyDiagnosticError).Unwrap() != original {
		t.Fatal("diagnostic cause lost")
	}
}
