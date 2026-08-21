package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/local/claude-relay/internal/credential"
	_ "modernc.org/sqlite"
)

type Account struct {
	ID            int64
	Alias         string
	Enabled       bool
	Pool          string
	CreatedAt     int64
	UpdatedAt     int64
	LastRefreshAt int64
	credential.Credential
}

// Account pools name both the two ingress keys and the placement of an account.
// Permeability between them is one way: Claude Code-shaped traffic is the shape a
// subscription is expected to produce, so the official ingress may draw from
// every pool, while the compatible ingress is fenced to the compatible pool.
// Official-pool accounts therefore never serve a non-Claude-Code request, and the
// official ingress keeps the full account set for load spreading and failover.
const (
	AccountPoolCompatible = "compatible"
	AccountPoolOfficial   = "official"
)

func ValidateAccountPool(pool string) error {
	switch strings.TrimSpace(pool) {
	case AccountPoolCompatible, AccountPoolOfficial:
		return nil
	default:
		return fmt.Errorf("account pool must be %q or %q", AccountPoolCompatible, AccountPoolOfficial)
	}
}

// accountPoolPredicate restricts a lookup to the accounts one ingress may select.
// Its single bound parameter is the ingress name, so every query keeps a fixed
// SQL text and argument order.
const accountPoolPredicate = `(?='` + AccountPoolOfficial + `' OR a.account_pool='` + AccountPoolCompatible + `')`

// IngressMayUse reports whether an ingress is allowed to select an account that
// sits in the given pool. It must stay equivalent to accountPoolPredicate, so it
// compares the stored values exactly rather than normalizing them.
func IngressMayUse(ingress, pool string) bool {
	return ingress == AccountPoolOfficial || pool == AccountPoolCompatible
}

// Cooldown is an active routing exclusion for one account, optionally scoped to
// a single model. An empty Model means the account is excluded from all models.
type Cooldown struct {
	AccountID int64
	Model     string
	UntilAt   int64
	Reason    string
}

type Store struct {
	db *sql.DB
	// lastBindingPrune is the Unix second of the last expired-binding sweep.
	// Zero on startup so the first Bind cleans up whatever a previous process
	// left behind.
	lastBindingPrune atomic.Int64
	// lastSnapshotPrune is the same for subscription usage snapshots, which are
	// written continuously once sampling follows relayed traffic.
	lastSnapshotPrune atomic.Int64
}

