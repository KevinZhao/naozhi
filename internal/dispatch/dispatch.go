package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/limits"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/textutil"
	"github.com/naozhi/naozhi/internal/usermsg"
)

// platformReplyTimeout caps every outbound platform.Reply / EditMessage call.
const platformReplyTimeout = 15 * time.Second

// shutdownReplyTimeout caps best-effort replies on the shutdown /
// context.Canceled path; shorter than platformReplyTimeout so teardown is
// not blocked on a slow IM API yet the notice still lands before SIGKILL.
const shutdownReplyTimeout = 5 * time.Second

// SessionGuard gates concurrent messages to one session. MessageQueue is the
// production IM implementation; session.Guard and test fakes also satisfy it,
// so keep the method set minimal. Kept as an interface deliberately (#1170).
type SessionGuard interface {
	TryAcquire(key string) bool
	ShouldSendWait(key string) bool
	Release(key string)
}

// Dispatcher holds the dependencies needed to dispatch incoming IM messages
// to the session router, handle slash commands, and stream results back.
type Dispatcher struct {
	// router is the SessionRouter subset used by dispatch (consumer.go);
	// non-nil in production wiring.
	router    SessionRouter
	platforms map[string]platform.Platform
	// agents / agentCommands are immutable after NewDispatcher; the IM hot
	// path reads them lock-free, so any future mutation MUST switch to
	// atomic.Pointer swap-on-write or add a mutex.
	agents        map[string]session.AgentOpts
	agentCommands map[string]string
	// knownAgentIDs is the read-only set isKnownAgent accepts: agentCommands
	// values plus "general"/"planner"; built once in NewDispatcher (#2148).
	knownAgentIDs map[string]struct{}
	// scheduler is the cron consumer surface for /cron commands (#1178).
	// nil when cron is disabled; every call site gates on `d.scheduler != nil`.
	scheduler CronCommands
	// projectMgr backs slash-command project handling (/new, /cd, /project).
	// Routing on the IM hot path MUST go through resolver only — do not
	// reintroduce ProjectForChat / EffectivePlanner* reads there.
	// nil when projects.root is unconfigured; handlers gate on nil (#457).
	projectMgr ProjectStore
	// resolver centralises (key, opts) derivation; NewDispatcher guarantees
	// non-nil. See docs/rfc/key-resolver.md.
	resolver    *session.KeyResolver
	guard       SessionGuard // used by Dashboard/WS path
	queue       *MessageQueue
	dedup       *platform.Dedup
	allowedRoot string
	claudeDir   string

	noOutputTimeout       time.Duration
	totalTimeout          time.Duration
	watchdogNoOutputKills *atomic.Int64
	watchdogTotalKills    *atomic.Int64

	// imageReader resolves cli-extracted image paths for outbound
	// platform.Image payloads; tests inject an in-memory fake (#884).
	// Always non-nil after NewDispatcher.
	imageReader ImageReader

	// stopCtx is the process-shutdown context. The passthrough send branch
	// detaches from the per-webhook ctx but must still observe SIGTERM via
	// this; NewDispatcher defaults it to context.Background() (#1320).
	stopCtx context.Context

	// Operational counters exposed via /health for triaging. Incremented
	// atomically and never reset (monotonic since process start).
	messageCount       atomic.Int64 // all non-slash-command IM messages accepted
	replyErrorCount    atomic.Int64 // errors returned by Capabilities.Send (includes timeouts)
	sendFailCount      atomic.Int64 // user-visible reply failures (platform send errors)
	lastReplySuccessNs atomic.Int64 // UnixNano of most recent successful user-visible reply; 0 until first success

	// caps groups the host-supplied hooks (Send / Takeover / ReplyFooter).
	// Always non-nil after NewDispatcher (Capabilities, wrapped legacy *Fn
	// closures, or NoopCapabilities{}). NewDispatcher returns
	// ErrSendWireupMissing when no usable Send is supplied and
	// AllowMissingSender is false; NoopCapabilities.Send still panics if reached.
	caps Capabilities

	// inboundLogCache memoizes the per-(platform,user,chat) logger built in
	// prepareInbound (#2233). Zero value ready; see inbound_logcache.go.
	inboundLogCache inboundLogCache
}

// keyForChat returns the routed session key for the chat coordinates and
// agentID via KeyResolver (project-bound general → planner precedence).
func (d *Dispatcher) keyForChat(platform, chatType, chatID, agentID string) string {
	return d.resolver.KeyForChat(platform, chatType, chatID, agentID)
}

// isKnownAgent reports whether agentID is a recognised agent target, used to
// whitelist an explicit IncomingMessage.AgentID (e.g. a Feishu card click) so
// a hostile or replayed value cannot route into an arbitrary agent (#2148).
func (d *Dispatcher) isKnownAgent(agentID string) bool {
	_, ok := d.knownAgentIDs[agentID]
	return ok
}

// Metrics returns a snapshot of operational counters for /health. Counters are
// monotonic since process start; lastReplySuccess is zero until a reply succeeds.
func (d *Dispatcher) Metrics() (messageCount, replyErrorCount, sendFailCount int64, lastReplySuccess time.Time) {
	ns := d.lastReplySuccessNs.Load()
	if ns != 0 {
		lastReplySuccess = time.Unix(0, ns)
	}
	return d.messageCount.Load(), d.replyErrorCount.Load(), d.sendFailCount.Load(), lastReplySuccess
}

// markReplySuccess records the time of the most recent successful reply.
func (d *Dispatcher) markReplySuccess() {
	d.lastReplySuccessNs.Store(time.Now().UnixNano())
}

