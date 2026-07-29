package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/local/claude-relay/internal/credential"
	_ "modernc.org/sqlite"
)

type Account struct {
	ID      int64
	Alias   string
	Enabled bool
	credential.Credential
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err == nil {
		if closeErr := file.Close(); closeErr != nil {
			return nil, fmt.Errorf("create database file: %w", closeErr)
		}
	} else if !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create database file: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("protect database file: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.initialize(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) initialize(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read database schema version: %w", err)
	}
	if version > 2 {
		return fmt.Errorf("database schema version %d is newer than this build supports", version)
	}
	statements := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA foreign_keys=ON`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS accounts (
			id INTEGER PRIMARY KEY,
			alias TEXT NOT NULL COLLATE NOCASE UNIQUE,
			type TEXT NOT NULL,
			access_token TEXT NOT NULL,
			refresh_token TEXT NOT NULL DEFAULT '',
			expires_at TEXT NOT NULL DEFAULT '',
			email TEXT NOT NULL DEFAULT '',
			account_uuid TEXT NOT NULL,
			device_id TEXT NOT NULL,
			extra_json TEXT NOT NULL DEFAULT '{}',
			enabled INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS accounts_uuid_idx ON accounts(account_uuid)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS accounts_uuid_unique_idx ON accounts(account_uuid)`,
		`CREATE TABLE IF NOT EXISTS session_bindings (
			route_key TEXT PRIMARY KEY,
			account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			expires_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS session_bindings_expiry_idx ON session_bindings(expires_at)`,
		`CREATE TABLE IF NOT EXISTS account_cooldowns (
			account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			model TEXT NOT NULL DEFAULT '',
			until_at INTEGER NOT NULL,
			reason TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(account_id, model)
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize database: %w", err)
		}
	}
	if version < 2 {
		// Version 2 introduces explicit account activation. Existing accounts are
		// intentionally disabled so an upgrade cannot take over credentials still
		// managed by another process.
		if _, err := s.db.ExecContext(ctx, `UPDATE accounts SET enabled=0`); err != nil {
			return fmt.Errorf("disable accounts during schema migration: %w", err)
		}
		if _, err := s.db.ExecContext(ctx, `PRAGMA user_version=2`); err != nil {
			return fmt.Errorf("record database schema version: %w", err)
		}
	}
	return nil
}

func (s *Store) ImportAccount(ctx context.Context, alias string, cred credential.Credential) (Account, error) {
	alias = strings.TrimSpace(alias)
	if err := ValidateAlias(alias); err != nil {
		return Account{}, err
	}
	extra, err := json.Marshal(cred.Extra)
	if err != nil {
		return Account{}, fmt.Errorf("encode account extras: %w", err)
	}
	now := time.Now().Unix()
	if existing, found, lookupErr := s.accountByUUIDAnyState(ctx, cred.AccountUUID); lookupErr != nil {
		return Account{}, lookupErr
	} else if found && !strings.EqualFold(existing.Alias, alias) {
		_, err = s.db.ExecContext(ctx, `UPDATE accounts SET alias=?,type=?,access_token=?,refresh_token=?,expires_at=?,email=?,
			account_uuid=?,device_id=?,extra_json=?,enabled=0,updated_at=? WHERE id=?`,
			alias, cred.Type, cred.AccessToken, cred.RefreshToken, cred.ExpiresAt, cred.Email,
			cred.AccountUUID, cred.DeviceID, string(extra), now, existing.ID)
		if err != nil {
			return Account{}, fmt.Errorf("update existing account identity: %w", err)
		}
		account, _, reloadErr := s.AccountByID(ctx, existing.ID)
		return account, reloadErr
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO accounts
		(alias,type,access_token,refresh_token,expires_at,email,account_uuid,device_id,extra_json,enabled,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,0,?,?)
		ON CONFLICT(alias) DO UPDATE SET
		type=excluded.type, access_token=excluded.access_token, refresh_token=excluded.refresh_token,
		expires_at=excluded.expires_at, email=excluded.email, account_uuid=excluded.account_uuid,
		device_id=excluded.device_id, extra_json=excluded.extra_json, enabled=0, updated_at=excluded.updated_at`,
		alias, cred.Type, cred.AccessToken, cred.RefreshToken, cred.ExpiresAt, cred.Email,
		cred.AccountUUID, cred.DeviceID, string(extra), now, now)
	if err != nil {
		return Account{}, fmt.Errorf("import account: %w", err)
	}
	account, found, err := s.AccountByAlias(ctx, alias)
	if err != nil {
		return Account{}, fmt.Errorf("reload imported account: %w", err)
	}
	if !found {
		return Account{}, fmt.Errorf("reload imported account: account disappeared")
	}
	return account, nil
}

