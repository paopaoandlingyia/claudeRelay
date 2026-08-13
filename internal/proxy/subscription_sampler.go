package proxy

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/local/claude-relay/internal/store"
)

const (
	// Anthropic reports the unified five-hour window on every Messages response.
	// Unlike the private OAuth usage surface, the reset value is a plain epoch
	// second that stays byte-identical for the whole window.
	fiveHourResetHeader       = "Anthropic-Ratelimit-Unified-5h-Reset"
	fiveHourUtilizationHeader = "Anthropic-Ratelimit-Unified-5h-Utilization"

	sampleWriteTimeout = 10 * time.Second
)

// normalizeResetIdentity reduces a window reset timestamp to the epoch second so
// that two readings of one window compare equal. The OAuth usage surface
// recomputes a microsecond-precision timestamp on every read
// (…18:30:00.458118, …18:30:00.814173), so comparing those strings verbatim
// rejects every pair that belongs to the same window.
func normalizeResetIdentity(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return strconv.FormatInt(seconds, 10)
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return strconv.FormatInt(parsed.Unix(), 10)
		}
	}
	return raw
}

// fiveHourReading is one account's window as a single response reported it.
type fiveHourReading struct {
	resetsAt    string
	usedPercent float64
	observedAt  int64
}

// readFiveHourWindow reads the unified rate limit headers. Absent or unparsable
// headers yield no reading rather than an invented zero.
//
// The caller passes the moment the response headers arrived. A streaming body
// runs for anything up to minutes, so timestamping once the body finished would
// let a response that started earlier be recorded as the later reading of the
// two, which is exactly the ordering the window ends depend on.
func readFiveHourWindow(headers http.Header, now time.Time) (fiveHourReading, bool) {
	reset := normalizeResetIdentity(headers.Get(fiveHourResetHeader))
	raw := strings.TrimSpace(headers.Get(fiveHourUtilizationHeader))
	if reset == "" || raw == "" {
		return fiveHourReading{}, false
	}
	fraction, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(fraction) || math.IsInf(fraction, 0) {
		return fiveHourReading{}, false
	}
	// The header reports a 0..1 fraction while the OAuth surface and stored
	// snapshots both use 0..100.
	return fiveHourReading{
		resetsAt:    reset,
		usedPercent: math.Max(0, math.Min(100, fraction*100)),
		observedAt:  now.UnixMilli(),
	}, true
}

// accountSampler owns one account's sampling. The relay allows several requests
// per account in flight at once, so their readings are funnelled through a
// single writer rather than reaching the database in whatever order they finish.
type accountSampler struct {
	stored            atomic.Pointer[fiveHourReading]
	maxQueuedReadings int

	// queue holds what the writer has yet to store. It is guarded by a lock
	// rather than atomics because storable already filters the repeats without
	// one, leaving this on the rare path where a reading actually changed.
	mu      sync.Mutex
	queue   []fiveHourReading
	writing bool
}

type subscriptionSampler struct {
	accounts          sync.Map
	maxQueuedReadings int
}

func newSubscriptionSampler(maxQueuedReadings int) *subscriptionSampler {
	return &subscriptionSampler{maxQueuedReadings: maxQueuedReadings}
}

// forAccount keys state by the account's own identity rather than its database
// row, because SQLite reuses a row id once an account is deleted.
func (s *subscriptionSampler) forAccount(account store.Account) *accountSampler {
	value, _ := s.accounts.LoadOrStore(accountUsageCacheKey(account), &accountSampler{maxQueuedReadings: s.maxQueuedReadings})
	return value.(*accountSampler)
}

// forget drops an account's sampling state. A deleted account takes its
// snapshots with it, so anything remembered here would suppress the first
// reading of whatever is imported next under the same identity, and without
// this the map would keep an entry for every account the relay has ever served.
func (s *subscriptionSampler) forget(account store.Account) {
	s.accounts.Delete(accountUsageCacheKey(account))
}

