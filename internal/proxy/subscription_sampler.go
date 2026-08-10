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
	window  string
	percent float64
}

// subscriptionSampler decides which responses are worth a snapshot. Every
// relayed response consults it, so the decision stays on atomics rather than a
// shared lock.
type subscriptionSampler struct {
	accounts sync.Map
}

func newSubscriptionSampler() *subscriptionSampler {
	return &subscriptionSampler{}
}

// due stores a reading only when it differs from the one already held. That is
// the whole rate limit: utilization arrives rounded to whole percent, so a busy
// account repeats a value for minutes at a time while a window can never present
// more than a hundred distinct ones. A clock-based floor on top of this would
// discard changed readings from traffic that arrives in a burst and leave that
// window holding a single reading, which has no delta to measure.
func (s *subscriptionSampler) due(accountID int64, resetsAt string, usedPercent float64) bool {
	value, _ := s.accounts.LoadOrStore(accountID, &atomic.Pointer[sampleMark]{})
	slot := value.(*atomic.Pointer[sampleMark])
	for {
		previous := slot.Load()
		if previous != nil && previous.window == resetsAt && previous.percent == usedPercent {
			return false
		}
		if slot.CompareAndSwap(previous, &sampleMark{window: resetsAt, percent: usedPercent}) {
			return true
		}
	}
}

// sampleFiveHourWindow records the serving account's five-hour window from the
// upstream response headers. It issues no upstream request of its own, so it
// neither touches the private OAuth surface nor drives token rotation, and the
// database work runs off the request goroutine.
func (s *Server) sampleFiveHourWindow(accountID int64, headers http.Header) {
	// An expired window leaves no five-hour headers until the next request opens a
	// new one. That vacancy is recorded as nothing rather than as a zeroed window.
	resetsAt, usedPercent, ok := parseFiveHourWindow(headers)
	if !ok || accountID <= 0 || !s.sampler.due(accountID, resetsAt, usedPercent) {
		return
	}
	// Flushing off the request goroutine lets a concurrent request reach the
	// counters before the cut is taken, so the stored cost can run a request or
	// two ahead of the stored utilization. Both ends of a window carry the same
	// bias and it largely cancels in the difference; what remains is cents
	// against the fraction of a dollar that one rounded percent already hides.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// The snapshot stores cumulative relay counters, so pending in-memory
		// usage has to reach the database before the reading is taken.
		if err := s.accounting.Flush(ctx); err != nil {
			slog.Warn("flush before subscription sample", "account_id", accountID, "error", err)
			return
		}
		if err := s.store.CaptureSubscriptionUsageSnapshot(ctx, accountID, time.Now().UnixMilli(), resetsAt, usedPercent); err != nil {
			slog.Warn("capture subscription sample", "account_id", accountID, "error", err)
			return
		}
		slog.Info("subscription window sampled", "account_id", accountID, "resets_at", resetsAt, "used_percent", usedPercent)
	}()
}