// DispatcherConfig holds all dependencies for constructing a Dispatcher.
type DispatcherConfig struct {
	Router        *session.Router
	Platforms     map[string]platform.Platform
	Agents        map[string]session.AgentOpts
	AgentCommands map[string]string
	// Scheduler is the cron consumer surface; nil disables /cron commands.
	Scheduler  CronCommands
	ProjectMgr *project.Manager
	// Resolver is the central (key, opts) derivation. Optional: when nil,
	// NewDispatcher fabricates a fallback from Agents / ProjectMgr.
	Resolver    *session.KeyResolver
	Guard       SessionGuard
	Queue       *MessageQueue
	Dedup       *platform.Dedup
	AllowedRoot string
	ClaudeDir   string

	// Capabilities groups the host-supplied hooks (Send / Takeover /
	// ReplyFooter). Wins over the legacy *Fn closures when both are set;
	// nil falls back to the closures, then NoopCapabilities{}.
	Capabilities Capabilities

	// ReplyFooterFn returns the per-session reply tag (e.g. "cc" / "kiro")
	// for a backend ID; empty backend means "not pinned yet". nil means no
	// footer.
	//
	// Deprecated: prefer DispatcherConfig.Capabilities. Removal, together
	// with SendFn / TakeoverFn / closureCapabilities, is gated on test
	// migrations (#374).
	ReplyFooterFn func(backendID string) string

	NoOutputTimeout       time.Duration
	TotalTimeout          time.Duration
	WatchdogNoOutputKills *atomic.Int64
	WatchdogTotalKills    *atomic.Int64

	// ImageReader resolves outbound image paths to bytes. Optional —
	// defaults to osImageReader{}; tests inject a fake (#884).
	ImageReader ImageReader

	// SendFn forwards a turn payload to the session router after guard /
	// queue gating has succeeded.
	//
	// Deprecated: prefer DispatcherConfig.Capabilities (#374).
	SendFn func(ctx context.Context, key string, sess *session.ManagedSession, text string, images []cli.Attachment, onEvent cli.EventCallback) (*cli.SendResult, error)
	// TakeoverFn is the optional auto-takeover hook invoked on the first
	// message of every chat. nil is treated as "return false".
	//
	// Deprecated: prefer DispatcherConfig.Capabilities (#374).
	TakeoverFn func(ctx context.Context, chatKey, key string, opts session.AgentOpts) bool

	// StopCtx is the process-shutdown context the passthrough goroutine
	// observes. Optional — nil falls back to context.Background() (#1320).
	StopCtx context.Context

	// AllowMissingSender opts out of the constructor-time "Send must be
	// wired" check for tests that never touch the send path. Production
	// MUST leave this false so a missing wireup fails loud at boot instead
	// of panicking on the first user message.
	AllowMissingSender bool
}

// ErrSendWireupMissing is returned by NewDispatcher when no usable Send hook
// was supplied; tests may opt out via DispatcherConfig.AllowMissingSender.
var ErrSendWireupMissing = errors.New("dispatch: Capabilities.Send is required (set DispatcherConfig.Capabilities or DispatcherConfig.SendFn; tests may set AllowMissingSender)")

// resolveOrFabricateKeyResolver returns the KeyResolver Dispatcher holds.
// Precedence (single track — do not copy this chain elsewhere, #543):
//
//  1. cfg.Resolver
//  2. cfg.Router.Resolver() (Router-attached singleton, #604)
//  3. a fresh resolver from cfg.Agents + project data source (nil-safe)
//
// Always non-nil, so callers dereference d.resolver without a guard.
func resolveOrFabricateKeyResolver(cfg DispatcherConfig) *session.KeyResolver {
	if cfg.Resolver != nil {
		return cfg.Resolver
	}
	if cfg.Router != nil {
		if r := cfg.Router.Resolver(); r != nil {
			return r
		}
	}
	var data session.PlannerDataSource
	if cfg.ProjectMgr != nil {
		data = project.NewDataSource(cfg.ProjectMgr)
	}
	return session.NewKeyResolver(cfg.Agents, data)
}

// NewDispatcher constructs a Dispatcher from cfg. Returns ErrSendWireupMissing
// when neither cfg.Capabilities (with a non-noop Send) nor cfg.SendFn is set
// and AllowMissingSender is false. Nil cfg.Router / cfg.Scheduler /
// cfg.ProjectMgr pointers are collapsed to untyped nil so `!= nil` gates behave.
func NewDispatcher(cfg DispatcherConfig) (*Dispatcher, error) {
	var router SessionRouter
	if cfg.Router != nil {
		router = cfg.Router
	}
	resolver := resolveOrFabricateKeyResolver(cfg)
	// Capabilities precedence: cfg.Capabilities, else legacy *Fn closures
	// wrapped in closureCapabilities, else NoopCapabilities{} (whose Send
	// panics; Takeover / ReplyFooter return false / "").
	caps := cfg.Capabilities
	if caps == nil {
		if cfg.SendFn != nil || cfg.TakeoverFn != nil || cfg.ReplyFooterFn != nil {
			caps = closureCapabilities{
				send:        cfg.SendFn,
				takeover:    cfg.TakeoverFn,
				replyFooter: cfg.ReplyFooterFn,
			}
		} else {
			caps = NoopCapabilities{}
		}
	}
	// Surface missing Send wireup at constructor time: a runtime panic on
	// the first message would arrive after healthcheck and put systemd into
	// a restart loop.
	if !cfg.AllowMissingSender {
		hasSend := false
		switch c := caps.(type) {
		case NoopCapabilities:
			hasSend = false
		case closureCapabilities:
			hasSend = c.send != nil
		default:
			// Other implementations are presumed to wire Send.
			hasSend = true
		}
		if !hasSend {
			return nil, ErrSendWireupMissing
		}
	}
	if cfg.Capabilities != nil && (cfg.SendFn != nil || cfg.TakeoverFn != nil || cfg.ReplyFooterFn != nil) {
		slog.Warn("dispatch: DispatcherConfig.Capabilities set; legacy SendFn/TakeoverFn/ReplyFooterFn ignored",
			"send_fn_set", cfg.SendFn != nil,
			"takeover_fn_set", cfg.TakeoverFn != nil,
			"reply_footer_fn_set", cfg.ReplyFooterFn != nil)
	}
	// Collapse a typed-nil CronCommands (nil pointer boxed into the
	// interface) so `d.scheduler != nil` gates behave (#1178).
	scheduler := cfg.Scheduler
	if scheduler != nil {
		v := reflect.ValueOf(scheduler)
		if v.Kind() == reflect.Pointer && v.IsNil() {
			scheduler = nil
		}
	}
	// Same typed-nil collapse for ProjectStore (#457).
	var projectStore ProjectStore
	if cfg.ProjectMgr != nil {
		projectStore = cfg.ProjectMgr
	}
	d := &Dispatcher{
		router:                router,
		platforms:             cfg.Platforms,
		agents:                cfg.Agents,
		agentCommands:         cfg.AgentCommands,
		scheduler:             scheduler,
		projectMgr:            projectStore,
		resolver:              resolver,
		guard:                 cfg.Guard,
		queue:                 cfg.Queue,
		dedup:                 cfg.Dedup,
		allowedRoot:           cfg.AllowedRoot,
		claudeDir:             cfg.ClaudeDir,
		noOutputTimeout:       cfg.NoOutputTimeout,
		totalTimeout:          cfg.TotalTimeout,
		watchdogNoOutputKills: cfg.WatchdogNoOutputKills,
		watchdogTotalKills:    cfg.WatchdogTotalKills,
		caps:                  caps,
	}
	// agentCommands is immutable after construction, so this snapshot stays
	// correct for the dispatcher's lifetime (#2148).
	d.knownAgentIDs = make(map[string]struct{}, len(d.agentCommands)+2)
	d.knownAgentIDs["general"] = struct{}{}
	d.knownAgentIDs["planner"] = struct{}{}
	for _, id := range d.agentCommands {
		d.knownAgentIDs[id] = struct{}{}
	}
	// Headless / test wiring may leave the watchdog counters nil; the
	// watchdog path calls .Add(1) unconditionally.
	if d.watchdogNoOutputKills == nil {
		d.watchdogNoOutputKills = new(atomic.Int64)
	}
	if d.watchdogTotalKills == nil {
		d.watchdogTotalKills = new(atomic.Int64)
	}
	// prepareInbound calls d.dedup.Seen unconditionally; default capacity
	// matches platform.NewDedup's zero-cap fallback.
	if d.dedup == nil {
		d.dedup = platform.NewDedup(0)
	}
	if cfg.ImageReader != nil {
		d.imageReader = cfg.ImageReader
	} else {
		d.imageReader = osImageReader{}
	}
	// Background (never cancels) for headless / test wiring (#1320).
	if cfg.StopCtx != nil {
		d.stopCtx = cfg.StopCtx
	} else {
		d.stopCtx = context.Background()
	}
	return d, nil
}

