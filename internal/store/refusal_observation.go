package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// RefusalBucket is an hourly operational count. It deliberately excludes the
// prompt, response body, and upstream explanation.
type RefusalBucket struct {
	BucketStart    int64
	AccountID      int64
	Category       string
	Count          int64
	LastObservedAt int64
}

type AccountRefusalSummary struct {
	Count24h     int64  `json:"count_24h"`
	LastCategory string `json:"last_category"`
	LastAt       int64  `json:"last_at"`
}

func (s *Store) AddRefusalBuckets(ctx context.Context, buckets []RefusalBucket) error {
	if len(buckets) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin refusal batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	statement, err := tx.PrepareContext(ctx, `INSERT INTO refusal_hourly(
		bucket_start,account_id,category,refusal_count,last_observed_at) VALUES(?,?,?,?,?)
		ON CONFLICT(bucket_start,account_id,category) DO UPDATE SET
		refusal_count=refusal_count+excluded.refusal_count,
		last_observed_at=MAX(last_observed_at,excluded.last_observed_at)`)
	if err != nil {
		return fmt.Errorf("prepare refusal batch: %w", err)
	}
	defer statement.Close()
	for _, bucket := range buckets {
		category := strings.TrimSpace(bucket.Category)
		if category == "" {
			category = "unknown"
		}
		if _, err := statement.ExecContext(ctx, bucket.BucketStart, bucket.AccountID, category,
			bucket.Count, bucket.LastObservedAt); err != nil {
			return fmt.Errorf("write refusal batch: %w", err)
		}
	}
	cutoffBucket := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Hour).Unix()
	if _, err := tx.ExecContext(ctx, `DELETE FROM refusal_hourly WHERE bucket_start<?`, cutoffBucket); err != nil {
		return fmt.Errorf("prune refusal observations: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit refusal batch: %w", err)
	}
	return nil
}

// RecentAccountRefusals returns one batched summary per account. Because the
// source data is hourly, the boundary bucket can include observations from up
// to one hour before cutoff; last_observed_at prevents wholly stale rows from
// being reported.
func (s *Store) RecentAccountRefusals(ctx context.Context, cutoff time.Time) (map[int64]AccountRefusalSummary, error) {
	cutoff = cutoff.UTC()
	rows, err := s.db.QueryContext(ctx, `SELECT account_id,category,refusal_count,last_observed_at
		FROM refusal_hourly WHERE bucket_start>=? AND last_observed_at>=?
		ORDER BY account_id,last_observed_at DESC,category`, cutoff.Truncate(time.Hour).Unix(), cutoff.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("query recent account refusals: %w", err)
	}
	defer rows.Close()
	summaries := make(map[int64]AccountRefusalSummary)
	for rows.Next() {
		var accountID int64
		var category string
		var count, lastAt int64
		if err := rows.Scan(&accountID, &category, &count, &lastAt); err != nil {
			return nil, fmt.Errorf("scan recent account refusal: %w", err)
		}
		summary := summaries[accountID]
		summary.Count24h += count
		if lastAt > summary.LastAt {
			summary.LastAt = lastAt
			summary.LastCategory = category
		}
		summaries[accountID] = summary
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query recent account refusals: %w", err)
	}
	return summaries, nil
}
