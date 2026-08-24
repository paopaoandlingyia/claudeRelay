package proxy

import (
	"context"
	"sync"
	"time"

	"github.com/local/claude-relay/internal/store"
)

const activeSessionWindow = 5 * time.Minute

// sessionAdmissionTracker closes the gap between selection and a successful
// upstream response. Confirmed sessions live in SQLite; this tracker counts
// new sessions while their first request is still in flight so concurrent
// admissions cannot all observe the same stale database count.
//
// The tracker is deliberately process-local. The supported deployment is one
// relay process, and a restart also ends every request whose provisional slot
// could have existed.
type sessionAdmissionTracker struct {
	mu         sync.Mutex
	store      *store.Store
	maxActive  int
	pending    map[string]*pendingSessionAdmission
	perAccount map[int64]int
}

type pendingSessionAdmission struct {
	accountID  int64
	references int
}

func newSessionAdmissionTracker(database *store.Store, maxActive int) *sessionAdmissionTracker {
	return &sessionAdmissionTracker{
		store:      database,
		maxActive:  maxActive,
		pending:    make(map[string]*pendingSessionAdmission),
		perAccount: make(map[int64]int),
	}
}

// pendingAccount returns the account already reserved by another first
// request for this session. Routing concurrent requests through that account
// preserves affinity before the successful binding reaches SQLite.
func (t *sessionAdmissionTracker) pendingAccount(routeKey string) (int64, bool) {
	if t == nil || routeKey == "" {
		return 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	pending, ok := t.pending[routeKey]
	if !ok {
		return 0, false
	}
	return pending.accountID, true
}

// reserve admits an existing session without consuming a new slot. A new
// session is counted atomically with other provisional admissions and returns
// an idempotent release function held until the request finishes.
func (t *sessionAdmissionTracker) reserve(ctx context.Context, routeKey string, accountID int64, existing bool) (func(), bool, error) {
	if t == nil || routeKey == "" || accountID <= 0 || existing {
		return func() {}, true, nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if pending, ok := t.pending[routeKey]; ok {
		if pending.accountID != accountID {
			return nil, false, nil
		}
		pending.references++
		return t.releaseFunc(routeKey, pending), true, nil
	}

	now := time.Now()
	active, err := t.store.ActiveSessionCount(ctx, accountID, now, now.Add(-activeSessionWindow))
	if err != nil {
		return nil, false, err
	}
	if t.maxActive > 0 && active+t.perAccount[accountID] >= t.maxActive {
		return nil, false, nil
	}

	pending := &pendingSessionAdmission{accountID: accountID, references: 1}
	t.pending[routeKey] = pending
	t.perAccount[accountID]++
	return t.releaseFunc(routeKey, pending), true, nil
}

func (t *sessionAdmissionTracker) releaseFunc(routeKey string, admission *pendingSessionAdmission) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			current, ok := t.pending[routeKey]
			if !ok || current != admission {
				return
			}
			current.references--
			if current.references > 0 {
				return
			}
			delete(t.pending, routeKey)
			if count := t.perAccount[current.accountID]; count <= 1 {
				delete(t.perAccount, current.accountID)
			} else {
				t.perAccount[current.accountID] = count - 1
			}
		})
	}
}