// fallbackDedupKey builds "fallback:<platform>:<chatID>:<messageID>:<unixMinute>"
// for messages whose adapter left EventID empty (Seen("") never records), so
// platform retries within the same minute dedup. The prefix keeps the
// namespace disjoint from real EventIDs; now is injected for tests (#1310).
func fallbackDedupKey(msg platform.IncomingMessage, now time.Time) string {
	return "fallback:" + msg.Platform + ":" + msg.ChatID + ":" + msg.MessageID + ":" + strconv.FormatInt(now.Unix()/60, 10)
}

// preparedInbound is the per-message state prepareInbound resolves for the
// dispatch-strategy tail of BuildHandler (#1527).
type preparedInbound struct {
	lg        *slog.Logger
	agentID   string
	cleanText string
	key       string
	opts      session.AgentOpts
	images    []cli.Attachment
}

// prepareInbound runs the front-matter common to every dispatch strategy
// (dedup, group-mention gate, slash commands, agent resolution, accounting,
// key/opts resolution, image conversion). Returns false when the message was
// fully handled or dropped here.
func (d *Dispatcher) prepareInbound(ctx context.Context, msg platform.IncomingMessage) (preparedInbound, bool) {
	// Dedup first: platform retries (e.g. Feishu webhook re-delivery) must
	// not double-dispatch. Empty EventID (#1310) falls back to a composite
	// minute-bucketed key since Seen("") never records. The ID is consumed
	// even if the message is later gated/dropped — benign in practice.
	dedupID := msg.EventID
	if dedupID == "" {
		dedupID = fallbackDedupKey(msg, time.Now())
	}
	if d.dedup.Seen(dedupID) {
		return preparedInbound{}, false
	}

	// Group chats respond only when @mentioned (1:1 chats unaffected).
	// Placed BEFORE dispatchCommand so slash commands in groups also need
	// @bot. Gated messages are silently dropped (no reply, no metric).
	if msg.ChatType == "group" && !msg.MentionMe {
		return preparedInbound{}, false
	}

	// Platform / UserID / ChatID are adversary-controlled webhook fields;
	// sanitize before slog so embedded \n / ANSI bytes cannot forge log
	// lines. The logger is memoized on the sanitized triple (#2233), so the
	// cache key cannot diverge from the attr values.
	sp := session.SanitizeLogAttr(msg.Platform)
	su := session.SanitizeLogAttr(msg.UserID)
	sc := session.SanitizeLogAttr(msg.ChatID)
	logKey := sp + "\x00" + su + "\x00" + sc
	lg := d.inboundLogCache.get(logKey)
	if lg == nil {
		lg = slog.With("platform", sp, "user", su, "chat", sc)
		d.inboundLogCache.put(logKey, lg)
	}
	trimmed := strings.TrimSpace(msg.Text)

	if d.dispatchCommand(ctx, msg, trimmed, lg) {
		return preparedInbound{}, false
	}

	// Resolve agent from command prefix (e.g. "/review code" -> agent=code-reviewer, text="code")
	agentID, cleanText := session.ResolveAgent(trimmed, d.agentCommands)

	// #2148: a synthetic message (e.g. a Feishu AskUserQuestion card click)
	// pins its target agent via msg.AgentID so the answer routes back to the
	// asking session; cleanText stays intact. Whitelist-validated so a
	// hostile/replayed value cannot route into an arbitrary agent.
	if msg.AgentID != "" && d.isKnownAgent(msg.AgentID) {
		agentID = msg.AgentID
	}

	if cleanText == "" && len(msg.Images) == 0 {
		if agentID != "general" {
			d.replyText(ctx, msg, "请在指令后输入内容。", lg)
		}
		return preparedInbound{}, false
	}

	// Warn about unrecognized slash commands (likely typos)
	// Skip paths like /home/user/... (contain slash after the leading one)
	if agentID == "general" && strings.HasPrefix(cleanText, "/") {
		cmd := cleanText
		if idx := strings.IndexByte(cleanText, ' '); idx >= 0 {
			cmd = cleanText[:idx]
		}
		if !strings.Contains(cmd[1:], "/") {
			// Sanitize the user-controlled cmd before echoing so embedded
			// ANSI / control bytes cannot inject formatting; the cap bounds
			// reply size.
			safeCmd := osutil.SanitizeForLog(cmd, 64)
			d.replyText(ctx, msg, "未知命令: "+safeCmd+"\n输入 /help 查看可用命令，或直接发送消息。", lg)
			return preparedInbound{}, false
		}
	}

	// Accepted messages only (post-dedup, post-command). Feeds /health and
	// /debug/vars (#892).
	d.messageCount.Add(1)
	dispatchMessageTotal.Add(1)

	// KeyResolver is the single source of truth for project-binding
	// precedence and ExtraArgs merge (docs/rfc/key-resolver.md §3.1).
	key, opts := d.resolver.ResolveForChat(msg.Platform, msg.ChatType, msg.ChatID, agentID)

	var images []cli.Attachment
	if len(msg.Images) > 0 {
		images = make([]cli.Attachment, 0, len(msg.Images))
		for _, img := range msg.Images {
			images = append(images, cli.Attachment{Data: img.Data, MimeType: img.MimeType})
		}
	}

	return preparedInbound{
		lg:        lg,
		agentID:   agentID,
		cleanText: cleanText,
		key:       key,
		opts:      opts,
		images:    images,
	}, true
}

