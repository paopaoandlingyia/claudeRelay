package proxy

import (
	"context"
	"fmt"
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
	refreshed, err := m.oauth.Refresh(ctx, current.RefreshToken)
	if err != nil {
		return store.Account{}, fmt.Errorf("refresh account %q: %w", current.Alias, err)
	}
	return m.store.UpdateTokens(ctx, current.ID, refreshed.AccessToken, refreshed.RefreshToken, refreshed.ExpiresAt.Format(time.RFC3339))
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
	refreshed, err := m.oauth.Refresh(ctx, current.RefreshToken)
	if err != nil {
		return store.Account{}, fmt.Errorf("refresh account %q: %w", current.Alias, err)
	}
	updated, err := m.store.UpdateTokens(ctx, current.ID, refreshed.AccessToken, refreshed.RefreshToken, refreshed.ExpiresAt.Format(time.RFC3339))
	if err != nil {
		return store.Account{}, err
	}
	return updated, nil
}
