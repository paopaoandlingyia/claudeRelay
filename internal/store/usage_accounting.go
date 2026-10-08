package store

import (
	"context"
	"fmt"
	"log/slog"
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

const (
	LongContextInputTokens     int64 = 100_000
	LongContextPriceMultiplier       = 5
)

// HasLongContext counts the entire prompt, including cache hits and writes.
// Compare by subtraction so even malformed large counters cannot overflow.
func (c UsageCounters) HasLongContext() bool {
	remaining := LongContextInputTokens
	for _, tokens := range []int64{c.InputTokens, c.CacheCreation5mTokens, c.CacheCreation1hTokens, c.CacheReadTokens} {
		if tokens > remaining {
			return true
		}
		remaining -= tokens
	}
	return false
}

// UsesLongContextPricing identifies the published Haiku 5.5 prompt-length rule.
// Stored price versions represent the lower tier; all five rates are 5x above
// 100k prompt tokens. A future model with different tiers needs its own rule.
func UsesLongContextPricing(model string) bool {
	return model == "claude-haiku-5-5" || strings.HasPrefix(model, "claude-haiku-5-5-")
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
	BucketStart          int64         `json:"bucket_start"`
	AccountID            int64         `json:"account_id"`
	Account              string        `json:"account,omitempty"`
	Model                string        `json:"model"`
	Counters             UsageCounters `json:"usage"`
	LongContextCounters  UsageCounters `json:"-"`
	ContextKnownRequests int64         `json:"-"`
}

func (b *UsageBucket) Add(other UsageBucket) {
	b.Counters.Add(other.Counters)
	b.LongContextCounters.Add(other.LongContextCounters)
	b.ContextKnownRequests += other.ContextKnownRequests
}

type UsageIngressBucket struct {
	BucketStart int64
	Ingress     string
	Counters    UsageCounters
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

var defaultOpusFiveFivePrice = ModelPrice{
	ModelPattern: "claude-opus-5-5*", InputUSDPerMTok: 4, OutputUSDPerMTok: 20,
	CacheCreation5mUSDPerMTok: 5, CacheCreation1hUSDPerMTok: 8, CacheReadUSDPerMTok: .2,
}

var defaultSonnetFiveFivePrice = ModelPrice{
	ModelPattern: "claude-sonnet-5-5*", InputUSDPerMTok: 2, OutputUSDPerMTok: 10,
	CacheCreation5mUSDPerMTok: 2.5, CacheCreation1hUSDPerMTok: 4, CacheReadUSDPerMTok: .1,
}

var defaultHaikuFiveFivePrice = ModelPrice{
	ModelPattern: "claude-haiku-5-5*", InputUSDPerMTok: .1, OutputUSDPerMTok: .5,
	CacheCreation5mUSDPerMTok: .125, CacheCreation1hUSDPerMTok: .2, CacheReadUSDPerMTok: .01,
}

var defaultModelPrices = []ModelPrice{
	defaultOpusFiveFivePrice,
	defaultSonnetFiveFivePrice,
	defaultHaikuFiveFivePrice,
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
	for _, price := range defaultModelPrices {
		if err := s.insertDefaultModelPrice(ctx, price); err != nil {
			return fmt.Errorf("insert default model price: %w", err)
		}
	}
	return nil
}

func (s *Store) insertDefaultModelPrice(ctx context.Context, price ModelPrice) error {
	if price.EffectiveFrom == 0 {
		price.EffectiveFrom = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO model_prices(
		model_pattern,effective_from,input_usd_per_mtok,output_usd_per_mtok,
		cache_creation_5m_usd_per_mtok,cache_creation_1h_usd_per_mtok,cache_read_usd_per_mtok,source,created_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, price.ModelPattern, price.EffectiveFrom, price.InputUSDPerMTok,
		price.OutputUSDPerMTok, price.CacheCreation5mUSDPerMTok, price.CacheCreation1hUSDPerMTok,
		price.CacheReadUSDPerMTok, "Anthropic API pricing", time.Now().Unix())
	return err
}

func (s *Store) AddUsageBuckets(ctx context.Context, buckets []UsageBucket) error {
	return s.AddUsageBucketsWithIngress(ctx, buckets, nil)
}

func (s *Store) AddUsageBucketsWithIngress(ctx context.Context, buckets []UsageBucket, ingressBuckets []UsageIngressBucket) error {
	if len(buckets) == 0 && len(ingressBuckets) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin usage batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	statement, err := tx.PrepareContext(ctx, `INSERT INTO usage_hourly(
		bucket_start,account_id,model,input_tokens,output_tokens,cache_creation_5m_tokens,
		cache_creation_1h_tokens,cache_read_tokens,request_count,incomplete_count,
		long_input_tokens,long_output_tokens,long_cache_creation_5m_tokens,
		long_cache_creation_1h_tokens,long_cache_read_tokens,context_known_requests)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(bucket_start,account_id,model) DO UPDATE SET
		input_tokens=input_tokens+excluded.input_tokens,output_tokens=output_tokens+excluded.output_tokens,
		cache_creation_5m_tokens=cache_creation_5m_tokens+excluded.cache_creation_5m_tokens,
		cache_creation_1h_tokens=cache_creation_1h_tokens+excluded.cache_creation_1h_tokens,
		cache_read_tokens=cache_read_tokens+excluded.cache_read_tokens,
		request_count=request_count+excluded.request_count,incomplete_count=incomplete_count+excluded.incomplete_count,
		long_input_tokens=long_input_tokens+excluded.long_input_tokens,
		long_output_tokens=long_output_tokens+excluded.long_output_tokens,
		long_cache_creation_5m_tokens=long_cache_creation_5m_tokens+excluded.long_cache_creation_5m_tokens,
		long_cache_creation_1h_tokens=long_cache_creation_1h_tokens+excluded.long_cache_creation_1h_tokens,
		long_cache_read_tokens=long_cache_read_tokens+excluded.long_cache_read_tokens,
		context_known_requests=context_known_requests+excluded.context_known_requests`)
	if err != nil {
		return fmt.Errorf("prepare usage batch: %w", err)
	}
	defer statement.Close()
	for _, bucket := range buckets {
		c := bucket.Counters
		long := bucket.LongContextCounters
		if bucket.ContextKnownRequests < 0 || bucket.ContextKnownRequests > c.Requests ||
			long.InputTokens < 0 || long.InputTokens > c.InputTokens ||
			long.OutputTokens < 0 || long.OutputTokens > c.OutputTokens ||
			long.CacheCreation5mTokens < 0 || long.CacheCreation5mTokens > c.CacheCreation5mTokens ||
			long.CacheCreation1hTokens < 0 || long.CacheCreation1hTokens > c.CacheCreation1hTokens ||
			long.CacheReadTokens < 0 || long.CacheReadTokens > c.CacheReadTokens {
			return fmt.Errorf("invalid prompt-context counters for account %d model %q", bucket.AccountID, bucket.Model)
		}
		if _, err := statement.ExecContext(ctx, bucket.BucketStart, bucket.AccountID, bucket.Model,
			c.InputTokens, c.OutputTokens, c.CacheCreation5mTokens, c.CacheCreation1hTokens,
			c.CacheReadTokens, c.Requests, c.Incomplete, long.InputTokens, long.OutputTokens,
			long.CacheCreation5mTokens, long.CacheCreation1hTokens, long.CacheReadTokens, bucket.ContextKnownRequests); err != nil {
			return fmt.Errorf("write usage batch: %w", err)
		}
	}
	if len(ingressBuckets) > 0 {
		ingressStatement, err := tx.PrepareContext(ctx, `INSERT INTO usage_ingress_hourly(
			bucket_start,ingress,input_tokens,output_tokens,cache_creation_5m_tokens,
			cache_creation_1h_tokens,cache_read_tokens,request_count,incomplete_count)
			VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(bucket_start,ingress) DO UPDATE SET
			input_tokens=input_tokens+excluded.input_tokens,output_tokens=output_tokens+excluded.output_tokens,
			cache_creation_5m_tokens=cache_creation_5m_tokens+excluded.cache_creation_5m_tokens,
			cache_creation_1h_tokens=cache_creation_1h_tokens+excluded.cache_creation_1h_tokens,
			cache_read_tokens=cache_read_tokens+excluded.cache_read_tokens,
			request_count=request_count+excluded.request_count,incomplete_count=incomplete_count+excluded.incomplete_count`)
		if err != nil {
			return fmt.Errorf("prepare ingress usage batch: %w", err)
		}
		defer ingressStatement.Close()
		for _, bucket := range ingressBuckets {
			c := bucket.Counters
			if _, err := ingressStatement.ExecContext(ctx, bucket.BucketStart, bucket.Ingress,
				c.InputTokens, c.OutputTokens, c.CacheCreation5mTokens, c.CacheCreation1hTokens,
				c.CacheReadTokens, c.Requests, c.Incomplete); err != nil {
				return fmt.Errorf("write ingress usage batch: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit usage batches: %w", err)
	}
	return nil
}

func (s *Store) UsageIngressBuckets(ctx context.Context, since int64) ([]UsageIngressBucket, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT bucket_start,ingress,input_tokens,output_tokens,
		cache_creation_5m_tokens,cache_creation_1h_tokens,cache_read_tokens,request_count,incomplete_count
		FROM usage_ingress_hourly WHERE bucket_start>=? ORDER BY bucket_start,ingress`, since)
	if err != nil {
		return nil, fmt.Errorf("query ingress usage buckets: %w", err)
	}
	defer rows.Close()
	var buckets []UsageIngressBucket
	for rows.Next() {
		var bucket UsageIngressBucket
		c := &bucket.Counters
		if err := rows.Scan(&bucket.BucketStart, &bucket.Ingress, &c.InputTokens, &c.OutputTokens,
			&c.CacheCreation5mTokens, &c.CacheCreation1hTokens, &c.CacheReadTokens,
			&c.Requests, &c.Incomplete); err != nil {
			return nil, fmt.Errorf("scan ingress usage bucket: %w", err)
		}
		buckets = append(buckets, bucket)
	}
	return buckets, rows.Err()
}

func (s *Store) UsageBuckets(ctx context.Context, since int64) ([]UsageBucket, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT u.bucket_start,u.account_id,a.alias,u.model,
		u.input_tokens,u.output_tokens,u.cache_creation_5m_tokens,u.cache_creation_1h_tokens,
		u.cache_read_tokens,u.request_count,u.incomplete_count,u.long_input_tokens,u.long_output_tokens,
		u.long_cache_creation_5m_tokens,u.long_cache_creation_1h_tokens,u.long_cache_read_tokens,u.context_known_requests FROM usage_hourly u
		JOIN accounts a ON a.id=u.account_id WHERE u.bucket_start>=? ORDER BY u.bucket_start,u.account_id,u.model`, since)
	if err != nil {
		return nil, fmt.Errorf("query usage buckets: %w", err)
	}
	defer rows.Close()
	var buckets []UsageBucket
	for rows.Next() {
		var bucket UsageBucket
		c := &bucket.Counters
		long := &bucket.LongContextCounters
		if err := rows.Scan(&bucket.BucketStart, &bucket.AccountID, &bucket.Account, &bucket.Model,
			&c.InputTokens, &c.OutputTokens, &c.CacheCreation5mTokens, &c.CacheCreation1hTokens,
			&c.CacheReadTokens, &c.Requests, &c.Incomplete, &long.InputTokens, &long.OutputTokens,
			&long.CacheCreation5mTokens, &long.CacheCreation1hTokens, &long.CacheReadTokens, &bucket.ContextKnownRequests); err != nil {
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

// backfillLongContextUsage trusts retained request observations only when every
// counter matches the existing hourly row. The relay stores request-start hours
// in usage_hourly but header-observation times in five_hour_events; cross-hour
// requests or cleared events can prevent reconciliation. Leave those rows
// unclassified, log their count, and expose them as unpriced rather than guess.
func (s *Store) backfillLongContextUsage(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin prompt-context backfill: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `WITH requests AS (
		SELECT *, (input_tokens+cache_creation_5m_tokens+cache_creation_1h_tokens+cache_read_tokens)>? AS long_prompt
		FROM five_hour_events WHERE kind='messages' AND usage_seen=1
		AND (model='claude-haiku-5-5' OR model LIKE 'claude-haiku-5-5-%')
	), observed AS (
		SELECT observed_at/3600000*3600 AS hour, account_id, model,
		SUM(input_tokens) AS input, SUM(output_tokens) AS output,
		SUM(cache_creation_5m_tokens) AS write5, SUM(cache_creation_1h_tokens) AS write1,
		SUM(cache_read_tokens) AS reads, COUNT(*) AS requests,
		SUM(CASE WHEN complete=0 THEN 1 ELSE 0 END) AS incomplete,
		SUM(CASE WHEN long_prompt THEN input_tokens ELSE 0 END) AS long_input,
		SUM(CASE WHEN long_prompt THEN output_tokens ELSE 0 END) AS long_output,
		SUM(CASE WHEN long_prompt THEN cache_creation_5m_tokens ELSE 0 END) AS long_write5,
		SUM(CASE WHEN long_prompt THEN cache_creation_1h_tokens ELSE 0 END) AS long_write1,
		SUM(CASE WHEN long_prompt THEN cache_read_tokens ELSE 0 END) AS long_reads
		FROM requests GROUP BY hour,account_id,model
	) SELECT u.bucket_start,u.account_id,u.model,o.long_input,o.long_output,o.long_write5,o.long_write1,o.long_reads,o.requests
		FROM usage_hourly u JOIN observed o ON u.bucket_start=o.hour AND u.account_id=o.account_id AND u.model=o.model
		WHERE u.input_tokens=o.input AND u.output_tokens=o.output AND u.cache_creation_5m_tokens=o.write5
		AND u.cache_creation_1h_tokens=o.write1 AND u.cache_read_tokens=o.reads
		AND u.request_count=o.requests AND u.incomplete_count=o.incomplete`, LongContextInputTokens)
	if err != nil {
		return fmt.Errorf("query retained prompt-context usage: %w", err)
	}
	var buckets []UsageBucket
	for rows.Next() {
		var b UsageBucket
		long := &b.LongContextCounters
		if err := rows.Scan(&b.BucketStart, &b.AccountID, &b.Model, &long.InputTokens, &long.OutputTokens,
			&long.CacheCreation5mTokens, &long.CacheCreation1hTokens, &long.CacheReadTokens, &b.ContextKnownRequests); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan retained prompt-context usage: %w", err)
		}
		buckets = append(buckets, b)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read retained prompt-context usage: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close retained prompt-context usage: %w", err)
	}
	for _, b := range buckets {
		long := b.LongContextCounters
		if _, err := tx.ExecContext(ctx, `UPDATE usage_hourly SET long_input_tokens=?,long_output_tokens=?,
			long_cache_creation_5m_tokens=?,long_cache_creation_1h_tokens=?,long_cache_read_tokens=?,context_known_requests=?
			WHERE bucket_start=? AND account_id=? AND model=?`, long.InputTokens, long.OutputTokens,
			long.CacheCreation5mTokens, long.CacheCreation1hTokens, long.CacheReadTokens, b.ContextKnownRequests,
			b.BucketStart, b.AccountID, b.Model); err != nil {
			return fmt.Errorf("backfill prompt-context usage: %w", err)
		}
	}
	var unknown int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_hourly
		WHERE (model='claude-haiku-5-5' OR model LIKE 'claude-haiku-5-5-%')
		AND context_known_requests<>request_count`).Scan(&unknown); err != nil {
		return fmt.Errorf("count unclassified prompt-context usage: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit prompt-context backfill: %w", err)
	}
	if unknown > 0 {
		slog.Warn("historical Haiku 5.5 usage lacks reconciled prompt sizes; API value remains unpriced", "hourly_rows", unknown)
	}
	return nil
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM usage_ingress_hourly`); err != nil {
		return fmt.Errorf("clear ingress usage accounting: %w", err)
	}
	return tx.Commit()
}
