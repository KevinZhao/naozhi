package dispatch

import (
	"context"
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session/sessionview"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/turn"
)

// imKind is what an IM request was submitted as, for its admission log line.
type imKind uint8

const (
	imMessage imKind = iota // an ordinary message (BuildHandler)
	imUrgent                // /urgent <message>
)

// imOrigin is one inbound IM message as a turn.Origin: it acks admission with
// a ⏳ (ReactionQueued) on the user's message, opens an imDelivery for every
// turn that answers it, and clears the ⏳ when the request is dropped.
type imOrigin struct {
	d       *Dispatcher
	msg     platform.IncomingMessage
	lg      *slog.Logger
	key     string
	agentID string
	opts    sessionview.AgentOpts
	kind    imKind
	textLen int
	images  int
	// ackDone is closed once the ⏳ of a request that runs at once has
	// landed or failed (startAck); nil for any other request. reacted is
	// written before the close and read only after it (awaitAck).
	ackDone chan struct{}
	reacted bool
}

func (d *Dispatcher) newIMOrigin(msg platform.IncomingMessage, lg *slog.Logger, key, agentID string, opts sessionview.AgentOpts, kind imKind, textLen, images int) *imOrigin {
	return &imOrigin{d: d, msg: msg, lg: lg, key: key, agentID: agentID, opts: opts, kind: kind, textLen: textLen, images: images}
}

// submit hands r to the orchestrator with o as its origin. A message from a
// platform the dispatcher does not know gets no turn: there is nowhere to
// deliver the reply. Neither does one past today's budget.
func (d *Dispatcher) submit(ctx context.Context, o *imOrigin, r turn.Request) {
	if d.platforms[o.msg.Platform] == nil {
		o.lg.Error("unknown platform")
		return
	}
	if !d.admitBudget(ctx, o) {
		return
	}
	r.Origin = o
	d.turns.Submit(ctx, r, imAdmission{ctx: ctx, stop: d.stopCtx})
}

// imAdmission runs an IM owner loop inline on the platform handler's
// goroutine, and a detached turn on its own goroutine whose ctx is cancelled
// by the process stop ctx but carries the inbound ctx's values (#1320). It
// never declines.
type imAdmission struct {
	ctx, stop context.Context
}

func (a imAdmission) Admit(kind turn.RunKind) (func(fn func(ctx context.Context)), bool) {
	if kind == turn.RunOwner {
		return func(fn func(ctx context.Context)) { fn(a.ctx) }, true
	}
	ctx := mergeStopAndValues(a.stop, a.ctx)
	return func(fn func(ctx context.Context)) { go fn(ctx) }, true
}

// Sink is the chat, plus the thread when there is one: two threads of a
// chat that share a session key each get the reply in their own thread.
func (o *imOrigin) Sink() string {
	if o.msg.ThreadID != "" {
		return o.Scope() + "#" + o.msg.ThreadID
	}
	return o.Scope()
}

// Scope is the chat, whichever thread: an owner loop whose batch has a
// message from the owner's chat does not also answer it in the owner's
// thread.
func (o *imOrigin) Scope() string {
	return "im:" + sessionkey.ChatKey(o.msg.Platform, o.msg.ChatType, o.msg.ChatID)
}

func (o *imOrigin) SessionOpts(string) sessionview.AgentOpts { return o.opts }

// Admitted acks the message with a ⏳, or a rate-limited busy notice and a
// log line when the queue is disabled. A queued request whose ⏳ fails gets a
// rate-limited notice instead; one that runs now or detached does not, its
// reply is the ack, and its ⏳ goes on in the background so the turn does not
// wait for the IM API (#3329). Every clear waits for that add (#1963).
func (o *imOrigin) Admitted(ctx context.Context, a turn.Ack) {
	d := o.d
	switch a {
	case turn.AckOwner:
		o.lg.Info("message received", "agent", o.agentID, "text_len", o.textLen, "images", o.images)
		o.startAck(ctx)
	case turn.AckDetached:
		if o.kind == imUrgent {
			o.lg.Info("/urgent dispatched", "key", o.key, "text_len", o.textLen)
		} else {
			o.lg.Info("message received (passthrough)", "agent", o.agentID, "text_len", o.textLen, "images", o.images)
		}
		o.startAck(ctx)
	case turn.AckQueued:
		if !d.ackQueuedWithReaction(ctx, o.msg, o.lg) {
			d.replyNotice(ctx, o.msg, o.key, "消息已收到，待当前回复完成后一并处理。", o.lg, "queued")
		}
	case turn.AckDropped:
		notified := d.replyNotice(ctx, o.msg, o.key, "正在处理上一条消息，请稍候...", o.lg, "busy")
		o.lg.Info("message dropped: session busy", "key", o.key, "notified", notified)
	case turn.AckShuttingDown:
		o.lg.Warn("message declined: shutting down", "key", o.key)
	}
}

