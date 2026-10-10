package store

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ValidateAccountProxy rejects ambiguous URLs rather than silently using another exit.
func ValidateAccountProxy(mode, address string) error {
	switch mode {
	case "direct", "global":
		if address != "" {
			return fmt.Errorf("proxy URL must be empty for %s mode", mode)
		}
	case "custom":
		u, err := url.Parse(address)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
			return fmt.Errorf("custom proxy must be an absolute HTTP(S) URL without a path, query or fragment")
		}
	default:
		return fmt.Errorf("proxy mode must be direct, custom or global")
	}
	return nil
}

// ProxyDisplay omits all authentication information from administration responses.
func ProxyDisplay(address string) string {
	if address == "" {
		return ""
	}
	u, err := url.Parse(address)
	if err != nil {
		return "invalid proxy URL"
	}
	u.User = nil
	return u.String()
}

func (s *Store) SetAccountProxy(ctx context.Context, alias, mode, address string) (Account, error) {
	address = strings.TrimSpace(address)
	if err := ValidateAccountProxy(mode, address); err != nil {
		return Account{}, err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE accounts SET proxy_mode=?,proxy_url=?,updated_at=? WHERE alias=?`, mode, address, time.Now().Unix(), alias)
	if err != nil {
		return Account{}, fmt.Errorf("update account proxy: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return Account{}, err
	}
	if count == 0 {
		return Account{}, fmt.Errorf("account %q was not found", alias)
	}
	account, _, err := s.AccountByAlias(ctx, alias)
	return account, err
}
