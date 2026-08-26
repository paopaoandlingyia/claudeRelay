package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
)

const (
	FiveHourEventMessages   = "messages"
	FiveHourEventOAuth      = "oauth_refresh"
	FiveHourEventExhaustion = "exhaustion"
)

// FiveHourEvent is one content-free observation. Messages events retain only
// billing metadata returned by Anthropic; OAuth and exhaustion events carry a
// quota reading but no request content or token usage.
type FiveHourEvent struct {
	ID          int64
	EventKey    string
	AccountID   int64
	Account     string
	ResetsAt    string
	Kind        string
	ObservedAt  int64
	CompletedAt int64
	Model       string
	Status      int
	UsedPercent float64
	Usage       UsageCounters
	UsageSeen   bool
	Complete    bool
}

type FiveHourWindow struct {
	AccountID         int64
	Account           string
	ResetsAt          string
	FirstObservedAt   int64
	LastObservedAt    int64
	FirstUsedPercent  float64
	LastUsedPercent   float64
	MaxUsedPercent    float64
	ExhaustedAt       int64
	ExhaustionReason  string
	EventCount        int64
	MissingUsageCount int64
	IncompleteCount   int64
	ByModel           map[string]UsageCounters
}

type FiveHourObservationStats struct {
	FirstObservedAt int64 `json:"first_observed_at"`
	LastObservedAt  int64 `json:"last_observed_at"`
	Windows         int64 `json:"windows"`
	Exhausted       int64 `json:"exhausted"`
	Events          int64 `json:"events"`
}

func (s *Store) AddFiveHourEvents(ctx context.Context, events []FiveHourEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin five-hour event batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, event := range events {
		if err := insertFiveHourEvent(ctx, tx, event); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit five-hour event batch: %w", err)
	}
	return nil
}

func insertFiveHourEvent(ctx context.Context, tx *sql.Tx, event FiveHourEvent) error {
	event.EventKey = strings.TrimSpace(event.EventKey)
	event.ResetsAt = strings.TrimSpace(event.ResetsAt)
	event.Kind = strings.TrimSpace(event.Kind)
	if event.EventKey == "" || event.AccountID <= 0 || event.Kind == "" || event.ObservedAt <= 0 || event.CompletedAt <= 0 {
		return fmt.Errorf("five-hour event requires a key, account, kind, and timestamps")
	}
	if event.CompletedAt < event.ObservedAt {
		return fmt.Errorf("five-hour event completion precedes its observation")
	}
	switch event.Kind {
	case FiveHourEventMessages, FiveHourEventOAuth, FiveHourEventExhaustion:
	default:
		return fmt.Errorf("unknown five-hour event kind %q", event.Kind)
	}
	if math.IsNaN(event.UsedPercent) || math.IsInf(event.UsedPercent, 0) || event.UsedPercent < -1 || event.UsedPercent > 100 {
		return fmt.Errorf("five-hour utilization must be -1 or between 0 and 100")
	}
	c := event.Usage
	if c.InputTokens < 0 || c.OutputTokens < 0 || c.CacheCreation5mTokens < 0 || c.CacheCreation1hTokens < 0 || c.CacheReadTokens < 0 {
		return fmt.Errorf("five-hour usage counters cannot be negative")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO five_hour_events(event_key,account_id,resets_at,kind,
		observed_at,completed_at,model,status,used_percent,input_tokens,output_tokens,
		cache_creation_5m_tokens,cache_creation_1h_tokens,cache_read_tokens,usage_seen,complete)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(event_key) DO NOTHING`, event.EventKey, event.AccountID, event.ResetsAt, event.Kind,
		event.ObservedAt, event.CompletedAt, strings.TrimSpace(event.Model), event.Status, event.UsedPercent,
		c.InputTokens, c.OutputTokens, c.CacheCreation5mTokens, c.CacheCreation1hTokens, c.CacheReadTokens,
		event.UsageSeen, event.Complete); err != nil {
		return fmt.Errorf("insert five-hour event: %w", err)
	}
	if event.ResetsAt == "" || (event.UsedPercent < 0 && event.Kind != FiveHourEventExhaustion) {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO five_hour_windows(account_id,resets_at,first_observed_at,
		last_observed_at,first_used_percent,last_used_percent,max_used_percent) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(account_id,resets_at) DO UPDATE SET
		first_used_percent=CASE WHEN excluded.first_observed_at<first_observed_at THEN excluded.first_used_percent ELSE first_used_percent END,
		first_observed_at=MIN(first_observed_at,excluded.first_observed_at),
		last_used_percent=CASE WHEN excluded.last_observed_at>last_observed_at THEN excluded.last_used_percent ELSE last_used_percent END,
		last_observed_at=MAX(last_observed_at,excluded.last_observed_at),
		max_used_percent=MAX(max_used_percent,excluded.max_used_percent)`, event.AccountID, event.ResetsAt,
		event.ObservedAt, event.ObservedAt, event.UsedPercent, event.UsedPercent, event.UsedPercent); err != nil {
		return fmt.Errorf("upsert five-hour window: %w", err)
	}
	return nil
}

