package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/agentroute"
	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/imauth"
	"github.com/naozhi/naozhi/internal/limits"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/ratelimit"
	"github.com/naozhi/naozhi/internal/replyfmt"
	"github.com/naozhi/naozhi/internal/session/sessionview"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/turn"
	"github.com/naozhi/naozhi/internal/usermsg"
)

// platformReplyTimeout caps every outbound platform.Reply / EditMessage call.
const platformReplyTimeout = 15 * time.Second

// shutdownReplyTimeout caps best-effort replies on the shutdown /
// context.Canceled path; shorter than platformReplyTimeout so teardown is
// not blocked on a slow IM API yet the notice still lands before SIGKILL.
const shutdownReplyTimeout = 5 * time.Second

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
	agents        map[string]sessionview.AgentOpts
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
	resolver KeyResolver
	// turns runs every IM turn (Submit) and reset (Reset); non-nil after
	// NewDispatcher.
	turns       Turns
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

	// fallbackBannerDelay is how long an IM turn whose message got no ⏳
	// runs before it posts a 思考中 banner; fallbackBannerDelayDefault
	// unless a test shortens it.
	fallbackBannerDelay time.Duration

	// Operational counters exposed via /health for triaging. Incremented
	// atomically and never reset (monotonic since process start).
	messageCount       atomic.Int64 // all non-slash-command IM messages accepted
	replyErrorCount    atomic.Int64 // failed sends reported to the user (includes timeouts)
	sendFailCount      atomic.Int64 // user-visible reply failures (platform send errors)
	lastReplySuccessNs atomic.Int64 // UnixNano of most recent successful user-visible reply; 0 until first success

	// caps groups the host-supplied hooks (Takeover / ReplyFooter). Always
	// non-nil after NewDispatcher (Capabilities, wrapped legacy *Fn closures,
	// or NoopCapabilities{}).
	caps Capabilities

	// inboundLogCache memoizes the per-(platform,user,chat) logger built in
	// prepareInbound (#2233). Zero value ready; see inbound_logcache.go.
	inboundLogCache inboundLogCache

	// reactionFailLogged holds each platform name whose ⏳ add has failed
	// once; later failures log at Debug (ackQueuedWithReaction).
	reactionFailLogged sync.Map

	// access is the IM sender policy prepareInbound enforces (authz.go);
	// nil allows everyone. Swapped whole by SetAccessPolicy.
	access      atomic.Pointer[imauth.Policy]
	denyReplies denyThrottle

	// inboundLimit is the per-sender bucket admitRate draws from
	// (ratelimit.go); nil when rateLimit is off.
	inboundLimit     *ratelimit.Limiter
	rateLimit        RateLimit
	rateLimitReplies denyThrottle

	// budget refuses turns past cost.budget (budget.go); nil admits all.
	budget        BudgetGate
	budgetReplies denyThrottle

	// groupScope splits a group chat's sessions (sessionChatID).
	groupScope GroupScope
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
	Router        SessionRouter
	Platforms     map[string]platform.Platform
	Agents        map[string]sessionview.AgentOpts
	AgentCommands map[string]string
	// Scheduler is the cron consumer surface; nil disables /cron commands.
	Scheduler  CronCommands
	ProjectMgr *project.Manager
	// Resolver is the central (key, opts) derivation. Optional: when nil,
	// NewDispatcher fabricates a fallback from Agents / ProjectMgr.
	Resolver KeyResolver
	// Turns is required: NewDispatcher returns ErrTurnsWireupMissing without it.
	Turns       Turns
	Dedup       *platform.Dedup
	AllowedRoot string
	ClaudeDir   string

	// Capabilities groups the host-supplied hooks (Takeover / ReplyFooter).
	// Wins over the legacy *Fn closures when both are set; nil falls back to
	// the closures, then NoopCapabilities{}.
	Capabilities Capabilities

	// ReplyFooterFn returns the per-session reply tag (e.g. "cc" / "kiro")
	// for a backend ID; empty backend means "not pinned yet". nil means no
	// footer.
	//
	// Deprecated: prefer DispatcherConfig.Capabilities. Removal, together
	// with TakeoverFn / closureCapabilities, is gated on test migrations
	// (#374).
	ReplyFooterFn func(backendID string) string

	NoOutputTimeout       time.Duration
	TotalTimeout          time.Duration
	WatchdogNoOutputKills *atomic.Int64
	WatchdogTotalKills    *atomic.Int64

	// ImageReader resolves outbound image paths to bytes. Optional —
	// defaults to osImageReader{}; tests inject a fake (#884).
	ImageReader ImageReader

	// TakeoverFn is the optional auto-takeover hook invoked on the first
	// message of every chat. nil is treated as "return false".
	//
	// Deprecated: prefer DispatcherConfig.Capabilities (#374).
	TakeoverFn func(ctx context.Context, chatKey, key string, opts sessionview.AgentOpts) bool

	// StopCtx is the process-shutdown context the passthrough goroutine
	// observes. Optional — nil falls back to context.Background() (#1320).
	StopCtx context.Context

	// Access is the IM sender policy; nil allows every sender.
	Access *imauth.Policy
	// RateLimit caps each sender's message rate; the zero value is unlimited.
	RateLimit RateLimit
	// Budget refuses turns once today's spend reaches cost.budget; nil, or a
	// nil pointer inside it, admits every turn.
	Budget BudgetGate
	// GroupScope is what one group-chat session covers; zero is per thread.
	GroupScope GroupScope
}

