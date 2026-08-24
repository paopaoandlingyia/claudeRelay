package proxy

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/local/claude-relay/internal/store"
)

const (
	ambiguous429Cooldown = 5 * time.Second

	cooldownReasonFiveHourExhausted = "anthropic_5h_window_exhausted"
	cooldownReasonSevenDayExhausted = "anthropic_7d_window_exhausted"
	cooldownReasonSevenDayOI        = "anthropic_7d_oi_window_exhausted"
	cooldownReasonRetryAfter429     = "anthropic_429_retry_after"
	cooldownReasonAmbiguous429      = "anthropic_429_ambiguous"
	legacyTooManyRequestsReason     = "Too Many Requests"
)

type accountCooldownDecision struct {
	model  string
	until  time.Time
	reason string
}

// cooldownDecisionForResponse deliberately separates failover from account
// health. A 529 or generic 5xx may be request-scoped or a service-wide outage,
// so it can trigger the bounded failover without poisoning account scheduling.
// A 429 receives a window-length cooldown only when its headers explicitly say
// that a quota window is exhausted.
func cooldownDecisionForResponse(headers http.Header, status int, requestedModel string, now time.Time) (accountCooldownDecision, bool) {
	if status != http.StatusTooManyRequests {
		return accountCooldownDecision{}, false
	}
	if decision, ok := exhaustedAnthropicWindow(headers, requestedModel, now); ok {
		return decision, true
	}
	if delay, ok := retryAfterHeader(headers.Get("Retry-After"), now); ok {
		return accountCooldownDecision{
			model:  requestedModel,
			until:  now.Add(delay),
			reason: cooldownReasonRetryAfter429,
		}, true
	}
	return accountCooldownDecision{
		model:  requestedModel,
		until:  now.Add(ambiguous429Cooldown),
		reason: cooldownReasonAmbiguous429,
	}, true
}

func exhaustedAnthropicWindow(headers http.Header, requestedModel string, now time.Time) (accountCooldownDecision, bool) {
	type candidate struct {
		until  time.Time
		reason string
	}
	var global *candidate
	for _, window := range []struct {
		name   string
		maxAge time.Duration
		reason string
	}{
		{name: "5h", maxAge: 6 * time.Hour, reason: cooldownReasonFiveHourExhausted},
		{name: "7d", maxAge: 8 * 24 * time.Hour, reason: cooldownReasonSevenDayExhausted},
	} {
		if !anthropicWindowExhausted(headers, window.name) {
			continue
		}
		reset, ok := anthropicWindowReset(headers, window.name, now, window.maxAge)
		if !ok {
			reset, ok = anthropicAggregateReset(headers, now, window.maxAge)
		}
		if !ok {
			continue
		}
		if global == nil || reset.After(global.until) {
			global = &candidate{until: reset, reason: window.reason}
		}
	}
	if global != nil {
		return accountCooldownDecision{until: global.until, reason: global.reason}, true
	}

	if anthropicWindowExhausted(headers, "7d_oi") {
		reset, ok := anthropicWindowReset(headers, "7d_oi", now, 8*24*time.Hour)
		if !ok {
			reset, ok = anthropicAggregateReset(headers, now, 8*24*time.Hour)
		}
		if ok {
			return accountCooldownDecision{
				model:  requestedModel,
				until:  reset,
				reason: cooldownReasonSevenDayOI,
			}, true
		}
	}
	return accountCooldownDecision{}, false
}

func anthropicWindowExhausted(headers http.Header, window string) bool {
	prefix := "anthropic-ratelimit-unified-" + window + "-"
	if strings.EqualFold(strings.TrimSpace(headers.Get(prefix+"status")), "rejected") ||
		strings.EqualFold(strings.TrimSpace(headers.Get(prefix+"surpassed-threshold")), "true") {
		return true
	}
	utilization, err := strconv.ParseFloat(strings.TrimSpace(headers.Get(prefix+"utilization")), 64)
	return err == nil && !math.IsNaN(utilization) && utilization >= 1-1e-9
}

func anthropicWindowReset(headers http.Header, window string, now time.Time, maxAge time.Duration) (time.Time, bool) {
	return parseAnthropicReset(headers.Get("anthropic-ratelimit-unified-"+window+"-reset"), now, maxAge)
}

func anthropicAggregateReset(headers http.Header, now time.Time, maxAge time.Duration) (time.Time, bool) {
	return parseAnthropicReset(headers.Get("anthropic-ratelimit-unified-reset"), now, maxAge)
}

func parseAnthropicReset(raw string, now time.Time, maxAge time.Duration) (time.Time, bool) {
	seconds, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	if seconds > 1e11 {
		seconds /= 1000
	}
	reset := time.Unix(seconds, 0)
	if !reset.After(now) || reset.After(now.Add(maxAge)) {
		return time.Time{}, false
	}
	return reset, true
}

func (s *Server) recoverCooldownsFromSuccess(ctx context.Context, accountID int64, model string, headers http.Header, observedAt time.Time) {
	// Any accepted request proves that the short request-scoped avoidance is no
	// longer useful. Reason and observation matching cannot erase a newer
	// hard-window decision written by another in-flight response.
	matches := []store.CooldownMatch{
		{Model: model, Reason: cooldownReasonAmbiguous429},
		{Model: model, Reason: cooldownReasonRetryAfter429},
	}

	if anthropicWindowAllowed(headers, "5h") {
		matches = append(matches, store.CooldownMatch{Model: "", Reason: cooldownReasonFiveHourExhausted})
		// Builds before the cooldown-policy split stored every 429 under the
		// requested model with only the HTTP status text as its reason.
		matches = append(matches, store.CooldownMatch{Model: model, Reason: legacyTooManyRequestsReason})
	}
	if anthropicWindowAllowed(headers, "7d") {
		matches = append(matches, store.CooldownMatch{Model: "", Reason: cooldownReasonSevenDayExhausted})
	}
	if anthropicWindowAllowed(headers, "7d_oi") {
		matches = append(matches, store.CooldownMatch{Model: model, Reason: cooldownReasonSevenDayOI})
	}
	cleared, err := s.store.ClearCooldownMatchesObservedBefore(ctx, accountID, matches, observedAt)
	if err != nil {
		slog.Warn("clear recovered account cooldowns", "account_id", accountID, "model", model, "error", err)
		return
	}
	if cleared > 0 {
		slog.Info("account cooldowns recovered", "account_id", accountID, "model", model, "count", cleared)
	}
}

func anthropicWindowAllowed(headers http.Header, window string) bool {
	return strings.EqualFold(strings.TrimSpace(headers.Get("anthropic-ratelimit-unified-"+window+"-status")), "allowed")
}
