package dispatch

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/platform"
)

// replyTracker manages IM status message streaming (thinking -> tool_use -> result).
//
// statusLines is read+mutated under linesMu by onEvent (called serially by the
// CLI event loop) and read by editLoop. Joining to a single string is deferred
// to the read path so we don't waste allocations on events that are coalesced
// away by the 1-per-second rate limit.
type replyTracker struct {
	ctx    context.Context
	p      platform.Platform
	chatID string
	// thinkingMsgID is written by the Reply goroutine spawned in onEvent and
	// read by editLoop + by sendAndReply (via waitReady→ctx.Done fallback).
	// When ctx cancels, waitReady can return before msgIDReady is closed,
	// so the subsequent read can race the goroutine's write. atomic.Pointer
	// gives race-detector–clean visibility without extending linesMu's scope.
	thinkingMsgID atomic.Pointer[string]
	msgIDReady    chan struct{}
	sent          sync.Once
	editCh        chan struct{} // buffered(1), signals editLoop to redraw
	done          chan struct{} // closed when the owning turn completes; exits editLoop
	linesMu       sync.Mutex    // guards statusLines
	// statusLines is a pre-allocated slice capped at maxStatusLines (8) by
	// appendStatusLine — it grows up to that bound and then drops the head
	// via copy-to-front (see status.go). Joining to a single string is
	// deferred to the read path (renderStatus). R230-CQ-15.
	statusLines []string

	// TodoWrite delivery: onEvent publishes the latest checklist text into
	// pendingTodo (atomic.Pointer — single-writer race-free overwrite) and
	// signals todoWake (buffered(1)) so todoLoop consumes exactly once per
	// burst. Claude Code emits TodoWrite as a full snapshot on every
	// mutation, so dropping intermediate states is safe (last render ==
	// latest truth). Replaces the previous drain-and-replace channel pattern
	// which had a TOCTOU race where todoLoop could consume the drained
	// value before onEvent's replace write, silently dropping the newest
	// snapshot.
	pendingTodo atomic.Pointer[string]
	todoWake    chan struct{}
	// lastTodoText is the last checklist text posted to chat; read and
	// written only from todoLoop so no synchronisation is required.
	lastTodoText string

	// loopWG tracks editLoop + todoLoop + (reserved) the initial-Reply
	// goroutine so stop() can wait for them before sendAndReply returns.
	// Without this, a slow goroutine parked inside a 15s platform Reply
	// could leak into the next turn and post a stale checklist for the
	// wrong session.
	loopWG sync.WaitGroup

	// initialReplyReservation ensures the pre-allocated loopWG slot for the
	// initial-Reply goroutine is Done'd exactly once — either by the
	// onEvent goroutine itself when it finishes the Reply, or by stop()
	// when the turn ends before any event fires. Pre-allocating the slot
	// (versus Add'ing inside sent.Do) avoids the WaitGroup race where
	// Add(1) could execute after Wait() returned with counter == 0.
	// supportsInterim=false trackers never reserve this slot, so releaseIfReserved
	// is a no-op.
	initialReplyReservation   sync.Once
	initialReplyReservationOn bool

	// supportsInterim caches platform.SupportsInterimMessages(p) at
	// construction time. The value is stable for the lifetime of a turn
	// and the function is called per streaming event in onEvent — caching
	// removes one interface dispatch per event on busy sessions.
	// R216-PERF-13.
	supportsInterim bool

	// askQuestionFired signals that this turn emitted at least one
	// AskUserQuestion card. Read by sendAndReply to suppress the bailout
	// text that `claude -p` always produces after auto-rejecting the
	// tool ("I've asked you..."). Without this suppression users see a
	// redundant message next to the card; with it, only the card surfaces
	// and the session "appears" to be waiting for the answer. Written
	// from onEvent (readLoop goroutine) and read after waitReady returns,
	// so atomic access is sufficient.
	askQuestionFired atomic.Bool
}

