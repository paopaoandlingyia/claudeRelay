package store

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
)

// UsageCounters contains only billing metadata returned by Anthropic. It never
// contains request or response content.
type UsageCounters struct {
	InputTokens           int64 `json:"input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	CacheCreation5mTokens int64 `json:"cache_creation_5m_tokens"`
	CacheCreation1hTokens int64 `json:"cache_creation_1h_tokens"`
	CacheReadTokens       int64 `json:"cache_read_tokens"`
	Requests              int64 `json:"requests"`
	Incomplete            int64 `json:"incomplete"`
}

func (c *UsageCounters) Add(other UsageCounters) {
	c.InputTokens += other.InputTokens
	c.OutputTokens += other.OutputTokens
	c.CacheCreation5mTokens += other.CacheCreation5mTokens
	c.CacheCreation1hTokens += other.CacheCreation1hTokens
	c.CacheReadTokens += other.CacheReadTokens
	c.Requests += other.Requests
	c.Incomplete += other.Incomplete
}

type UsageBucket struct {
	BucketStart int64         `json:"bucket_start"`
	AccountID   int64         `json:"account_id"`
	Account     string        `json:"account,omitempty"`
	Model       string        `json:"model"`
	Counters    UsageCounters `json:"usage"`
}

type ModelPrice struct {
	ID                        int64   `json:"id"`
	ModelPattern              string  `json:"model_pattern"`
	EffectiveFrom             int64   `json:"effective_from"`
	InputUSDPerMTok           float64 `json:"input_usd_per_mtok"`
	OutputUSDPerMTok          float64 `json:"output_usd_per_mtok"`
	CacheCreation5mUSDPerMTok float64 `json:"cache_creation_5m_usd_per_mtok"`
	CacheCreation1hUSDPerMTok float64 `json:"cache_creation_1h_usd_per_mtok"`
	CacheReadUSDPerMTok       float64 `json:"cache_read_usd_per_mtok"`
	Source                    string  `json:"source,omitempty"`
	CreatedAt                 int64   `json:"created_at"`
}

var defaultModelPrices = []ModelPrice{
	{ModelPattern: "claude-opus-5*", InputUSDPerMTok: 5, OutputUSDPerMTok: 25, CacheCreation5mUSDPerMTok: 6.25, CacheCreation1hUSDPerMTok: 10, CacheReadUSDPerMTok: .5},
	{ModelPattern: "claude-opus-4-8*", InputUSDPerMTok: 5, OutputUSDPerMTok: 25, CacheCreation5mUSDPerMTok: 6.25, CacheCreation1hUSDPerMTok: 10, CacheReadUSDPerMTok: .5},
	{ModelPattern: "claude-opus-4-7*", InputUSDPerMTok: 5, OutputUSDPerMTok: 25, CacheCreation5mUSDPerMTok: 6.25, CacheCreation1hUSDPerMTok: 10, CacheReadUSDPerMTok: .5},
	{ModelPattern: "claude-opus-4-6*", InputUSDPerMTok: 5, OutputUSDPerMTok: 25, CacheCreation5mUSDPerMTok: 6.25, CacheCreation1hUSDPerMTok: 10, CacheReadUSDPerMTok: .5},
	{ModelPattern: "claude-opus-4-5*", InputUSDPerMTok: 5, OutputUSDPerMTok: 25, CacheCreation5mUSDPerMTok: 6.25, CacheCreation1hUSDPerMTok: 10, CacheReadUSDPerMTok: .5},
	{ModelPattern: "claude-sonnet-5*", InputUSDPerMTok: 2, OutputUSDPerMTok: 10, CacheCreation5mUSDPerMTok: 2.5, CacheCreation1hUSDPerMTok: 4, CacheReadUSDPerMTok: .2},
	{ModelPattern: "claude-sonnet-4-6*", InputUSDPerMTok: 3, OutputUSDPerMTok: 15, CacheCreation5mUSDPerMTok: 3.75, CacheCreation1hUSDPerMTok: 6, CacheReadUSDPerMTok: .3},
	{ModelPattern: "claude-sonnet-4-5*", InputUSDPerMTok: 3, OutputUSDPerMTok: 15, CacheCreation5mUSDPerMTok: 3.75, CacheCreation1hUSDPerMTok: 6, CacheReadUSDPerMTok: .3},
	{ModelPattern: "claude-haiku-4-5*", InputUSDPerMTok: 1, OutputUSDPerMTok: 5, CacheCreation5mUSDPerMTok: 1.25, CacheCreation1hUSDPerMTok: 2, CacheReadUSDPerMTok: .1},
}

func (s *Store) insertDefaultModelPrices(ctx context.Context) error {
	now := time.Now().Unix()
	for _, price := range defaultModelPrices {
		if price.EffectiveFrom == 0 {
			price.EffectiveFrom = 1
		}
		if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO model_prices(
			model_pattern,effective_from,input_usd_per_mtok,output_usd_per_mtok,
			cache_creation_5m_usd_per_mtok,cache_creation_1h_usd_per_mtok,cache_read_usd_per_mtok,source,created_at)
			VALUES(?,?,?,?,?,?,?,?,?)`, price.ModelPattern, price.EffectiveFrom, price.InputUSDPerMTok,
			price.OutputUSDPerMTok, price.CacheCreation5mUSDPerMTok, price.CacheCreation1hUSDPerMTok,
			price.CacheReadUSDPerMTok, "Anthropic API pricing", now); err != nil {
			return fmt.Errorf("insert default model price: %w", err)
		}
	}
	return nil
}

