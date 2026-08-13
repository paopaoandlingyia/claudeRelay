package proxy

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"sync"

	"github.com/local/claude-relay/internal/store"
)

// accountLoadTracker keeps the short-lived load signal used by the single
// relay process. Active requests are intentionally not persisted: a restart
// clears work that is no longer in flight, and SQLite is not a suitable hot
// counter for every streamed request.
type accountLoadTracker struct {
	mu       sync.Mutex
	inFlight map[int64]int
}

// snapshot returns the current per-account request load for administration
// views. Callers receive a copy so rendering never holds the hot-path lock.
func (t *accountLoadTracker) snapshot() map[int64]int {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	result := make(map[int64]int, len(t.inFlight))
	for accountID, count := range t.inFlight {
		result[accountID] = count
	}
	return result
}

func newAccountLoadTracker() *accountLoadTracker {
	return &accountLoadTracker{inFlight: make(map[int64]int)}
}

// reserve records one in-flight request and returns an idempotent release
// function. The reservation is held until the relay has finished copying the
// upstream response body, including an SSE stream.
func (t *accountLoadTracker) reserve(accountID int64) func() {
	if t == nil || accountID <= 0 {
		return func() {}
	}

	t.mu.Lock()
	t.inFlight[accountID]++
	t.mu.Unlock()
	return t.releaseFunc(accountID)
}

// reserveBelow atomically reserves a slot only when the current load is below
// limit. It returns the load observed before the reservation so the selector
// can decide whether a sticky account is locally overloaded.
func (t *accountLoadTracker) reserveBelow(accountID int64, limit int) (release func(), current int, ok bool) {
	if t == nil || accountID <= 0 {
		return func() {}, 0, true
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	current = t.inFlight[accountID]
	if limit > 0 && current >= limit {
		return nil, current, false
	}
	t.inFlight[accountID] = current + 1
	return t.releaseFuncLocked(accountID), current, true
}

// reserveLeastLoaded selects and reserves one candidate while holding the
// tracker lock. A positive maxLoad restricts candidates to current load below
// that value; zero means no threshold. selectionKey is only a tie-breaker, so
// existing cache affinity remains deterministic when loads are equal.
func (t *accountLoadTracker) reserveLeastLoaded(candidates []store.Account, selectionKey string, excluded map[int64]bool, maxLoad int) (store.Account, func(), bool) {
	if len(candidates) == 0 {
		return store.Account{}, nil, false
	}
	if t == nil {
		return store.Account{}, nil, false
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	chosen := -1
	chosenLoad := 0
	var chosenScore [32]byte
	for i := range candidates {
		candidate := candidates[i]
		if excluded[candidate.ID] {
			continue
		}
		load := t.inFlight[candidate.ID]
		if maxLoad > 0 && load >= maxLoad {
			continue
		}
		score := accountSelectionScore(selectionKey, candidate.Alias)
		if chosen < 0 || load < chosenLoad ||
			(load == chosenLoad && bytes.Compare(score[:], chosenScore[:]) > 0) {
			chosen = i
			chosenLoad = load
			chosenScore = score
		}
	}
	if chosen < 0 {
		return store.Account{}, nil, false
	}

	account := candidates[chosen]
	t.inFlight[account.ID] = chosenLoad + 1
	return account, t.releaseFuncLocked(account.ID), true
}

func (t *accountLoadTracker) releaseFunc(accountID int64) func() {
	if t == nil || accountID <= 0 {
		return func() {}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.releaseFuncLocked(accountID)
}

func (t *accountLoadTracker) releaseFuncLocked(accountID int64) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			current := t.inFlight[accountID]
			if current <= 1 {
				delete(t.inFlight, accountID)
				return
			}
			t.inFlight[accountID] = current - 1
		})
	}
}

func accountSelectionScore(selectionKey, alias string) [32]byte {
	return sha256.Sum256([]byte(selectionKey + "\x00" + strings.ToLower(alias)))
}