// handleQueuedNonOwner runs the queue non-owner branch: interrupt-mode control
// request for the active turn, plus the enqueue-vs-disabled acknowledgement.
// shouldInterrupt / enqueued / evictedID come from queue.Enqueue; a non-empty
// evictedID is the oldest queued message dropped by backpressure, whose
// HOURGLASS reaction is cleared here (#1945).
func (d *Dispatcher) handleQueuedNonOwner(ctx context.Context, msg platform.IncomingMessage, p preparedInbound, shouldInterrupt, enqueued bool, evictedID string) {
	lg, key := p.lg, p.key
	// An evicted message never enters a DoneOrDrain batch, so ownerLoop's
	// clearQueuedReactions never reaches it; clear its HOURGLASS here (#1945).
	if evictedID != "" {
		d.clearQueuedReaction(ctx, msg.Platform, evictedID, lg)
	}
	// Interrupt mode: fire a control_request so the in-flight turn aborts;
	// the owner loop's Send() then returns and drains this message as the
	// next prompt. All non-Sent outcomes degrade to Collect semantics.
	if shouldInterrupt {
		switch outcome := d.router.InterruptSessionViaControl(key); outcome {
		case session.InterruptSent:
			lg.Info("interrupt mode: aborted active turn to process follow-up",
				"key", key)
		case session.InterruptNoTurn:
			// Turn not active yet; the owner loop drains the follow-up later.
			lg.Debug("interrupt mode: session idle or spawning, will process follow-up after current turn",
				"key", key)
		case session.InterruptNoSession:
			lg.Debug("interrupt mode: session not found, falling back to collect",
				"key", key)
		case session.InterruptUnsupported:
			lg.Debug("interrupt mode: protocol does not support stdin interrupt, falling back to collect",
				"key", key)
		case session.InterruptError:
			// ManagedSession.InterruptViaControl already warned; paired dispatch-side trace.
			lg.Warn("interrupt mode: transport error, falling back to collect",
				"key", key)
		}
	}
	if enqueued {
		// Prefer an in-place reaction on the user's message; fall back to the
		// rate-limited text notice if the platform can't react.
		if !d.ackQueuedWithReaction(ctx, msg, lg) {
			if d.queue.ShouldNotify(key) {
				d.replyText(ctx, msg, "消息已收到，待当前回复完成后一并处理。", lg)
			}
		}
	} else {
		// Queue disabled (maxDepth<=0): drop with a rate-limited notice.
		if d.queue.ShouldNotify(key) {
			d.replyText(ctx, msg, "正在处理上一条消息，请稍候...", lg)
		}
	}
}

// BuildHandler returns a platform.MessageHandler wired to this Dispatcher.
func (d *Dispatcher) BuildHandler() platform.MessageHandler {
	return func(ctx context.Context, msg platform.IncomingMessage) {
		p, ok := d.prepareInbound(ctx, msg)
		if !ok {
			return
		}
		lg, agentID, cleanText := p.lg, p.agentID, p.cleanText
		key, opts, images := p.key, p.opts, p.images

		// Passthrough: every message gets its own goroutine; ordering/merging
		// is handled by the CLI commandQueue + Process sendSlot FIFO. Protocols
		// without --replay-user-messages (e.g. ACP) silently downgrade to the
		// sendMu-serialized Send path.
		if d.queue != nil && d.queue.Mode() == ModePassthrough {
			lg.Info("message received (passthrough)", "agent", agentID, "text_len", len(cleanText), "images", len(images))
			// Detach from the webhook ctx (handlers return in seconds, turns
			// take minutes) but keep d.stopCtx as the cancel source so the
			// goroutine still aborts on SIGTERM (#1320).
			sendCtx := mergeStopAndValues(d.stopCtx, ctx)
			// Ack BEFORE spawning so AddReaction stores the reaction_id before
			// goSendAndReply's deferred clearQueuedReaction can run on a fast-fail
			// turn; the reverse order leaves a permanent HOURGLASS (#1963).
			d.ackQueuedWithReaction(ctx, msg, lg)
			d.goSendAndReply(WithPassthrough(sendCtx), key, cleanText, images, agentID, opts, msg, lg, true)
			return
		}

		// Enqueue message. If queue is nil or disabled, fall back to Guard.
		if d.queue != nil {
			qm := QueuedMsg{
				Text:      cleanText,
				Images:    images,
				MessageID: msg.MessageID,
				EnqueueAt: time.Now(),
			}
			isOwner, enqueued, shouldInterrupt, gen, evictedID := d.queue.Enqueue(key, qm)
			if !isOwner {
				d.handleQueuedNonOwner(ctx, msg, p, shouldInterrupt, enqueued, evictedID)
				return
			}
			// I am the owner — enter the process-and-drain loop.
			lg.Info("message received", "agent", agentID, "text_len", len(cleanText), "images", len(images))
			d.ownerLoop(ctx, key, gen, qm, agentID, opts, msg, lg)
			return
		}

		// Fallback: Guard-based path (no queue configured).
		if !d.guard.TryAcquire(key) {
			if d.guard.ShouldSendWait(key) {
				d.replyText(ctx, msg, "正在处理上一条消息，请稍候...", lg)
			}
			return
		}
		defer d.guard.Release(key)
		defer d.router.NotifyIdle()

		lg.Info("message received", "agent", agentID, "text_len", len(cleanText), "images", len(images))
		d.sendAndReply(ctx, key, cleanText, images, agentID, opts, msg, lg, true)
	}
}

