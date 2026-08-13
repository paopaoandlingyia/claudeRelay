package proxy

import (
	"net/http"
	"testing"
	"time"

	"github.com/local/claude-relay/internal/credential"
	"github.com/local/claude-relay/internal/store"
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

func TestReadFiveHourWindow(t *testing.T) {
	t.Parallel()
	headers := http.Header{}
	headers.Set(fiveHourResetHeader, "1786386600")
	headers.Set(fiveHourUtilizationHeader, "0.31")
	now := time.Unix(1786386000, 0)
	reading, ok := readFiveHourWindow(headers, now)
	// The header carries a 0..1 fraction while snapshots store 0..100, and the
	// reading is timestamped by its caller rather than when it is written.
	if !ok || reading.resetsAt != "1786386600" || reading.usedPercent != 31 || reading.observedAt != now.UnixMilli() {
		t.Fatalf("reading = %+v ok = %v", reading, ok)
	}
	// An expired window reports nothing until the next request opens a new one,
	// and that vacancy must not be recorded as a window sitting at zero.
	headers.Del(fiveHourUtilizationHeader)
	if _, ok := readFiveHourWindow(headers, now); ok {
		t.Fatal("a vacant window produced a reading")
	}
}

func testAccountSampler(t *testing.T, uuid string) *accountSampler {
	t.Helper()
	return newSubscriptionSampler().forAccount(store.Account{ID: 1,
		Credential: credential.Credential{AccountUUID: uuid}})
}

// Repeats are dropped and changes are kept however fast they arrive. Traffic
// that lands in one burst still has to leave more than one reading behind, or
// the window it belongs to holds nothing that can be measured.
func TestSubscriptionSamplerDropsRepeatsAndKeepsChanges(t *testing.T) {
	t.Parallel()
	sampler := testAccountSampler(t, "11111111-1111-4111-8111-111111111111")
	reading := func(window string, percent float64, at int64) fiveHourReading {
		return fiveHourReading{resetsAt: window, usedPercent: percent, observedAt: at}
	}
	if !sampler.storable(reading("a", 10, 100)) {
		t.Fatal("the first reading of a window was rejected")
	}
	sampler.stored.Store(&fiveHourReading{resetsAt: "a", usedPercent: 10, observedAt: 100})
	if sampler.storable(reading("a", 10, 200)) {
		t.Fatal("an unchanged reading was stored again")
	}
	if !sampler.storable(reading("a", 11, 200)) || !sampler.storable(reading("b", 10, 200)) {
		t.Fatal("a changed reading or a new window was rejected")
	}
}

// Sampling goroutines racing inside one account can reach the writer out of
// order. A reading overtaken by a later one must not be stored, because the
// counters it would be paired with already include the later one's traffic.
func TestSubscriptionSamplerRejectsOvertakenReadings(t *testing.T) {
	t.Parallel()
	sampler := testAccountSampler(t, "11111111-1111-4111-8111-111111111111")
	sampler.stored.Store(&fiveHourReading{resetsAt: "a", usedPercent: 32, observedAt: 200})
	if sampler.storable(fiveHourReading{resetsAt: "a", usedPercent: 31, observedAt: 100}) {
		t.Fatal("a reading observed before the stored one was accepted")
	}
	// A stale reading of the previous window must not be waved through on the
	// strength of its window: accepting it would drag the record backwards and
	// pair the old window with counters from the new one.
	sampler.stored.Store(&fiveHourReading{resetsAt: "b", usedPercent: 1, observedAt: 300})
	if sampler.storable(fiveHourReading{resetsAt: "a", usedPercent: 33, observedAt: 250}) {
		t.Fatal("a stale reading of the previous window was accepted")
	}
	if !sampler.storable(fiveHourReading{resetsAt: "b", usedPercent: 2, observedAt: 400}) {
		t.Fatal("a reading observed after the stored one was rejected")
	}
}

// Readings arriving while the writer works are kept rather than collapsed into
// the newest. Two changed readings of one window are the minimum that window can
// be measured from, and the last reading of one window and the first of the next
// are both endpoints, so losing either leaves a window with nothing to measure.
func TestSubscriptionSamplerKeepsQueuedReadingsWhileWriting(t *testing.T) {
	t.Parallel()
	sampler := testAccountSampler(t, "11111111-1111-4111-8111-111111111111")
	reading := func(window string, percent float64, at int64) fiveHourReading {
		return fiveHourReading{resetsAt: window, usedPercent: percent, observedAt: at}
	}
	if !sampler.enqueue(reading("a", 10, 100)) {
		t.Fatal("the first reading did not start a writer")
	}
	if sampler.enqueue(reading("a", 11, 200)) || sampler.enqueue(reading("b", 1, 300)) {
		t.Fatal("a second writer was started while one was already running")
	}
	// Out of order readings never displace what is already queued.
	sampler.enqueue(reading("b", 0, 250))
	for _, want := range []float64{10, 11, 1} {
		got, ok := sampler.next()
		if !ok || got.usedPercent != want {
			t.Fatalf("want %v next, got %+v ok=%v", want, got, ok)
		}
	}
	if _, ok := sampler.next(); ok {
		t.Fatal("the queue held more than the readings offered to it")
	}
	// Running out releases the writer, so the next reading starts a new one.
	if !sampler.enqueue(reading("b", 2, 400)) {
		t.Fatal("an emptied queue did not release the writer")
	}
}

// A database that stops accepting writes must not let the queue grow with
// traffic. The ceiling is the relay's own in-flight limit per account, so a
// burst still keeps every reading it could have produced.
func TestSubscriptionSamplerBoundsItsQueue(t *testing.T) {
	t.Parallel()
	sampler := testAccountSampler(t, "11111111-1111-4111-8111-111111111111")
	for step := range maxQueuedReadings * 4 {
		sampler.enqueue(fiveHourReading{resetsAt: "a", usedPercent: float64(step), observedAt: int64(step)})
	}
	sampler.mu.Lock()
	queued := len(sampler.queue)
	newest := sampler.queue[queued-1].usedPercent
	sampler.mu.Unlock()
	if queued != maxQueuedReadings {
		t.Fatalf("queue held %d readings, want the ceiling of %d", queued, maxQueuedReadings)
	}
	// The overflow collapses into the newest reading rather than the oldest,
	// because the oldest is the anchor its window is measured from.
	if newest != float64(maxQueuedReadings*4-1) {
		t.Fatalf("want the newest reading at the tail, got %v", newest)
	}
}

// A deleted account takes its snapshots with it, and SQLite hands its row id to
// whatever is imported next. Neither successor may inherit these readings, or
// its first reading is dismissed as a repeat of one no longer in the database.
func TestSubscriptionSamplerSeparatesAccountIncarnations(t *testing.T) {
	t.Parallel()
	sampler := newSubscriptionSampler()
	account := func(id int64, uuid string) store.Account {
		return store.Account{ID: id, Credential: credential.Credential{AccountUUID: uuid}}
	}
	first := sampler.forAccount(account(1, "11111111-1111-4111-8111-111111111111"))
	if first != sampler.forAccount(account(1, "11111111-1111-4111-8111-111111111111")) {
		t.Fatal("one account did not keep its own sampling state")
	}
	// A different credential landing on the reused row.
	if first == sampler.forAccount(account(1, "22222222-2222-4222-8222-222222222222")) {
		t.Fatal("two accounts sharing a row id shared their sampling state")
	}
	// The same credential imported again, which SQLite may put back on the same
	// row, so the deletion rather than the key has to clear the state.
	sampler.forget(account(1, "11111111-1111-4111-8111-111111111111"))
	if first == sampler.forAccount(account(1, "11111111-1111-4111-8111-111111111111")) {
		t.Fatal("a reimported account inherited the deleted incarnation's readings")
	}
}