func (t *replyTracker) releaseInitialReplySlot() {
	if !t.initialReplyReservationOn {
		return
	}
	t.initialReplyReservation.Do(func() {
		t.loopWG.Done()
	})
}

// getThinkingMsgID returns the id or "" if not yet set.
func (t *replyTracker) getThinkingMsgID() string {
	if p := t.thinkingMsgID.Load(); p != nil {
		return *p
	}
	return ""
}

func newIMEventTracker(ctx context.Context, p platform.Platform, chatID string) *replyTracker {
	supportsInterim := platform.SupportsInterimMessages(p)
	t := &replyTracker{
		ctx:             ctx,
		p:               p,
		chatID:          chatID,
		msgIDReady:      make(chan struct{}),
		editCh:          make(chan struct{}, 1),
		todoWake:        make(chan struct{}, 1),
		done:            make(chan struct{}),
		supportsInterim: supportsInterim,
	}
	// statusLines is only ever written when supportsInterim is true (see
	// onEvent's gate). Skip the per-turn make on platforms (Weixin,
	// non-edit Discord) that never use it. R216-PERF-19.
	if supportsInterim {
		t.statusLines = make([]string, 0, maxStatusLines)
	}
	if !supportsInterim {
		t.sent.Do(func() {
			close(t.msgIDReady)
		})
	} else {
		t.loopWG.Add(1)
		go t.editLoop()
		// Reserve a WaitGroup slot for the initial-Reply goroutine spawned
		// in onEvent's sent.Do. Adding inside sent.Do races stop()'s
		// loopWG.Wait() — once Wait observes counter == 0 it may return
		// before onEvent fires, and a later Add(1) is forbidden. The
		// reservation is released exactly once by releaseInitialReplySlot,
		// called either from the onEvent goroutine's defer or from stop().
		t.loopWG.Add(1)
		t.initialReplyReservationOn = true
	}
	t.loopWG.Add(1)
	go t.todoLoop()
	return t
}

// todoLoop reads the latest pendingTodo snapshot on each wake signal and
// posts it synchronously so at most one Reply is in flight at a time. The
// atomic.Pointer mailbox + wake semaphore pattern avoids the TOCTOU window
// that a drain-and-replace channel had: onEvent can overwrite pendingTodo
// unconditionally, todoLoop always reads the freshest value. Exits when
// t.done closes or ctx cancels. Defers Done so loopWG.Wait() unblocks in
// stop(). A final pendingTodo check on ctx.Done is deliberately skipped —
// if the turn was cancelled, posting a stale checklist to the chat is
// worse than dropping it.
func (t *replyTracker) todoLoop() {
	defer t.loopWG.Done()
	for {
		select {
		case <-t.todoWake:
			if p := t.pendingTodo.Swap(nil); p != nil {
				t.sendTodoMessage(*p)
			}
		case <-t.done:
			return
		case <-t.ctx.Done():
			return
		}
	}
}

