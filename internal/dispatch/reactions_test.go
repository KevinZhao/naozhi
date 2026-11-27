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
	mu      sync.Mutex
	added   []reactionCall
	removed []reactionCall
	addErr  error
	// beforeAdd, when set, runs at the start of each AddReaction, outside mu.
	beforeAdd func(messageID string)
	// seq is every landed add and remove in order, as "add:<id>" / "remove:<id>".
	seq []string
}

type reactionCall struct {
	msgID    string
	reaction platform.ReactionType
}

func (f *fakeReactorPlatform) AddReaction(_ context.Context, messageID string, r platform.ReactionType) error {
	if f.beforeAdd != nil {
		f.beforeAdd(messageID)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return f.addErr
	}
	f.added = append(f.added, reactionCall{msgID: messageID, reaction: r})
	f.seq = append(f.seq, "add:"+messageID)
	return nil
}

func (f *fakeReactorPlatform) RemoveReaction(_ context.Context, messageID string, r platform.ReactionType) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, reactionCall{msgID: messageID, reaction: r})
	f.seq = append(f.seq, "remove:"+messageID)
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

func newReactorTestDispatcher(p platform.Platform) *Dispatcher {
	d := newTestDispatcher(&fakePlatform{})
	d.platforms = map[string]platform.Platform{"fake": p}
	return d
}

func TestAckQueuedWithReaction_NoMessageID(t *testing.T) {
	t.Parallel()
	fp := &fakeReactorPlatform{}
	d := newReactorTestDispatcher(fp)

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
	d := newTestDispatcher(fp)
	msg := platform.IncomingMessage{Platform: "fake", MessageID: "m1", ChatID: "c1"}
	if d.ackQueuedWithReaction(context.Background(), msg, nil) {
		t.Fatal("expected false when platform lacks Reactor")
	}
}

func TestAckQueuedWithReaction_Success(t *testing.T) {
	t.Parallel()
	fp := &fakeReactorPlatform{}
	d := newReactorTestDispatcher(fp)

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
	d := newReactorTestDispatcher(fp)

	msg := platform.IncomingMessage{Platform: "fake", MessageID: "m1", ChatID: "c1"}
	if d.ackQueuedWithReaction(context.Background(), msg, nil) {
		t.Fatal("expected false when AddReaction errors")
	}
}

func TestClearQueuedReactions_RemovesOnlyThoseWithMessageID(t *testing.T) {
	t.Parallel()
	fp := &fakeReactorPlatform{}
	d := newReactorTestDispatcher(fp)

	queued := []string{"m1", "", "m3"} // "" has no ID — skipped
	d.clearQueuedReactions(context.Background(), "fake", queued, nil)

	if len(fp.removed) != 2 {
		t.Fatalf("expected 2 removals, got %d: %+v", len(fp.removed), fp.removed)
	}
	if fp.removed[0].msgID != "m1" || fp.removed[1].msgID != "m3" {
		t.Errorf("unexpected removal msgIDs: %+v", fp.removed)
	}
}

// TestClearQueuedReaction_RemovesSingle pins the singular helper a dropped
// IM request (imOrigin.Dropped) clears its HOURGLASS through.
func TestClearQueuedReaction_RemovesSingle(t *testing.T) {
	t.Parallel()
	fp := &fakeReactorPlatform{}
	d := newReactorTestDispatcher(fp)

	d.clearQueuedReaction(context.Background(), "fake", "", nil) // no ID: no call
	d.clearQueuedReaction(context.Background(), "fake", "m1", nil)

	if len(fp.removed) != 1 {
		t.Fatalf("expected 1 removal, got %d: %+v", len(fp.removed), fp.removed)
	}
	if fp.removed[0].msgID != "m1" || fp.removed[0].reaction != platform.ReactionQueued {
		t.Errorf("unexpected removal: %+v", fp.removed[0])
	}
}
