package proxy

import (
	"net/http"
	"strings"
	"time"

	"github.com/local/claude-relay/internal/store"
)

type availabilityResponse struct {
	GeneratedAt  int64           `json:"generated_at"`
	Availability map[string]bool `json:"availability"`
}

func (s *Server) availability(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	accounts, err := s.store.AllAccounts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to inspect account availability")
		return
	}
	cooldowns, err := s.store.ActiveCooldowns(r.Context(), now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to inspect account availability")
		return
	}

	// Only account-wide exclusions can make an entire ingress unavailable.
	// Model-scoped cooldowns include ordinary upstream 429 avoidance and must not
	// turn transient request throttling into a published service outage.
	excluded := make(map[int64]bool)
	for _, cooldown := range cooldowns {
		if cooldown.Model == "" {
			excluded[cooldown.AccountID] = true
		}
	}

	available := func(ingress string) bool {
		for _, account := range accounts {
			if !account.Enabled || !store.IngressMayUse(ingress, account.Pool) || excluded[account.ID] {
				continue
			}
			refreshable := s.cfg.AutoRefresh && strings.TrimSpace(account.RefreshToken) != ""
			if !account.Credential.IsExpired(now) || refreshable {
				return true
			}
		}
		return false
	}

	writeJSON(w, http.StatusOK, availabilityResponse{
		GeneratedAt: now.Unix(),
		Availability: map[string]bool{
			store.AccountPoolCompatible: available(store.AccountPoolCompatible),
			store.AccountPoolOfficial:   s.cfg.OfficialAPIKey != "" && available(store.AccountPoolOfficial),
		},
	})
}
