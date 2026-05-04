package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// SessionGuard prevents multiple concurrent messages to the same session.
// Used by the Dashboard/WebSocket path (server/send.go) which retains
// interrupt-on-busy behavior. The IM path uses MessageQueue instead.
type SessionGuard interface {
	TryAcquire(key string) bool
	ShouldSendWait(key string) bool
	Release(key string)
}

// Dispatcher holds the dependencies needed to dispatch incoming IM messages
// to the session router, handle slash commands, and stream results back.
type Dispatcher struct {
	router        *session.Router
	platforms     map[string]platform.Platform
	agents        map[string]session.AgentOpts
	agentCommands map[string]string
	scheduler     *cron.Scheduler
	projectMgr    *project.Manager
	guard         SessionGuard // used by Dashboard/WS path
	queue         *MessageQueue
	dedup         *platform.Dedup
	allowedRoot   string
	claudeDir     string
	replyFooter   string

	noOutputTimeout       time.Duration
	totalTimeout          time.Duration
	watchdogNoOutputKills *atomic.Int64
	watchdogTotalKills    *atomic.Int64

	// Operational counters exposed via /health for triaging. Incremented
	// atomically and never reset (monotonic since process start).
	messageCount       atomic.Int64 // all non-slash-command IM messages accepted
	replyErrorCount    atomic.Int64 // errors returned by sendFn (includes timeouts)
	sendFailCount      atomic.Int64 // user-visible reply failures (platform send errors)
	lastReplySuccessNs atomic.Int64 // UnixNano of most recent successful user-visible reply; 0 until first success

	sendFn     func(ctx context.Context, key string, sess *session.ManagedSession, text string, images []cli.ImageData, onEvent cli.EventCallback) (*cli.SendResult, error)
	takeoverFn func(ctx context.Context, chatKey, key string, opts session.AgentOpts) bool
}

// Metrics returns a snapshot of operational counters for /health.
// Counter values are monotonic since process start. lastReplySuccess is the
// wall-clock time of the most recent successful user-visible reply; the zero
// value means "no reply has succeeded yet this process".
func (d *Dispatcher) Metrics() (messageCount, replyErrorCount, sendFailCount int64, lastReplySuccess time.Time) {
	ns := d.lastReplySuccessNs.Load()
	if ns != 0 {
		lastReplySuccess = time.Unix(0, ns)
	}
	return d.messageCount.Load(), d.replyErrorCount.Load(), d.sendFailCount.Load(), lastReplySuccess
}

// markReplySuccess records the wall-clock instant of the most recent
// successful reply (non-empty text to the user's chat).
func (d *Dispatcher) markReplySuccess() {
	d.lastReplySuccessNs.Store(time.Now().UnixNano())
}

// DispatcherConfig holds all dependencies for constructing a Dispatcher.
type DispatcherConfig struct {
	Router        *session.Router
	Platforms     map[string]platform.Platform
	Agents        map[string]session.AgentOpts
	AgentCommands map[string]string
	Scheduler     *cron.Scheduler
	ProjectMgr    *project.Manager
	Guard         SessionGuard
	Queue         *MessageQueue
	Dedup         *platform.Dedup
	AllowedRoot   string
	ClaudeDir     string
	ReplyFooter   string

	NoOutputTimeout       time.Duration
	TotalTimeout          time.Duration
	WatchdogNoOutputKills *atomic.Int64
	WatchdogTotalKills    *atomic.Int64

	SendFn     func(ctx context.Context, key string, sess *session.ManagedSession, text string, images []cli.ImageData, onEvent cli.EventCallback) (*cli.SendResult, error)
	TakeoverFn func(ctx context.Context, chatKey, key string, opts session.AgentOpts) bool
}

// NewDispatcher creates a Dispatcher from the given config.
func NewDispatcher(cfg DispatcherConfig) *Dispatcher {
	return &Dispatcher{
		router:                cfg.Router,
		platforms:             cfg.Platforms,
		agents:                cfg.Agents,
		agentCommands:         cfg.AgentCommands,
		scheduler:             cfg.Scheduler,
		projectMgr:            cfg.ProjectMgr,
		guard:                 cfg.Guard,
		queue:                 cfg.Queue,
		dedup:                 cfg.Dedup,
		allowedRoot:           cfg.AllowedRoot,
		claudeDir:             cfg.ClaudeDir,
		replyFooter:           cfg.ReplyFooter,
		noOutputTimeout:       cfg.NoOutputTimeout,
		totalTimeout:          cfg.TotalTimeout,
		watchdogNoOutputKills: cfg.WatchdogNoOutputKills,
		watchdogTotalKills:    cfg.WatchdogTotalKills,
		sendFn:                cfg.SendFn,
		takeoverFn:            cfg.TakeoverFn,
	}
}

