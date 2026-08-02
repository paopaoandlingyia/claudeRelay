package proxy

import (
	"testing"

	"github.com/local/claude-relay/internal/store"
)

func TestAccountLoadTrackerSelectsLeastLoadedCandidate(t *testing.T) {
	tracker := newAccountLoadTracker()
	accounts := []store.Account{{ID: 1, Alias: "primary"}, {ID: 2, Alias: "secondary"}}

	first, firstRelease, ok := tracker.reserveLeastLoaded(accounts, "route", nil, 0)
	if !ok {
		t.Fatal("first reservation failed")
	}
	defer firstRelease()

	second, secondRelease, ok := tracker.reserveLeastLoaded(accounts, "route", nil, 0)
	if !ok {
		t.Fatal("second reservation failed")
	}
	defer secondRelease()
	if first.ID == second.ID {
		t.Fatalf("least-loaded selection reused busy account %d", first.ID)
	}
}

func TestAccountLoadTrackerStickyThresholdAndRelease(t *testing.T) {
	tracker := newAccountLoadTracker()
	release := tracker.reserve(1)
	defer release()

	nextRelease, current, ok := tracker.reserveBelow(1, 1)
	if ok || current != 1 || nextRelease != nil {
		t.Fatalf("reserve below threshold = release %v current %d ok %v", nextRelease != nil, current, ok)
	}

	release()
	nextRelease, current, ok = tracker.reserveBelow(1, 1)
	if !ok || current != 0 || nextRelease == nil {
		t.Fatalf("reserve after release = release %v current %d ok %v", nextRelease != nil, current, ok)
	}
	nextRelease()
}

func TestAccountLoadTrackerLeastLoadedThresholdExcludesBusyCandidates(t *testing.T) {
	tracker := newAccountLoadTracker()
	firstRelease := tracker.reserve(1)
	secondRelease := tracker.reserve(1)
	defer firstRelease()
	defer secondRelease()

	accounts := []store.Account{{ID: 1, Alias: "primary"}, {ID: 2, Alias: "secondary"}}
	selected, release, ok := tracker.reserveLeastLoaded(accounts, "route", map[int64]bool{2: true}, 2)
	if ok || release != nil || selected.ID != 0 {
		t.Fatalf("excluded threshold selection = account %#v release %v ok %v", selected, release != nil, ok)
	}
}