// sqliteDSNParams configures every connection the pool opens.
//
// foreign_keys, busy_timeout, and synchronous are per-connection settings: a
// PRAGMA executed once at startup only reaches the connection that ran it, so
// raising the pool size would silently leave later connections without
// ON DELETE CASCADE enforcement. Keeping them in the DSN makes the
// configuration a property of the database handle instead of a property of
// initialization order. journal_mode is persisted in the file, but it is listed
// here too so a fresh database is never briefly opened in rollback-journal mode.
//
// synchronous=NORMAL is the standard WAL pairing: a process crash is still
// fully safe, and only host power loss can drop the most recent commits. The
// worst case is losing a few sticky bindings and cooldowns, which costs one
// account re-selection — not worth an fsync on every write.
const sqliteDSNParams = "_busy_timeout=5000&_journal_mode=WAL&_synchronous=NORMAL&_foreign_keys=on"

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
	db, err := sql.Open("sqlite", path+"?"+sqliteDSNParams)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// One connection keeps every statement serialized, which the relay can
	// afford: the tables are tiny and no request writes to them while a response
	// is streaming. ensureColumn also depends on it. Raising this is safe only
	// because the PRAGMAs above travel with each new connection.
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
	if version > 5 {
		return fmt.Errorf("database schema version %d is newer than this build supports", version)
	}
	statements := []string{
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
			updated_at INTEGER NOT NULL,
			last_refresh_at INTEGER NOT NULL DEFAULT 0,
			account_pool TEXT NOT NULL DEFAULT 'compatible' CHECK(account_pool IN ('compatible','official'))
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
		`CREATE TABLE IF NOT EXISTS usage_hourly (
			bucket_start INTEGER NOT NULL,
			account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			model TEXT NOT NULL,
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			cache_creation_5m_tokens INTEGER NOT NULL DEFAULT 0,
			cache_creation_1h_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens INTEGER NOT NULL DEFAULT 0,
			request_count INTEGER NOT NULL DEFAULT 0,
			incomplete_count INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY(bucket_start, account_id, model)
		)`,
		`CREATE INDEX IF NOT EXISTS usage_hourly_account_time_idx ON usage_hourly(account_id,bucket_start)`,
		`CREATE INDEX IF NOT EXISTS usage_hourly_time_idx ON usage_hourly(bucket_start)`,
		`CREATE TABLE IF NOT EXISTS model_prices (
			id INTEGER PRIMARY KEY,
			model_pattern TEXT NOT NULL,
			effective_from INTEGER NOT NULL,
			input_usd_per_mtok REAL NOT NULL,
			output_usd_per_mtok REAL NOT NULL,
			cache_creation_5m_usd_per_mtok REAL NOT NULL,
			cache_creation_1h_usd_per_mtok REAL NOT NULL,
			cache_read_usd_per_mtok REAL NOT NULL,
			source TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			UNIQUE(model_pattern,effective_from)
		)`,
		`CREATE INDEX IF NOT EXISTS model_prices_effective_idx ON model_prices(effective_from)`,
		`CREATE TABLE IF NOT EXISTS subscription_usage_snapshots (
			id INTEGER PRIMARY KEY,
			account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			observed_at INTEGER NOT NULL,
			resets_at TEXT NOT NULL DEFAULT '',
			used_percent REAL NOT NULL,
			totals_json TEXT NOT NULL,
			UNIQUE(account_id,observed_at)
		)`,
		`CREATE INDEX IF NOT EXISTS subscription_usage_snapshots_account_time_idx ON subscription_usage_snapshots(account_id,observed_at)`,
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
	}
	if version < 3 {
		// Version 3 records when a refresh-token rotation last succeeded so the
		// console can distinguish a healthy account from a stale one.
		if err := s.ensureColumn(ctx, "accounts", "last_refresh_at", "INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `PRAGMA user_version=3`); err != nil {
			return fmt.Errorf("record database schema version: %w", err)
		}
	}
	if version < 4 {
		// Existing accounts remain in the compatible pool so an upgrade cannot
		// unexpectedly expose them through the stricter official ingress.
		if err := s.ensureColumn(ctx, "accounts", "account_pool",
			"TEXT NOT NULL DEFAULT 'compatible' CHECK(account_pool IN ('compatible','official'))"); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `PRAGMA user_version=4`); err != nil {
			return fmt.Errorf("record database schema version: %w", err)
		}
	}
	if version < 5 {
		if err := s.insertDefaultModelPrices(ctx); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `PRAGMA user_version=5`); err != nil {
			return fmt.Errorf("record database schema version: %w", err)
		}
	}
	return nil
}

