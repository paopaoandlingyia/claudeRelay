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
	sampler := newSubscriptionSampler()
	cases := []struct {
		name    string
		window  string
		percent float64
		want    bool
	}{
		{"first response of a window", "a", 10, true},
		{"unchanged reading", "a", 10, false},
		{"changed reading in the same burst", "a", 11, true},
		{"changed again in the same burst", "a", 12, true},
		{"unchanged reading again", "a", 12, false},
		{"anchor of a new window", "b", 2, true},
	}
	for _, testCase := range cases {
		if got := sampler.due(1, testCase.window, testCase.percent); got != testCase.want {
			t.Fatalf("%s: due = %v, want %v", testCase.name, got, testCase.want)
		}
	}
	if !sampler.due(2, "a", 10) {
		t.Fatal("one account suppressed another")
	}
}
