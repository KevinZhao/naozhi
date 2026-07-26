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
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
)

const mergeHintText = "已合并到上一条回复。"

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
// sendAndReply follower path (a MergedCount>1 result with Text=="") and injects
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
		onEvent cli.EventCallback
	)
	interim := cli.Event{
		Type:    "assistant",
		Message: &cli.AssistantMessage{Content: []cli.ContentBlock{{Type: "text", Text: "working"}}},
	}

	sendFn := func(
		_ context.Context,
		_ string,
		_ *session.ManagedSession,
		_ string,
		_ []cli.ImageData,
		cb cli.EventCallback,
	) (*cli.SendResult, error) {
		cbMu.Lock()
		onEvent = cb
		cbMu.Unlock()

		// Interim assistant event posts the "💭思考中…" banner on this follower
		// slot (process_readloop interim fan-out claims all currentTurnSlots).
		if cb != nil {
			cb(interim)
		}
		// The merge then collapses the turn: the follower result carries
		// MergedCount>1 with an empty Text — the early-return branch under test.
		return &cli.SendResult{Text: "", MergedCount: 2}, nil
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
	d, err := NewDispatcher(DispatcherConfig{
		Router:                router,
		Platforms:             map[string]platform.Platform{"fake": probe},
		Agents:                map[string]session.AgentOpts{},
		AgentCommands:         map[string]string{},
		Guard:                 newFakeGuard(),
		Dedup:                 platform.NewDedup(100),
		SendFn:                sendFn,
		TakeoverFn:            func(_ context.Context, _, _ string, _ session.AgentOpts) bool { return false },
		WatchdogNoOutputKills: new(atomic.Int64),
		WatchdogTotalKills:    new(atomic.Int64),
		NoOutputTimeout:       5 * time.Second,
		TotalTimeout:          30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}

	// Pre-register the session so GetOrCreate does not try to spawn a real CLI
	// process (the test router has no wrapper) — without this sendAndReply
	// bails before ever reaching the follower branch.
	const key = "fake:direct:chat1:general"
	router.InjectSession(key, session.NewTestProcess())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	d.sendAndReply(ctx,
		key,
		"hello", nil,
		"general", session.AgentOpts{},
		platform.IncomingMessage{
			Platform: "fake", EventID: "e1",
			UserID: "u1", ChatID: "chat1", ChatType: "direct", Text: "hello",
		},
		slog.Default(),
		false,
	)

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

// TestMergeFollower_FinalizedBlocksResidualRepaint pins editLoop's guard in
// isolation: with finalized set, a residual buffered editCh signal must not
// repaint the stale interim status. This half deliberately drives the tracker
// directly — the real-path coverage lives in the test above.
func TestMergeFollower_FinalizedBlocksResidualRepaint(t *testing.T) {
	t.Parallel()

	fp := &fakePlatform{supportsInterim: true, replyMsgID: "banner-1"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tracker := newIMEventTracker(ctx, fp, "chat1", "direct", "general")

	tracker.onEvent(cli.Event{
		Type:    "assistant",
		Message: &cli.AssistantMessage{Content: []cli.ContentBlock{{Type: "text", Text: "working"}}},
	})

	tracker.waitReady(ctx)
	tracker.markFinalized()
	msgID := tracker.getThinkingMsgID()
	if msgID == "" {
		t.Fatal("#2338 setup: interim event did not post a banner (no thinkingMsgID)")
	}
	if err := fp.EditMessage(ctx, msgID, mergeHintText); err != nil {
		t.Fatalf("edit banner: %v", err)
	}

	// A residual buffered editCh signal fires after the merge-hint edit but
	// before the deferred stop() — exactly the #2338 race window.
	select {
	case tracker.editCh <- struct{}{}:
	default:
	}

	// Give editLoop time to wake on the residual signal and (correctly) skip.
	time.Sleep(120 * time.Millisecond)
	tracker.stop()

	fp.mu.Lock()
	edits := append([]fakeEdit(nil), fp.edits...)
	fp.mu.Unlock()

	if len(edits) == 0 {
		t.Fatal("#2338: expected at least the merge-hint edit; got none")
	}
	for _, e := range edits {
		if e.text != mergeHintText {
			t.Errorf("#2338: stray non-merge-hint edit %q after finalize; edits=%+v", e.text, edits)
		}
	}
}