// storable reports whether a reading says something the stored one does not.
// Utilization is reported in whole percent, so a busy account repeats a value
// for minutes at a time while a window can never present more than a hundred
// distinct ones; that alone bounds how often this writes.
//
// Time is compared before the window, so the record can only move forwards.
// Asking about the window first would wave through a stale reading of the
// previous window on the strength of its window alone, and drag the record back
// to that window so the current one is sampled again from scratch.
func (a *accountSampler) storable(reading fiveHourReading) bool {
	previous := a.stored.Load()
	if previous == nil {
		return true
	}
	if reading.observedAt <= previous.observedAt {
		return false
	}
	if previous.resetsAt != reading.resetsAt {
		return true
	}
	return previous.usedPercent != reading.usedPercent
}

// enqueue adds a reading and reports whether the caller has to start the writer.
//
// A full queue collapses into its newest reading rather than growing. Readings
// are only dropped once there are more of them than the relay can have responses
// in flight, so this cannot discard the pair of readings a window needs: two
// changed readings of one window, or the last of one window and the first of the
// next, both fit long before the ceiling.
func (a *accountSampler) enqueue(reading fiveHourReading) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	last := len(a.queue) - 1
	switch {
	case last >= 0 && reading.observedAt <= a.queue[last].observedAt:
	case len(a.queue) < a.maxQueuedReadings:
		a.queue = append(a.queue, reading)
	default:
		a.queue[last] = reading
	}
	if a.writing {
		return false
	}
	a.writing = true
	return true
}

// next hands the writer its next reading, releasing the writer once the queue
// has run out so the release and the emptiness are decided together.
func (a *accountSampler) next() (fiveHourReading, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.queue) == 0 {
		a.writing = false
		return fiveHourReading{}, false
	}
	reading := a.queue[0]
	a.queue = a.queue[1:]
	return reading, true
}

func (a *accountSampler) release() {
	a.mu.Lock()
	a.writing = false
	a.mu.Unlock()
}

// sampleFiveHourWindow records the serving account's five-hour window as the
// response reported it. It issues no upstream request of its own, so it neither
// touches the private OAuth usage surface nor drives token rotation, and the
// database work runs off the request goroutine.
func (s *Server) sampleFiveHourWindow(account store.Account, reading fiveHourReading) {
	sampler := s.sampler.forAccount(account)
	if !sampler.storable(reading) || !sampler.enqueue(reading) {
		return
	}
	go s.drainFiveHourSamples(account, sampler)
}

func (s *Server) drainFiveHourSamples(account store.Account, sampler *accountSampler) {
	for {
		reading, ok := sampler.next()
		if !ok {
			return
		}
		if !sampler.storable(reading) {
			continue
		}
		if !s.writeFiveHourSample(account, reading) {
			// The reading stays unrecorded so a later response carrying it
			// retries, and the writer is released rather than held against an
			// account whose writes are failing. Anything still queued is picked
			// up by the response that starts the next writer.
			sampler.release()
			return
		}
		sampler.stored.Store(&reading)
	}
}

func (s *Server) writeFiveHourSample(account store.Account, reading fiveHourReading) bool {
	ctx, cancel := context.WithTimeout(context.Background(), sampleWriteTimeout)
	defer cancel()
	// The snapshot stores cumulative relay counters, so pending in-memory usage
	// has to reach the database before the reading is taken.
	if err := s.accounting.Flush(ctx); err != nil {
		slog.Warn("flush before subscription sample", "account", account.Alias, "error", err)
		return false
	}
	if err := s.store.CaptureSubscriptionUsageSnapshot(ctx, account.ID, reading.observedAt,
		reading.resetsAt, reading.usedPercent); err != nil {
		slog.Warn("capture subscription sample", "account", account.Alias, "error", err)
		return false
	}
	slog.Info("subscription window sampled", "account", account.Alias,
		"resets_at", reading.resetsAt, "used_percent", reading.usedPercent)
	return true
}