// startAck adds the ⏳ on its own goroutine. WithoutCancel: a detached
// request's inbound ctx can end as soon as Submit returns; the add is still
// bounded by reactionAckTimeout.
func (o *imOrigin) startAck(ctx context.Context) {
	done := make(chan struct{})
	o.ackDone = done
	go func() {
		defer close(done)
		o.reacted = o.d.ackQueuedWithReaction(context.WithoutCancel(ctx), o.msg, o.lg)
	}()
}

// ackWaitLimit bounds awaitAck past reactionAckTimeout, for a Reactor that
// ignores its ctx.
const ackWaitLimit = reactionAckTimeout + time.Second

// awaitAck waits for startAck's add and reports whether the ⏳ landed; false
// at once when no add was started, and false when the add outlives
// ackWaitLimit.
func (o *imOrigin) awaitAck() bool {
	if o.ackDone == nil {
		return false
	}
	t := time.NewTimer(ackWaitLimit)
	defer t.Stop()
	select {
	case <-o.ackDone:
		return o.reacted
	case <-t.C:
		return false
	}
}

// Dropped clears the ⏳ of a request that will never get a turn (#1945,
// #2013). Only queued requests are dropped, and their add is synchronous;
// awaitAck guards a dropped request that ran startAck, so its ⏳ cannot land
// behind the clear. A removed key also gets a notice (#3297): the user did
// not ask for the removal, and a platform without reactions promised to
// answer. It is rate-limited per sink on the key (chats can share a planner
// key), so the queued messages of a chat or thread share one.
func (o *imOrigin) Dropped(ctx context.Context, why turn.DropReason) {
	o.awaitAck()
	o.d.clearQueuedReaction(ctx, o.msg.Platform, o.msg.MessageID, o.lg)
	if why == turn.DropRemoved {
		o.d.replyNotice(ctx, o.msg, o.key+"\x00"+o.Sink(), "会话已结束，这条消息未被处理，请重新发送。", o.lg, "removed")
	}
}

// Begin opens the reply to o's chat. An Observer is answered like a Head:
// the owner's chat gets the reply to whatever the owner loop drained from
// other chats (#3004 分叉 19a), and so does every chat with a request in the
// batch.
func (o *imOrigin) Begin(_ context.Context, t turn.TurnInfo) turn.Delivery {
	p := o.d.platforms[o.msg.Platform]
	if p == nil {
		return nil
	}
	lg := o.lg.With("key", o.key, "agent", o.agentID)
	return &imDelivery{o: o, info: t, p: p, lg: lg}
}

// imDelivery is one turn's reply to one IM chat.
type imDelivery struct {
	o       *imOrigin
	info    turn.TurnInfo
	p       platform.Platform
	lg      *slog.Logger
	tracker *replyTracker
}

// Blocking: the IM reply goes out after the dashboard's post-turn broadcast,
// the order the IM path has always had.
func (dl *imDelivery) Blocking() bool { return true }

// BeforeSession starts the tracker that streams the turn's progress into the
// chat. A head that runs at once is armed with a fallback banner that posts
// only if its message got no ⏳, so a slow spawn is covered too. On a first
// turn of a session the whole chat shares it then offers the chat's external
// session for takeover; a thread's or member's session starts fresh. The
// result is ignored: GetOrCreate resumes an adopted session and spawns a
// fresh one otherwise.
func (dl *imDelivery) BeforeSession(ctx context.Context) {
	o := dl.o
	dl.tracker = newIMEventTracker(ctx, dl.p, replyDestOf(o.msg), o.msg.ChatType, o.agentID)
	if dl.info.Role == turn.RoleHead && o.ackDone != nil {
		dl.tracker.armFallbackBanner(o.d.fallbackBannerDelay, o.awaitAck)
	}
	if !dl.info.First || o.d.scopedSession(o.msg, o.key) {
		return
	}
	_ = o.d.caps.Takeover(ctx, sessionkey.ChatKey(o.msg.Platform, o.msg.ChatType, o.msg.ChatID), o.key, o.opts)
}

