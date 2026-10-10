package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/local/claude-relay/internal/store"
)

type accountExitKey struct{}

func withAccountExit(ctx context.Context, account store.Account) context.Context {
	return context.WithValue(ctx, accountExitKey{}, account)
}

type accountTransportEntry struct {
	mode, address string
	transport     *http.Transport
}

// One connection pool per account; replacing an exit closes only idle connections.
// Requests already in progress retain their selected exit until completion.
type accountTransport struct {
	global    *http.Transport
	globalURL string
	mu        sync.Mutex
	accounts  map[int64]accountTransportEntry
}

func (t *accountTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	account, present := request.Context().Value(accountExitKey{}).(store.Account)
	if !present {
		return nil, fmt.Errorf("account exit is required for upstream requests")
	}
	if err := store.ValidateAccountProxy(account.ProxyMode, account.ProxyURL); err != nil {
		return nil, err
	}
	if account.ProxyMode == "global" {
		return t.roundTripGlobal(request)
	}
	t.mu.Lock()
	entry, found := t.accounts[account.ID]
	if account.ID == 0 || !found || entry.mode != account.ProxyMode || entry.address != account.ProxyURL {
		if found && account.ID != 0 {
			entry.transport.CloseIdleConnections()
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil // Direct must ignore HTTP_PROXY, HTTPS_PROXY and NO_PROXY.
		if account.ProxyMode == "custom" {
			address, err := url.Parse(account.ProxyURL)
			if err != nil {
				t.mu.Unlock()
				return nil, fmt.Errorf("invalid account proxy URL")
			}
			transport.Proxy = http.ProxyURL(address)
		}
		entry = accountTransportEntry{account.ProxyMode, account.ProxyURL, transport}
		if account.ID != 0 {
			t.accounts[account.ID] = entry
		}
	}
	t.mu.Unlock()
	// OAuth sessions have no account ID yet; their short-lived pools are never cached.
	if account.ID == 0 {
		defer entry.transport.CloseIdleConnections()
	}
	response, err := entry.transport.RoundTrip(request)
	if err != nil {
		// net/http errors can include the proxy URL. Preserve the diagnostic while removing credentials.
		if account.ProxyURL != "" {
			err = redactProxyError(err, account.ProxyURL)
		}
	}
	return response, err
}

func (t *accountTransport) roundTripGlobal(request *http.Request) (*http.Response, error) {
	response, err := t.global.RoundTrip(request)
	if err != nil {
		address := t.globalURL
		// Global inheritance also supports Go's HTTP_PROXY/HTTPS_PROXY settings.
		// Resolve the actual address to redact environment-supplied credentials too.
		if t.global.Proxy != nil {
			proxyURL, proxyErr := t.global.Proxy(request)
			if proxyErr == nil && proxyURL != nil {
				address = proxyURL.String()
			}
		}
		err = redactProxyError(err, address)
	}
	return response, err
}

type proxyDiagnosticError struct {
	cause   error
	message string
}

func (e *proxyDiagnosticError) Error() string { return e.message }
func (e *proxyDiagnosticError) Unwrap() error { return e.cause }

func redactProxyError(err error, address string) error {
	parsed, parseErr := url.Parse(address)
	if parseErr != nil || parsed.User == nil {
		return err
	}
	message := strings.ReplaceAll(err.Error(), address, store.ProxyDisplay(address))
	message = strings.ReplaceAll(message, parsed.User.String(), "[redacted]")
	if password, ok := parsed.User.Password(); ok && password != "" {
		message = strings.ReplaceAll(message, password, "[redacted]")
	}
	if username := parsed.User.Username(); username != "" {
		message = strings.ReplaceAll(message, username, "[redacted]")
	}
	return &proxyDiagnosticError{cause: err, message: message}
}

func proxyHasAuth(address string) bool {
	parsed, err := url.Parse(address)
	return err == nil && parsed.User != nil
}

func (t *accountTransport) forget(id int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if entry, found := t.accounts[id]; found {
		entry.transport.CloseIdleConnections()
		delete(t.accounts, id)
	}
}

func (t *accountTransport) CloseIdleConnections() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.global.CloseIdleConnections()
	for _, entry := range t.accounts {
		entry.transport.CloseIdleConnections()
	}
}

type accountProxyRequest struct {
	Mode string  `json:"mode"`
	URL  *string `json:"url,omitempty"` // Omitted URL preserves an existing custom secret.
}

func (s *Server) setAccountProxy(w http.ResponseWriter, r *http.Request) {
	account, found, err := s.store.AccountByAlias(r.Context(), r.PathValue("alias"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found_error", "account was not found")
		return
	}
	var input accountProxyRequest
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	address := ""
	if input.Mode == "custom" && input.URL == nil && account.ProxyMode == "custom" {
		address = account.ProxyURL
	}
	if input.URL != nil {
		address = *input.URL
	}
	updated, err := s.store.SetAccountProxy(r.Context(), account.Alias, input.Mode, address)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	s.exits.forget(account.ID)
	slog.Info("account exit changed", "account", account.Alias, "mode", updated.ProxyMode, "proxy", store.ProxyDisplay(updated.ProxyURL))
	writeJSON(w, http.StatusOK, accountResponse(updated))
}
