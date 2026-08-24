package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/local/claude-relay/internal/store"
)

func TestSessionAdmissionCountsPendingRoutesAtomically(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "http://127.0.0.1:1", 4096)
	account, found, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil || !found {
		t.Fatalf("default account = %#v found=%v err=%v", account, found, err)
	}
	tracker := newSessionAdmissionTracker(server.store, 1)

	releaseFirst, admitted, err := tracker.reserve(t.Context(), "session:route-a", account.ID, false)
	if err != nil || !admitted {
		t.Fatalf("first admission admitted=%v err=%v", admitted, err)
	}
	releaseShared, admitted, err := tracker.reserve(t.Context(), "session:route-a", account.ID, false)
	if err != nil || !admitted {
		t.Fatalf("shared route admission admitted=%v err=%v", admitted, err)
	}
	if _, admitted, err = tracker.reserve(t.Context(), "session:route-b", account.ID, false); err != nil || admitted {
		t.Fatalf("distinct route while full admitted=%v err=%v", admitted, err)
	}

	releaseFirst()
	if _, admitted, err = tracker.reserve(t.Context(), "session:route-b", account.ID, false); err != nil || admitted {
		t.Fatalf("route admitted before all shared references released: admitted=%v err=%v", admitted, err)
	}
	releaseShared()
	releaseNext, admitted, err := tracker.reserve(t.Context(), "session:route-b", account.ID, false)
	if err != nil || !admitted {
		t.Fatalf("route after release admitted=%v err=%v", admitted, err)
	}
	releaseNext()
}

func TestSessionAdmissionDoesNotOversubscribeConcurrentRoutes(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "http://127.0.0.1:1", 4096)
	account, found, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil || !found {
		t.Fatalf("default account = %#v found=%v err=%v", account, found, err)
	}
	const (
		limit    = 3
		attempts = 20
	)
	tracker := newSessionAdmissionTracker(server.store, limit)
	start := make(chan struct{})
	release := make(chan struct{})
	results := make(chan struct {
		admitted bool
		err      error
	}, attempts)
	var workers sync.WaitGroup
	workers.Add(attempts)
	for index := range attempts {
		go func() {
			defer workers.Done()
			<-start
			releaseSlot, admitted, reserveErr := tracker.reserve(t.Context(), fmt.Sprintf("session:route-%d", index), account.ID, false)
			results <- struct {
				admitted bool
				err      error
			}{admitted: admitted, err: reserveErr}
			if admitted {
				<-release
				releaseSlot()
			}
		}()
	}
	close(start)
	admitted := 0
	var reserveErr error
	for range attempts {
		result := <-results
		if reserveErr == nil && result.err != nil {
			reserveErr = result.err
		}
		if result.admitted {
			admitted++
		}
	}
	close(release)
	workers.Wait()
	if reserveErr != nil {
		t.Fatal(reserveErr)
	}
	if admitted != limit {
		t.Fatalf("concurrent admissions=%d, want %d", admitted, limit)
	}
}

func TestCacheAffinityRoutesDoNotConsumeSessionAdmission(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "http://127.0.0.1:1", 4096)
	account, found, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil || !found {
		t.Fatalf("default account = %#v found=%v err=%v", account, found, err)
	}
	tracker := newSessionAdmissionTracker(server.store, 1)
	for _, routeKey := range []string{"prefix:one-shot-a", "prefix:one-shot-b"} {
		release, admitted, reserveErr := tracker.reserve(t.Context(), routeKey, account.ID, false)
		if reserveErr != nil || !admitted {
			t.Fatalf("cache-affinity route %q admitted=%v err=%v", routeKey, admitted, reserveErr)
		}
		release()
	}
	if len(tracker.pending) != 0 || len(tracker.perAccount) != 0 {
		t.Fatalf("cache-affinity routes created session reservations: pending=%d accounts=%d", len(tracker.pending), len(tracker.perAccount))
	}
}

func TestNewSessionsUseAnotherAccountWhenPendingSlotIsFull(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "http://127.0.0.1:1", 4096)
	importTestAccount(t, server.store, "secondary", "token-secondary", "22222222-2222-4222-8222-222222222222", "b")
	server.selector.sessions = newSessionAdmissionTracker(server.store, 1)

	first, err := server.selector.selectAccount(t.Context(), requestRoute{
		ConversationKey: "session:route-a",
		SelectionKey:    "selection-a",
		Ingress:         store.AccountPoolCompatible,
	}, "", map[int64]bool{})
	if err != nil {
		t.Fatal(err)
	}
	defer first.release()
	defer first.releaseSession()

	second, err := server.selector.selectAccount(t.Context(), requestRoute{
		ConversationKey: "session:route-b",
		SelectionKey:    "selection-b",
		Ingress:         store.AccountPoolCompatible,
	}, "", map[int64]bool{})
	if err != nil {
		t.Fatal(err)
	}
	defer second.release()
	defer second.releaseSession()
	if first.Account.ID == second.Account.ID {
		t.Fatalf("both pending sessions selected account %q", first.Account.Alias)
	}
}

func TestSuccessfulSessionBecomesConfirmedAdmission(t *testing.T) {
	t.Parallel()
	var upstreamCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	tracker := newSessionAdmissionTracker(server.store, 1)
	server.selector.sessions = tracker
	body := `{"model":"claude-test","messages":[{"role":"user","content":"session admission"}]}`

	send := func(session, forcedAlias string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		request.Header.Set("x-api-key", "downstream-key")
		request.Header.Set("X-Claude-Session-Id", session)
		if forcedAlias != "" {
			request.Header.Set(accountHeader, forcedAlias)
		}
		server.routes().ServeHTTP(recorder, request)
		return recorder
	}

	first := send("session-a", "")
	if first.Code != http.StatusOK {
		t.Fatalf("first session status=%d body=%s", first.Code, first.Body.String())
	}
	headers := http.Header{"X-Claude-Session-Id": []string{"session-a"}}
	route, err := deriveRequestRoute([]byte(body), headers, store.AccountPoolCompatible)
	if err != nil {
		t.Fatal(err)
	}
	bound, found, err := server.store.BoundAccount(t.Context(), route.ConversationKey, store.AccountPoolCompatible, time.Now())
	if err != nil || !found {
		t.Fatalf("confirmed binding = %#v found=%v err=%v", bound, found, err)
	}
	tracker.mu.Lock()
	pendingRoutes, pendingAccounts := len(tracker.pending), len(tracker.perAccount)
	tracker.mu.Unlock()
	if pendingRoutes != 0 || pendingAccounts != 0 {
		t.Fatalf("provisional admission remained after binding: routes=%d accounts=%d", pendingRoutes, pendingAccounts)
	}

	existing := send("session-a", "")
	if existing.Code != http.StatusOK {
		t.Fatalf("existing session status=%d body=%s", existing.Code, existing.Body.String())
	}
	newSession := send("session-b", bound.Alias)
	if newSession.Code != http.StatusTooManyRequests {
		t.Fatalf("new session status=%d body=%s", newSession.Code, newSession.Body.String())
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstream calls=%d, want only the admitted sessions", upstreamCalls)
	}
}
