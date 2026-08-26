package proxy

import (
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
	// observations use 0..100. The header's own value moves in whole percent,
	// so rounding after the conversion keeps the residue of the multiplication
	// from reaching an estimate as its denominator.
	return fiveHourReading{
		resetsAt:    reset,
		usedPercent: math.Round(math.Max(0, math.Min(100, fraction*100))),
		observedAt:  now.UnixMilli(),
	}, true
}

type accountSampler struct {
	observed atomic.Pointer[fiveHourReading]
}

type subscriptionSampler struct {
	accounts sync.Map
}

func newSubscriptionSampler(_ int) *subscriptionSampler {
	return &subscriptionSampler{}
}

func (a *accountSampler) latest() *fiveHourReading {
	return a.observed.Load()
}

// observe moves the live view forward as soon as response headers arrive.
// Persistence still waits for the response body so its usage counters include
// the request that produced this reading.
func (a *accountSampler) observe(reading fiveHourReading) {
	for {
		previous := a.observed.Load()
		if previous != nil && reading.observedAt <= previous.observedAt {
			return
		}
		current := reading
		if a.observed.CompareAndSwap(previous, &current) {
			return
		}
	}
}

func (s *subscriptionSampler) observe(account store.Account, reading fiveHourReading) {
	s.forAccount(account).observe(reading)
}

// current returns only a reading whose five-hour window is still active. A
// response from the previous window must disappear from the console at reset
// rather than looking like the current account balance.
func (s *subscriptionSampler) current(account store.Account, now time.Time) (fiveHourReading, bool) {
	value, ok := s.accounts.Load(accountUsageCacheKey(account))
	if !ok {
		return fiveHourReading{}, false
	}
	reading := value.(*accountSampler).latest()
	if reading == nil {
		return fiveHourReading{}, false
	}
	reset, err := strconv.ParseInt(reading.resetsAt, 10, 64)
	if err != nil || !now.Before(time.Unix(reset, 0)) {
		return fiveHourReading{}, false
	}
	return *reading, true
}

// forAccount keys state by the account's own identity rather than its database
// row, because SQLite reuses a row id once an account is deleted.
func (s *subscriptionSampler) forAccount(account store.Account) *accountSampler {
	value, _ := s.accounts.LoadOrStore(accountUsageCacheKey(account), &accountSampler{})
	return value.(*accountSampler)
}

// forget drops an account's live sampling state so a reimport starts clean and
// the map does not keep an entry for every account the relay has ever served.
func (s *subscriptionSampler) forget(account store.Account) {
	s.accounts.Delete(accountUsageCacheKey(account))
}
