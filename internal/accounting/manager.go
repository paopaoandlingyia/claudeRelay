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
	store         *store.Store
	flushMu       sync.Mutex
	mu            sync.Mutex
	pending       map[bucketKey]store.UsageCounters
	pendingEvents []store.FiveHourEvent
}

// FiveHourContext ties one Messages response to the quota reading that arrived
// with its headers. ObservedAt and CompletedAt deliberately differ: streaming
// responses can finish minutes after Anthropic identified their quota window.
type FiveHourContext struct {
	EventKey    string
	ResetsAt    string
	ObservedAt  time.Time
	CompletedAt time.Time
	Status      int
	UsedPercent float64
}

func NewManager(database *store.Store) *Manager {
	return &Manager{store: database, pending: make(map[bucketKey]store.UsageCounters)}
}

func (m *Manager) Record(accountID int64, model string, at time.Time, usage Usage, fiveHour FiveHourContext) {
	if m == nil || accountID <= 0 {
		return
	}
	if model == "" {
		model = "unknown"
	}
	m.mu.Lock()
	if usage.Seen {
		key := bucketKey{bucketStart: at.UTC().Truncate(time.Hour).Unix(), accountID: accountID, model: model}
		delta := store.UsageCounters{
			InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
			CacheCreation5mTokens: usage.CacheCreation5mTokens, CacheCreation1hTokens: usage.CacheCreation1hTokens,
			CacheReadTokens: usage.CacheReadTokens, Requests: 1,
		}
		if !usage.Complete {
			delta.Incomplete = 1
		}
		current := m.pending[key]
		current.Add(delta)
		m.pending[key] = current
	}
	if fiveHour.EventKey != "" {
		observedAt := fiveHour.ObservedAt
		if observedAt.IsZero() {
			observedAt = at
		}
		completedAt := fiveHour.CompletedAt
		if completedAt.IsZero() {
			completedAt = observedAt
		}
		m.pendingEvents = append(m.pendingEvents, store.FiveHourEvent{
			EventKey: fiveHour.EventKey, AccountID: accountID, ResetsAt: fiveHour.ResetsAt,
			Kind: store.FiveHourEventMessages, ObservedAt: observedAt.UnixMilli(),
			CompletedAt: completedAt.UnixMilli(), Model: model, Status: fiveHour.Status,
			UsedPercent: fiveHour.UsedPercent,
			Usage: store.UsageCounters{
				InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
				CacheCreation5mTokens: usage.CacheCreation5mTokens,
				CacheCreation1hTokens: usage.CacheCreation1hTokens, CacheReadTokens: usage.CacheReadTokens,
				Requests: 1,
			},
			UsageSeen: usage.Seen, Complete: usage.Complete,
		})
	}
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
	events := m.pendingEvents
	m.pendingEvents = nil
	m.mu.Unlock()
	if len(batch) == 0 && len(events) == 0 {
		return nil
	}
	if err := m.store.AddFiveHourEvents(ctx, events); err != nil {
		m.mu.Lock()
		m.pendingEvents = append(events, m.pendingEvents...)
		for key, counters := range batch {
			current := m.pending[key]
			current.Add(counters)
			m.pending[key] = current
		}
		m.mu.Unlock()
		return fmt.Errorf("persist five-hour observations: %w", err)
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

// ClearFiveHourObservations serializes deletion with Flush so an event that was
// already detached into a batch cannot reappear immediately after the clear.
func (m *Manager) ClearFiveHourObservations(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.flushMu.Lock()
	defer m.flushMu.Unlock()
	m.mu.Lock()
	m.pendingEvents = nil
	m.mu.Unlock()
	return m.store.ClearFiveHourObservations(ctx)
}