// discardQueue is a nil-safe helper to clear queued messages for a key. In
// passthrough mode it also fires ErrSessionReset to in-flight SendPassthrough
// callers, and it clears the HOURGLASS reaction of every dropped message
// (#2013). ctx is the command handler's live request ctx, so no detach.
func (d *Dispatcher) discardQueue(ctx context.Context, msg platform.IncomingMessage, key string) {
	if d.queue != nil {
		dropped := d.queue.DiscardAndReturn(key)
		d.clearQueuedReactions(ctx, msg.Platform, dropped, nil)
	}
	if d.router != nil {
		d.router.DiscardPassthroughPending(key, cli.ErrSessionReset)
	}
}

// ownerLoop processes the first message, then drains and coalesces queued
// messages until the queue is empty. gen is the Enqueue generation cookie: if
// Discard bumps it (e.g. /new), DoneOrDrain returns nil and the loop exits so
// two goroutines never own the same key. A deferred recover releases
// ownership on panic.
func (d *Dispatcher) ownerLoop(
	ctx context.Context,
	key string,
	gen uint64,
	first QueuedMsg,
	agentID string,
	opts session.AgentOpts,
	msg platform.IncomingMessage,
	lg *slog.Logger,
) {
	// Enrich once per ownerLoop rather than per drained turn.
	lg = lg.With("key", key, "agent", agentID)
	// Defer order matters (LIFO): NotifyIdle is registered first so it runs
	// AFTER the recover below — the session must not read "idle" while a
	// panic is still mid-flight.
	defer d.router.NotifyIdle()
	// A drained batch is out of the ring before sendAndReply runs; on panic
	// handleOwnerLoopPanic's DiscardAndReturn cannot see it, so track it here
	// and clear its reactions in the recover defer. `first` is not tracked:
	// the owner's own message never gets a queued reaction.
	var pendingClear []QueuedMsg
	defer func() {
		if r := recover(); r != nil {
			// Turn ctx may already be Done (shutdown racing the panic); detach.
			if len(pendingClear) > 0 {
				d.clearQueuedReactions(context.WithoutCancel(ctx), msg.Platform, pendingClear, lg)
			}
			d.handleOwnerLoopPanic(key, msg, r, lg)
		}
	}()

	d.sendAndReply(ctx, key, first.Text, first.Images, agentID, opts, msg, lg, true)

	// Drain loop: after each turn, wait collectDelay then drain.
	collectTimer := time.NewTimer(d.queue.CollectDelay())
	defer collectTimer.Stop()
	for {
		select {
		case <-ctx.Done():
			// Clear HOURGLASS on messages still queued when the ctx is
			// cancelled (e.g. restart); the ⏳ would otherwise survive in the
			// platform's reaction cache. ctx is Done, so detach (#2013).
			dropped := d.queue.DiscardAndReturn(key)
			d.clearQueuedReactions(context.WithoutCancel(ctx), msg.Platform, dropped, lg)
			return
		case <-collectTimer.C:
		}

		queued := d.queue.DoneOrDrain(key, gen)
		if queued == nil {
			return // Queue empty or generation mismatch — stop.
		}

		// Out of the ring from here until cleared below; the recover defer
		// owns cleanup on panic.
		pendingClear = queued
		text, images := CoalesceMessages(queued)
		lg.Info("processing queued messages", "count", len(queued), "merged_len", len(text))
		d.sendAndReply(ctx, key, text, images, agentID, opts, msg, lg, false)
		// Clear the drained batch's queued reactions. Detached via
		// WithoutCancel: on a shutdown-during-turn race ctx is already Done and
		// a child WithTimeout would be born cancelled (#2262).
		d.clearQueuedReactions(context.WithoutCancel(ctx), msg.Platform, queued, lg)
		// Cleared normally; drop the recover fallback handle.
		pendingClear = nil
		// Go 1.23+: Reset on a Timer whose channel was just consumed by the case arm above is race-free; no Stop+drain needed.
		collectTimer.Reset(d.queue.CollectDelay())
	}
}

// handleOwnerLoopPanic is the deferred panic recovery for ownerLoop (split
// out so it is unit-testable): logs the panic with stack, discards the queue
// so a stale owner is not left holding the key, and replies "please retry".
// A nested recover absorbs a cascading panic from the reply. lg may be nil
// (falls back to slog.Default).
func (d *Dispatcher) handleOwnerLoopPanic(key string, msg platform.IncomingMessage, r any, lg *slog.Logger) {
	metrics.PanicRecoveredTotal.Add(1)
	if lg == nil {
		lg = slog.Default()
	}
	lg.Error("ownerLoop panic", "key", key, "panic", r, "stack", string(debug.Stack()))
	if d.queue != nil {
		// The process survives and the platform is reachable, so clear the
		// HOURGLASS of the dropped messages (#2013). No live request ctx here.
		dropped := d.queue.DiscardAndReturn(key)
		d.clearQueuedReactions(context.WithoutCancel(context.Background()), msg.Platform, dropped, lg)
	}
	func() {
		defer func() {
			if rr := recover(); rr != nil {
				lg.Error("ownerLoop reply panic recovered", "key", key, "panic", rr)
			}
		}()
		notifyCtx, cancel := NotifyCtx(context.Background(), NotifyKindOwnerLoopPanic, platformReplyTimeout)
		defer cancel()
		d.replyText(notifyCtx, msg, "处理异常，请稍后重试。", nil)
	}()
}

