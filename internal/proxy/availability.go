package proxy

import (
	"net/http"
	"time"
)

const availabilityTTLSeconds = 60

type availabilityResponse struct {
	Status     string                           `json:"status"`
	ObservedAt int64                            `json:"observed_at"`
	TTLSeconds int                              `json:"ttl_seconds"`
	Models     map[string]availabilityModelView `json:"models,omitempty"`
}

type availabilityModelView struct {
	Status string `json:"status"`
}

func (s *Server) availability(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	accounts, err := s.store.Accounts(r.Context(), requestIngress(r.Context()), "", now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to determine availability")
		return
	}

	status := "unavailable"
	for _, account := range accounts {
		serviceable := account.ExpiresAt == ""
		if account.ExpiresAt != "" {
			expiresAt, parseErr := time.Parse(time.RFC3339, account.ExpiresAt)
			serviceable = parseErr == nil && (expiresAt.After(now) || (s.cfg.AutoRefresh && account.RefreshToken != ""))
		}
		if serviceable {
			status = "available"
			break
		}
	}

	w.Header().Set("Cache-Control", "private, max-age=30")
	writeJSON(w, http.StatusOK, availabilityResponse{
		Status:     status,
		ObservedAt: now.Unix(),
		TTLSeconds: availabilityTTLSeconds,
	})
}
