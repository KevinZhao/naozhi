package dispatch

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
)

// fakeReactorPlatform is a fakePlatform that also implements platform.Reactor.
// It records Add/Remove calls so tests can assert the correct reactions were
// applied to the correct message IDs.
type fakeReactorPlatform struct {
	fakePlatform
	mu        sync.Mutex
	added     []reactionCall
	removed   []reactionCall
	addErr    error
	removeErr error
}

type reactionCall struct {
	msgID    string
	reaction platform.ReactionType
}

func (f *fakeReactorPlatform) AddReaction(_ context.Context, messageID string, r platform.ReactionType) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return f.addErr
	}
	f.added = append(f.added, reactionCall{msgID: messageID, reaction: r})
	return nil
}

func (f *fakeReactorPlatform) RemoveReaction(_ context.Context, messageID string, r platform.ReactionType) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = append(f.removed, reactionCall{msgID: messageID, reaction: r})
	return nil
}

// sanity — fakeReactorPlatform must satisfy both Platform and Reactor.
var (
	_ platform.Platform = (*fakeReactorPlatform)(nil)
	_ platform.Reactor  = (*fakeReactorPlatform)(nil)
)

// Override embedded methods that satisfy Platform so RegisterRoutes resolves
// through the embedded fakePlatform.
func (f *fakeReactorPlatform) RegisterRoutes(_ *http.ServeMux, _ platform.MessageHandler) {}

func newTestDispatcherWithPlatform(p platform.Platform) *Dispatcher {
	fp, _ := p.(*fakePlatform)
	if fp == nil {
		// Wrap any non-fake platform into a dispatcher using newTestDispatcher's
		// config; we only need a minimal setup.
		d := newTestDispatcher(&fakePlatform{}, nil)
		d.platforms = map[string]platform.Platform{p.Name(): p}
		return d
	}
	return newTestDispatcher(fp, nil)
}

func TestAckQueuedWithReaction_NoMessageID(t *testing.T) {
	t.Parallel()
	fp := &fakeReactorPlatform{}
	d := newTestDispatcherWithPlatform(fp)
	d.platforms = map[string]platform.Platform{"fake": fp}

	msg := platform.IncomingMessage{Platform: "fake", ChatID: "c1"} // MessageID empty
	if d.ackQueuedWithReaction(context.Background(), msg, nil) {
		t.Fatal("expected false when MessageID is empty")
	}
	if len(fp.added) != 0 {
		t.Errorf("AddReaction should not be called, got %d calls", len(fp.added))
	}
}

func TestAckQueuedWithReaction_NonReactorPlatform(t *testing.T) {
	t.Parallel()
	fp := &fakePlatform{} // no Reactor capability
	d := newTestDispatcher(fp, nil)
	msg := platform.IncomingMessage{Platform: "fake", MessageID: "m1", ChatID: "c1"}
	if d.ackQueuedWithReaction(context.Background(), msg, nil) {
		t.Fatal("expected false when platform lacks Reactor")
	}
}

func TestAckQueuedWithReaction_Success(t *testing.T) {
	t.Parallel()
	fp := &fakeReactorPlatform{}
	d := newTestDispatcherWithPlatform(fp)
	d.platforms = map[string]platform.Platform{"fake": fp}

	msg := platform.IncomingMessage{Platform: "fake", MessageID: "m1", ChatID: "c1"}
	if !d.ackQueuedWithReaction(context.Background(), msg, nil) {
		t.Fatal("expected true on successful AddReaction")
	}
	if len(fp.added) != 1 || fp.added[0].msgID != "m1" || fp.added[0].reaction != platform.ReactionQueued {
		t.Errorf("unexpected AddReaction calls: %+v", fp.added)
	}
}

func TestAckQueuedWithReaction_AddErrorFallsBack(t *testing.T) {
	t.Parallel()
	fp := &fakeReactorPlatform{addErr: errors.New("network")}
	d := newTestDispatcherWithPlatform(fp)
	d.platforms = map[string]platform.Platform{"fake": fp}

	msg := platform.IncomingMessage{Platform: "fake", MessageID: "m1", ChatID: "c1"}
	if d.ackQueuedWithReaction(context.Background(), msg, nil) {
		t.Fatal("expected false when AddReaction errors")
	}
}

func TestClearQueuedReactions_RemovesOnlyThoseWithMessageID(t *testing.T) {
	t.Parallel()
	fp := &fakeReactorPlatform{}
	d := newTestDispatcherWithPlatform(fp)
	d.platforms = map[string]platform.Platform{"fake": fp}

	queued := []QueuedMsg{
		{Text: "a", MessageID: "m1"},
		{Text: "b", MessageID: ""}, // no ID — skipped
		{Text: "c", MessageID: "m3"},
	}
	d.clearQueuedReactions(context.Background(), "fake", queued, nil)

	if len(fp.removed) != 2 {
		t.Fatalf("expected 2 removals, got %d: %+v", len(fp.removed), fp.removed)
	}
	if fp.removed[0].msgID != "m1" || fp.removed[1].msgID != "m3" {
		t.Errorf("unexpected removal msgIDs: %+v", fp.removed)
	}
}

// TestClearQueuedReaction_RemovesSingle pins #1946: the passthrough / /urgent
// path clears its HOURGLASS via the singular helper once the turn finishes.
func TestClearQueuedReaction_RemovesSingle(t *testing.T) {
	t.Parallel()
	fp := &fakeReactorPlatform{}
	d := newTestDispatcherWithPlatform(fp)
	d.platforms = map[string]platform.Platform{"fake": fp}

	d.clearQueuedReaction(context.Background(), "fake", "m1", nil)

	if len(fp.removed) != 1 {
		t.Fatalf("expected 1 removal, got %d: %+v", len(fp.removed), fp.removed)
	}
	if fp.removed[0].msgID != "m1" || fp.removed[0].reaction != platform.ReactionQueued {
		t.Errorf("unexpected removal: %+v", fp.removed[0])
	}
}