// goSendAndReply runs sendAndReply in its own goroutine with a panic recover,
// so a panic in one detached passthrough / /urgent turn fails only that turn
// (#1773). Reuses handleOwnerLoopPanic for log + discard + retry reply.
func (d *Dispatcher) goSendAndReply(
	ctx context.Context,
	key, text string,
	images []cli.Attachment,
	agentID string,
	opts session.AgentOpts,
	msg platform.IncomingMessage,
	lg *slog.Logger,
	isFirst bool,
) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				d.handleOwnerLoopPanic(key, msg, r, lg)
			}
		}()
		// Clear the HOURGLASS the passthrough / /urgent ack added once the
		// turn finishes; this path never enters ownerLoop's drain loop (#1946).
		// WithoutCancel: the turn ctx may be expired or Canceled by then;
		// clearQueuedReaction re-bounds with its own timeout.
		defer d.clearQueuedReaction(context.WithoutCancel(ctx), msg.Platform, msg.MessageID, lg)
		d.sendAndReply(ctx, key, text, images, agentID, opts, msg, lg, isFirst)
	}()
}

// resolveReplyCtx returns a context safe for an end-of-turn reply: when ctx is
// Done with context.Canceled (shutdown), it returns a fresh NotifyCtx with the
// shutdownReplyTimeout budget; otherwise ctx unchanged and a nil cleanup.
// Callers MUST defer cleanup() when non-nil or the timer goroutine leaks.
// DeadlineExceeded is a legitimate per-turn timeout and is NOT extended (#550).
func resolveReplyCtx(ctx context.Context) (replyCtx context.Context, cleanup func()) {
	if ctx == nil {
		// Caller lost its turn ctx; mint a shutdown-budget ctx.
		return NotifyCtx(context.Background(), NotifyKindShutdown, shutdownReplyTimeout)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		return ctx, nil
	}
	notifyCtx, cancel := NotifyCtx(ctx, NotifyKindShutdown, shutdownReplyTimeout)
	return notifyCtx, cancel
}

// handleGetOrCreateError maps a router.GetOrCreate failure into the reply ctx,
// an optional cleanup, and a Chinese error message (via usermsg.ForSendError
// so it cannot drift from the WS send_ack path). cleanup is non-nil on the
// shutdown / context.Canceled branch and MUST be deferred by the caller.
// Shutdown cancellation logs at Info (expected on every restart), other
// failures at Error.
func (d *Dispatcher) handleGetOrCreateError(
	ctx context.Context,
	err error,
	lg *slog.Logger,
) (replyCtx context.Context, cleanup func(), errMsg string) {
	if errors.Is(err, context.Canceled) {
		lg.Info("get session cancelled during shutdown", "err", err)
	} else {
		lg.Error("get session", "err", err)
	}
	// Empty key keeps the regular (non-cron) phrasing.
	errMsg = usermsg.ForSendError(err, "")
	replyCtx, cleanup = resolveReplyCtx(ctx)
	return replyCtx, cleanup, errMsg
}

// handleSendError maps a Capabilities.Send failure into the user-facing error
// reply, watchdog counter bumps, and metrics increments (#624). It does NOT
// report whether the error reply landed — that failure is only logged at Warn.
func (d *Dispatcher) handleSendError(
	ctx context.Context,
	err error,
	key string,
	msg platform.IncomingMessage,
	p platform.Platform,
	lg *slog.Logger,
) {
	// ErrSessionReset is a user control-flow signal (/new, /clear), not an
	// error: no extra reply and no /health error-counter bump.
	if errors.Is(err, cli.ErrSessionReset) {
		return
	}
	d.replyErrorCount.Add(1)
	dispatchReplyErrorTotal.Add(1)
	lg.Error("send to claude", "err", err)
	// usermsg.UserMessage renders the configured timeout durations in
	// Chinese (dashboard uses the generic ForSendError). Watchdog counters
	// stay here because the IM side owns that configuration.
	switch {
	case errors.Is(err, cli.ErrNoOutputTimeout):
		d.watchdogNoOutputKills.Add(1)
	case errors.Is(err, cli.ErrTotalTimeout):
		d.watchdogTotalKills.Add(1)
	}
	errMsg := usermsg.UserMessage(err, key, d.noOutputTimeout, d.totalTimeout)
	// IM-only emoji decoration for the timeout cases. Other surfaces
	// (dashboard send_ack) deliberately stay emoji-free.
	if errors.Is(err, cli.ErrNoOutputTimeout) || errors.Is(err, cli.ErrTotalTimeout) {
		errMsg = "⏱️ " + errMsg
	}
	// On shutdown the inbound ctx is already Done; swap so the error reply
	// still lands.
	replyCtx, cleanup := resolveReplyCtx(ctx)
	if cleanup != nil {
		defer cleanup()
	}
	if _, err := platform.ReplyWithRetry(replyCtx, p, platform.OutgoingMessage{ChatID: msg.ChatID, Text: errMsg}, limits.PlatformReplyMaxAttempts); err != nil {
		d.sendFailCount.Add(1)
		dispatchSendFailTotal.Add(1)
		lg.Warn("error reply also failed", "chat", msg.ChatID, "err", err)
	}
}

