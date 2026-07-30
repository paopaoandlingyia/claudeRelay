package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/local/claude-relay/internal/claudeoauth"
	"github.com/local/claude-relay/internal/config"
	"github.com/local/claude-relay/internal/metrics"
	"github.com/local/claude-relay/internal/store"
	"github.com/local/claude-relay/internal/webui"
)

var allowedPaths = map[string]struct{}{
	"/v1/messages":              {},
	"/v1/messages/count_tokens": {},
}

type Server struct {
	cfg        config.Config
	store      *store.Store
	selector   accountSelector
	upstream   *url.URL
	httpServer *http.Server
	client     *http.Client
	oauth      *claudeoauth.Client
	tokens     *tokenManager
	metrics    *metrics.Recorder
	startedAt  time.Time
}

func NewServer(cfg config.Config, database *store.Store) (*Server, error) {
	if database == nil {
		return nil, fmt.Errorf("account store is required")
	}
	upstream, err := url.Parse(cfg.UpstreamBaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse upstream URL: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.UpstreamProxy != "" {
		proxyURL, parseErr := url.Parse(cfg.UpstreamProxy)
		if parseErr != nil {
			return nil, fmt.Errorf("parse upstream proxy URL: %w", parseErr)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	oauthClient := claudeoauth.New(&http.Client{Transport: transport, Timeout: 60 * time.Second})
	server := &Server{
		cfg:       cfg,
		store:     database,
		selector:  accountSelector{store: database},
		upstream:  upstream,
		client:    &http.Client{Transport: transport},
		oauth:     oauthClient,
		metrics:   metrics.New(cfg.RequestLogSize),
		startedAt: time.Now(),
	}
	server.tokens = &tokenManager{store: database, oauth: oauthClient, autoRefresh: cfg.AutoRefresh}
	server.httpServer = &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	return server, nil
}

func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		slog.Info("relay listening", "address", s.cfg.Listen, "upstream", s.upstream.String())
		errCh <- s.httpServer.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", webui.Index)
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", webui.Assets()))
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("POST /v1/messages", s.forward)
	mux.HandleFunc("POST /v1/messages/count_tokens", s.forward)
	mux.HandleFunc("GET /admin/v1/overview", s.overview)
	mux.HandleFunc("GET /admin/v1/requests", s.listRequests)
	mux.HandleFunc("GET /admin/v1/accounts", s.listAccounts)
	mux.HandleFunc("POST /admin/v1/accounts/import", s.importAccount)
	mux.HandleFunc("DELETE /admin/v1/accounts/{alias}", s.deleteAccount)
	mux.HandleFunc("POST /admin/v1/accounts/{alias}/enable", func(w http.ResponseWriter, r *http.Request) { s.setAccountEnabled(w, r, true) })
	mux.HandleFunc("POST /admin/v1/accounts/{alias}/disable", func(w http.ResponseWriter, r *http.Request) { s.setAccountEnabled(w, r, false) })
	mux.HandleFunc("POST /admin/v1/accounts/{alias}/rename", s.renameAccount)
	mux.HandleFunc("POST /admin/v1/accounts/{alias}/refresh", s.refreshAccount)
	mux.HandleFunc("POST /admin/v1/accounts/{alias}/cooldown/clear", s.clearAccountCooldown)
	mux.HandleFunc("POST /admin/v1/accounts/{alias}/check", s.checkAccount)
	mux.HandleFunc("POST /admin/v1/oauth/claude/start", s.startClaudeOAuth)
	mux.HandleFunc("POST /admin/v1/oauth/claude/exchange", s.exchangeClaudeOAuth)
	return withRequestID(s.securityHeaders(s.authenticate(mux)))
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/admin/") {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Pragma", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/assets/") {
			next.ServeHTTP(w, r)
			return
		}
		provided := strings.TrimSpace(r.Header.Get("x-api-key"))
		if provided == "" {
			provided = strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		}
		expected := s.cfg.RelayAPIKey
		if strings.HasPrefix(r.URL.Path, "/admin/") {
			expected = s.cfg.AdminAPIKey
		}
		providedHash := sha256.Sum256([]byte(provided))
		expectedHash := sha256.Sum256([]byte(expected))
		if subtle.ConstantTimeCompare(providedHash[:], expectedHash[:]) != 1 {
			writeError(w, http.StatusUnauthorized, "authentication_error", "invalid API key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"status":"ok"}`)
}

func (s *Server) forward(w http.ResponseWriter, incoming *http.Request) {
	requestID := requestIDFromContext(incoming.Context())
	started := time.Now()
	event := metrics.Event{RequestID: requestID, Time: started, Path: incoming.URL.Path}
	defer func() {
		event.Duration = time.Since(started)
		s.metrics.Record(event)
	}()
	fail := func(status int, errorType, message string) {
		event.Status = status
		event.Error = message
		writeError(w, status, errorType, message)
	}

	if _, ok := allowedPaths[incoming.URL.Path]; !ok {
		fail(http.StatusNotFound, "not_found_error", "unsupported endpoint")
		return
	}
	limited := http.MaxBytesReader(w, incoming.Body, s.cfg.MaxRequestBytes)
	body, err := io.ReadAll(limited)
	if closeErr := limited.Close(); closeErr != nil {
		slog.Warn("close incoming request", "request_id", requestID, "error", closeErr)
	}
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			fail(http.StatusRequestEntityTooLarge, "invalid_request_error", "request body is too large")
			return
		}
		fail(http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}
	includeMetadata := incoming.URL.Path == "/v1/messages"
	route, routeErr := deriveRequestRoute(body, incoming.Header, s.cfg.RelayAPIKey)
	if routeErr != nil {
		fail(http.StatusBadRequest, "invalid_request_error", routeErr.Error())
		return
	}
	event.Model = route.Model
	forcedAlias := incoming.Header.Get(accountHeader)
	excluded := make(map[int64]bool)
	var response *http.Response
	var selected selection
	for attempt := 0; attempt < 2; attempt++ {
		selected, err = s.selector.selectAccount(incoming.Context(), route, forcedAlias, excluded)
		if err != nil {
			// A failed alternate selection has no final account. Keep the first
			// failed attempt only in Failover so per-account metrics do not count
			// the same account twice.
			if event.Failover != nil {
				event.Account = ""
				event.Selection = ""
			}
			fail(http.StatusServiceUnavailable, "api_error", err.Error())
			return
		}
		event.Account = selected.Account.Alias
		event.Selection = selected.Source
		freshAccount, refreshErr := s.tokens.ensureFresh(incoming.Context(), selected.Account)
		if refreshErr != nil {
			_ = s.store.Cooldown(incoming.Context(), selected.Account.ID, "", time.Now().Add(time.Minute), "oauth_refresh_failed")
			if selected.Pinned || strings.TrimSpace(forcedAlias) != "" || attempt == 1 {
				fail(http.StatusServiceUnavailable, "authentication_error", refreshErr.Error())
				return
			}
			excluded[selected.Account.ID] = true
			event.Failover = &metrics.Failover{Account: selected.Account.Alias, Error: refreshErr.Error()}
			continue
		}
		selected.Account = freshAccount
		transformedBody, changed, transformErr := addSubscriptionAttribution(body, incoming.Header, selected.Account.Credential, includeMetadata, route.ConversationKey)
		if transformErr != nil {
			fail(http.StatusBadRequest, "invalid_request_error", transformErr.Error())
			return
		}
		if changed {
			slog.Info("added subscription attribution", "request_id", requestID, "path", incoming.URL.Path, "account", selected.Account.Alias)
		}
		response, err = s.doUpstream(incoming, transformedBody, selected.Account.AccessToken)
		if err == nil && !retryableStatus(response.StatusCode) {
			break
		}
		if err == nil {
			cooldown := retryAfter(response.Header, response.StatusCode)
			_ = s.store.Cooldown(incoming.Context(), selected.Account.ID, route.Model, time.Now().Add(cooldown), http.StatusText(response.StatusCode))
		}
		if selected.Pinned || strings.TrimSpace(forcedAlias) != "" || attempt == 1 {
			break
		}
		failover := metrics.Failover{Account: selected.Account.Alias}
		if response != nil {
			failover.Status = response.StatusCode
			_ = response.Body.Close()
			response = nil
		} else if err != nil {
			failover.Error = err.Error()
		}
		event.Failover = &failover
		excluded[selected.Account.ID] = true
	}
	if err != nil {
		slog.Error("upstream request failed", "request_id", requestID, "path", incoming.URL.Path, "account", selected.Account.Alias, "duration_ms", time.Since(started).Milliseconds(), "error", err)
		fail(http.StatusBadGateway, "api_error", "upstream request failed")
		return
	}
	if response == nil {
		fail(http.StatusBadGateway, "api_error", "upstream request failed")
		return
	}
	event.Status = response.StatusCode
	defer func() {
		if err := response.Body.Close(); err != nil {
			slog.Warn("close upstream response", "request_id", requestID, "error", err)
		}
	}()

	copyResponseHeaders(w.Header(), response.Header)
	w.Header().Set(requestIDHeader, requestID)
	w.Header().Set("X-Claude-Relay-Account", selected.Account.Alias)
	w.WriteHeader(response.StatusCode)
	if _, err := io.Copy(flushWriter{ResponseWriter: w}, response.Body); err != nil {
		slog.Warn("relay response interrupted", "request_id", requestID, "path", incoming.URL.Path, "status", response.StatusCode, "error", err)
		event.Error = "response interrupted"
		return
	}
	if response.StatusCode < 400 {
		if bindErr := s.store.Bind(incoming.Context(), route.ConversationKey, selected.Account.ID, stickySessionTTL); bindErr != nil {
			slog.Warn("persist session binding", "request_id", requestID, "error", bindErr)
		}
	}
	slog.Info("request completed", "request_id", requestID, "path", incoming.URL.Path, "account", selected.Account.Alias, "selection", selected.Source, "status", response.StatusCode, "duration_ms", time.Since(started).Milliseconds())
}

func (s *Server) doUpstream(incoming *http.Request, body []byte, accessToken string) (*http.Response, error) {
	target := *s.upstream
	target.Path = strings.TrimRight(s.upstream.Path, "/") + incoming.URL.Path
	query := incoming.URL.Query()
	query.Set("beta", "true")
	target.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(incoming.Context(), http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyRequestHeaders(request.Header, incoming.Header)
	request.Header.Del("x-api-key")
	request.Header.Set("Authorization", "Bearer "+accessToken)
	if request.Header.Get("anthropic-version") == "" {
		request.Header.Set("anthropic-version", "2023-06-01")
	}
	if request.Header.Get("content-type") == "" {
		request.Header.Set("content-type", "application/json")
	}
	request.Host = s.upstream.Host
	return s.client.Do(request)
}

func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == 529 || status >= 500
}

func retryAfter(headers http.Header, status int) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(headers.Get("Retry-After"))); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if status == http.StatusTooManyRequests {
		return 30 * time.Second
	}
	return 10 * time.Second
}

type flushWriter struct {
	http.ResponseWriter
}

func (w flushWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}

func writeError(w http.ResponseWriter, status int, errorType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"type":"error","error":{"type":%q,"message":%q}}`, errorType, message)
}