// ErrTurnsWireupMissing is returned by NewDispatcher when DispatcherConfig.Turns
// is nil: every IM message would otherwise fail on its first turn, after the
// healthcheck has passed.
var ErrTurnsWireupMissing = errors.New("dispatch: DispatcherConfig.Turns is required")

// NewDispatcher constructs a Dispatcher from cfg. Returns
// ErrTurnsWireupMissing when cfg.Turns is nil (or a typed nil). Nil
// cfg.Router / cfg.Scheduler / cfg.ProjectMgr pointers are collapsed to
// untyped nil so `!= nil` gates behave.
func NewDispatcher(cfg DispatcherConfig) (*Dispatcher, error) {
	if isNilInterface(cfg.Turns) {
		return nil, ErrTurnsWireupMissing
	}
	var router SessionRouter
	if !isNilInterface(cfg.Router) {
		router = cfg.Router
	}
	resolver := resolveOrFabricateKeyResolver(cfg)
	// Capabilities precedence: cfg.Capabilities, else legacy *Fn closures
	// wrapped in closureCapabilities, else NoopCapabilities{}.
	caps := cfg.Capabilities
	if caps == nil {
		if cfg.TakeoverFn != nil || cfg.ReplyFooterFn != nil {
			caps = closureCapabilities{
				takeover:    cfg.TakeoverFn,
				replyFooter: cfg.ReplyFooterFn,
			}
		} else {
			caps = NoopCapabilities{}
		}
	}
	if cfg.Capabilities != nil && (cfg.TakeoverFn != nil || cfg.ReplyFooterFn != nil) {
		slog.Warn("dispatch: DispatcherConfig.Capabilities set; legacy TakeoverFn/ReplyFooterFn ignored",
			"takeover_fn_set", cfg.TakeoverFn != nil,
			"reply_footer_fn_set", cfg.ReplyFooterFn != nil)
	}
	// Collapse a typed-nil CronCommands (nil pointer boxed into the
	// interface) so `d.scheduler != nil` gates behave (#1178).
	var scheduler CronCommands
	if !isNilInterface(cfg.Scheduler) {
		scheduler = cfg.Scheduler
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
		turns:                 cfg.Turns,
		dedup:                 cfg.Dedup,
		allowedRoot:           cfg.AllowedRoot,
		claudeDir:             cfg.ClaudeDir,
		noOutputTimeout:       cfg.NoOutputTimeout,
		totalTimeout:          cfg.TotalTimeout,
		watchdogNoOutputKills: cfg.WatchdogNoOutputKills,
		watchdogTotalKills:    cfg.WatchdogTotalKills,
		caps:                  caps,
		fallbackBannerDelay:   fallbackBannerDelayDefault,
		inboundLimit:          newInboundLimiter(cfg.RateLimit),
		rateLimit:             cfg.RateLimit,
		rateLimitReplies:      denyThrottle{window: rateLimitReplyWindow},
		budgetReplies:         denyThrottle{window: budgetReplyWindow},
		groupScope:            cfg.GroupScope,
	}
	if !isNilInterface(cfg.Budget) {
		d.budget = cfg.Budget
	}
	d.access.Store(cfg.Access)
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

// isNilInterface reports whether v is nil or a nil pointer boxed into an
// interface, which would pass a `!= nil` gate and panic on first use.
func isNilInterface(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
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
	opts      sessionview.AgentOpts
	images    []clievent.Attachment
}

// inboundLogger returns the logger carrying msg's platform/user/chat attrs.
// Those fields are adversary-controlled, so they are sanitized before slog;
// the logger is memoized on the sanitized triple (#2233), so the cache key
// cannot diverge from the attr values.
func (d *Dispatcher) inboundLogger(msg platform.IncomingMessage) *slog.Logger {
	sp := sessionkey.SanitizeLogAttr(msg.Platform)
	su := sessionkey.SanitizeLogAttr(msg.UserID)
	sc := sessionkey.SanitizeLogAttr(msg.ChatID)
	logKey := sp + "\x00" + su + "\x00" + sc
	lg := d.inboundLogCache.get(logKey)
	if lg == nil {
		lg = slog.With("platform", sp, "user", su, "chat", sc)
		d.inboundLogCache.put(logKey, lg)
	}
	return lg
}

// prepareInbound runs the front-matter common to every dispatch strategy
// (dedup, group-mention gate, sender authorization, rate limit, slash
// commands, agent
// resolution, accounting, key/opts resolution, image conversion). Returns
// false when the message was fully handled or dropped here.
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
	if unmentionedInGroup(msg) {
		return preparedInbound{}, false
	}

	lg := d.inboundLogger(msg)
	trimmed := strings.TrimSpace(msg.Text)

	// Sender authorization: after the mention gate so un-mentioned group
	// chatter stays a silent drop, before anything that acts on the message.
	if !d.authorize(ctx, msg, trimmed, lg) {
		return preparedInbound{}, false
	}
	// After authorization so a refused sender spends no tokens, before
	// commands so command spam is limited too.
	if !d.admitRate(ctx, msg, trimmed, lg) {
		return preparedInbound{}, false
	}

	if d.dispatchCommand(ctx, msg, trimmed, lg) {
		return preparedInbound{}, false
	}

	// Resolve agent from command prefix (e.g. "/review code" -> agent=code-reviewer, text="code")
	agentID, cleanText := agentroute.ResolveAgent(trimmed, d.agentCommands)

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
	key, opts := d.resolver.ResolveForChat(msg.Platform, msg.ChatType, d.sessionChatID(msg), agentID)

	var images []clievent.Attachment
	if len(msg.Images) > 0 {
		images = make([]clievent.Attachment, 0, len(msg.Images))
		for _, img := range msg.Images {
			images = append(images, clievent.Attachment{Data: img.Data, MimeType: img.MimeType})
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

// BuildHandler returns a platform.MessageHandler wired to this Dispatcher:
// a message that survives prepareInbound is submitted as an IM turn.
func (d *Dispatcher) BuildHandler() platform.MessageHandler {
	return func(ctx context.Context, msg platform.IncomingMessage) {
		p, ok := d.prepareInbound(ctx, msg)
		if !ok {
			return
		}
		o := d.newIMOrigin(msg, p.lg, p.key, p.agentID, p.opts, imMessage, len(p.cleanText), len(p.images))
		d.submit(ctx, o, turn.Request{Key: p.key, Text: p.cleanText, Images: p.images})
	}
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

// handleGetOrCreateError maps a failed GetOrCreate (turn.StageSession) into the reply ctx,
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

// handleSendError maps a failed send (turn.StageSend) into the user-facing error
// reply, watchdog counter bumps, and metrics increments (#624). It does NOT
// report whether the error reply landed — that failure is only logged at Warn.
// Only the turn's primary delivery counts, so a turn answered in two chats
// counts once.
func (d *Dispatcher) handleSendError(
	ctx context.Context,
	err error,
	key string,
	msg platform.IncomingMessage,
	p platform.Platform,
	lg *slog.Logger,
	primary bool,
) {
	// ErrSessionReset is a user control-flow signal (/new, /clear), not an
	// error: no extra reply and no /health error-counter bump.
	if errors.Is(err, clierr.ErrSessionReset) {
		return
	}
	lg.Error("send to claude", "err", err)
	// usermsg.UserMessage renders the configured timeout durations in
	// Chinese (dashboard uses the generic ForSendError). Watchdog counters
	// stay here because the IM side owns that configuration.
	if primary {
		d.replyErrorCount.Add(1)
		dispatchReplyErrorTotal.Add(1)
		switch {
		case errors.Is(err, clierr.ErrNoOutputTimeout):
			d.watchdogNoOutputKills.Add(1)
		case errors.Is(err, clierr.ErrTotalTimeout):
			d.watchdogTotalKills.Add(1)
		}
	}
	errMsg := usermsg.UserMessage(err, key, d.noOutputTimeout, d.totalTimeout)
	// IM-only emoji decoration for the timeout cases. Other surfaces
	// (dashboard send_ack) deliberately stay emoji-free.
	if errors.Is(err, clierr.ErrNoOutputTimeout) || errors.Is(err, clierr.ErrTotalTimeout) {
		errMsg = "⏱️ " + errMsg
	}
	// On shutdown the inbound ctx is already Done; swap so the error reply
	// still lands.
	replyCtx, cleanup := resolveReplyCtx(ctx)
	if cleanup != nil {
		defer cleanup()
	}
	if _, err := platform.ReplyWithRetry(replyCtx, p, replyDestOf(msg).text(errMsg), limits.PlatformReplyMaxAttempts); err != nil {
		d.sendFailCount.Add(1)
		dispatchSendFailTotal.Add(1)
		lg.Warn("error reply also failed", "chat", msg.ChatID, "err", err)
	}
}

// ReplyDest is where the replies to one inbound message go: its chat and,
// when it was posted in a thread or topic, that thread.
type ReplyDest struct {
	ChatID   string
	ThreadID string
}

func replyDestOf(msg platform.IncomingMessage) ReplyDest {
	return ReplyDest{ChatID: msg.ChatID, ThreadID: msg.ThreadID}
}

// text is a text message to r.
func (r ReplyDest) text(s string) platform.OutgoingMessage {
	return platform.OutgoingMessage{ChatID: r.ChatID, ThreadID: r.ThreadID, Text: s}
}

// sendOutboundImages delivers each turn image as its own reply bubble.
func (d *Dispatcher) sendOutboundImages(ctx context.Context, p platform.Platform, to ReplyDest, images []platform.Image) {
	for _, img := range images {
		// ReplyWithRetry (not bare Reply) so an image gets the same
		// token-rotation retry as text (#2305).
		if _, err := platform.ReplyWithRetry(ctx, p, platform.OutgoingMessage{
			ChatID:   to.ChatID,
			ThreadID: to.ThreadID,
			Images:   []platform.Image{img},
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
	imagePaths := clievent.ExtractImagePaths(replyText)
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
				outImages = append(outImages, platform.Image{Data: data, MimeType: clievent.MimeFromPath(path)})
				turnImageBytes += len(data)
			}
			// Over budget: skip the attachment but still rewrite the path.
		}
		replyText = strings.ReplaceAll(replyText, path, "[图片]")
	}
	return outImages, replyText
}

// decorateReplyText post-processes the raw CLI result text for IM delivery:
// turnReplyText's answer or failure notice, then the partial-reply and
// merge-group chips and the per-session ReplyFooter. Returns "" when nothing
// should be sent (#656).
func (d *Dispatcher) decorateReplyText(result *clievent.SendResult, sess turn.Session) string {
	replyText := turnReplyText(result)
	// claude cut the answer off (aborted_streaming keeps the partial text);
	// keyed on CLIAborted, not Aborted, which a late interrupt can stamp on
	// a turn that finished.
	if result.CLIAborted() && replyText != "" {
		replyText += replyChipPartial
	}
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

// replyChipPartial marks a reply claude aborted part-way.
const replyChipPartial = "\n\n*— 已中断，以上为部分回复*"

// SendSplitReply sends a reply, splitting into multiple messages if too long.
func (d *Dispatcher) SendSplitReply(ctx context.Context, p platform.Platform, to ReplyDest, text string) {
	d.sendChunks(ctx, p, to, replyChunks(p, text))
}

// replyChunks returns the messages p gets for text: one when it fits
// p.MaxReplyLength, else the split chunks with their "[i/N]" suffix. The
// banner edit uses it too, so both paths honour the same limit.
func replyChunks(p platform.Platform, text string) []string {
	maxLen := p.MaxReplyLength()
	if maxLen <= 0 {
		maxLen = platform.DefaultMaxReplyLen
	}

	// Single-use-token platforms (e.g. WeChat iLink) can deliver only ONE
	// message per inbound turn; N chunks would lose [2/N]..[N/N]. Collapse
	// to one truncated message with a visible marker (#2136).
	if platform.UsesSingleUseReplyToken(p) {
		if utf8.RuneCountInString(text) > maxLen {
			text = replyfmt.TruncateForSingleReply(text, maxLen)
		}
		return []string{text}
	}

	// Byte-length fast path: len(text) is an upper bound on the rune count,
	// so len(text) <= maxLen means no split is needed; skip the rune scan.
	if len(text) <= maxLen {
		return []string{text}
	}

	// When splitting, each chunk gets a "\n— [i/N]" suffix; splitting at the
	// raw limit would push full chunks past hard API ceilings (Discord 2000,
	// rejected outright, and ReplyWithRetry re-sends the same payload). Reserve
	// the worst-case suffix using an upper-bound chunk count computed at the
	// reduced width — over-reserving is safe, under-reserving is not (#2008).
	// Count runes once for both the reservation and SplitTextWithCount (#2283).
	runeCount := utf8.RuneCountInString(text)
	// The reservation and the "no room for any suffix" fallback live in
	// replyfmt so the cron notify path gets the same page numbers (J2 #2548).
	splitLen, suppressSuffix := replyfmt.ReserveForPageSuffix(maxLen, runeCount)

	chunks := platform.SplitTextWithCount(text, splitLen, runeCount)
	if total := len(chunks); total > 1 && !suppressSuffix {
		for i := range chunks {
			chunks[i] += replyfmt.PageSuffix(i+1, total)
		}
	}
	return chunks
}

// sendChunks sends each chunk as its own message, counting failures per chunk.
func (d *Dispatcher) sendChunks(ctx context.Context, p platform.Platform, to ReplyDest, chunks []string) {
	for i, chunk := range chunks {
		if _, err := platform.ReplyWithRetry(ctx, p, to.text(chunk), limits.PlatformReplyMaxAttempts); err != nil {
			d.sendFailCount.Add(1)
			dispatchSendFailTotal.Add(1)
			slog.Error("reply chunk failed after retries", "chat", to.ChatID, "chunk", i+1, "err", err)
		} else {
			d.markReplySuccess()
		}
	}
}