// sendAndReply performs one turn: GetOrCreate session, send message, deliver reply.
// isFirst indicates whether this is the first message (triggers takeover/session-new
// notifications); queued follow-ups skip these.
func (d *Dispatcher) sendAndReply(
	ctx context.Context,
	key, text string,
	images []cli.Attachment,
	agentID string,
	opts session.AgentOpts,
	msg platform.IncomingMessage,
	lg *slog.Logger,
	isFirst bool,
) {
	// Takeover only on the first message. The bool result is ignored: on
	// success the external session was registered for resume and GetOrCreate
	// rebuilds with it; on false GetOrCreate spawns fresh. Same caller flow.
	if isFirst {
		_ = d.caps.Takeover(ctx, session.ChatKey(msg.Platform, msg.ChatType, msg.ChatID), key, opts)
	}

	sess, sessStatus, err := d.router.GetOrCreate(ctx, key, opts)
	if err != nil {
		replyCtx, cleanup, errMsg := d.handleGetOrCreateError(ctx, err, lg)
		if cleanup != nil {
			defer cleanup()
		}
		d.replyText(replyCtx, msg, errMsg, lg)
		return
	}

	p := d.platforms[msg.Platform]
	if p == nil {
		lg.Error("unknown platform")
		return
	}

	// Session lifecycle notifications only on first message.
	if isFirst {
		if sessStatus == session.SessionNew && platform.SupportsInterimMessages(p) {
			d.replyText(ctx, msg, "新会话已创建（之前的上下文已失效）。", lg)
		}
	}

	tracker := newIMEventTracker(ctx, p, msg.ChatID, msg.ChatType, agentID)
	defer tracker.stop()

	result, err := d.caps.Send(ctx, key, sess, text, images, tracker.onEvent)
	if err != nil {
		d.handleSendError(ctx, err, key, msg, p, lg)
		return
	}

	lg.Info("message replied", "result_len", len(result.Text), "cost", result.CostUSD,
		"merged_count", result.MergedCount, "merged_with_head", result.MergedWithHead)

	// Passthrough merge fan-out: follower slots get MergedCount>1 and empty
	// Text; the head slot delivered the reply, so followers surface a short
	// "合并" hint on the user's message instead.
	if result.MergedCount > 1 && result.Text == "" {
		// A follower's tracker may already have posted a "💭思考中…" banner
		// (interim fan-out claims all slots); with no final text it would be
		// orphaned, so collapse it into the merge hint (#2290).
		tracker.waitReady(ctx)
		// Finalize before the edit so a late editLoop redraw does not
		// overwrite the hint with the stale banner (#2338).
		tracker.markFinalized()
		if msgID := tracker.getThinkingMsgID(); msgID != "" {
			if err := p.EditMessage(ctx, msgID, "已合并到上一条回复。"); err != nil {
				slog.Debug("merge follower banner edit failed", "msg_id", msgID, "err", err)
			}
		}
		d.ackMergedFollower(ctx, msg, key, result.MergedCount, lg)
		d.markReplySuccess()
		return
	}

	// Record success regardless of text length: an empty result (tool-only
	// turn) is still a healthy roundtrip for /health's lastReplySuccess.
	d.markReplySuccess()

	replyText := d.decorateReplyText(result, sess)
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
	// so the IM view is card + one "waiting" line. Dashboard renders the card
	// natively, so suppressing the text only drops a duplicate bubble.
	if tracker.askQuestionFired.Load() {
		if msgID := tracker.getThinkingMsgID(); msgID != "" {
			// Best-effort; log and move on.
			if err := p.EditMessage(ctx, msgID, "⏳ 等待你的选择…"); err != nil {
				slog.Debug("ask_question: banner edit failed", "err", err)
			}
		}
		lg.Info("ask_question suppressed redundant reply", "result_len", len(result.Text))
	} else if replyText != "" {
		if msgID := tracker.getThinkingMsgID(); msgID != "" {
			if err := p.EditMessage(ctx, msgID, replyText); err != nil {
				slog.Warn("edit message failed, sending new", "err", err)
				d.SendSplitReply(ctx, p, msg.ChatID, replyText)
			}
		} else {
			d.SendSplitReply(ctx, p, msg.ChatID, replyText)
		}
	}

	// outImages derive from replyText; when the card suppresses the text,
	// suppress its images too or orphaned bubbles follow the card (#1959).
	if !tracker.askQuestionFired.Load() {
		d.sendOutboundImages(ctx, p, msg.ChatID, outImages)
	}
}

// sendOutboundImages delivers each turn image as its own reply bubble.
func (d *Dispatcher) sendOutboundImages(ctx context.Context, p platform.Platform, chatID string, images []platform.Image) {
	for _, img := range images {
		// ReplyWithRetry (not bare Reply) so an image gets the same
		// token-rotation retry as text (#2305).
		if _, err := platform.ReplyWithRetry(ctx, p, platform.OutgoingMessage{
			ChatID: chatID,
			Images: []platform.Image{img},
		}, limits.PlatformReplyMaxAttempts); err != nil {
			// Failed image sends must show in /health like text failures.
			d.sendFailCount.Add(1)
			dispatchSendFailTotal.Add(1)
			slog.Warn("send image failed", "err", err)
		}
	}
}

// maxTurnImageBytes caps total outbound image bytes per reply turn (#2196):
// up to 10 paths × 10 MiB each could otherwise hold ~100 MiB in memory.
const maxTurnImageBytes = 20 * 1024 * 1024

// readTurnImages resolves the image paths embedded in replyText into
// platform.Image attachments and rewrites every path to "[图片]". Images past
// the maxTurnImageBytes budget are skipped but their paths are STILL rewritten
// so the visible text is identical regardless of attachment outcome.
func (d *Dispatcher) readTurnImages(replyText string) ([]platform.Image, string) {
	imagePaths := cli.ExtractImagePaths(replyText)
	if len(imagePaths) == 0 {
		return nil, replyText
	}
	var outImages []platform.Image
	var turnImageBytes int
	// ReplaceAll loop beats strings.NewReplacer for the 1-2 paths typical
	// here. Every path is replaced even when ReadFile fails or the budget
	// is exhausted.
	for _, path := range imagePaths {
		data, err := d.imageReader.ReadFile(path)
		if err == nil {
			if turnImageBytes+len(data) <= maxTurnImageBytes {
				outImages = append(outImages, platform.Image{Data: data, MimeType: cli.MimeFromPath(path)})
				turnImageBytes += len(data)
			}
			// Over budget: skip the attachment but still rewrite the path.
		}
		replyText = strings.ReplaceAll(replyText, path, "[图片]")
	}
	return outImages, replyText
}

// decorateReplyText post-processes the raw CLI result text for IM delivery:
// redacts secrets, localises API errors, appends the merge-group chip and the
// per-session ReplyFooter. Returns "" when nothing should be sent (#656).
func (d *Dispatcher) decorateReplyText(result *cli.SendResult, sess *session.ManagedSession) string {
	// Redact credential shapes (sk-ant-, ghp_, AKIA, …) BEFORE localising so
	// an echoed plaintext token never reaches the IM channel (#1571).
	replyText := localizeAPIError(textutil.RedactSecrets(result.Text))
	// Head slot of a merge group: append a small chip so the user knows the
	// single bot bubble covers N messages.
	if result.MergedCount > 1 && replyText != "" {
		replyText += "\n\n*— 合并了 " + strconv.Itoa(result.MergedCount) + " 条消息的回复*"
	}
	// nil sess (session pruned but reply still fires) passes "" so
	// ReplyFooter falls back to the router default; NoopCapabilities yields "".
	var backendID string
	if sess != nil {
		backendID = sess.Backend()
	}
	// Guard on replyText != "" so an empty-text turn does not emit a lone
	// "— cc" footer bubble (#1985).
	if footer := d.caps.ReplyFooter(backendID); footer != "" && replyText != "" {
		replyText += "\n\n— " + footer
	}
	return replyText
}

