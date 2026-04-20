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
	fp := &fakePlatform{} // no Reactor capability
	d := newTestDispatcher(fp, nil)
	msg := platform.IncomingMessage{Platform: "fake", MessageID: "m1", ChatID: "c1"}
	if d.ackQueuedWithReaction(context.Background(), msg, nil) {
		t.Fatal("expected false when platform lacks Reactor")
	}
}

func TestAckQueuedWithReaction_Success(t *testing.T) {
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
	fp := &fakeReactorPlatform{addErr: errors.New("network")}
	d := newTestDispatcherWithPlatform(fp)
	d.platforms = map[string]platform.Platform{"fake": fp}

	msg := platform.IncomingMessage{Platform: "fake", MessageID: "m1", ChatID: "c1"}
	if d.ackQueuedWithReaction(context.Background(), msg, nil) {
		t.Fatal("expected false when AddReaction errors")
	}
}

func TestClearQueuedReactions_RemovesOnlyThoseWithMessageID(t *testing.T) {
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

func TestClearQueuedReactions_NonReactorIsNoOp(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	// Must not panic and must leave no side effects.
	d.clearQueuedReactions(context.Background(), "fake",
		[]QueuedMsg{{MessageID: "m1"}}, nil)
}

func TestClearQueuedReactions_ErrorsSwallowed(t *testing.T) {
	fp := &fakeReactorPlatform{removeErr: errors.New("rate limit")}
	d := newTestDispatcherWithPlatform(fp)
	d.platforms = map[string]platform.Platform{"fake": fp}

	// Should not panic; errors are logged and swallowed.
	d.clearQueuedReactions(context.Background(), "fake",
		[]QueuedMsg{{MessageID: "m1"}}, nil)
}