// SessionReady tells the chat when its session came back without its
// context and returns the tracker's callback for the turn's events. Only
// SessionResumeLost gets the notice: SessionNew never follows lost context (a
// first chat, a reset that already replied, a dashboard Remove, a prune of an
// orphan that never had an ID; #3000).
func (dl *imDelivery) SessionReady(ctx context.Context, st sessionview.SessionStatus) clievent.EventCallback {
	if st == sessionview.SessionResumeLost && platform.SupportsInterimMessages(dl.p) {
		dl.o.d.replyNotice(ctx, dl.o.msg, "", "之前的会话记录已丢失，已开始新会话。", dl.lg, "resume_lost")
	}
	return dl.tracker.onEvent
}

// Finish replies with the turn's outcome, then clears the ⏳ of every request
// this delivery answers, also when the reply panics: the turn layer never
// calls a panicked Finish again. On a panic outcome the ⏳ go first and the
// reply is the generic retry notice.
func (dl *imDelivery) Finish(ctx context.Context, out turn.Outcome) {
	o, d := dl.o, dl.o.d
	if dl.tracker != nil {
		// stop is idempotent; the defer covers a reply that panics.
		defer dl.tracker.stop()
	}
	if out.Panic {
		if dl.tracker != nil {
			dl.tracker.stop()
		}
		d.clearQueuedReactions(context.WithoutCancel(ctx), o.msg.Platform, dl.queuedIDs(), dl.lg)
		notifyCtx, cancel := NotifyCtx(context.Background(), NotifyKindOwnerLoopPanic, platformReplyTimeout)
		defer cancel()
		d.replyText(notifyCtx, o.msg, "处理异常，请稍后重试。", dl.lg)
		return
	}
	// WithoutCancel: on a shutdown-during-turn race ctx is already Done and
	// a child WithTimeout would be born cancelled (#2262).
	defer func() {
		d.clearQueuedReactions(context.WithoutCancel(ctx), o.msg.Platform, dl.queuedIDs(), dl.lg)
	}()
	if out.Stage != turn.StageDone && dl.tracker != nil {
		// Before the error text, so a fallback banner cannot post below it.
		dl.tracker.stop()
	}
	switch out.Stage {
	case turn.StageSession:
		replyCtx, cleanup, errMsg := d.handleGetOrCreateError(ctx, out.Err, dl.lg)
		d.replyText(replyCtx, o.msg, errMsg, dl.lg)
		if cleanup != nil {
			cleanup()
		}
	case turn.StageSend:
		d.handleSendError(ctx, out.Err, o.key, o.msg, dl.p, dl.lg, dl.info.Primary)
	default:
		dl.reply(ctx, out.Result, out.Sess)
	}
	if dl.tracker != nil {
		dl.tracker.stop()
	}
}

// queuedIDs are the message IDs carrying a ⏳ that this delivery answers: the
// Mates, and the head itself unless it is a first turn's message whose ⏳
// never landed. It waits for the head's add still in flight, so the ⏳
// cannot land after it is cleared.
func (dl *imDelivery) queuedIDs() []string {
	var ids []string
	if dl.info.Role == turn.RoleHead {
		if reacted := dl.o.awaitAck(); !dl.info.First || reacted {
			ids = append(ids, dl.o.msg.MessageID)
		}
	}
	for _, m := range dl.info.Mates {
		if mo, ok := m.(*imOrigin); ok {
			ids = append(ids, mo.msg.MessageID)
		}
	}
	return ids
}