func (s *Store) accountByUUIDAnyState(ctx context.Context, uuid string) (Account, bool, error) {
	account, err := scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts a WHERE a.account_uuid=? LIMIT 1`, uuid))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, false, nil
	}
	return account, err == nil, err
}

func ValidateAlias(alias string) error {
	alias = strings.TrimSpace(alias)
	if alias == "" || len(alias) > 64 || !validAlias(alias) {
		return fmt.Errorf("account alias must use 1-64 ASCII letters, digits, dots, underscores, or hyphens")
	}
	return nil
}

func validAlias(alias string) bool {
	if alias == "" {
		return false
	}
	for _, char := range alias {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '.' || char == '_' || char == '-' {
			continue
		}
		return false
	}
	return true
}

func (s *Store) AccountCount(ctx context.Context) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count accounts: %w", err)
	}
	return count, nil
}

func (s *Store) Accounts(ctx context.Context, model string, now time.Time) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+accountColumns+` FROM accounts a
		WHERE a.enabled=1 AND NOT EXISTS (
			SELECT 1 FROM account_cooldowns c WHERE c.account_id=a.id AND c.until_at>? AND (c.model='' OR c.model=?))
		ORDER BY a.id`, now.Unix(), model)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()
	var accounts []Account
	for rows.Next() {
		account, scanErr := scanAccount(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

func (s *Store) AllAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+accountColumns+` FROM accounts a ORDER BY a.alias`)
	if err != nil {
		return nil, fmt.Errorf("list all accounts: %w", err)
	}
	defer rows.Close()
	var accounts []Account
	for rows.Next() {
		account, scanErr := scanAccount(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

const accountColumns = `a.id,a.alias,a.enabled,a.type,a.access_token,a.refresh_token,a.expires_at,a.email,a.account_uuid,a.device_id,a.extra_json`

type scanner interface{ Scan(...any) error }

func scanAccount(row scanner) (Account, error) {
	var account Account
	var extra string
	if err := row.Scan(&account.ID, &account.Alias, &account.Enabled, &account.Type,
		&account.AccessToken, &account.RefreshToken, &account.ExpiresAt, &account.Email,
		&account.AccountUUID, &account.DeviceID, &extra); err != nil {
		return Account{}, fmt.Errorf("scan account: %w", err)
	}
	if extra != "" && extra != "null" {
		if err := json.Unmarshal([]byte(extra), &account.Extra); err != nil {
			return Account{}, fmt.Errorf("decode account extras: %w", err)
		}
	}
	return account, nil
}

func (s *Store) AccountByAlias(ctx context.Context, alias string) (Account, bool, error) {
	account, err := scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts a WHERE a.alias=?`, alias))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, false, nil
	}
	return account, err == nil, err
}

func (s *Store) AccountByID(ctx context.Context, id int64) (Account, bool, error) {
	account, err := scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts a WHERE a.id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, false, nil
	}
	return account, err == nil, err
}

func (s *Store) SetAccountEnabled(ctx context.Context, alias string, enabled bool) (Account, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE accounts SET enabled=?,updated_at=? WHERE alias=?`, enabled, time.Now().Unix(), alias)
	if err != nil {
		return Account{}, fmt.Errorf("update account state: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return Account{}, fmt.Errorf("inspect account state update: %w", err)
	}
	if changed == 0 {
		return Account{}, fmt.Errorf("account %q was not found", alias)
	}
	account, _, err := s.AccountByAlias(ctx, alias)
	return account, err
}

func (s *Store) UpdateTokens(ctx context.Context, id int64, accessToken, refreshToken, expiresAt string) (Account, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE accounts SET access_token=?,refresh_token=?,expires_at=?,updated_at=? WHERE id=?`,
		accessToken, refreshToken, expiresAt, time.Now().Unix(), id)
	if err != nil {
		return Account{}, fmt.Errorf("persist refreshed tokens: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return Account{}, fmt.Errorf("inspect refreshed token update: %w", err)
	}
	if changed == 0 {
		return Account{}, fmt.Errorf("account id %d was not found", id)
	}
	account, _, err := s.AccountByID(ctx, id)
	return account, err
}

func (s *Store) AccountByUUID(ctx context.Context, uuid string) (Account, bool, error) {
	account, err := scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts a WHERE a.account_uuid=? AND a.enabled=1 LIMIT 1`, uuid))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, false, nil
	}
	return account, err == nil, err
}

func (s *Store) BoundAccount(ctx context.Context, routeKey string, now time.Time) (Account, bool, error) {
	account, err := scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM session_bindings b
		JOIN accounts a ON a.id=b.account_id WHERE b.route_key=? AND b.expires_at>? AND a.enabled=1`, routeKey, now.Unix()))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, false, nil
	}
	return account, err == nil, err
}

func (s *Store) Bind(ctx context.Context, routeKey string, accountID int64, ttl time.Duration) error {
	if routeKey == "" {
		return nil
	}
	now := time.Now()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM session_bindings WHERE expires_at<=?`, now.Unix()); err != nil {
		return fmt.Errorf("prune expired session bindings: %w", err)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO session_bindings(route_key,account_id,expires_at,updated_at)
		VALUES(?,?,?,?) ON CONFLICT(route_key) DO UPDATE SET account_id=excluded.account_id,
		expires_at=excluded.expires_at,updated_at=excluded.updated_at`, routeKey, accountID, now.Add(ttl).Unix(), now.Unix())
	if err != nil {
		return fmt.Errorf("bind session: %w", err)
	}
	return nil
}

func (s *Store) Cooldown(ctx context.Context, accountID int64, model string, until time.Time, reason string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO account_cooldowns(account_id,model,until_at,reason) VALUES(?,?,?,?)
		ON CONFLICT(account_id,model) DO UPDATE SET until_at=excluded.until_at,reason=excluded.reason`,
		accountID, model, until.Unix(), reason)
	if err != nil {
		return fmt.Errorf("set account cooldown: %w", err)
	}
	return nil
}

func (s *Store) IsCooling(ctx context.Context, accountID int64, model string, now time.Time) (bool, error) {
	var cooling bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM account_cooldowns
		WHERE account_id=? AND until_at>? AND (model='' OR model=?))`, accountID, now.Unix(), model).Scan(&cooling)
	if err != nil {
		return false, fmt.Errorf("check account cooldown: %w", err)
	}
	return cooling, nil
}
