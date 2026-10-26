package dispatch

// #2338: the passthrough merge-follower early-return path edits the banner to
// the merge hint but historically never called markFinalized(). Because the
// tracker's editLoop survives across the deferred stop(), a residual buffered
// editCh signal (left by the interim fan-out that posted the "💭思考中…" banner)
// could wake editLoop AFTER the merge-hint edit and repaint the stale status,
// orphaning the user on a "thinking…" bubble that never resolves. The fix
// applies the same markFinalized() guard the success path uses (#2291) before
// the follower's EditMessage.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

const mergeHintText = "已合并到上一条回复。"

// waitForEdit blocks until fp has recorded at least one edit to msgID, so a
// test can synchronise on editLoop's asynchronous repaint instead of sleeping.
// Fails the test rather than hanging if the repaint never arrives.
func waitForEdit(t *testing.T, fp *fakePlatform, msgID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fp.mu.Lock()
		seen := false
		for _, e := range fp.edits {
			if e.msgID == msgID {
				seen = true
				break
			}
		}
		fp.mu.Unlock()
		if seen {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for an edit to %q", msgID)
}

// residualEditPlatform intercepts EditMessage so the test can inject a residual
// interim event at the exact moment dispatch.go collapses the follower banner
// into the merge hint — i.e. inside the #2338 race window, after the hint edit
// but before the deferred tracker.stop(). Whatever editLoop does with that
// signal is then recorded here.
type residualEditPlatform struct {
	*fakePlatform

	mu    sync.Mutex
	seen  []string
	onCue func() // invoked once, when the merge hint is edited
	cued  bool
}

func (p *residualEditPlatform) Name() string { return "fake" }

func (p *residualEditPlatform) EditMessage(ctx context.Context, msgID, text string) error {
	err := p.fakePlatform.EditMessage(ctx, msgID, text)

	p.mu.Lock()
	p.seen = append(p.seen, text)
	fire := false
	if text == mergeHintText && !p.cued {
		p.cued = true
		fire = true
	}
	cue := p.onCue
	p.mu.Unlock()

	if fire && cue != nil {
		cue()
	}
	return err
}

func (p *residualEditPlatform) texts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

// TestMergeFollower_ResidualEditDoesNotRepaintStaleBanner drives the REAL
// IM delivery follower path (a MergedWithHead result with Text=="") and injects
// a residual interim event while that path is collapsing the banner. With the
// #2338 fix, dispatch.go has called markFinalized() before the merge-hint edit,
// so editLoop's `if t.finalized.Load() { continue }` guard drops the residual
// signal and the merge hint stays the last thing the user sees.
//
// Deleting `tracker.markFinalized()` from the follower branch in dispatch.go
// makes this test FAIL: editLoop repaints "💭思考中…" over the merge hint.
func TestMergeFollower_ResidualEditDoesNotRepaintStaleBanner(t *testing.T) {
	t.Parallel()

	fp := &fakePlatform{supportsInterim: true, replyMsgID: "banner-1"}
	probe := &residualEditPlatform{fakePlatform: fp}

	// onEvent is captured from the send callback so the residual event can be
	// replayed later through the very same path the interim fan-out uses.
	var (
		cbMu    sync.Mutex
		onEvent clievent.EventCallback
	)
	interim := clievent.Event{
		Type:    "assistant",
		Message: &clievent.AssistantMessage{Content: []clievent.ContentBlock{{Type: "text", Text: "working"}}},
	}

	sendFn := func(
		_ context.Context,
		_ string,
		_ turn.Session,
		_ string,
		_ []clievent.Attachment,
		cb clievent.EventCallback,
	) (*clievent.SendResult, error) {
		cbMu.Lock()
		onEvent = cb
		cbMu.Unlock()

		// Interim assistant event posts the "💭思考中…" banner on this follower
		// slot (process_readloop interim fan-out claims all currentTurnSlots).
		if cb != nil {
			cb(interim)
		}
		// Let editLoop's repaint for THAT interim event land before returning.
		//
		// Without this the test was flaky (7/40 locally, and it red-lit
		// test-macos on unrelated PRs): the follower branch only calls
		// tracker.waitReady, which merely guarantees the banner was POSTED —
		// verified by instrumenting it, waitReady returns with zero edits
		// recorded. The first repaint could therefore arrive AFTER the
		// merge-hint edit and be indistinguishable, to the assertion below,
		// from the illegal residual repaint this test exists to catch. Both
		// show up as a "💭思考中…" edit following the hint.
		//
		// Draining it here keeps the scenario honest: everything after the
		// merge hint is then genuinely attributable to the residual signal.
		waitForEdit(t, fp, fp.replyMsgID)

		// The merge then collapses the turn: the follower result points at its
		// head with an empty Text — the early-return branch under test.
		return &clievent.SendResult{Text: "", MergedCount: 2, MergedWithHead: 1}, nil
	}

	probe.onCue = func() {
		cbMu.Lock()
		cb := onEvent
		cbMu.Unlock()
		if cb == nil {
			return
		}
		// Residual interim status arriving after the merge-hint edit. This
		// re-signals editCh exactly as a late fan-out event would.
		cb(interim)
		// editLoop rate-limits redraws to one per second, so wait past that
		// window to give a (buggy) unguarded loop a real chance to repaint
		// before the deferred stop() tears the tracker down.
		time.Sleep(1500 * time.Millisecond)
	}

	// Build the dispatcher around a router we still hold concretely, so the
	// session can be pre-registered below.
	router := session.NewRouter(session.RouterConfig{MaxProcs: 10})
	d := newTestDispatcher(fp, withRouter(router), withSendFn(sendFn))
	d.platforms = map[string]platform.Platform{"fake": probe}

	// Pre-register the session so GetOrCreate does not try to spawn a real CLI
	// process (the test router has no wrapper) — without this the turn
	// bails before ever reaching the follower branch.
	const key = "fake:direct:chat1:general"
	router.InjectSession(key, session.NewTestProcess())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	runIMTurn(ctx, d, key, "hello", platform.IncomingMessage{
		Platform: "fake", EventID: "e1",
		UserID: "u1", ChatID: "chat1", ChatType: "direct", Text: "hello",
	}, false)

	edits := probe.texts()

	hintAt := -1
	for i, text := range edits {
		if text == mergeHintText {
			hintAt = i
			break
		}
	}
	if hintAt < 0 {
		t.Fatalf("#2338 setup: follower branch never edited the banner to the merge hint; edits=%q", edits)
	}

	// Nothing may repaint after the merge hint — least of all the stale
	// interim status the user was supposed to be freed from.
	for _, text := range edits[hintAt+1:] {
		if strings.Contains(text, "思考中") {
			t.Errorf("#2338: stale 💭思考中… banner repainted over the merge hint by a residual "+
				"editCh signal (tracker not finalized before the follower edit); edits=%q", edits)
			break
		}
		t.Errorf("#2338: unexpected edit %q after the merge hint; edits=%q", text, edits)
	}
}