// sendAskQuestionCard posts the AskUserQuestion card on a detached goroutine.
// onEvent runs on the readLoop path; a synchronous Feishu Open API call
// could park there for up to 15s on flaky networks, stalling every event
// for every session multiplexed through this process. The handler returns
// immediately while the card post completes in the background, bounded by
// its own 15s ctx. Any error falls back to a plain-text fallback post.
//
// Safety: snapshot (p, chatID) so later mutations to t don't race with the
// goroutine. R218-GO-1: rctx derives from context.Background() rather than
// turnCtx — the turn ctx may already be near its deadline (or cancelled by
// a fresh /new from the user) by the time the card is dispatched, which
// would silently abort the Feishu Open API call mid-flight and leave the
// user staring at an empty status line. The card is essentially a UI
// notification with its own 15s budget; it should outlive the originating
// turn so the user actually sees the question.
func (t *replyTracker) sendAskQuestionCard(aq *cli.AskQuestion) {
	if aq == nil || len(aq.Items) == 0 {
		return
	}
	p := t.p
	chatID := t.chatID

	// Track on loopWG so stop() blocks until the card send finishes — without
	// it a slow Feishu Reply parked inside SendQuestionCard could leak past the
	// turn boundary and post for the wrong session. R249-GO-1.
	t.loopWG.Add(1)
	go func() {
		defer t.loopWG.Done()
		defer func() {
			if r := recover(); r != nil {
				slog.Warn("ask_question: card send panic recovered",
					"chat_id", chatID, "tool_use_id", aq.ToolUseID, "panic", r)
			}
		}()
		// R247-ARCH-10 (#632): card-send detach goes through NotifyCtx
		// alongside the other dispatch sites. The card must outlive the
		// originating turn so a near-deadline /new doesn't drop it
		// mid-flight (R218-GO-1).
		rctx, cancel := NotifyCtx(nil, NotifyKindAskQuestionCard, platformReplyTimeout)
		defer cancel()

		if sender, ok := platform.AsCapability[platform.QuestionCardSender](p); ok {
			card := platform.QuestionCard{
				ToolUseID: aq.ToolUseID,
				Items:     make([]platform.QuestionItem, 0, len(aq.Items)),
			}
			for _, q := range aq.Items {
				opts := make([]platform.QuestionOption, 0, len(q.Options))
				for _, o := range q.Options {
					opts = append(opts, platform.QuestionOption{Label: o.Label, Description: o.Description})
				}
				card.Items = append(card.Items, platform.QuestionItem{
					Question: q.Question, Header: q.Header,
					MultiSelect: q.MultiSelect, Options: opts,
				})
			}
			if _, err := sender.SendQuestionCard(rctx, chatID, card); err != nil {
				slog.Warn("ask_question card send failed, falling back to text",
					"chat_id", chatID, "tool_use_id", aq.ToolUseID, "err", err)
				t.sendAskQuestionFallback(rctx, aq)
			}
			return
		}
		t.sendAskQuestionFallback(rctx, aq)
	}()
}