// reply delivers a turn that ended with a result: the merge hint for a
// passthrough merge follower, else the decorated answer or failure notice
// (edited into the progress banner when there is one) and its images. An
// aborted turn with nothing to say only marks the banner.
func (dl *imDelivery) reply(ctx context.Context, result *clievent.SendResult, sess turn.Session) {
	o, d, p, tracker := dl.o, dl.o.d, dl.p, dl.tracker
	dl.lg.Info("message replied", "result_len", len(result.Text), "cost", result.CostUSD,
		"merged_count", result.MergedCount, "merged_with_head", result.MergedWithHead)

	// A merge follower (MergedWithHead set; the head slot is 0 and may carry
	// an empty failed result) collapses any "💭思考中…" banner into the merge
	// hint (#2290), finalized first so a late editLoop redraw does not
	// overwrite it (#2338).
	if result.MergedWithHead != 0 && result.Text == "" {
		tracker.waitReady(ctx)
		tracker.markFinalized()
		if msgID := tracker.getThinkingMsgID(); msgID != "" {
			if err := p.EditMessage(ctx, msgID, "已合并到上一条回复。"); err != nil {
				slog.Debug("merge follower banner edit failed", "msg_id", msgID, "err", err)
			}
		}
		d.ackMergedFollower(ctx, o.msg, o.key, result.MergedCount, dl.lg)
		d.markReplySuccess()
		return
	}

	// Record success regardless of text length or outcome: an empty or failed
	// result is still a healthy roundtrip for /health's lastReplySuccess.
	d.markReplySuccess()

	replyText := d.decorateReplyText(result, sess)
	if replyText != "" {
		replyText += d.budgetWarnLine(o.key)
	}
	outImages, replyText := d.readTurnImages(replyText)

	// Passthrough turns are bound to d.stopCtx; if SIGTERM lands between
	// Send returning and delivery, a Done ctx would silently drop the answer.
	// Swap to the shutdown-budget ctx like the error paths (#2316).
	ctx, cleanup := resolveReplyCtx(ctx)
	if cleanup != nil {
		defer cleanup()
	}

	tracker.waitReady(ctx)

	// Finalize before the final edit so a late editLoop redraw cannot
	// overwrite the real answer with stale interim status (#2291).
	tracker.markFinalized()

	// AskUserQuestion: `claude -p` auto-rejects the tool and emits a bailout
	// text redundant with the card; replace it with a wait-hint on the banner
	// so the IM view is card + one "waiting" line.
	if tracker.askQuestionFired.Load() {
		if msgID := tracker.getThinkingMsgID(); msgID != "" {
			if err := p.EditMessage(ctx, msgID, "⏳ 等待你的选择…"); err != nil {
				slog.Debug("ask_question: banner edit failed", "err", err)
			}
		}
		dl.lg.Info("ask_question suppressed redundant reply", "result_len", len(result.Text))
	} else if replyText != "" {
		if msgID := tracker.getThinkingMsgID(); msgID != "" {
			d.replyIntoBanner(ctx, p, replyDestOf(o.msg), msgID, replyText)
		} else {
			d.SendSplitReply(ctx, p, replyDestOf(o.msg), replyText)
		}
	} else if result.Aborted || result.CLIAborted() {
		// naozhi stopped the turn (/stop, interrupt, /urgent), which already
		// said so, or claude reports it aborted; either way only the banner's
		// last tool status needs replacing.
		if msgID := tracker.getThinkingMsgID(); msgID != "" {
			if err := p.EditMessage(ctx, msgID, bannerAborted); err != nil {
				slog.Debug("aborted turn banner edit failed", "msg_id", msgID, "err", err)
			}
		}
	}

	// outImages derive from replyText; when the card suppresses the text,
	// suppress its images too or orphaned bubbles follow the card (#1959).
	if !tracker.askQuestionFired.Load() {
		d.sendOutboundImages(ctx, p, replyDestOf(o.msg), outImages)
	}
}

// bannerAnsweredBelow replaces the progress banner when the answer could not
// be edited into it and went out as new messages instead.
const bannerAnsweredBelow = "✅ 已回复，见下方"

// bannerAborted replaces the progress banner of an aborted turn.
const bannerAborted = "已中断。"

// replyIntoBanner edits the first reply chunk into the progress banner and
// sends the rest as new messages, so the edit obeys MaxReplyLength like any
// send. If that edit fails, every chunk is sent and the banner is replaced by
// bannerAnsweredBelow so it does not keep showing the last tool status.
func (d *Dispatcher) replyIntoBanner(ctx context.Context, p platform.Platform, to ReplyDest, msgID, text string) {
	chunks := replyChunks(p, text)
	if err := p.EditMessage(ctx, msgID, chunks[0]); err != nil {
		slog.Warn("edit message failed, sending new", "err", err, "chunks", len(chunks))
		d.sendChunks(ctx, p, to, chunks)
		if err := p.EditMessage(ctx, msgID, bannerAnsweredBelow); err != nil {
			slog.Debug("banner answered-below edit failed", "msg_id", msgID, "err", err)
		}
		return
	}
	d.sendChunks(ctx, p, to, chunks[1:])
}
