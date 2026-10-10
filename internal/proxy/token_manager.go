package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/local/claude-relay/internal/claudeoauth"
	"github.com/local/claude-relay/internal/store"
)

const refreshLeadTime = 5 * time.Minute

type tokenManager struct {
	store       *store.Store
	oauth       *claudeoauth.Client
	autoRefresh bool
	locks       sync.Map
}

// refreshNow rotates an account's tokens regardless of how much lifetime the
// current access token has left. Callers are responsible for enforcing the
// ownership rules that ensureFresh checks inline.
func (m *tokenManager) refreshNow(ctx context.Context, account store.Account) (store.Account, error) {
	lockValue, _ := m.locks.LoadOrStore(account.ID, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	current, found, err := m.store.AccountByID(ctx, account.ID)
	if err != nil {
		return store.Account{}, err
	}
	if !found {
		return store.Account{}, fmt.Errorf("account %q was removed before token refresh", account.Alias)
	}
	if !current.Enabled {
		return store.Account{}, fmt.Errorf("account %q was disabled before token refresh", current.Alias)
	}
	if current.RefreshToken == "" {
		return store.Account{}, fmt.Errorf("account %q cannot refresh because its refresh token is missing", current.Alias)
	}
	return m.refreshAndPersist(ctx, current, "manual")
}

func (m *tokenManager) ensureFresh(ctx context.Context, selected store.Account) (store.Account, error) {
	if selected.ExpiresAt == "" {
		return selected, nil
	}
	expiresAt, err := time.Parse(time.RFC3339, selected.ExpiresAt)
	if err != nil {
		return store.Account{}, fmt.Errorf("account %q has an invalid token expiry", selected.Alias)
	}
	now := time.Now()
	if now.Add(refreshLeadTime).Before(expiresAt) {
		return selected, nil
	}
	if !m.autoRefresh {
		if now.Before(expiresAt) {
			return selected, nil
		}
		return store.Account{}, fmt.Errorf("account %q access token expired while automatic refresh is disabled", selected.Alias)
	}
	if selected.RefreshToken == "" {
		if now.Before(expiresAt) {
			return selected, nil
		}
		return store.Account{}, fmt.Errorf("account %q access token expired and has no refresh token", selected.Alias)
	}

	lockValue, _ := m.locks.LoadOrStore(selected.ID, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	current, found, err := m.store.AccountByID(ctx, selected.ID)
	if err != nil {
		return store.Account{}, err
	}
	if !found || !current.Enabled {
		return store.Account{}, fmt.Errorf("account %q was disabled before token refresh", selected.Alias)
	}
	if current.ExpiresAt != "" {
		currentExpiry, parseErr := time.Parse(time.RFC3339, current.ExpiresAt)
		if parseErr != nil {
			return store.Account{}, fmt.Errorf("account %q has an invalid token expiry", current.Alias)
		}
		if time.Now().Add(refreshLeadTime).Before(currentExpiry) {
			return current, nil
		}
	}
	if current.RefreshToken == "" {
		return store.Account{}, fmt.Errorf("account %q cannot refresh because its refresh token is missing", current.Alias)
	}
	return m.refreshAndPersist(ctx, current, "automatic")
}

// Both callers hold the account lock; success is logged only after both tokens
// have been persisted, so the log establishes ownership of the new token chain.
func (m *tokenManager) refreshAndPersist(ctx context.Context, current store.Account, trigger string) (store.Account, error) {
	started := time.Now()
	slog.Info("OAuth refresh started", "account", current.Alias, "trigger", trigger,
		"previous_expires_at", current.ExpiresAt, "previous_refresh_at", current.LastRefreshAt)
	refreshed, err := m.oauth.Refresh(withAccountExit(ctx, current), current.RefreshToken)
	if err != nil {
		slog.Warn("OAuth refresh failed", "account", current.Alias, "trigger", trigger,
			"duration_ms", time.Since(started).Milliseconds(), "error", err)
		return store.Account{}, fmt.Errorf("refresh account %q: %w", current.Alias, err)
	}
	updated, err := m.store.UpdateTokens(ctx, current.ID, refreshed.AccessToken, refreshed.RefreshToken, refreshed.ExpiresAt.Format(time.RFC3339))
	if err != nil {
		slog.Error("persist OAuth refresh failed", "account", current.Alias, "trigger", trigger, "error", err)
		return store.Account{}, err
	}
	slog.Info("OAuth refresh succeeded", "account", current.Alias, "trigger", trigger,
		"duration_ms", time.Since(started).Milliseconds(), "expires_at", updated.ExpiresAt)
	return updated, nil
}
