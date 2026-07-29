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
	"strings"
	"time"

	"github.com/local/claude-relay/internal/config"
	"github.com/local/claude-relay/internal/credential"
)

var allowedPaths = map[string]struct{}{
	"/v1/messages":              {},
	"/v1/messages/count_tokens": {},
}

type Server struct {
	cfg        config.Config
	credential credential.Credential
	upstream   *url.URL
	httpServer *http.Server
	client     *http.Client
}

func NewServer(cfg config.Config, cred credential.Credential) (*Server, error) {
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
	server := &Server{
		cfg:        cfg,
		credential: cred,
		upstream:   upstream,
		client:     &http.Client{Transport: transport},
	}
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
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("POST /v1/messages", s.forward)
	mux.HandleFunc("POST /v1/messages/count_tokens", s.forward)
	return s.authenticate(mux)
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		provided := strings.TrimSpace(r.Header.Get("x-api-key"))
		if provided == "" {
			provided = strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		}
		providedHash := sha256.Sum256([]byte(provided))
		expectedHash := sha256.Sum256([]byte(s.cfg.APIKey))
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
	if _, ok := allowedPaths[incoming.URL.Path]; !ok {
		writeError(w, http.StatusNotFound, "not_found_error", "unsupported endpoint")
		return
	}
	limited := http.MaxBytesReader(w, incoming.Body, s.cfg.MaxRequestBytes)
	body, err := io.ReadAll(limited)
	if closeErr := limited.Close(); closeErr != nil {
		slog.Warn("close incoming request", "error", closeErr)
	}
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body is too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}
	includeMetadata := incoming.URL.Path == "/v1/messages"
	transformedBody, changed, transformErr := addSubscriptionAttribution(body, incoming.Header, s.credential, includeMetadata)
	if transformErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", transformErr.Error())
		return
	}
	body = transformedBody
	if changed {
		slog.Info("added subscription attribution", "path", incoming.URL.Path)
	}

	target := *s.upstream
	target.Path = strings.TrimRight(s.upstream.Path, "/") + incoming.URL.Path
	query := incoming.URL.Query()
	query.Set("beta", "true")
	target.RawQuery = query.Encode()

	upstreamRequest, err := http.NewRequestWithContext(incoming.Context(), http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to create upstream request")
		return
	}
	copyRequestHeaders(upstreamRequest.Header, incoming.Header)
	upstreamRequest.Header.Del("x-api-key")
	upstreamRequest.Header.Set("Authorization", "Bearer "+s.credential.AccessToken)
	if upstreamRequest.Header.Get("anthropic-version") == "" {
		upstreamRequest.Header.Set("anthropic-version", "2023-06-01")
	}
	if upstreamRequest.Header.Get("content-type") == "" {
		upstreamRequest.Header.Set("content-type", "application/json")
	}
	upstreamRequest.Host = s.upstream.Host

	started := time.Now()
	response, err := s.client.Do(upstreamRequest)
	if err != nil {
		slog.Error("upstream request failed", "path", incoming.URL.Path, "duration_ms", time.Since(started).Milliseconds(), "error", err)
		writeError(w, http.StatusBadGateway, "api_error", "upstream request failed")
		return
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			slog.Warn("close upstream response", "error", err)
		}
	}()

	copyResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	if _, err := io.Copy(flushWriter{ResponseWriter: w}, response.Body); err != nil {
		slog.Warn("relay response interrupted", "path", incoming.URL.Path, "status", response.StatusCode, "error", err)
		return
	}
	slog.Info("request completed", "path", incoming.URL.Path, "status", response.StatusCode, "duration_ms", time.Since(started).Milliseconds())
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
