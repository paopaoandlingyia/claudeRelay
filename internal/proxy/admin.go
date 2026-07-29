package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/local/claude-relay/internal/store"
)

type accountView struct {
	Alias           string `json:"alias"`
	Enabled         bool   `json:"enabled"`
	Email           string `json:"email,omitempty"`
	AccountUUID     string `json:"account_uuid"`
	ExpiresAt       string `json:"expires_at,omitempty"`
	HasRefreshToken bool   `json:"has_refresh_token"`
}

func accountResponse(account store.Account) accountView {
	return accountView{
		Alias:           account.Alias,
		Enabled:         account.Enabled,
		Email:           account.Email,
		AccountUUID:     account.AccountUUID,
		ExpiresAt:       account.ExpiresAt,
		HasRefreshToken: strings.TrimSpace(account.RefreshToken) != "",
	}
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.store.AllAccounts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to list accounts")
		return
	}
	views := make([]accountView, 0, len(accounts))
	for _, account := range accounts {
		views = append(views, accountResponse(account))
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": views, "auto_refresh_enabled": s.cfg.AutoRefresh})
}

func (s *Server) setAccountEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	alias := strings.TrimSpace(r.PathValue("alias"))
	account, err := s.store.SetAccountEnabled(r.Context(), alias, enabled)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, accountResponse(account))
}

type oauthStartRequest struct {
	Alias string `json:"alias"`
}

func (s *Server) startClaudeOAuth(w http.ResponseWriter, r *http.Request) {
	var request oauthStartRequest
	if err := decodeAdminJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	request.Alias = strings.TrimSpace(request.Alias)
	if err := store.ValidateAlias(request.Alias); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	result, err := s.oauth.Start(request.Alias)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to start OAuth login")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type oauthExchangeRequest struct {
	SessionID string `json:"session_id"`
	Code      string `json:"code"`
}

func (s *Server) exchangeClaudeOAuth(w http.ResponseWriter, r *http.Request) {
	var request oauthExchangeRequest
	if err := decodeAdminJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	alias, cred, err := s.oauth.Exchange(r.Context(), strings.TrimSpace(request.SessionID), request.Code)
	if err != nil {
		writeError(w, http.StatusBadRequest, "authentication_error", err.Error())
		return
	}
	account, err := s.store.ImportAccount(r.Context(), alias, cred)
	if err != nil {
		writeError(w, http.StatusConflict, "api_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, accountResponse(account))
}

func decodeAdminJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	limited := http.MaxBytesReader(w, r.Body, 1<<20)
	defer limited.Close()
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode JSON body: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("decode JSON body: trailing JSON value")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	var buffer bytes.Buffer
	if err := json.NewEncoder(&buffer).Encode(value); err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to encode response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.Copy(w, &buffer)
}