// ensureColumn adds a column when an older database predates it. The row scan
// completes before the ALTER statement because the pool holds a single connection.
func (s *Store) ensureColumn(ctx context.Context, table, column, definition string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return fmt.Errorf("inspect table %s: %w", table, err)
	}
	present := false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return fmt.Errorf("inspect table %s: %w", table, err)
		}
		if strings.EqualFold(name, column) {
			present = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("inspect table %s: %w", table, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("inspect table %s: %w", table, err)
	}
	if present {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, definition)); err != nil {
		return fmt.Errorf("add column %s.%s: %w", table, column, err)
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

// Accounts lists the healthy accounts the given ingress may select for a model.
func (s *Store) Accounts(ctx context.Context, ingress, model string, now time.Time) ([]Account, error) {
	if err := ValidateAccountPool(ingress); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+accountColumns+` FROM accounts a
		WHERE a.enabled=1 AND `+accountPoolPredicate+` AND NOT EXISTS (
			SELECT 1 FROM account_cooldowns c WHERE c.account_id=a.id AND c.until_at>? AND (c.model='' OR c.model=?))
		ORDER BY a.id`, ingress, now.Unix(), model)
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

const accountColumns = `a.id,a.alias,a.enabled,a.account_pool,a.type,a.access_token,a.refresh_token,a.expires_at,a.email,a.account_uuid,a.device_id,a.extra_json,a.created_at,a.updated_at,a.last_refresh_at`

type scanner interface{ Scan(...any) error }

func scanAccount(row scanner) (Account, error) {
	var account Account
	var extra string
	if err := row.Scan(&account.ID, &account.Alias, &account.Enabled, &account.Pool, &account.Type,
		&account.AccessToken, &account.RefreshToken, &account.ExpiresAt, &account.Email,
		&account.AccountUUID, &account.DeviceID, &extra,
		&account.CreatedAt, &account.UpdatedAt, &account.LastRefreshAt); err != nil {
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

// SetAccountPool changes an account's placement and removes its sticky bindings
// atomically, because a move into the official pool must not leave a compatible
// conversation pinned to a now-fenced account. Cooldowns remain attached to the
// credential because a pool change does not make an upstream limit disappear.
func (s *Store) SetAccountPool(ctx context.Context, alias, pool string) (Account, error) {
	pool = strings.TrimSpace(pool)
	if err := ValidateAccountPool(pool); err != nil {
		return Account{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Account{}, fmt.Errorf("begin account pool update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var id int64
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT id,account_pool FROM accounts WHERE alias=?`, alias).Scan(&id, &current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Account{}, fmt.Errorf("account %q was not found", alias)
		}
		return Account{}, fmt.Errorf("read account pool: %w", err)
	}
	if current != pool {
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET account_pool=?,updated_at=? WHERE id=?`, pool, time.Now().Unix(), id); err != nil {
			return Account{}, fmt.Errorf("update account pool: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM session_bindings WHERE account_id=?`, id); err != nil {
			return Account{}, fmt.Errorf("clear account pool bindings: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Account{}, fmt.Errorf("commit account pool update: %w", err)
	}
	account, _, err := s.AccountByID(ctx, id)
	return account, err
}

// DeleteAccount removes an account together with its cooldowns and session
// bindings. The relay stops being the owner of that refresh-token chain, so a
// caller must be sure the credential is either revoked or managed elsewhere.
func (s *Store) DeleteAccount(ctx context.Context, alias string) (Account, error) {
	account, found, err := s.AccountByAlias(ctx, alias)
	if err != nil {
		return Account{}, err
	}
	if !found {
		return Account{}, fmt.Errorf("account %q was not found", alias)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM accounts WHERE id=?`, account.ID); err != nil {
		return Account{}, fmt.Errorf("delete account: %w", err)
	}
	return account, nil
}

// RenameAccount changes the alias used by routing, logs, and the private
// selection header. Account identity, tokens, and enabled state are preserved.
func (s *Store) RenameAccount(ctx context.Context, alias, newAlias string) (Account, error) {
	newAlias = strings.TrimSpace(newAlias)
	if err := ValidateAlias(newAlias); err != nil {
		return Account{}, err
	}
	account, found, err := s.AccountByAlias(ctx, alias)
	if err != nil {
		return Account{}, err
	}
	if !found {
		return Account{}, fmt.Errorf("account %q was not found", alias)
	}
	if strings.EqualFold(account.Alias, newAlias) {
		return account, nil
	}
	if _, taken, err := s.AccountByAlias(ctx, newAlias); err != nil {
		return Account{}, err
	} else if taken {
		return Account{}, fmt.Errorf("account alias %q is already in use", newAlias)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE accounts SET alias=?,updated_at=? WHERE id=?`,
		newAlias, time.Now().Unix(), account.ID); err != nil {
		return Account{}, fmt.Errorf("rename account: %w", err)
	}
	renamed, _, err := s.AccountByID(ctx, account.ID)
	return renamed, err
}

// ActiveCooldowns returns every routing exclusion that has not yet expired.
func (s *Store) ActiveCooldowns(ctx context.Context, now time.Time) ([]Cooldown, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT account_id,model,until_at,reason FROM account_cooldowns
		WHERE until_at>? ORDER BY until_at DESC`, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("list account cooldowns: %w", err)
	}
	defer rows.Close()
	var cooldowns []Cooldown
	for rows.Next() {
		var cooldown Cooldown
		if err := rows.Scan(&cooldown.AccountID, &cooldown.Model, &cooldown.UntilAt, &cooldown.Reason); err != nil {
			return nil, fmt.Errorf("scan account cooldown: %w", err)
		}
		cooldowns = append(cooldowns, cooldown)
	}
	return cooldowns, rows.Err()
}

// ClearCooldowns returns an account to rotation immediately and reports how many
// exclusions were removed.
func (s *Store) ClearCooldowns(ctx context.Context, accountID int64) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM account_cooldowns WHERE account_id=?`, accountID)
	if err != nil {
		return 0, fmt.Errorf("clear account cooldowns: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("inspect cleared cooldowns: %w", err)
	}
	return removed, nil
}

// SessionBindingCounts reports how many live sticky bindings each account holds.
func (s *Store) SessionBindingCounts(ctx context.Context, now time.Time) (map[int64]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT account_id,COUNT(*) FROM session_bindings
		WHERE expires_at>? GROUP BY account_id`, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("count session bindings: %w", err)
	}
	defer rows.Close()
	counts := make(map[int64]int)
	for rows.Next() {
		var accountID int64
		var count int
		if err := rows.Scan(&accountID, &count); err != nil {
			return nil, fmt.Errorf("scan session binding count: %w", err)
		}
		counts[accountID] = count
	}
	return counts, rows.Err()
}

// ActiveSessionCounts reports live bindings that have carried a successful
// request since cutoff. This is intentionally distinct from the one-hour
// sticky-binding count: it approximates recently active client sessions rather
// than retained routing affinity.
func (s *Store) ActiveSessionCounts(ctx context.Context, now, cutoff time.Time) (map[int64]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT account_id,COUNT(*) FROM session_bindings
		WHERE expires_at>? AND updated_at>? GROUP BY account_id`, now.Unix(), cutoff.Unix())
	if err != nil {
		return nil, fmt.Errorf("count active sessions: %w", err)
	}
	defer rows.Close()
	counts := make(map[int64]int)
	for rows.Next() {
		var accountID int64
		var count int
		if err := rows.Scan(&accountID, &count); err != nil {
			return nil, fmt.Errorf("scan active session count: %w", err)
		}
		counts[accountID] = count
	}
	return counts, rows.Err()
}

// ActiveSessionCount returns the number of recently successful, live bindings
// for one account. It is used as an admission signal for new conversations.
func (s *Store) ActiveSessionCount(ctx context.Context, accountID int64, now, cutoff time.Time) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_bindings
		WHERE account_id=? AND expires_at>? AND updated_at>?`, accountID, now.Unix(), cutoff.Unix()).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count account active sessions: %w", err)
	}
	return count, nil
}

func (s *Store) UpdateTokens(ctx context.Context, id int64, accessToken, refreshToken, expiresAt string) (Account, error) {
	now := time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `UPDATE accounts SET access_token=?,refresh_token=?,expires_at=?,updated_at=?,last_refresh_at=? WHERE id=?`,
		accessToken, refreshToken, expiresAt, now, now, id)
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

func (s *Store) AccountByUUID(ctx context.Context, uuid, ingress string) (Account, bool, error) {
	account, err := scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts a
		WHERE a.account_uuid=? AND `+accountPoolPredicate+` AND a.enabled=1 LIMIT 1`, uuid, ingress))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, false, nil
	}
	return account, err == nil, err
}

func (s *Store) BoundAccount(ctx context.Context, routeKey, ingress string, now time.Time) (Account, bool, error) {
	account, err := scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM session_bindings b
		JOIN accounts a ON a.id=b.account_id WHERE b.route_key=? AND b.expires_at>? AND `+accountPoolPredicate+` AND a.enabled=1`, routeKey, now.Unix(), ingress))
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
	s.pruneExpiredBindings(ctx, now)
	_, err := s.db.ExecContext(ctx, `INSERT INTO session_bindings(route_key,account_id,expires_at,updated_at)
		VALUES(?,?,?,?) ON CONFLICT(route_key) DO UPDATE SET account_id=excluded.account_id,
		expires_at=excluded.expires_at,updated_at=excluded.updated_at`, routeKey, accountID, now.Add(ttl).Unix(), now.Unix())
	if err != nil {
		return fmt.Errorf("bind session: %w", err)
	}
	return nil
}

// bindingPruneInterval bounds how often expired bindings are swept. Every read
// already filters on expires_at, so the sweep only reclaims space and can never
// change routing. Running it inside each Bind added a write transaction to every
// relayed request for no behavioural gain.
const bindingPruneInterval = time.Minute

// pruneExpiredBindings deletes expired bindings at most once per interval. A
// failure is logged rather than returned because housekeeping must not fail the
// binding the caller actually asked for; the next sweep retries.
func (s *Store) pruneExpiredBindings(ctx context.Context, now time.Time) {
	last := s.lastBindingPrune.Load()
	if now.Unix()-last < int64(bindingPruneInterval.Seconds()) {
		return
	}
	if !s.lastBindingPrune.CompareAndSwap(last, now.Unix()) {
		return
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM session_bindings WHERE expires_at<=?`, now.Unix()); err != nil {
		slog.Warn("prune expired session bindings", "error", err)
	}
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
