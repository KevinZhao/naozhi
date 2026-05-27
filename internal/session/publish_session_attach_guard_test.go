package session

// R215-ARCH-P2-2 follow-up: the publishSessionLocked funnel collapsed five
// production publish sites into one helper, but it still trusted the
// alreadyAttached parameter. A future caller that flips alreadyAttached
// to true without first calling SetHistorySource would silently leave the
// session with src==nil, which short-circuits EventEntriesBeforeCtx to
// `return nil` and yields a blank dashboard "history" drawer for that
// session — exactly the symptom R215-ARCH-P2-2 was filed against.
//
// The guard added in publishSessionLocked converts the silent failure
// into an observable one (slog.Error) and installs history.Noop so
// downstream callers don't see nil. This test pins that contract.

import (
	"testing"
)

// TestPublishSessionLocked_AlreadyAttachedButNilStillGetsNoop simulates a
// future regression where a caller passes alreadyAttached=true without
// having actually called SetHistorySource. The funnel must NOT publish
// the session with src==nil; the post-publish guarantee is that
// loadHistorySource returns non-nil for any session reachable through
// r.sessions.
func TestPublishSessionLocked_AlreadyAttachedButNilStillGetsNoop(t *testing.T) {
	t.Parallel()

	r := minimalRouter(t)
	s := &ManagedSession{key: "guard:direct:user1:general"}

	// Caller LIES: claims alreadyAttached but never set the source.
	r.mu.Lock()
	r.publishSessionLocked(s.key, s, true)
	r.mu.Unlock()

	if got := s.loadHistorySource(); got == nil {
		t.Fatal("publishSessionLocked left HistorySource nil despite the post-publish guard — EventEntriesBeforeCtx would silently return empty and the dashboard 'history' drawer would blank")
	}

	// Verify the session WAS actually inserted (the guard fires inline,
	// not as an early return).
	r.mu.RLock()
	stored, ok := r.sessions[s.key]
	r.mu.RUnlock()
	if !ok || stored != s {
		t.Fatalf("publishSessionLocked guard short-circuited the insertion: got=%v ok=%v", stored, ok)
	}
}
