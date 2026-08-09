package accounting

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/local/claude-relay/internal/store"
)

const FlushInterval = 5 * time.Second

type bucketKey struct {
	bucketStart int64
	accountID   int64
	model       string
}

type Manager struct {
	store   *store.Store
	flushMu sync.Mutex
	mu      sync.Mutex
	pending map[bucketKey]store.UsageCounters
}

func NewManager(database *store.Store) *Manager {
	return &Manager{store: database, pending: make(map[bucketKey]store.UsageCounters)}
}

func (m *Manager) Record(accountID int64, model string, at time.Time, usage Usage) {
	if m == nil || accountID <= 0 || !usage.Seen {
		return
	}
	if model == "" {
		model = "unknown"
	}
	key := bucketKey{bucketStart: at.UTC().Truncate(time.Hour).Unix(), accountID: accountID, model: model}
	delta := store.UsageCounters{
		InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
		CacheCreation5mTokens: usage.CacheCreation5mTokens, CacheCreation1hTokens: usage.CacheCreation1hTokens,
		CacheReadTokens: usage.CacheReadTokens, Requests: 1,
	}
	if !usage.Complete {
		delta.Incomplete = 1
	}
	m.mu.Lock()
	current := m.pending[key]
	current.Add(delta)
	m.pending[key] = current
	m.mu.Unlock()
}

func (m *Manager) Run(ctx context.Context) {
	if m == nil {
		return
	}
	ticker := time.NewTicker(FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			flushCtx, cancel := context.WithTimeout(context.Background(), FlushInterval)
			if err := m.Flush(flushCtx); err != nil {
				slog.Warn("flush usage accounting", "error", err)
			}
			cancel()
		case <-ctx.Done():
			return
		}
	}
}

func (m *Manager) Flush(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.flushMu.Lock()
	defer m.flushMu.Unlock()
	m.mu.Lock()
	batch := m.pending
	m.pending = make(map[bucketKey]store.UsageCounters)
	m.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	buckets := make([]store.UsageBucket, 0, len(batch))
	for key, counters := range batch {
		buckets = append(buckets, store.UsageBucket{BucketStart: key.bucketStart, AccountID: key.accountID, Model: key.model, Counters: counters})
	}
	if err := m.store.AddUsageBuckets(ctx, buckets); err != nil {
		m.mu.Lock()
		for key, counters := range batch {
			current := m.pending[key]
			current.Add(counters)
			m.pending[key] = current
		}
		m.mu.Unlock()
		return fmt.Errorf("persist usage accounting: %w", err)
	}
	return nil
}

func (m *Manager) Clear(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.flushMu.Lock()
	defer m.flushMu.Unlock()
	m.mu.Lock()
	m.pending = make(map[bucketKey]store.UsageCounters)
	m.mu.Unlock()
	return m.store.ClearUsageAccounting(ctx)
}
