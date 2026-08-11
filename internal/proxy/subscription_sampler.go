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

// parseFiveHourWindow reads the unified rate-limit headers. Absent or unparsable
// headers yield no sample rather than an invented zero.
func parseFiveHourWindow(headers http.Header) (resetsAt string, usedPercent float64, ok bool) {
	reset := normalizeResetIdentity(headers.Get(fiveHourResetHeader))
	raw := strings.TrimSpace(headers.Get(fiveHourUtilizationHeader))
	if reset == "" || raw == "" {
		return "", 0, false
	}
	fraction, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(fraction) || math.IsInf(fraction, 0) {
		return "", 0, false
	}
	// The header reports a 0..1 fraction while the OAuth surface and stored
	// snapshots both use 0..100.
	return reset, math.Max(0, math.Min(100, fraction*100)), true
}

type sampleMark struct {
	window     string
	percent    float64
	observedAt int64
}

// accountSampler serializes one account's sampling. The relay allows several
// requests per account in flight at once, so without this their sampling
// goroutines would reach the database in whatever order they happened to finish.
type accountSampler struct {
	mu   sync.Mutex
	mark atomic.Pointer[sampleMark]
}

type subscriptionSampler struct {
	accounts sync.Map
}

func newSubscriptionSampler() *subscriptionSampler {
	return &subscriptionSampler{}
}

func (s *subscriptionSampler) forAccount(accountID int64) *accountSampler {
	value, _ := s.accounts.LoadOrStore(accountID, &accountSampler{})
	return value.(*accountSampler)
}

// differs reports whether a reading says anything the stored one does not.
// Utilization is reported in whole percent, so a busy account repeats a value
// for minutes at a time while a window can never present more than a hundred
// distinct ones; that alone bounds how often this writes.
//
// The lock-free read only decides whether starting a goroutine is worthwhile.
// storable repeats the question under the account's lock, which is where the
// answer is authoritative.
func (a *accountSampler) differs(resetsAt string, usedPercent float64) bool {
	previous := a.mark.Load()
	return previous == nil || previous.window != resetsAt || previous.percent != usedPercent
}

// storable additionally rejects a reading that a later one has already
// overtaken. Goroutines racing inside one account can reach this point out of
// order, and an older reading arriving late would otherwise be written with
// counters that already include the traffic of the newer one.
//
// Time is compared before the window, so the mark can only ever move forwards.
// Asking about the window first would wave through a stale reading of the
// previous window on the strength of its window alone, and drag the mark back
// to that window so the current one is sampled again from scratch.
func (a *accountSampler) storable(resetsAt string, usedPercent float64, observedAt int64) bool {
	previous := a.mark.Load()
	if previous == nil {
		return true
	}
	if observedAt <= previous.observedAt {
		return false
	}
	if previous.window != resetsAt {
		return true
	}
	return previous.percent != usedPercent
}

// sampleFiveHourWindow records the serving account's five-hour window from the
// upstream response headers. It issues no upstream request of its own, so it
// neither touches the private OAuth surface nor drives token rotation, and the
// database work runs off the request goroutine.
func (s *Server) sampleFiveHourWindow(accountID int64, headers http.Header) {
	// An expired window leaves no five-hour headers until the next request opens a
	// new one. That vacancy is recorded as nothing rather than as a zeroed window.
	resetsAt, usedPercent, ok := parseFiveHourWindow(headers)
	if !ok || accountID <= 0 {
		return
	}
	account := s.sampler.forAccount(accountID)
	if !account.differs(resetsAt, usedPercent) {
		return
	}
	// Stamped on the request goroutine, because the reading belongs to this
	// response. Taking the time inside the sampling goroutine would let one that
	// overtakes another present a later reading as the window's earliest.
	observedAt := time.Now().UnixMilli()
	go func() {
		account.mu.Lock()
		defer account.mu.Unlock()
		if !account.storable(resetsAt, usedPercent, observedAt) {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), sampleWriteTimeout)
		defer cancel()
		// The snapshot stores cumulative relay counters, so pending in-memory
		// usage has to reach the database before the reading is taken. Holding
		// the account's lock across both keeps that cut from interleaving with
		// another sample of the same account.
		if err := s.accounting.Flush(ctx); err != nil {
			slog.Warn("flush before subscription sample", "account_id", accountID, "error", err)
			return
		}
		if err := s.store.CaptureSubscriptionUsageSnapshot(ctx, accountID, observedAt, resetsAt, usedPercent); err != nil {
			slog.Warn("capture subscription sample", "account_id", accountID, "error", err)
			return
		}
		// Recorded only once the write has landed, so a failure leaves the
		// reading to be retried by the next response still carrying it.
		account.mark.Store(&sampleMark{window: resetsAt, percent: usedPercent, observedAt: observedAt})
		slog.Info("subscription window sampled", "account_id", accountID, "resets_at", resetsAt, "used_percent", usedPercent)
	}()
}
