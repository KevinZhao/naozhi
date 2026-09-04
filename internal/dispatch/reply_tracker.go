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
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/platform"
)

// replyTracker manages IM status message streaming (thinking -> tool_use -> result).
// statusLines is mutated under linesMu by onEvent (serial CLI event loop) and
// read by editLoop; joining is deferred to the read path so events coalesced
// away by the 1/s rate limit cost no allocation.
type replyTracker struct {
	ctx    context.Context
	p      platform.Platform
	chatID string
	// chatType ("direct"/"group") is embedded into AskUserQuestion cards so
	// transports that can't recover it from the card callback (Feishu WS)
	// route the answer back to the originating session key.
	chatType string
	// agentID is embedded into AskUserQuestion cards so the answer routes back
	// to the asking agent session rather than "general" (#2148).
	agentID string
	// thinkingMsgID is written by the Reply goroutine spawned in onEvent and
	// read by editLoop/sendAndReply; on ctx cancel waitReady may return before
	// msgIDReady closes, so the read races the write — hence atomic.
	thinkingMsgID atomic.Pointer[string]
	msgIDReady    chan struct{}
	sent          sync.Once
	editCh        chan struct{} // buffered(1), signals editLoop to redraw
	done          chan struct{} // closed when the owning turn completes; exits editLoop
	// finalized is set by sendAndReply just before it writes the final answer
	// onto the banner. editLoop checks it on wake so a residual buffered editCh
	// signal cannot repaint stale interim status over the real answer (#2291).
	finalized atomic.Bool
	linesMu   sync.Mutex // guards statusLines
	// statusLines is capped at maxStatusLines by appendStatusLine (drops the
	// head when full); joined lazily in renderStatus.
	statusLines []string

	// TodoWrite delivery: onEvent stores the latest checklist into pendingTodo
	// (last-write-wins; TodoWrite is a full snapshot so intermediate states are
	// discardable) and signals todoWake (buffered(1)) so todoLoop consumes once
	// per burst with no drain-vs-replace TOCTOU window.
	pendingTodo atomic.Pointer[string]
	todoWake    chan struct{}
	// lastTodoText is read/written only from todoLoop; no synchronisation needed.
	lastTodoText string

	// loopWG tracks editLoop + todoLoop + the reserved initial-Reply goroutine
	// so stop() waits for them; otherwise a goroutine parked in a slow platform
	// Reply could leak into the next turn and post for the wrong session.
	loopWG sync.WaitGroup

	// initialReplyReservation Done's the pre-allocated loopWG slot for the
	// initial-Reply goroutine exactly once — from that goroutine or from stop()
	// if the turn ends before any event. Pre-allocating avoids Add(1) racing a
	// Wait() that already returned. Unreserved when supportsInterim=false.
	initialReplyReservation   sync.Once
	initialReplyReservationOn bool

	// supportsInterim caches platform.SupportsInterimMessages(p); it is
	// consulted per streaming event.
	supportsInterim bool

	// singleUseToken caches platform.UsesSingleUseReplyToken(p). Such
	// platforms (Weixin iLink) accept ONE reply per inbound message, so a
	// standalone TodoWrite Reply would burn the token and the final answer
	// would be rejected upstream; TodoWrite delivery is suppressed (#2147).
	singleUseToken bool

	// askQuestionFired records that this turn emitted an AskUserQuestion card;
	// sendAndReply uses it to suppress the bailout text `claude -p` produces
	// after auto-rejecting the tool, so only the card surfaces. Written from
	// onEvent, read after waitReady — atomic suffices.
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

// markFinalized signals that the final answer is being committed to the
// banner; editLoop then drops pending status redraws (#2291). Idempotent.
func (t *replyTracker) markFinalized() {
	t.finalized.Store(true)
}

// getThinkingMsgID returns the id or "" if not yet set.
func (t *replyTracker) getThinkingMsgID() string {
	if p := t.thinkingMsgID.Load(); p != nil {
		return *p
	}
	return ""
}

func newIMEventTracker(ctx context.Context, p platform.Platform, chatID, chatType, agentID string) *replyTracker {
	supportsInterim := platform.SupportsInterimMessages(p)
	singleUseToken := platform.UsesSingleUseReplyToken(p)
	t := &replyTracker{
		ctx:             ctx,
		p:               p,
		chatID:          chatID,
		chatType:        chatType,
		agentID:         agentID,
		msgIDReady:      make(chan struct{}),
		editCh:          make(chan struct{}, 1),
		todoWake:        make(chan struct{}, 1),
		done:            make(chan struct{}),
		supportsInterim: supportsInterim,
		singleUseToken:  singleUseToken,
	}
	// statusLines is only written when supportsInterim (onEvent's gate).
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
		// Reserve the loopWG slot for the initial-Reply goroutine here: Add(1)
		// inside sent.Do could run after stop()'s Wait returned with counter 0.
		// Released exactly once via releaseInitialReplySlot.
		t.loopWG.Add(1)
		t.initialReplyReservationOn = true
	}
	// Single-use-token platforms never post standalone TodoWrite (#2147); see
	// the matching gate in onEvent.
	if !t.singleUseToken {
		t.loopWG.Add(1)
		go t.todoLoop()
	}
	return t
}

// todoLoop posts the latest pendingTodo snapshot on each wake, synchronously
// so at most one Reply is in flight. Exits when t.done closes or ctx cancels;
// a final pendingTodo flush on ctx.Done is deliberately skipped — posting a
// stale checklist to a cancelled turn is worse than dropping it.
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