// BuildHandler returns a platform.MessageHandler wired to this Dispatcher.
func (d *Dispatcher) BuildHandler() platform.MessageHandler {
	return func(ctx context.Context, msg platform.IncomingMessage) {
		// Dedup check at the top prevents duplicate processing from platform
		// retries (e.g., Feishu webhook timeout → re-delivery with same event_id).
		// Note: if guard fails below, the eventID is still consumed. This means
		// a platform retry during guard contention won't be re-processed. In
		// practice this is benign — the handler responds fast enough that
		// platforms don't retry, and the user is told to resend.
		if d.dedup.Seen(msg.EventID) {
			return
		}

		// Group chat gate: in group chats, only respond when explicitly mentioned.
		// Direct (1:1) chats are unaffected — every message is processed.
		//
		// Rationale: bots deployed in multi-user group chats should not reply to
		// every utterance; standard IM UX (Slack, Discord, Feishu bot guidance)
		// expects @bot to be the activation signal. Naozhi's primary usage is
		// 1:1 operator → agent, so groups are the exception.
		//
		// MentionMe is populated by each platform's transport layer:
		//   - slack / discord / weixin: already matched against bot self-ID (accurate)
		//   - feishu: currently "any mention" (loose) — tightened in a follow-up commit
		//
		// Gate is placed BEFORE dispatchCommand so slash commands in groups also
		// require @bot — consistent with social etiquette and simpler (single decision
		// point). Gated messages are silently dropped: no reply, no metric increment,
		// dedup entry stays consumed (platform retry won't re-process).
		if msg.ChatType == "group" && !msg.MentionMe {
			return
		}

		// Sanitize the IM-originated attrs before they reach slog. Platform,
		// UserID, and ChatID all flow through adversary-controlled IM webhook
		// fields; an attacker-chosen chat ID with embedded \n, \t, or ANSI
		// escape bytes would otherwise fragment log lines and let the
		// attacker forge entries. session.SanitizeLogAttr mirrors the
		// session-key component sanitization (strips C0/bidi/zero-width,
		// replaces colons, bounds length) so the logger's attr view matches
		// the session-key view in the log. R60-GO-H1.
		log := slog.With(
			"platform", session.SanitizeLogAttr(msg.Platform),
			"user", session.SanitizeLogAttr(msg.UserID),
			"chat", session.SanitizeLogAttr(msg.ChatID),
		)
		trimmed := strings.TrimSpace(msg.Text)

		// Dispatch slash commands (/help, /new, /cron, /cd, /pwd, /project)
		if d.dispatchCommand(ctx, msg, trimmed, log) {
			return
		}

		// Resolve agent from command prefix (e.g. "/review code" -> agent=code-reviewer, text="code")
		agentID, cleanText := session.ResolveAgent(trimmed, d.agentCommands)
		if cleanText == "" && len(msg.Images) == 0 {
			if agentID != "general" {
				d.replyText(ctx, msg, "请在指令后输入内容。", log)
			}
			return
		}

		// Warn about unrecognized slash commands (likely typos)
		// Skip paths like /home/user/... (contain slash after the leading one)
		if agentID == "general" && strings.HasPrefix(cleanText, "/") {
			cmd := cleanText
			if idx := strings.IndexByte(cleanText, ' '); idx >= 0 {
				cmd = cleanText[:idx]
			}
			if !strings.Contains(cmd[1:], "/") {
				d.replyText(ctx, msg, "未知命令: "+cmd+"\n输入 /help 查看可用命令，或直接发送消息。", log)
				return
			}
		}

		// Count accepted messages (post-dedup, post-command-filter). Does not
		// include slash commands, ignored non-text items, or dedup hits.
		d.messageCount.Add(1)

		// Determine session key and opts: project-bound chat routes to planner
		var key string
		opts := d.agents[agentID] // zero value = use router defaults

		if d.projectMgr != nil {
			if proj := d.projectMgr.ProjectForChat(msg.Platform, msg.ChatType, msg.ChatID); proj != nil {
				if agentID == "general" {
					key = proj.PlannerSessionKey()
					opts.Exempt = true
					opts.Workspace = proj.Path
					if m := d.projectMgr.EffectivePlannerModel(proj); m != "" {
						opts.Model = m
					}
					if p := d.projectMgr.EffectivePlannerPrompt(proj); p != "" {
						// Three-arg slice `[:len:len]` forces append to
						// allocate a fresh backing array instead of
						// writing past the caller's slice length —
						// opts.ExtraArgs starts life as a copy of the
						// shared d.agents[agentID].ExtraArgs value, and
						// the underlying array is therefore aliased with
						// every other caller that read the same map
						// entry. A plain two-arg append would smash those
						// neighbours with "--append-system-prompt" the
						// first time cap>len. Downstream router.go also
						// does a make+copy before handing args to the
						// CLI spawn, but we harden here too so a future
						// refactor that drops the downstream copy cannot
						// cross-contaminate agent configs. R37-CONCUR1.
						opts.ExtraArgs = append(opts.ExtraArgs[:len(opts.ExtraArgs):len(opts.ExtraArgs)], "--append-system-prompt", p)
					}
				} else {
					key = session.SessionKey(msg.Platform, msg.ChatType, msg.ChatID, agentID)
					opts.Workspace = proj.Path
				}
			}
		}
		if key == "" {
			key = session.SessionKey(msg.Platform, msg.ChatType, msg.ChatID, agentID)
		}

		// Convert platform images to CLI image data
		var images []cli.ImageData
		for _, img := range msg.Images {
			images = append(images, cli.ImageData{Data: img.Data, MimeType: img.MimeType})
		}

		// Enqueue message. If queue is nil or disabled, fall back to Guard.
		if d.queue != nil {
			qm := QueuedMsg{
				Text:      cleanText,
				Images:    images,
				MessageID: msg.MessageID,
				EnqueueAt: time.Now(),
			}
			isOwner, enqueued, shouldInterrupt, gen := d.queue.Enqueue(key, qm)
			if !isOwner {
				// Interrupt mode: the first queued follow-up for the active
				// turn fires a control_request to the CLI so the in-flight
				// turn aborts within ~300ms. The ongoing owner loop's Send()
				// will observe the CLI's natural result event, return, then
				// drain this queued message as the next prompt. All non-Sent
				// outcomes degrade to Collect semantics: the queued message
				// is still processed once the turn completes naturally.
				if shouldInterrupt {
					switch outcome := d.router.InterruptSessionViaControl(key); outcome {
					case session.InterruptSent:
						log.Info("interrupt mode: aborted active turn to process follow-up",
							"session_key", key)
					case session.InterruptNoTurn:
						// Session is spawning or idle — the turn isn't active yet,
						// so nothing to interrupt. The follow-up will be drained
						// by the owner loop after the first turn completes.
						log.Debug("interrupt mode: session idle or spawning, will process follow-up after current turn",
							"session_key", key)
					case session.InterruptNoSession:
						log.Debug("interrupt mode: session not found, falling back to collect",
							"session_key", key)
					case session.InterruptUnsupported:
						log.Debug("interrupt mode: protocol does not support stdin interrupt, falling back to collect",
							"session_key", key)
					case session.InterruptError:
						// Warn already emitted inside ManagedSession.InterruptViaControl;
						// keep a paired trace here to anchor the dispatch side.
						log.Warn("interrupt mode: transport error, falling back to collect",
							"session_key", key)
					}
				}
				if enqueued {
					// Prefer an in-place reaction on the user's own message
					// (non-intrusive) over a new bot chat bubble. Fall back to
					// the text notice if the platform isn't Reactor-capable,
					// has no inbound MessageID, or the reaction call fails —
					// ShouldNotify still rate-limits the fallback.
					if !d.ackQueuedWithReaction(ctx, msg, log) {
						if d.queue.ShouldNotify(key) {
							d.replyText(ctx, msg, "消息已收到，待当前回复完成后一并处理。", log)
						}
					}
				} else {
					// Queue disabled (maxDepth<=0) — degrade to old drop behavior.
					if d.queue.ShouldNotify(key) {
						d.replyText(ctx, msg, "正在处理上一条消息，请稍候...", log)
					}
				}
				return
			}
			// I am the owner — enter the process-and-drain loop.
			log.Info("message received", "agent", agentID, "text_len", len(cleanText), "images", len(images))
			d.ownerLoop(ctx, key, gen, qm, agentID, opts, msg, log)
			return
		}

		// Fallback: Guard-based path (no queue configured).
		if !d.guard.TryAcquire(key) {
			if d.guard.ShouldSendWait(key) {
				d.replyText(ctx, msg, "正在处理上一条消息，请稍候...", log)
			}
			return
		}
		defer d.guard.Release(key)
		defer d.router.NotifyIdle()

		log.Info("message received", "agent", agentID, "text_len", len(cleanText), "images", len(images))
		d.sendAndReply(ctx, key, cleanText, images, agentID, opts, msg, log, true)
	}
}