func (s *Store) AddUsageBuckets(ctx context.Context, buckets []UsageBucket) error {
	if len(buckets) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin usage batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	statement, err := tx.PrepareContext(ctx, `INSERT INTO usage_hourly(
		bucket_start,account_id,model,input_tokens,output_tokens,cache_creation_5m_tokens,
		cache_creation_1h_tokens,cache_read_tokens,request_count,incomplete_count)
		VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(bucket_start,account_id,model) DO UPDATE SET
		input_tokens=input_tokens+excluded.input_tokens,output_tokens=output_tokens+excluded.output_tokens,
		cache_creation_5m_tokens=cache_creation_5m_tokens+excluded.cache_creation_5m_tokens,
		cache_creation_1h_tokens=cache_creation_1h_tokens+excluded.cache_creation_1h_tokens,
		cache_read_tokens=cache_read_tokens+excluded.cache_read_tokens,
		request_count=request_count+excluded.request_count,incomplete_count=incomplete_count+excluded.incomplete_count`)
	if err != nil {
		return fmt.Errorf("prepare usage batch: %w", err)
	}
	defer statement.Close()
	for _, bucket := range buckets {
		c := bucket.Counters
		if _, err := statement.ExecContext(ctx, bucket.BucketStart, bucket.AccountID, bucket.Model,
			c.InputTokens, c.OutputTokens, c.CacheCreation5mTokens, c.CacheCreation1hTokens,
			c.CacheReadTokens, c.Requests, c.Incomplete); err != nil {
			return fmt.Errorf("write usage batch: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit usage batch: %w", err)
	}
	return nil
}

func (s *Store) UsageBuckets(ctx context.Context, since int64) ([]UsageBucket, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT u.bucket_start,u.account_id,a.alias,u.model,
		u.input_tokens,u.output_tokens,u.cache_creation_5m_tokens,u.cache_creation_1h_tokens,
		u.cache_read_tokens,u.request_count,u.incomplete_count FROM usage_hourly u
		JOIN accounts a ON a.id=u.account_id WHERE u.bucket_start>=? ORDER BY u.bucket_start,u.account_id,u.model`, since)
	if err != nil {
		return nil, fmt.Errorf("query usage buckets: %w", err)
	}
	defer rows.Close()
	var buckets []UsageBucket
	for rows.Next() {
		var bucket UsageBucket
		c := &bucket.Counters
		if err := rows.Scan(&bucket.BucketStart, &bucket.AccountID, &bucket.Account, &bucket.Model,
			&c.InputTokens, &c.OutputTokens, &c.CacheCreation5mTokens, &c.CacheCreation1hTokens,
			&c.CacheReadTokens, &c.Requests, &c.Incomplete); err != nil {
			return nil, fmt.Errorf("scan usage bucket: %w", err)
		}
		buckets = append(buckets, bucket)
	}
	return buckets, rows.Err()
}

func (s *Store) UsageTotalsByModel(ctx context.Context, accountID int64) (map[string]UsageCounters, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT model,SUM(input_tokens),SUM(output_tokens),
		SUM(cache_creation_5m_tokens),SUM(cache_creation_1h_tokens),SUM(cache_read_tokens),
		SUM(request_count),SUM(incomplete_count) FROM usage_hourly WHERE account_id=? GROUP BY model`, accountID)
	if err != nil {
		return nil, fmt.Errorf("query usage totals: %w", err)
	}
	defer rows.Close()
	totals := make(map[string]UsageCounters)
	for rows.Next() {
		var model string
		var c UsageCounters
		if err := rows.Scan(&model, &c.InputTokens, &c.OutputTokens, &c.CacheCreation5mTokens,
			&c.CacheCreation1hTokens, &c.CacheReadTokens, &c.Requests, &c.Incomplete); err != nil {
			return nil, fmt.Errorf("scan usage totals: %w", err)
		}
		totals[model] = c
	}
	return totals, rows.Err()
}

