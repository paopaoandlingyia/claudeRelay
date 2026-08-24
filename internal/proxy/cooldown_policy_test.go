package proxy

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/local/claude-relay/internal/store"
)

func TestCooldownPolicyScopesOnlyExplicitlyExhaustedWindows(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	headers := http.Header{}
	headers.Set("anthropic-ratelimit-unified-7d_oi-status", "rejected")
	headers.Set("anthropic-ratelimit-unified-7d_oi-reset", strconv.FormatInt(now.Add(24*time.Hour).Unix(), 10))

	decision, ok := cooldownDecisionForResponse(headers, http.StatusTooManyRequests, "claude-fable", now)
	if !ok || decision.reason != cooldownReasonSevenDayOI || decision.model != "claude-fable" || !decision.until.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("7d_oi decision = %#v ok=%v", decision, ok)
	}

	headers.Set("anthropic-ratelimit-unified-7d-status", "rejected")
	headers.Set("anthropic-ratelimit-unified-7d-reset", strconv.FormatInt(now.Add(48*time.Hour).Unix(), 10))
	decision, ok = cooldownDecisionForResponse(headers, http.StatusTooManyRequests, "claude-fable", now)
	if !ok || decision.reason != cooldownReasonSevenDayExhausted || decision.model != "" || !decision.until.Equal(now.Add(48*time.Hour)) {
		t.Fatalf("account-wide 7d decision = %#v ok=%v", decision, ok)
	}
}

func TestCooldownPolicyUsesRetryAfterBeforeShortFallback(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	headers := http.Header{"Retry-After": []string{"12.5"}}
	decision, ok := cooldownDecisionForResponse(headers, http.StatusTooManyRequests, "claude-test", now)
	if !ok || decision.reason != cooldownReasonRetryAfter429 || decision.model != "claude-test" || !decision.until.Equal(now.Add(12500*time.Millisecond)) {
		t.Fatalf("Retry-After decision = %#v ok=%v", decision, ok)
	}
}

func TestSuccessfulAllowedResponseClearsMatchingCooldowns(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "http://127.0.0.1:1", 4096)
	account, found, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil || !found {
		t.Fatalf("default account = %#v found=%v err=%v", account, found, err)
	}
	model := "claude-test"
	future := time.Now().Add(time.Hour)
	if err := server.store.Cooldown(t.Context(), account.ID, "", future, cooldownReasonFiveHourExhausted); err != nil {
		t.Fatal(err)
	}
	if err := server.store.Cooldown(t.Context(), account.ID, model, future, legacyTooManyRequestsReason); err != nil {
		t.Fatal(err)
	}

	headers := http.Header{}
	headers.Set("anthropic-ratelimit-unified-5h-status", "allowed")
	server.recoverCooldownsFromSuccess(t.Context(), account.ID, model, headers, time.Now())
	if cooling, err := server.store.IsCooling(t.Context(), account.ID, model, time.Now()); err != nil || cooling {
		t.Fatalf("recovered five-hour cooldown cooling=%v err=%v", cooling, err)
	}

	if err := server.store.Cooldown(t.Context(), account.ID, "", future, cooldownReasonSevenDayExhausted); err != nil {
		t.Fatal(err)
	}
	server.recoverCooldownsFromSuccess(t.Context(), account.ID, model, headers, time.Now())
	if cooling, err := server.store.IsCooling(t.Context(), account.ID, model, time.Now()); err != nil || !cooling {
		t.Fatalf("five-hour recovery cleared seven-day cooldown: cooling=%v err=%v", cooling, err)
	}
	headers.Set("anthropic-ratelimit-unified-7d-status", "allowed")
	server.recoverCooldownsFromSuccess(t.Context(), account.ID, model, headers, time.Now())
	if cooling, err := server.store.IsCooling(t.Context(), account.ID, model, time.Now()); err != nil || cooling {
		t.Fatalf("recovered seven-day cooldown cooling=%v err=%v", cooling, err)
	}
}

func TestSuccessfulResponseClearsShortRequestCooldown(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "http://127.0.0.1:1", 4096)
	account, found, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil || !found {
		t.Fatalf("default account = %#v found=%v err=%v", account, found, err)
	}
	if err := server.store.Cooldown(t.Context(), account.ID, "claude-test", time.Now().Add(time.Minute), cooldownReasonAmbiguous429); err != nil {
		t.Fatal(err)
	}
	server.recoverCooldownsFromSuccess(t.Context(), account.ID, "claude-test", http.Header{}, time.Now())
	accounts, err := server.store.Accounts(t.Context(), store.AccountPoolCompatible, "claude-test", time.Now())
	if err != nil || len(accounts) != 1 {
		t.Fatalf("routable accounts=%d err=%v", len(accounts), err)
	}
}