// discardQueue is a nil-safe helper to clear queued messages for a key.
func (d *Dispatcher) discardQueue(key string) {
	if d.queue != nil {
		d.queue.Discard(key)
	}
}

// ownerLoop processes the first message directly, then drains and coalesces
// any queued messages until the queue is empty. The owner goroutine is the
// platform handler goroutine that first acquired ownership via Enqueue.
//
// gen is the generation cookie from Enqueue. If Discard bumps the generation
// (e.g., user sends /new), DoneOrDrain returns nil and ownerLoop exits,
// preventing two goroutines from owning the same key.
//
// Panic-safe: a deferred recover releases ownership so a panic in SendFn
// doesn't leave the queue permanently locked.
func (d *Dispatcher) ownerLoop(
	ctx context.Context,
	key string,
	gen uint64,
	first QueuedMsg,
	agentID string,
	opts session.AgentOpts,
	msg platform.IncomingMessage,
	log *slog.Logger,
) {
	defer func() {
		if r := recover(); r != nil {
			d.handleOwnerLoopPanic(key, msg, r)
		}
	}()
	defer d.router.NotifyIdle()

	// Enrich the logger once for the whole ownerLoop lifetime. Previously
	// sendAndReply re-did this `log.With` on every drained turn — a coalesced
	// burst of 5 follow-ups meant 5 identical handler-chain allocs. Lifting
	// it here costs exactly one alloc per ownerLoop regardless of drain
	// depth. R61-PERF-12.
	log = log.With("session_key", key, "agent", agentID)

	// Process first message.
	d.sendAndReply(ctx, key, first.Text, first.Images, agentID, opts, msg, log, true)

	// Drain loop: after each turn, wait collectDelay then drain.
	collectTimer := time.NewTimer(d.queue.CollectDelay())
	defer collectTimer.Stop()
	for {
		select {
		case <-ctx.Done():
			d.queue.Discard(key)
			return
		case <-collectTimer.C:
		}

		queued := d.queue.DoneOrDrain(key, gen)
		if queued == nil {
			return // Queue empty or generation mismatch — stop.
		}

		text, images := CoalesceMessages(queued)
		log.Info("processing queued messages", "count", len(queued), "merged_len", len(text))
		d.sendAndReply(ctx, key, text, images, agentID, opts, msg, log, false)
		// Drained queued messages were acknowledged with a queue reaction
		// when they arrived; clear those reactions now that their content
		// was processed. Best-effort — errors only log.
		d.clearQueuedReactions(ctx, msg.Platform, queued, log)
		// Defensive Stop+drain before Reset: if a future refactor changes the
		// loop shape so the <-collectTimer.C arm can be skipped (e.g. an
		// early-continue branch), Reset without drain would let a stale tick
		// fire immediately on the next iteration.
		if !collectTimer.Stop() {
			select {
			case <-collectTimer.C:
			default:
			}
		}
		collectTimer.Reset(d.queue.CollectDelay())
	}
}