// pageSuffixRuneWidth returns the rune width of the worst-case page suffix
// "\n— [i/total]": 6 fixed runes ('\n' '—' ' ' '[' '/' ']') plus two numbers
// with total's digit count (#2008).
func pageSuffixRuneWidth(total int) int {
	if total < 1 {
		total = 1
	}
	digits := len(strconv.Itoa(total))
	return 6 + 2*digits
}

// upperBoundChunks returns a ceiling on how many chunks SplitText produces for
// runeCount runes at splitWidth; it must never under-estimate because the
// caller reserves the page-suffix budget from it. SplitText may break early at
// a newline past the chunk midpoint, so the shortest chunk is ~ceil(splitWidth/2)
// — a naive ceil(runeCount/splitWidth) can under-estimate by 2x (#2056).
func upperBoundChunks(runeCount, splitWidth int) int {
	if splitWidth <= 0 {
		return runeCount + 1
	}
	minChunk := (splitWidth + 1) / 2 // ceil(splitWidth/2), worst-case shortest chunk
	if minChunk < 1 {
		minChunk = 1
	}
	return (runeCount + minChunk - 1) / minChunk
}

// singleReplyTruncMarker is appended (within the rune budget) when a reply to
// a single-use-token platform must be truncated to fit one message. #2136.
const singleReplyTruncMarker = "\n…(truncated)"

// singleReplyTruncMarkerRunes is the rune width of singleReplyTruncMarker.
var singleReplyTruncMarkerRunes = utf8.RuneCountInString(singleReplyTruncMarker)

// truncateForSingleReply trims text to at most maxRunes runes, reserving room
// for a visible truncation marker; when maxRunes cannot fit the marker it
// falls back to bare rune-safe truncation.
func truncateForSingleReply(text string, maxRunes int) string {
	keep := maxRunes - singleReplyTruncMarkerRunes
	if keep <= 0 {
		// No room for the marker — keep as much content as fits.
		return textutil.TruncateRunesNoEllipsis(text, maxRunes)
	}
	return textutil.TruncateRunesNoEllipsis(text, keep) + singleReplyTruncMarker
}

// SendSplitReply sends a reply, splitting into multiple messages if too long.
func (d *Dispatcher) SendSplitReply(ctx context.Context, p platform.Platform, chatID, text string) {
	maxLen := p.MaxReplyLength()
	if maxLen <= 0 {
		maxLen = platform.DefaultMaxReplyLen
	}

	// Single-use-token platforms (e.g. WeChat iLink) can deliver only ONE
	// message per inbound turn; N chunks would lose [2/N]..[N/N]. Collapse
	// to one truncated message with a visible marker (#2136).
	if platform.UsesSingleUseReplyToken(p) {
		if utf8.RuneCountInString(text) > maxLen {
			text = truncateForSingleReply(text, maxLen)
		}
		if _, err := platform.ReplyWithRetry(ctx, p, platform.OutgoingMessage{ChatID: chatID, Text: text}, limits.PlatformReplyMaxAttempts); err != nil {
			d.sendFailCount.Add(1)
			dispatchSendFailTotal.Add(1)
			slog.Error("single-reply send failed after retries", "chat", chatID, "err", err)
		} else {
			d.markReplySuccess()
		}
		return
	}

	// Byte-length fast path: len(text) is an upper bound on the rune count,
	// so len(text) <= maxLen means no split is needed; skip the rune scan.
	if len(text) <= maxLen {
		if _, err := platform.ReplyWithRetry(ctx, p, platform.OutgoingMessage{ChatID: chatID, Text: text}, limits.PlatformReplyMaxAttempts); err != nil {
			d.sendFailCount.Add(1)
			dispatchSendFailTotal.Add(1)
			slog.Error("reply chunk failed after retries", "chat", chatID, "chunk", 1, "err", err)
		} else {
			d.markReplySuccess()
		}
		return
	}

	// When splitting, each chunk gets a "\n— [i/N]" suffix; splitting at the
	// raw limit would push full chunks past hard API ceilings (Discord 2000,
	// rejected outright, and ReplyWithRetry re-sends the same payload). Reserve
	// the worst-case suffix using an upper-bound chunk count computed at the
	// reduced width — over-reserving is safe, under-reserving is not (#2008).
	splitLen := maxLen
	// maxLen smaller than the suffix itself (config only clamps <=0) makes the
	// reservation non-positive; suppress the suffix instead of emitting
	// guaranteed-oversized chunks (#2057).
	suppressSuffix := false
	// Count runes once for both the reservation and SplitTextWithCount (#2283).
	runeCount := utf8.RuneCountInString(text)
	if runeCount > maxLen {
		// First-pass reservation assuming a 1-digit count, then widen the
		// reservation to the worst-case suffix for the resulting chunk count.
		reserved := maxLen - pageSuffixRuneWidth(upperBoundChunks(runeCount, maxLen-pageSuffixRuneWidth(1)))
		if reserved > 0 {
			splitLen = reserved
		} else {
			// No room for any suffix at this maxLen — split at the raw
			// limit and skip the "[i/N]" marker so chunks stay <= maxLen.
			suppressSuffix = true
		}
	}

	chunks := platform.SplitTextWithCount(text, splitLen, runeCount)
	total := len(chunks)
	for i, chunk := range chunks {
		if total > 1 && !suppressSuffix {
			chunk += "\n— [" + strconv.Itoa(i+1) + "/" + strconv.Itoa(total) + "]"
		}
		if _, err := platform.ReplyWithRetry(ctx, p, platform.OutgoingMessage{ChatID: chatID, Text: chunk}, limits.PlatformReplyMaxAttempts); err != nil {
			d.sendFailCount.Add(1)
			dispatchSendFailTotal.Add(1)
			slog.Error("reply chunk failed after retries", "chat", chatID, "chunk", i+1, "err", err)
		} else {
			d.markReplySuccess()
		}
	}
}