// sendAskQuestionFallback posts a plain-text message listing the questions +
// options so a user on a platform without native card support can still reply
// free-form (their next message becomes the answer).
func (t *replyTracker) sendAskQuestionFallback(ctx context.Context, aq *cli.AskQuestion) {
	var b strings.Builder
	b.WriteString("Claude 想请你确认：\n")
	for qi, q := range aq.Items {
		if q.Header != "" {
			fmt.Fprintf(&b, "\n【%s】", q.Header)
		} else {
			fmt.Fprintf(&b, "\n问题 %d：", qi+1)
		}
		b.WriteString(q.Question)
		b.WriteString("\n")
		for oi, o := range q.Options {
			fmt.Fprintf(&b, "  %d. %s", oi+1, o.Label)
			if o.Description != "" {
				fmt.Fprintf(&b, " — %s", o.Description)
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\n直接回复选项内容即可（例如：「Error style: Return an error」）。")
	if _, err := t.p.Reply(ctx, platform.OutgoingMessage{ChatID: t.chatID, Text: b.String()}); err != nil {
		slog.Debug("ask_question text fallback failed",
			"chat_id", t.chatID, "tool_use_id", aq.ToolUseID, "err", err)
	}
}

// sendTodoMessage posts the rendered checklist as a standalone Reply. Identical
// consecutive checklists are suppressed so repeated TodoWrite calls that didn't
// change anything don't spam the chat. Uses an independent bounded ctx so a
// hung platform call can't outlive the turn. todoLoop is the sole caller and
// runs in a single goroutine, so the dedup field is unsynchronised by design —
// the mutex Round 47 had was protecting a field with only one reader/writer.
func (t *replyTracker) sendTodoMessage(text string) {
	if text == "" {
		return
	}
	if t.lastTodoText == text {
		return
	}
	t.lastTodoText = text

	// R236-GO-1: detach from t.ctx so a near-deadline turn can still finish writing TodoWrite.
	// R247-ARCH-10 (#632): routed through NotifyCtx for parity with the
	// other dispatch detached-reply sites.
	rctx, cancel := NotifyCtx(t.ctx, NotifyKindTodoMessage, platformReplyTimeout)
	defer cancel()
	if _, err := t.p.Reply(rctx, platform.OutgoingMessage{ChatID: t.chatID, Text: text}); err != nil {
		// R238-CR-5: previously slog.Debug — silent because R236-GO-1
		// detached this Reply from t.ctx, so cancellation no longer
		// short-circuits errors. Promote to Warn to match the
		// ask_question card-send failure path on the same tracker
		// (Warn is the in-this-file convention for platform Reply
		// failures the user-visible turn cared about).
		slog.Warn("todo reply failed", "chat_id", t.chatID, "err", err)
	}
}

// stop signals the editLoop and todoLoop goroutines to exit and waits for
// them to finish. Safe to call multiple times. Waiting prevents a loop
// parked inside a slow platform Reply from leaking into the next turn and
// posting a stale status/checklist for the wrong session.
func (t *replyTracker) stop() {
	select {
	case <-t.done:
	default:
		close(t.done)
	}
	// Release the pre-allocated initial-Reply slot if onEvent never fired.
	// releaseInitialReplySlot is a no-op when the slot was already released
	// by the onEvent goroutine's defer.
	t.releaseInitialReplySlot()
	t.loopWG.Wait()
	// R246-GO-14: clear the pendingTodo mailbox after the loop has exited.
	// onEvent may have stashed a final todo snapshot just before close(t.done)
	// raced ahead of todoLoop's wake; without this Store(nil) the *string
	// (and its underlying byte buffer) stays reachable from the tracker
	// instance until the tracker itself is GC'd, holding ~few-hundred bytes
	// per stopped session for an extra GC cycle. Done after loopWG.Wait so
	// the loop cannot be racing against this Store at the same instant.
	t.pendingTodo.Store(nil)
}

func (t *replyTracker) onEvent(ev cli.Event) {
	// AskUserQuestion: when the assistant emits a tool_use for this tool,
	// the CLI auto-rejects it (verified in test/e2e/askuser — CC injects
	// is_error:true tool_result within ~3ms in -p mode). We surface the
	// question as a native interactive card (or a plain-text fallback)
	// so the next user turn carries the selected option(s).
	if ev.AskQuestion != nil {
		t.askQuestionFired.Store(true)
		t.sendAskQuestionCard(ev.AskQuestion)
		// Fall through so the existing status-banner logic (tool_use line etc.)
		// also runs — the card is a parallel surface, not a replacement.
	}

	// TodoWrite gets its own chat bubble: send as a standalone Reply so it
	// isn't overwritten by the next banner edit, and so platforms that don't
	// support interim edits (Weixin) still surface the checklist — the task
	// list is terminal output, not a transient "thinking" banner.
	//
	// Hand off to todoLoop via an atomic.Pointer mailbox + wake semaphore:
	// overwrite pendingTodo unconditionally (last-write-wins; TodoWrite is a
	// full snapshot so intermediate states are discardable), then signal
	// todoWake with a non-blocking send. todoLoop Swap-reads the pointer on
	// each wake so it always sees the freshest value — no race window where
	// a consumer drains and the producer's replace finds an empty queue.
	if text, ok := extractTodoMessage(ev); ok {
		t.pendingTodo.Store(&text)
		select {
		case t.todoWake <- struct{}{}:
		default:
			// Wake already pending; todoLoop will pick up the fresher
			// pendingTodo value when it processes the existing signal.
		}
		return
	}

	if !t.supportsInterim {
		return
	}

	// #1957: Only assistant events carry meaningful status content.
	// Result events and other non-assistant frames must not fire the
	// initial Reply banner: in passthrough mode a result event is
	// delivered to onEvent for each slot owner (including merged-follower
	// slots), and result events have ev.Message==nil which would cause
	// formatEventLine to return "" → fallback "💭 思考中..." → a permanent
	// orphan banner on platforms that support interim edits (Feishu).
	if ev.Type != "assistant" {
		return
	}

	line := formatEventLine(ev)
	if line == "" {
		line = "💭 思考中..."
	}

	t.linesMu.Lock()
	t.statusLines = appendStatusLine(t.statusLines, line)
	t.linesMu.Unlock()

	// First event fires the initial Reply. Render only here; subsequent events
	// defer rendering to editLoop's rate-limited drain.
	t.sent.Do(func() {
		snapshot := t.renderStatus()
		// The WaitGroup slot was pre-allocated in newIMEventTracker so that
		// stop() can't observe counter == 0 and return before this goroutine
		// finishes. releaseInitialReplySlot (via its sync.Once) ensures
		// the slot is Done'd exactly once regardless of whether onEvent
		// or stop runs first.
		go func() {
			defer t.releaseInitialReplySlot()
			defer close(t.msgIDReady)
			// Independent bounded ctx: a hung platform HTTP call would
			// otherwise keep this goroutine alive for the full turn timeout
			// (5min), blocking the editLoop waiter and downstream
			// shutdown WaitGroups. 15s is well above normal p99 Feishu
			// reply latency (<2s) and respects the parent ctx for early
			// cancel.
			rctx, cancel := context.WithTimeout(t.ctx, platformReplyTimeout)
			defer cancel()
			id, err := t.p.Reply(rctx, platform.OutgoingMessage{ChatID: t.chatID, Text: snapshot})
			if err == nil {
				t.thinkingMsgID.Store(&id)
			}
		}()
	})

	// Signal editLoop non-blockingly that new status is available.
	select {
	case t.editCh <- struct{}{}:
	default:
	}
}

// renderStatus joins statusLines into a single display string. Called once per
// rate-limited edit (and once for the initial Reply) — not per event.
func (t *replyTracker) renderStatus() string {
	t.linesMu.Lock()
	defer t.linesMu.Unlock()
	if len(t.statusLines) == 0 {
		return ""
	}
	// strings.Join allocates both a growing []byte scratch buffer and the
	// final string. For the common 3-10 line case a Builder with a capacity
	// estimate issues a single allocation.
	total := len(t.statusLines) - 1 // separators
	for _, l := range t.statusLines {
		total += len(l)
	}
	var b strings.Builder
	b.Grow(total)
	for i, l := range t.statusLines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(l)
	}
	return b.String()
}

// editLoop runs in a goroutine and rate-limits EditMessage calls to 1/s.
// This keeps onEvent non-blocking so Process.Send can drain eventCh at full speed.
// Exits when t.done is closed (turn completed) or ctx is cancelled.
func (t *replyTracker) editLoop() {
	defer t.loopWG.Done()
	select {
	case <-t.msgIDReady:
	case <-t.done:
		return
	case <-t.ctx.Done():
		return
	}

	// Go 1.23+ made timer Stop/Reset self-draining; the manual channel drain
	// of pre-1.23 idioms is no longer needed (and would even deadlock on a
	// zero-duration timer that has not yet fired on a slow scheduler).
	rateTimer := time.NewTimer(0)
	defer rateTimer.Stop()

	for {
		select {
		case <-t.editCh:
			// Render lazily — only once per rate-limited edit rather than per event.
			text := t.renderStatus()
			if msgID := t.getThinkingMsgID(); msgID != "" && text != "" {
				if err := t.p.EditMessage(t.ctx, msgID, text); err != nil {
					slog.Debug("status edit failed", "msg_id", msgID, "err", err)
				}
			}
			rateTimer.Reset(time.Second)
			select {
			case <-rateTimer.C:
			case <-t.done:
				return
			case <-t.ctx.Done():
				return
			}
		case <-t.done:
			return
		case <-t.ctx.Done():
			return
		}
	}
}

func (t *replyTracker) waitReady(ctx context.Context) {
	t.sent.Do(func() {
		close(t.msgIDReady)
	})
	select {
	case <-t.msgIDReady:
	case <-ctx.Done():
	}
}