// handleOwnerLoopPanic is the deferred panic recovery helper for ownerLoop.
// Split out of the defer so the recover path can be unit-tested directly
// without having to construct a real panicking ownerLoop stack (GetOrCreate
// short-circuits before sendFn in the test harness). It:
//
//  1. Logs the panic with a full stack trace for operator triage.
//  2. Clears the message queue so a stale owner is not left holding the key.
//  3. Replies to the user with a "please retry" message so the IM peer is not
//     left waiting indefinitely for a response the process can no longer
//     produce. RETRY3.
//
// A nested recover around the reply call absorbs a cascading panic (e.g.,
// platform SDK panicking on a nil chat handle) so the outer defer always
// completes and the process can drain other owners cleanly.
func (d *Dispatcher) handleOwnerLoopPanic(key string, msg platform.IncomingMessage, r any) {
	slog.Error("ownerLoop panic", "key", key, "panic", r, "stack", string(debug.Stack()))
	if d.queue != nil {
		d.queue.Discard(key)
	}
	func() {
		defer func() { _ = recover() }()
		notifyCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		d.replyText(notifyCtx, msg, "处理异常，请稍后重试。", nil)
	}()
}

// sendAndReply performs one turn: GetOrCreate session, send message, deliver reply.
// isFirst indicates whether this is the first message (triggers takeover/session-new
// notifications); queued follow-ups skip these.
func (d *Dispatcher) sendAndReply(
	ctx context.Context,
	key, text string,
	images []cli.ImageData,
	agentID string,
	opts session.AgentOpts,
	msg platform.IncomingMessage,
	log *slog.Logger,
	isFirst bool,
) {
	// Session-key + agent attrs are attached once in ownerLoop (R61-PERF-12)
	// so every Info/Warn/Error line below carries enough context for an
	// operator to grep a full turn end-to-end without paying a per-call
	// handler-chain alloc.

	// Takeover check only on first message for a key.
	if isFirst {
		d.takeoverFn(ctx, session.ChatKey(msg.Platform, msg.ChatType, msg.ChatID), key, opts)
	}

	sess, sessStatus, err := d.router.GetOrCreate(ctx, key, opts)
	if err != nil {
		// Shutdown-path cancellation is expected noise, not an alarm;
		// downgrade to Info so ops dashboards don't light up on every
		// restart. Unexpected failures stay at Error.
		if errors.Is(err, context.Canceled) {
			log.Info("get session cancelled during shutdown", "err", err)
		} else {
			log.Error("get session", "err", err)
		}
		var errMsg string
		switch {
		case errors.Is(err, session.ErrMaxProcs):
			errMsg = "当前处理已满，请稍后重试。"
		case errors.Is(err, context.Canceled):
			errMsg = "系统正在重启，请稍后重试。"
		default:
			errMsg = "会话创建失败，请发送 /new 重置后重试。"
		}
		d.replyText(ctx, msg, errMsg, log)
		return
	}

	p := d.platforms[msg.Platform]
	if p == nil {
		log.Error("unknown platform")
		return
	}

	// Session lifecycle notifications only on first message.
	if isFirst {
		if sessStatus == session.SessionNew && platform.SupportsInterimMessages(p) {
			d.replyText(ctx, msg, "新会话已创建（之前的上下文已失效）。", log)
		}
	}

	tracker := newIMEventTracker(ctx, p, msg.ChatID)
	defer tracker.stop()

	result, err := d.sendFn(ctx, key, sess, text, images, tracker.onEvent)
	if err != nil {
		d.replyErrorCount.Add(1)
		log.Error("send to claude", "err", err)
		var errMsg string
		switch {
		case errors.Is(err, cli.ErrNoOutputTimeout):
			d.watchdogNoOutputKills.Add(1)
			errMsg = fmt.Sprintf("⏱️ 处理超时（%s 无输出），请简化任务后重试。", formatChineseDuration(d.noOutputTimeout))
		case errors.Is(err, cli.ErrTotalTimeout):
			d.watchdogTotalKills.Add(1)
			errMsg = fmt.Sprintf("⏱️ 处理超时（总耗时超过 %s），请拆分为更小的任务。", formatChineseDuration(d.totalTimeout))
		case errors.Is(err, cli.ErrProcessExited):
			// Subprocess died mid-turn. The next user message triggers
			// GetOrCreate to spawn a fresh process transparently; tell
			// the user to resend (do NOT claim "already reconnected" —
			// the reconnect happens on their next message).
			errMsg = "进程意外退出，请重新发送消息，系统会自动重启会话。"
		default:
			errMsg = "处理失败，请发送 /new 重置后重试。"
		}
		if _, err := platform.ReplyWithRetry(ctx, p, platform.OutgoingMessage{ChatID: msg.ChatID, Text: errMsg}, 3); err != nil {
			d.sendFailCount.Add(1)
			log.Warn("error reply also failed", "chat", msg.ChatID, "err", err)
		}
		return
	}

	log.Info("message replied", "result_len", len(result.Text), "cost", result.CostUSD)

	// Record turn success regardless of reply text length. A successful
	// sendFn with empty result (e.g. a turn that only produces tool calls
	// or whose text was stripped) still constitutes a healthy end-to-end
	// roundtrip; gating markReplySuccess on non-empty text previously made
	// /health's lastReplySuccess go stale on otherwise-healthy sessions.
	d.markReplySuccess()

	replyText := localizeAPIError(result.Text)
	if d.replyFooter != "" {
		replyText += "\n\n— " + d.replyFooter
	}
	var outImages []platform.Image
	for _, path := range cli.ExtractImagePaths(replyText) {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		outImages = append(outImages, platform.Image{Data: data, MimeType: cli.MimeFromPath(path)})
		replyText = strings.ReplaceAll(replyText, path, "[图片]")
	}

	tracker.waitReady(ctx)

	if replyText != "" {
		if msgID := tracker.getThinkingMsgID(); msgID != "" {
			if err := p.EditMessage(ctx, msgID, replyText); err != nil {
				slog.Warn("edit message failed, sending new", "err", err)
				d.SendSplitReply(ctx, p, msg.ChatID, replyText)
			}
		} else {
			d.SendSplitReply(ctx, p, msg.ChatID, replyText)
		}
	}

	for _, img := range outImages {
		if _, err := p.Reply(ctx, platform.OutgoingMessage{
			ChatID: msg.ChatID,
			Images: []platform.Image{img},
		}); err != nil {
			slog.Warn("send image failed", "err", err)
		}
	}
}

