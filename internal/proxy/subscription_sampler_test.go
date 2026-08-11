package proxy

import (
	"net/http"
	"testing"
)

// The OAuth usage surface recomputes its reset timestamp on every read. These
// four values came from four consecutive reads of one account inside a single
// five-hour window: their raw strings never repeat, so comparing them verbatim
// rejects every pair that belongs to the same window. The rate-limit header
// reports the same instant as a plain epoch second and has to agree with them.
func TestNormalizeResetIdentityCollapsesObservedJitter(t *testing.T) {
	t.Parallel()
	identities := make(map[string]bool)
	for _, raw := range []string{
		"2026-08-10T18:30:00.458118+00:00",
		"2026-08-10T18:30:00.814173+00:00",
		"2026-08-10T18:30:00.876031+00:00",
		"2026-08-10T18:30:00.800263+00:00",
		"1786386600",
	} {
		identities[normalizeResetIdentity(raw)] = true
	}
	if len(identities) != 1 {
		t.Fatalf("five readings of one window produced %d identities: %v", len(identities), identities)
	}
}

func TestParseFiveHourWindow(t *testing.T) {
	t.Parallel()
	headers := http.Header{}
	headers.Set(fiveHourResetHeader, "1786386600")
	headers.Set(fiveHourUtilizationHeader, "0.31")
	// The header carries a 0..1 fraction while snapshots store 0..100.
	if resetsAt, usedPercent, ok := parseFiveHourWindow(headers); !ok || resetsAt != "1786386600" || usedPercent != 31 {
		t.Fatalf("resetsAt=%q usedPercent=%v ok=%v", resetsAt, usedPercent, ok)
	}
	// An expired window reports nothing until the next request opens a new one,
	// and that vacancy must not be recorded as a window sitting at zero.
	headers.Del(fiveHourUtilizationHeader)
	if _, _, ok := parseFiveHourWindow(headers); ok {
		t.Fatal("a vacant window produced a sample")
	}
}

// Repeats are dropped and changes are kept however fast they arrive. Traffic
// that lands in one burst still has to leave more than one reading behind, or
// the window it belongs to holds nothing that can be measured.
func TestSubscriptionSamplerDropsRepeatsAndKeepsChanges(t *testing.T) {
	t.Parallel()
	account := newSubscriptionSampler().forAccount(1)
	store := func(window string, percent float64, observedAt int64) {
		account.mark.Store(&sampleMark{window: window, percent: percent, observedAt: observedAt})
	}
	if !account.differs("a", 10) {
		t.Fatal("the first response of a window was not sampled")
	}
	store("a", 10, 100)
	if account.differs("a", 10) {
		t.Fatal("an unchanged reading was sampled again")
	}
	if !account.differs("a", 11) || !account.differs("b", 10) {
		t.Fatal("a changed reading or a new window was suppressed")
	}
	if !newSubscriptionSampler().forAccount(2).differs("a", 10) {
		t.Fatal("one account suppressed another")
	}
}

// Sampling goroutines racing inside one account can reach the store out of
// order. A reading overtaken by a later one must not be written, because the
// counters it would be paired with already include the later one's traffic.
func TestSubscriptionSamplerRejectsOvertakenReadings(t *testing.T) {
	t.Parallel()
	account := newSubscriptionSampler().forAccount(1)
	account.mark.Store(&sampleMark{window: "a", percent: 32, observedAt: 200})
	if account.storable("a", 31, 100) {
		t.Fatal("a reading observed before the stored one was accepted")
	}
	if !account.storable("a", 33, 300) {
		t.Fatal("a reading observed after the stored one was rejected")
	}
	// A new window starts over, so its first reading is always storable.
	if !account.storable("b", 1, 50) {
		t.Fatal("the anchor of a new window was rejected")
	}
}
