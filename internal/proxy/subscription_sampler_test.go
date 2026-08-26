package proxy

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/local/claude-relay/internal/config"
	"github.com/local/claude-relay/internal/credential"
	"github.com/local/claude-relay/internal/store"
)

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
	if !ok || reading.resetsAt != "1786386600" || reading.usedPercent != 31 || reading.observedAt != now.UnixMilli() {
		t.Fatalf("reading = %+v ok = %v", reading, ok)
	}
	headers.Del(fiveHourUtilizationHeader)
	if _, ok := readFiveHourWindow(headers, now); ok {
		t.Fatal("a response without a utilization value produced a reading")
	}
}

func TestReadFiveHourWindowRoundsUtilization(t *testing.T) {
	t.Parallel()
	headers := http.Header{}
	headers.Set(fiveHourResetHeader, "1786676400")
	for _, test := range []struct {
		raw  string
		want float64
	}{{raw: "0.060000000000000036", want: 6}, {raw: "0.07000000000000001", want: 7}} {
		headers.Set(fiveHourUtilizationHeader, test.raw)
		reading, ok := readFiveHourWindow(headers, time.Unix(1786676000, 0))
		if !ok || reading.usedPercent != test.want {
			t.Errorf("utilization %q produced %v, want %v", test.raw, reading.usedPercent, test.want)
		}
	}
}

func TestCurrentFiveHourWindowExpiresAtReset(t *testing.T) {
	t.Parallel()
	now := time.Unix(1786676000, 0)
	account := store.Account{ID: 1, Credential: credential.Credential{AccountUUID: "11111111-1111-4111-8111-111111111111"}}
	sampler := newSubscriptionSampler(config.DefaultMaxInflightPerAccount)
	sampler.observe(account, fiveHourReading{
		resetsAt: strconv.FormatInt(now.Add(time.Minute).Unix(), 10), usedPercent: 31, observedAt: now.UnixMilli(),
	})
	if reading, ok := sampler.current(account, now); !ok || reading.usedPercent != 31 {
		t.Fatalf("active window = %+v ok=%v", reading, ok)
	}
	if reading, ok := sampler.current(account, now.Add(time.Minute)); ok {
		t.Fatalf("expired window remained current: %+v", reading)
	}
}

func TestLiveFiveHourWindowOnlyMovesForward(t *testing.T) {
	t.Parallel()
	now := time.Unix(1786676000, 0)
	account := store.Account{ID: 1, Credential: credential.Credential{AccountUUID: "11111111-1111-4111-8111-111111111111"}}
	sampler := newSubscriptionSampler(config.DefaultMaxInflightPerAccount)
	reset := strconv.FormatInt(now.Add(4*time.Hour).Unix(), 10)
	sampler.observe(account, fiveHourReading{resetsAt: reset, usedPercent: 31, observedAt: now.Add(time.Second).UnixMilli()})
	sampler.observe(account, fiveHourReading{resetsAt: reset, usedPercent: 20, observedAt: now.UnixMilli()})
	reading, ok := sampler.current(account, now)
	if !ok || reading.usedPercent != 31 {
		t.Fatalf("older concurrent reading replaced the live view: %+v ok=%v", reading, ok)
	}
}

func TestSubscriptionSamplerSeparatesAccountIncarnations(t *testing.T) {
	t.Parallel()
	sampler := newSubscriptionSampler(config.DefaultMaxInflightPerAccount)
	account := func(id int64, uuid string) store.Account {
		return store.Account{ID: id, Credential: credential.Credential{AccountUUID: uuid}}
	}
	first := sampler.forAccount(account(1, "11111111-1111-4111-8111-111111111111"))
	if first != sampler.forAccount(account(1, "11111111-1111-4111-8111-111111111111")) {
		t.Fatal("one account did not keep its own live sampling state")
	}
	if first == sampler.forAccount(account(1, "22222222-2222-4222-8222-222222222222")) {
		t.Fatal("two accounts sharing a row id shared live sampling state")
	}
	sampler.forget(account(1, "11111111-1111-4111-8111-111111111111"))
	if first == sampler.forAccount(account(1, "11111111-1111-4111-8111-111111111111")) {
		t.Fatal("a reimported account inherited the deleted incarnation's live state")
	}
}
