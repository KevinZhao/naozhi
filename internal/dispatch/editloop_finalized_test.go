package dispatch

// #2291: once the IM delivery has committed (or is about to commit) the final
// answer to the banner, a residual buffered editCh signal must NOT trigger a
// status redraw that overwrites the real answer with stale interim status.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestEditLoop_SkipsRedrawAfterFinalized verifies the #2291 fix: after
// markFinalized, editLoop drops a pending editCh signal instead of repainting.
func TestEditLoop_SkipsRedrawAfterFinalized(t *testing.T) {
	t.Parallel()

	fp := &fakePlatform{supportsInterim: true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tracker := newIMEventTracker(ctx, fp, "chat1", "direct", "")

	// Simulate a banner having been posted and some interim status queued.
	id := "banner-1"
	tracker.thinkingMsgID.Store(&id)
	tracker.linesMu.Lock()
	tracker.statusLines = appendStatusLine(tracker.statusLines, "💭 思考中…")
	tracker.linesMu.Unlock()

	// Final answer is being committed: mark finalized, then leave a residual
	// buffered editCh signal as the readloop would after a late interim event.
	tracker.markFinalized()
	select {
	case tracker.editCh <- struct{}{}:
	default:
	}

	// Release editLoop (it parks on msgIDReady until waitReady closes it).
	tracker.waitReady(ctx)

	// Give editLoop time to wake on the residual signal and (correctly) skip.
	time.Sleep(120 * time.Millisecond)

	fp.mu.Lock()
	n := len(fp.edits)
	got := append([]fakeEdit(nil), fp.edits...)
	fp.mu.Unlock()
	if n != 0 {
		t.Errorf("#2291: editLoop performed %d edit(s) after finalize, want 0: %+v", n, got)
	}

	tracker.stop()
}

// gatedEditPlatform parks every EditMessage except finalText until release is
// closed, signalling entered first and noting the ctx deadline. Edits are
// recorded on return, so the recorded order is the order they were applied.
type gatedEditPlatform struct {
	fakePlatform
	finalText string
	entered   chan struct{}
	release   chan struct{}
	// gatedDeadline is the ctx deadline of the last gated edit; guarded by mu.
	gatedDeadline time.Time
}

func (g *gatedEditPlatform) EditMessage(ctx context.Context, msgID, text string) error {
	if text != g.finalText {
		dl, _ := ctx.Deadline()
		g.mu.Lock()
		g.gatedDeadline = dl
		g.mu.Unlock()
		select {
		case g.entered <- struct{}{}:
		default:
		}
		select {
		case <-g.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return g.fakePlatform.EditMessage(ctx, msgID, text)
}

// TestMarkFinalized_WaitsForInFlightRedraw pins #3066: a status redraw that
// passed the finalized check before markFinalized must land before the final
// answer, so markFinalized has to wait for it, and the redraw it waits for
// is bounded by platformReplyTimeout.
func TestMarkFinalized_WaitsForInFlightRedraw(t *testing.T) {
	t.Parallel()

	fp := &gatedEditPlatform{
		fakePlatform: fakePlatform{supportsInterim: true, replyMsgID: "banner-1"},
		finalText:    "FINAL",
		entered:      make(chan struct{}, 1),
		release:      make(chan struct{}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The turn ctx has no deadline, so any deadline on the redraw is its own.
	turnCtx, turnCancel := context.WithCancel(context.Background())
	defer turnCancel()
	start := time.Now()
	tracker := newIMEventTracker(turnCtx, fp, "chat1", "direct", "")
	defer tracker.stop()
	release := sync.OnceFunc(func() { close(fp.release) })
	defer release() // a failed assertion must not leave stop() waiting on the gate

	// The first event posts the banner and queues a redraw, which parks
	// inside the gated EditMessage.
	tracker.onEvent(clievent.Event{Type: "assistant"})
	select {
	case <-fp.entered:
	case <-ctx.Done():
		t.Fatal("editLoop never started the status redraw")
	}

	finDone := make(chan struct{})
	go func() {
		tracker.markFinalized()
		close(finDone)
	}()
	select {
	case <-finDone:
		t.Fatal("markFinalized returned while a status redraw was still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	release()
	select {
	case <-finDone:
	case <-ctx.Done():
		t.Fatal("markFinalized did not return after the redraw finished")
	}
	if err := fp.EditMessage(ctx, "banner-1", "FINAL"); err != nil {
		t.Fatal(err)
	}

	fp.mu.Lock()
	edits := append([]fakeEdit(nil), fp.edits...)
	dl := fp.gatedDeadline
	fp.mu.Unlock()
	if len(edits) != 2 || edits[0].text == "FINAL" || edits[1].text != "FINAL" {
		t.Errorf("edits = %+v, want the status redraw then FINAL last", edits)
	}
	if dl.IsZero() || dl.After(start.Add(platformReplyTimeout+time.Second)) {
		t.Errorf("status redraw deadline = %v, want one within platformReplyTimeout of %v", dl, start)
	}
}