// sendAskQuestionCard posts the AskUserQuestion card on a detached goroutine:
// onEvent runs on the readLoop path and a synchronous Feishu call could park
// it for up to 15s, stalling every session in this process. The card's ctx
// derives from context.Background(), not the turn ctx: the turn may be near
// its deadline or cancelled by a fresh /new, which would abort the API call
// mid-flight and leave the user with no question. Errors fall back to a
// plain-text post. (p, chatID) are snapshotted so later mutations to t don't
// race the goroutine.
func (t *replyTracker) sendAskQuestionCard(aq *clievent.AskQuestion) {
	if aq == nil || len(aq.Items) == 0 {
		return
	}
	p := t.p
	chatID := t.chatID

	// Track on loopWG so stop() blocks until the card send finishes and cannot
	// leak past the turn boundary.
	t.loopWG.Add(1)
	go func() {
		defer t.loopWG.Done()
		defer func() {
			if r := recover(); r != nil {
				slog.Warn("ask_question: card send panic recovered",
					"chat_id", chatID, "tool_use_id", aq.ToolUseID, "panic", r)
			}
		}()
		// Detached via NotifyCtx like the other dispatch reply sites (#632).
		rctx, cancel := NotifyCtx(context.Background(), NotifyKindAskQuestionCard, platformReplyTimeout)
		defer cancel()

		if sender, ok := platform.AsCapability[platform.QuestionCardSender](p); ok {
			card := platform.QuestionCard{
				ToolUseID: aq.ToolUseID,
				ChatType:  t.chatType,
				AgentID:   t.agentID,
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
func (t *replyTracker) sendAskQuestionFallback(ctx context.Context, aq *clievent.AskQuestion) {
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

// sendTodoMessage posts the rendered checklist as a standalone Reply, skipping
// identical consecutive checklists. todoLoop is the sole caller (one
// goroutine), so lastTodoText needs no lock.
func (t *replyTracker) sendTodoMessage(text string) {
	if text == "" {
		return
	}
	if t.lastTodoText == text {
		return
	}
	t.lastTodoText = text

	// Detached from t.ctx via NotifyCtx so a near-deadline turn still delivers (#632).
	rctx, cancel := NotifyCtx(t.ctx, NotifyKindTodoMessage, platformReplyTimeout)
	defer cancel()
	if _, err := t.p.Reply(rctx, platform.OutgoingMessage{ChatID: t.chatID, Text: text}); err != nil {
		// Warn, not Debug: the Reply is detached, so cancellation no longer masks errors.
		slog.Warn("todo reply failed", "chat_id", t.chatID, "err", err)
	}
}

// stop signals editLoop/todoLoop to exit and waits for them, so a loop parked
// in a slow platform Reply cannot leak into the next turn and post a stale
// status/checklist for the wrong session. Safe to call multiple times.
func (t *replyTracker) stop() {
	select {
	case <-t.done:
	default:
		close(t.done)
	}
	// No-op if the onEvent goroutine already released the slot.
	t.releaseInitialReplySlot()
	t.loopWG.Wait()
	// Clear the mailbox after the loop exited so a final snapshot stashed just
	// before close(t.done) doesn't stay reachable until the tracker is GC'd.
	t.pendingTodo.Store(nil)
}

func (t *replyTracker) onEvent(ev cli.Event) {
	// The CLI auto-rejects AskUserQuestion in -p mode (is_error tool_result
	// within ~3ms); surface it as a card (or text fallback) so the next user
	// turn carries the selected option(s).
	if ev.AskQuestion != nil {
		t.askQuestionFired.Store(true)
		t.sendAskQuestionCard(ev.AskQuestion)
		// Fall through: the card is a parallel surface, the status banner still runs.
	}

	// TodoWrite gets its own chat bubble so it isn't overwritten by the next
	// banner edit and still surfaces on platforms without interim edits. Hand
	// off to todoLoop via the pendingTodo mailbox + todoWake (see fields).
	if text, ok := extractTodoMessage(ev); ok {
		// Single-use-token platforms: drop it, todoLoop isn't running (#2147).
		if t.singleUseToken {
			return
		}
		t.pendingTodo.Store(&text)
		select {
		case t.todoWake <- struct{}{}:
		default:
			// Wake already pending; todoLoop will read the fresher value.
		}
		return
	}

	if !t.supportsInterim {
		return
	}

	// Only assistant events carry status content (#1957): in passthrough mode
	// result events (ev.Message==nil) reach every slot owner and would fire a
	// permanent orphan "💭 思考中..." banner.
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
		// The loopWG slot was pre-reserved in newIMEventTracker; see releaseInitialReplySlot.
		go func() {
			defer t.releaseInitialReplySlot()
			defer close(t.msgIDReady)
			// Bounded ctx: a hung platform call must not hold this goroutine
			// (and editLoop + shutdown WaitGroups) for the full turn timeout.
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
	// Builder+Grow: one allocation instead of strings.Join's two.
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

	rateTimer := time.NewTimer(0)
	defer rateTimer.Stop()

	for {
		select {
		case <-t.editCh:
			// Skip the redraw once sendAndReply committed the final answer; a
			// residual buffered signal must not repaint stale status (#2291).
			if t.finalized.Load() {
				continue
			}
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