// SendSplitReply sends a reply, splitting into multiple messages if too long.
func (d *Dispatcher) SendSplitReply(ctx context.Context, p platform.Platform, chatID, text string) {
	maxLen := p.MaxReplyLength()
	if maxLen <= 0 {
		maxLen = 4000
	}

	chunks := platform.SplitText(text, maxLen)
	total := len(chunks)
	for i, chunk := range chunks {
		if total > 1 {
			chunk += fmt.Sprintf("\n— [%d/%d]", i+1, total)
		}
		if _, err := platform.ReplyWithRetry(ctx, p, platform.OutgoingMessage{ChatID: chatID, Text: chunk}, 3); err != nil {
			d.sendFailCount.Add(1)
			slog.Error("reply chunk failed after retries", "chat", chatID, "chunk", i+1, "err", err)
		} else {
			d.markReplySuccess()
		}
	}
}

// formatChineseDuration formats a duration into a short Chinese string.
func formatChineseDuration(d time.Duration) string {
	if d <= 0 {
		return "未知"
	}
	if d >= time.Hour && d%time.Hour == 0 {
		return fmt.Sprintf("%d 小时", int(d.Hours()))
	}
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	}
	return fmt.Sprintf("%d 秒", int(d.Seconds()))
}

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
	statusLines   []string      // pre-allocated capped ring; joined lazily

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
	t := &replyTracker{
		ctx:         ctx,
		p:           p,
		chatID:      chatID,
		statusLines: make([]string, 0, maxStatusLines),
		msgIDReady:  make(chan struct{}),
		editCh:      make(chan struct{}, 1),
		todoWake:    make(chan struct{}, 1),
		done:        make(chan struct{}),
	}
	if !platform.SupportsInterimMessages(p) {
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

	rctx, cancel := context.WithTimeout(t.ctx, 15*time.Second)
	defer cancel()
	if _, err := t.p.Reply(rctx, platform.OutgoingMessage{ChatID: t.chatID, Text: text}); err != nil {
		slog.Debug("todo reply failed", "chat_id", t.chatID, "err", err)
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
}

func (t *replyTracker) onEvent(ev cli.Event) {
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

	if !platform.SupportsInterimMessages(t.p) {
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
			rctx, cancel := context.WithTimeout(t.ctx, 15*time.Second)
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