func (s *Store) MarkFiveHourExhausted(ctx context.Context, event FiveHourEvent, reason string) error {
	event.Kind = FiveHourEventExhaustion
	event.Complete = true
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin five-hour exhaustion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertFiveHourEvent(ctx, tx, event); err != nil {
		return err
	}
	if strings.TrimSpace(event.ResetsAt) == "" {
		return fmt.Errorf("five-hour exhaustion requires a reset identity")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE five_hour_windows SET exhausted_at=CASE WHEN exhausted_at=0 OR ?<exhausted_at THEN ? ELSE exhausted_at END,
		exhaustion_reason=CASE WHEN exhausted_at=0 OR ?<exhausted_at THEN ? ELSE exhaustion_reason END
		WHERE account_id=? AND resets_at=?`, event.ObservedAt, event.ObservedAt, event.ObservedAt,
		strings.TrimSpace(reason), event.AccountID, strings.TrimSpace(event.ResetsAt)); err != nil {
		return fmt.Errorf("mark five-hour window exhausted: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit five-hour exhaustion: %w", err)
	}
	return nil
}

func (s *Store) FiveHourWindows(ctx context.Context, exhausted bool, nowMillis int64, limit int) ([]FiveHourWindow, error) {
	condition := `w.exhausted_at=0 AND CAST(w.resets_at AS INTEGER)*1000>?
		AND EXISTS (SELECT 1 FROM five_hour_events e WHERE e.account_id=w.account_id AND e.resets_at=w.resets_at AND e.kind='messages')`
	order := `w.last_observed_at DESC`
	args := []any{nowMillis}
	if exhausted {
		condition = `w.exhausted_at>0`
		order = `w.exhausted_at DESC`
		args = nil
	}
	if limit <= 0 {
		limit = 100
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT w.account_id,a.alias,w.resets_at,w.first_observed_at,
		w.last_observed_at,w.first_used_percent,w.last_used_percent,w.max_used_percent,
		w.exhausted_at,w.exhaustion_reason FROM five_hour_windows w JOIN accounts a ON a.id=w.account_id
		WHERE `+condition+` ORDER BY `+order+` LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("query five-hour windows: %w", err)
	}
	defer rows.Close()
	var windows []FiveHourWindow
	for rows.Next() {
		var window FiveHourWindow
		if err := rows.Scan(&window.AccountID, &window.Account, &window.ResetsAt, &window.FirstObservedAt,
			&window.LastObservedAt, &window.FirstUsedPercent, &window.LastUsedPercent, &window.MaxUsedPercent,
			&window.ExhaustedAt, &window.ExhaustionReason); err != nil {
			return nil, fmt.Errorf("scan five-hour window: %w", err)
		}
		window.ByModel = make(map[string]UsageCounters)
		windows = append(windows, window)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for index := range windows {
		window := &windows[index]
		usageRows, err := s.db.QueryContext(ctx, `SELECT model,SUM(input_tokens),SUM(output_tokens),
			SUM(cache_creation_5m_tokens),SUM(cache_creation_1h_tokens),SUM(cache_read_tokens),
			COUNT(*),SUM(CASE WHEN usage_seen=0 THEN 1 ELSE 0 END),SUM(CASE WHEN complete=0 THEN 1 ELSE 0 END)
			FROM five_hour_events WHERE account_id=? AND resets_at=? AND kind=? GROUP BY model`,
			window.AccountID, window.ResetsAt, FiveHourEventMessages)
		if err != nil {
			return nil, fmt.Errorf("query five-hour window models: %w", err)
		}
		for usageRows.Next() {
			var model string
			var counters UsageCounters
			var missing int64
			if err := usageRows.Scan(&model, &counters.InputTokens, &counters.OutputTokens,
				&counters.CacheCreation5mTokens, &counters.CacheCreation1hTokens, &counters.CacheReadTokens,
				&counters.Requests, &missing, &counters.Incomplete); err != nil {
				_ = usageRows.Close()
				return nil, fmt.Errorf("scan five-hour window model: %w", err)
			}
			window.EventCount += counters.Requests
			window.MissingUsageCount += missing
			window.IncompleteCount += counters.Incomplete
			window.ByModel[model] = counters
		}
		if err := usageRows.Close(); err != nil {
			return nil, err
		}
	}
	return windows, nil
}

func (s *Store) FiveHourObservationStats(ctx context.Context) (FiveHourObservationStats, error) {
	var stats FiveHourObservationStats
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MIN(observed_at),0),COALESCE(MAX(observed_at),0),COUNT(*)
		FROM five_hour_events`).Scan(&stats.FirstObservedAt, &stats.LastObservedAt, &stats.Events); err != nil {
		return stats, fmt.Errorf("query five-hour event stats: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN exhausted_at>0 THEN 1 ELSE 0 END),0)
		FROM five_hour_windows`).Scan(&stats.Windows, &stats.Exhausted); err != nil {
		return stats, fmt.Errorf("query five-hour window stats: %w", err)
	}
	return stats, nil
}

func (s *Store) AllFiveHourWindows(ctx context.Context) ([]FiveHourWindow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT w.account_id,a.alias,w.resets_at,w.first_observed_at,
		w.last_observed_at,w.first_used_percent,w.last_used_percent,w.max_used_percent,
		w.exhausted_at,w.exhaustion_reason FROM five_hour_windows w JOIN accounts a ON a.id=w.account_id
		ORDER BY w.first_observed_at`)
	if err != nil {
		return nil, fmt.Errorf("query all five-hour windows: %w", err)
	}
	defer rows.Close()
	var windows []FiveHourWindow
	for rows.Next() {
		var window FiveHourWindow
		if err := rows.Scan(&window.AccountID, &window.Account, &window.ResetsAt, &window.FirstObservedAt,
			&window.LastObservedAt, &window.FirstUsedPercent, &window.LastUsedPercent, &window.MaxUsedPercent,
			&window.ExhaustedAt, &window.ExhaustionReason); err != nil {
			return nil, err
		}
		windows = append(windows, window)
	}
	return windows, rows.Err()
}

func (s *Store) AllFiveHourEvents(ctx context.Context) ([]FiveHourEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT e.id,e.event_key,e.account_id,a.alias,e.resets_at,e.kind,e.observed_at,
		e.completed_at,e.model,e.status,e.used_percent,e.input_tokens,e.output_tokens,
		e.cache_creation_5m_tokens,e.cache_creation_1h_tokens,e.cache_read_tokens,e.usage_seen,e.complete
		FROM five_hour_events e JOIN accounts a ON a.id=e.account_id ORDER BY e.observed_at,e.id`)
	if err != nil {
		return nil, fmt.Errorf("query all five-hour events: %w", err)
	}
	defer rows.Close()
	var events []FiveHourEvent
	for rows.Next() {
		var event FiveHourEvent
		if err := rows.Scan(&event.ID, &event.EventKey, &event.AccountID, &event.Account, &event.ResetsAt,
			&event.Kind, &event.ObservedAt, &event.CompletedAt, &event.Model, &event.Status, &event.UsedPercent,
			&event.Usage.InputTokens, &event.Usage.OutputTokens, &event.Usage.CacheCreation5mTokens,
			&event.Usage.CacheCreation1hTokens, &event.Usage.CacheReadTokens, &event.UsageSeen, &event.Complete); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) ClearFiveHourObservations(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin clear five-hour observations: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM five_hour_events`); err != nil {
		return fmt.Errorf("clear five-hour events: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM five_hour_windows`); err != nil {
		return fmt.Errorf("clear five-hour windows: %w", err)
	}
	return tx.Commit()
}