func (s *Store) ModelPrices(ctx context.Context) ([]ModelPrice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,model_pattern,effective_from,input_usd_per_mtok,
		output_usd_per_mtok,cache_creation_5m_usd_per_mtok,cache_creation_1h_usd_per_mtok,
		cache_read_usd_per_mtok,source,created_at FROM model_prices ORDER BY model_pattern,effective_from DESC`)
	if err != nil {
		return nil, fmt.Errorf("query model prices: %w", err)
	}
	defer rows.Close()
	var prices []ModelPrice
	for rows.Next() {
		var price ModelPrice
		if err := rows.Scan(&price.ID, &price.ModelPattern, &price.EffectiveFrom, &price.InputUSDPerMTok,
			&price.OutputUSDPerMTok, &price.CacheCreation5mUSDPerMTok, &price.CacheCreation1hUSDPerMTok,
			&price.CacheReadUSDPerMTok, &price.Source, &price.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan model price: %w", err)
		}
		prices = append(prices, price)
	}
	return prices, rows.Err()
}

func validatePrice(price ModelPrice) error {
	price.ModelPattern = strings.TrimSpace(price.ModelPattern)
	if price.ModelPattern == "" || len(price.ModelPattern) > 128 || strings.Count(price.ModelPattern, "*") > 1 ||
		(strings.Contains(price.ModelPattern, "*") && !strings.HasSuffix(price.ModelPattern, "*")) {
		return fmt.Errorf("model_pattern must be an exact model ID or a prefix ending in *")
	}
	values := []float64{price.InputUSDPerMTok, price.OutputUSDPerMTok, price.CacheCreation5mUSDPerMTok,
		price.CacheCreation1hUSDPerMTok, price.CacheReadUSDPerMTok}
	for _, value := range values {
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("prices must be finite non-negative numbers")
		}
	}
	return nil
}

func (s *Store) SaveModelPrice(ctx context.Context, price ModelPrice) (ModelPrice, error) {
	price.ModelPattern = strings.TrimSpace(price.ModelPattern)
	price.Source = strings.TrimSpace(price.Source)
	if price.EffectiveFrom == 0 {
		price.EffectiveFrom = time.Now().Unix()
	}
	if err := validatePrice(price); err != nil {
		return ModelPrice{}, err
	}
	price.CreatedAt = time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `INSERT INTO model_prices(model_pattern,effective_from,
		input_usd_per_mtok,output_usd_per_mtok,cache_creation_5m_usd_per_mtok,
		cache_creation_1h_usd_per_mtok,cache_read_usd_per_mtok,source,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		price.ModelPattern, price.EffectiveFrom, price.InputUSDPerMTok, price.OutputUSDPerMTok,
		price.CacheCreation5mUSDPerMTok, price.CacheCreation1hUSDPerMTok, price.CacheReadUSDPerMTok,
		price.Source, price.CreatedAt)
	if err != nil {
		return ModelPrice{}, fmt.Errorf("save model price: %w", err)
	}
	price.ID, _ = result.LastInsertId()
	return price, nil
}

func (s *Store) ClearUsageAccounting(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin clear usage accounting: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM usage_hourly`); err != nil {
		return fmt.Errorf("clear usage accounting: %w", err)
	}
	return tx.Commit()
}