func TestClearQueuedReaction_EmptyIDIsNoOp(t *testing.T) {
	t.Parallel()
	fp := &fakeReactorPlatform{}
	d := newTestDispatcherWithPlatform(fp)
	d.platforms = map[string]platform.Platform{"fake": fp}

	d.clearQueuedReaction(context.Background(), "fake", "", nil)
	if len(fp.removed) != 0 {
		t.Fatalf("empty MessageID must not call RemoveReaction, got %d", len(fp.removed))
	}
}

func TestClearQueuedReaction_NonReactorIsNoOp(t *testing.T) {
	t.Parallel()
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	// Must not panic and must leave no side effects.
	d.clearQueuedReaction(context.Background(), "fake", "m1", nil)
}

func TestClearQueuedReaction_ErrorSwallowed(t *testing.T) {
	t.Parallel()
	fp := &fakeReactorPlatform{removeErr: errors.New("rate limit")}
	d := newTestDispatcherWithPlatform(fp)
	d.platforms = map[string]platform.Platform{"fake": fp}
	// Should not panic; error is logged and swallowed.
	d.clearQueuedReaction(context.Background(), "fake", "m1", nil)
}

func TestClearQueuedReactions_NonReactorIsNoOp(t *testing.T) {
	t.Parallel()
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	// Must not panic and must leave no side effects.
	d.clearQueuedReactions(context.Background(), "fake",
		[]QueuedMsg{{MessageID: "m1"}}, nil)
}

func TestClearQueuedReactions_ErrorsSwallowed(t *testing.T) {
	t.Parallel()
	fp := &fakeReactorPlatform{removeErr: errors.New("rate limit")}
	d := newTestDispatcherWithPlatform(fp)
	d.platforms = map[string]platform.Platform{"fake": fp}

	// Should not panic; errors are logged and swallowed.
	d.clearQueuedReactions(context.Background(), "fake",
		[]QueuedMsg{{MessageID: "m1"}}, nil)
}

// loadDeleteReactor models Feishu's reaction-cache contract
// (feishu.go:1334-1409): AddReaction stores a reaction_id keyed by
// (messageID, reaction) only AFTER the HTTP round-trip succeeds, and
// RemoveReaction is a LoadAndDelete — a miss (no prior stored Add) is a
// no-op that leaves nothing on the platform. The platform-visible
// HOURGLASS is therefore "present" iff a stored Add was not later
// removed. This is the exact semantics R20260608-133914-LB-3 (#1963)
// depends on: a clear that fires before the matching ack's Store is a
// no-op, and if no clear runs after the Store the HOURGLASS is permanent.
type loadDeleteReactor struct {
	fakePlatform
	mu      sync.Mutex
	present map[string]bool // key "msgID|reaction" -> stored & not removed
}

func (r *loadDeleteReactor) key(msgID string, rt platform.ReactionType) string {
	return msgID + "|" + string(rt)
}

func (r *loadDeleteReactor) AddReaction(_ context.Context, msgID string, rt platform.ReactionType) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.present == nil {
		r.present = map[string]bool{}
	}
	r.present[r.key(msgID, rt)] = true // Store only after success
	return nil
}

func (r *loadDeleteReactor) RemoveReaction(_ context.Context, msgID string, rt platform.ReactionType) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.present, r.key(msgID, rt)) // LoadAndDelete: miss is a no-op
	return nil
}

func (r *loadDeleteReactor) lingering(msgID string, rt platform.ReactionType) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.present[r.key(msgID, rt)]
}

func (r *loadDeleteReactor) RegisterRoutes(_ *http.ServeMux, _ platform.MessageHandler) {}

var _ platform.Reactor = (*loadDeleteReactor)(nil)

// TestAckThenClear_NoLingeringHourglass is the regression guard for
// R20260608-133914-LB-3 (#1963). The passthrough fast-fail path runs the
// turn goroutine's clearQueuedReaction and the synchronous
// ackQueuedWithReaction; with the buggy order (spawn-then-ack) the clear
// can fire its LoadAndDelete no-op before the ack stores the reaction_id,
// leaving a permanent HOURGLASS. The fix acks first. This table pins both
// orders against the real LoadAndDelete contract so a future reorder that
// reintroduces clear-before-ack fails here.
func TestAckThenClear_NoLingeringHourglass(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		clearFirst    bool // simulates the fast-fail goroutine clearing before ack
		wantLingering bool
	}{
		{"ack_before_clear_fixed", false, false},
		{"clear_before_ack_buggy", true, true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := &loadDeleteReactor{}
			d := newTestDispatcher(&fakePlatform{}, nil)
			d.platforms = map[string]platform.Platform{"fake": r}
			msg := platform.IncomingMessage{Platform: "fake", MessageID: "m1", ChatID: "c1"}

			ack := func() { d.ackQueuedWithReaction(context.Background(), msg, nil) }
			clear := func() { d.clearQueuedReaction(context.Background(), "fake", msg.MessageID, nil) }

			if tt.clearFirst {
				clear()
				ack()
			} else {
				ack()
				clear()
			}

			if got := r.lingering("m1", platform.ReactionQueued); got != tt.wantLingering {
				t.Errorf("lingering HOURGLASS = %v, want %v", got, tt.wantLingering)
			}
		})
	}
}
