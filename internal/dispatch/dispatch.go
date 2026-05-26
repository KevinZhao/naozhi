package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/usermsg"
)

// platformReplyTimeout caps every outbound platform.Reply / EditMessage
// call dispatch makes. Shared by all four call sites so a future per-
// platform tuning lands in one place. R228-ARCH-12.
const platformReplyTimeout = 15 * time.Second

// shutdownReplyTimeout caps platform.Reply attempts on the shutdown /
// context.Canceled fallback path. Deliberately shorter than
// platformReplyTimeout (15s): the surrounding ctx is already Done because
// the dispatcher / session subsystem is tearing down, so we want a fast
// best-effort notice ("系统正在重启，请稍后重试。") rather than blocking
// the shutdown sequence on a slow IM API. 5s matches the conservative end
// of platform retry budgets so the message still has a realistic chance of
// landing before systemctl SIGKILLs the process. R239-CR-5.
const shutdownReplyTimeout = 5 * time.Second

// platformReplyMaxAttempts is the retry count passed to
// platform.ReplyWithRetry on dispatch's two outbound paths (error-reply
// fallback at the end of processMessage and the chunk-loop in
// SendSplitReply). Shared so the two sites can't drift independently
// (R240-CR-5). 3 attempts matches the conservative IM platform budget
// where transient 5xx responses typically clear within 1-2 retries; bumps
// should be considered against the per-attempt platformReplyTimeout
// (15s × 3 = 45s worst-case) staying inside outer ctx deadlines.
const platformReplyMaxAttempts = 3

// SessionGuard prevents multiple concurrent messages to the same session.
// MessageQueue is the production implementation; the IM path injects a
// MessageQueue here so queue-mode gates and the guard contract stay
// compatible. session.Guard happens to satisfy the same shape and is
// retained as a structural option for future Dashboard/WS reuse — note
// that today server/send.go references *session.Guard concretely rather
// than going through this interface.
//
// Keep the method set minimal: any future guard variant has to fit.
//
// R228-ARCH-11 (archive 2026-05-23): the ticket framed this as "1 method
// interface that's actually either-or — delete it, use the concrete type".
// That misreads the consumer surface. Three implementations live behind
// SessionGuard today: MessageQueue (prod IM path), session.Guard (prod
// Dashboard/WS via msgqueue.go SessionGuard-compat methods), and the
// dispatch_test.go::fakeGuard test seam. Collapsing to a concrete type
// would force test wiring through MessageQueue's full enqueue/drain
// machinery just to exercise busy-flag transitions — losing the unit-
// level isolation that fakeGuard delivers in ~10 LOC. The "either-or"
// branch in dispatch.go is a runtime selector between queue-mode and
// guard-mode wiring (NewDispatcher's d.queue != nil vs d.guard fallback),
// not a structural redundancy. Keep the interface; the cost is one extra
// indirection on a path already dominated by queue.Enqueue / Release.
type SessionGuard interface {
	TryAcquire(key string) bool
	ShouldSendWait(key string) bool
	Release(key string)
}

// Dispatcher holds the dependencies needed to dispatch incoming IM messages
// to the session router, handle slash commands, and stream results back.
type Dispatcher struct {
	// router is the SessionRouter subset used by dispatch (consumer.go).
	// *session.Router satisfies this implicitly; kept as an interface so
	// tests can inject fakes and a future Router sub-aggregation can
	// swap implementations without touching dispatch internals. The
	// router field itself is guaranteed non-nil in production wiring.
	router    SessionRouter
	platforms map[string]platform.Platform
	// agents / agentCommands are populated from DispatcherConfig at
	// NewDispatcher and treated as immutable thereafter. The IM hot path
	// (BuildHandler, sendAndReply, slash commands) reads these maps without
	// any lock, so any future code that needs to mutate them MUST switch to
	// atomic.Pointer[map[...]] swap-on-write or guard with a new mutex.
	// Document mirrors `internal/cron/scheduler.go` Scheduler.agents
	// (R242-GO-18) so the contract is identical across both consumers of
	// session.AgentOpts maps.
	agents        map[string]session.AgentOpts
	agentCommands map[string]string
	scheduler     *cron.Scheduler
	// projectMgr is used by slash-command handlers for: (a) UX echo of
	// the bound project's name from /new, /cd, /project; (b) /cd guard
	// against workspace-fixed projects; (c) /new resolution of planner
	// vs agent keys when chat is bound; (d) /project [off|list|<name>]
	// state mutations.
	//
	// Routing decisions on the IM hot path (BuildHandler / sendAndReply)
	// MUST go through resolver only — do not reintroduce ProjectForChat
	// / EffectivePlanner* calls in the IM / queue / send paths or the
	// legacy duplicate-routing branches that R-key-resolver collapsed
	// will quietly come back.  Any new ProjectForChat / EffectivePlanner*
	// read on the hot path should fail review.
	projectMgr *project.Manager
	// resolver centralises (key, opts) derivation for the IM and slash-
	// command paths. NewDispatcher guarantees this field is non-nil — when
	// callers don't supply a resolver the constructor fabricates a project-
	// less fallback so call sites can dereference unconditionally.
	// See docs/rfc/key-resolver.md Phase 2.
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

	// imageReader resolves cli-extracted image paths to bytes for the
	// outbound platform.Image payload (sendAndReply). Production wires
	// osImageReader{} which delegates to os.ReadFile; tests inject an
	// in-memory map so reply-footer / image-attachment assertions don't
	// touch disk. R245-ARCH-33 (#884) — previously dispatch.go reached
	// for os.ReadFile directly, leaving no seam for tests to mock the
	// filesystem branch. Always non-nil after NewDispatcher.
	imageReader ImageReader

	// Operational counters exposed via /health for triaging. Incremented
	// atomically and never reset (monotonic since process start).
	messageCount       atomic.Int64 // all non-slash-command IM messages accepted
	replyErrorCount    atomic.Int64 // errors returned by Capabilities.Send (includes timeouts)
	sendFailCount      atomic.Int64 // user-visible reply failures (platform send errors)
	lastReplySuccessNs atomic.Int64 // UnixNano of most recent successful user-visible reply; 0 until first success

	// caps groups the host-supplied hooks (Send / Takeover / ReplyFooter)
	// that Dispatcher needs to reach back into the surrounding Server.
	// Always non-nil after NewDispatcher: callers either set
	// DispatcherConfig.Capabilities directly or supply legacy *Fn closures
	// (which the constructor wraps in a closureCapabilities adapter); when
	// neither is provided, NoopCapabilities{} is installed so the hot path
	// can call methods unconditionally.
	//
	// Wireup contract:
	//   - Capabilities.Send is required for production. R250-ARCH-12:
	//     NewDispatcher returns ErrSendWireupMissing when no usable Send is
	//     supplied (no Capabilities, no SendFn, no AllowMissingSender) so
	//     the caller controls the failure mode (callable from systemd-aware
	//     boot path) instead of crashing with a panic. NoopCapabilities.Send
	//     still panics if reached at runtime to catch the AllowMissingSender
	//     opt-out cases that misuse the dispatcher.
	//   - Capabilities.Takeover defaults to false (no external session).
	//   - Capabilities.ReplyFooter defaults to "" (no footer).
	//
	// See internal/dispatch/capabilities.go for the interface and the
	// NoopCapabilities default. R243-ARCH-10 collapsed three closure
	// fields (sendFn / takeoverFn / replyFooterFn) into this single
	// interface to make wireup harder to forget.
	caps Capabilities
}

// keyForChat returns the routed session key for the given chat coordinates
// and agentID. Delegates to KeyResolver, which encodes project-bound
// general → planner precedence. NewDispatcher guarantees resolver is
// non-nil even for headless/test wiring (falls back to a project-less
// resolver), so no nil-branch is needed here. Kept as a Dispatcher method
// so slash-command handlers share a single derivation path with the main
// IM path — see docs/rfc/key-resolver.md §4.2-4.4.
func (d *Dispatcher) keyForChat(platform, chatType, chatID, agentID string) string {
	return d.resolver.KeyForChat(platform, chatType, chatID, agentID)
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
	// Resolver is the central (key, opts) derivation. Optional: when nil,
	// NewDispatcher fabricates a fallback resolver from cfg.Agents and a
	// DataSource derived from cfg.ProjectMgr (which may itself be nil for
	// pure-headless tests). Production wiring in cmd/naozhi.main always
	// passes a shared live KeyResolver.
	Resolver    *session.KeyResolver
	Guard       SessionGuard
	Queue       *MessageQueue
	Dedup       *platform.Dedup
	AllowedRoot string
	ClaudeDir   string

	// Capabilities groups the host-supplied hooks (Send / Takeover /
	// ReplyFooter) that Dispatcher needs to reach back into the surrounding
	// Server. Preferred over the legacy SendFn / TakeoverFn / ReplyFooterFn
	// closures below — when both are set, Capabilities wins.
	//
	// nil is allowed: NewDispatcher falls back to the legacy *Fn closures
	// (wrapped in an internal closureCapabilities adapter) and finally to
	// NoopCapabilities{} if those are nil too. NoopCapabilities.Send panics
	// to mirror the legacy "no fallback" contract for the send path.
	//
	// Tracked under R243-ARCH-10. See capabilities.go for the interface.
	Capabilities Capabilities

	// ReplyFooterFn returns the per-session reply tag (e.g. "cc" / "kiro")
	// given the session's backend ID. The IM reply path appends "\n\n— <tag>"
	// to outbound messages so users can see which backend produced the reply.
	// Empty backend means "session has no backend pinned yet" — fn typically
	// resolves to the router default's tag.
	//
	// nil means "no footer", same as the legacy ReplyFooter="" default.
	// docs/rfc/multi-backend.md §7 (per-session ReplyTag).
	//
	// Deprecated: prefer DispatcherConfig.Capabilities. R243-ARCH-10 collapsed
	// the three closure fields into Capabilities so wireup is harder to forget
	// and future hooks add an interface method instead of a new closure +
	// nil-fallback line. This field is still honoured for backward
	// compatibility but new code should set Capabilities directly.
	//
	// Removal trigger (#374): production has been Capabilities-only since
	// R243-ARCH-10 (server.go::Server.Start uses serverCaps; cmd/* and
	// internal/platform/* never reference *Fn). The remaining call sites
	// are tests (internal/dispatch/dispatch_test.go::buildDispatcher,
	// internal/server/server_test.go and capability-adapter coverage in
	// dispatch_test.go::TestNewDispatcher_*Capabilities*). Once those tests
	// migrate to dispatch.closureCapabilities literals or a small test
	// helper, ReplyFooterFn / SendFn / TakeoverFn plus the
	// closureCapabilities adapter and the legacy-detection branch in
	// NewDispatcher (the "if cfg.SendFn != nil ..." block and the
	// Capabilities-and-*Fn-both-set slog.Warn) can be removed in one
	// pass. Target: 2026-Q3, gated on those test migrations landing.
	// See R248-ARCH-3 in docs/TODO.md (linked from issue #374).
	ReplyFooterFn func(backendID string) string

	NoOutputTimeout       time.Duration
	TotalTimeout          time.Duration
	WatchdogNoOutputKills *atomic.Int64
	WatchdogTotalKills    *atomic.Int64

	// ImageReader resolves outbound image paths to bytes when the cli
	// reply contains attachment markers. Optional — NewDispatcher
	// installs osImageReader{} (os.ReadFile delegation) when nil so
	// production wiring keeps zero-config. Tests inject a fake to
	// exercise the read-success / read-failure branches without
	// touching the filesystem. R245-ARCH-33 (#884).
	ImageReader ImageReader

	// SendFn forwards a turn payload to the session router after guard /
	// queue gating has succeeded. Production wires Server.sendWithBroadcast.
	//
	// Deprecated: prefer DispatcherConfig.Capabilities. See ReplyFooterFn
	// for the consolidated removal trigger (#374).
	SendFn func(ctx context.Context, key string, sess *session.ManagedSession, text string, images []cli.ImageData, onEvent cli.EventCallback) (*cli.SendResult, error)
	// TakeoverFn is the optional auto-takeover hook invoked on the first
	// message of every chat. nil is treated as "return false".
	//
	// Deprecated: prefer DispatcherConfig.Capabilities. See ReplyFooterFn
	// for the consolidated removal trigger (#374).
	TakeoverFn func(ctx context.Context, chatKey, key string, opts session.AgentOpts) bool

	// AllowMissingSender opts out of the constructor-time "Send must be
	// wired" check. Test wiring that builds a Dispatcher without ever
	// touching the IM send path (e.g. pure routing / queue / commands tests)
	// sets this true so NewDispatcher does not panic.
	//
	// Production code MUST leave this false: production wiring always sets
	// Capabilities (or, legacy, SendFn). Without the gate, a missing
	// wireup surfaces as a runtime panic on the first user message —
	// healthcheck-ok-then-systemd-restart-loop, which is worse than
	// silent drop because it leaves no clear failure signal at boot.
	// R248-ARCH-2.
	AllowMissingSender bool
}

// NewDispatcher creates a Dispatcher from the given config.
//
// cfg.Router is a concrete *session.Router but Dispatcher.router is
// the SessionRouter interface. Assigning a nil *session.Router into
// an interface field produces a typed-nil: the field compares !=
// nil yet dereferences panic. Normalise to untyped nil so call-site
// guards like `if d.router != nil` behave as readers expect.
// Production wiring (server.Start) never passes nil; the guard covers
// headless/test wiring that may leave the field zeroed.
//
// Resolver is required for the main IM path. To keep test/headless
// constructions ergonomic the constructor builds a fallback resolver
// from (cfg.Agents, project DataSource derived from cfg.ProjectMgr)
// when cfg.Resolver is nil — the project data source short-circuits
// when ProjectMgr is also nil so behaviour matches pre-resolver code.
// This eliminates the legacy nil-resolver inline branches scattered
// across dispatch / commands / urgent.
// ErrSendWireupMissing is returned by NewDispatcher when no usable Send
// hook was supplied. R250-ARCH-12: prefer surfacing missing wireup as an
// error the caller can branch on, rather than as a panic that crashes
// systemd before logs flush. Tests that intentionally omit Send wireup
// can opt out via DispatcherConfig.AllowMissingSender.
var ErrSendWireupMissing = errors.New("dispatch: Capabilities.Send is required (set DispatcherConfig.Capabilities or DispatcherConfig.SendFn; tests may set AllowMissingSender)")

// NewDispatcher constructs a Dispatcher from cfg. Returns
// ErrSendWireupMissing when neither cfg.Capabilities (with non-noop Send)
// nor cfg.SendFn is set and AllowMissingSender is false. R250-ARCH-12
// converted this from a constructor-time panic to a returned error so the
// caller controls the failure mode (systemd-friendly logging vs panic
// stack trace).
func NewDispatcher(cfg DispatcherConfig) (*Dispatcher, error) {
	var router SessionRouter
	if cfg.Router != nil {
		router = cfg.Router
	}
	resolver := cfg.Resolver
	if resolver == nil {
		var data session.PlannerDataSource
		if cfg.ProjectMgr != nil {
			data = project.NewDataSource(cfg.ProjectMgr)
		}
		resolver = session.NewKeyResolver(cfg.Agents, data)
	}
	// Resolve Capabilities precedence:
	//   1. cfg.Capabilities wins when set (preferred path);
	//   2. otherwise, if any legacy *Fn closure is non-nil, wrap them in a
	//      closureCapabilities adapter (preserves the historical wireup so
	//      existing test seams keep building);
	//   3. otherwise, NoopCapabilities{} so the hot path always has a
	//      non-nil receiver. NoopCapabilities.Send panics — mirroring the
	//      legacy "no fallback for SendFn" contract — while Takeover and
	//      ReplyFooter return their documented defaults (false / "").
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
	// R248-ARCH-2 boot-panic gate: surface missing Send wireup at
	// constructor-time, not on the first user message. The legacy
	// NoopCapabilities.Send / closureCapabilities (with c.send==nil) both
	// panic when actually invoked — but that arrives AFTER healthcheck,
	// systemd marks the unit healthy, and the first user message turns into
	// a panic-restart loop. Catching it here lets a misconfigured boot fail
	// loud before any traffic is accepted. Tests that genuinely never call
	// Send opt out via AllowMissingSender.
	if !cfg.AllowMissingSender {
		hasSend := false
		switch c := caps.(type) {
		case NoopCapabilities:
			hasSend = false
		case closureCapabilities:
			hasSend = c.send != nil
		default:
			// Any other Capabilities implementation is presumed to wire
			// Send; if its Send actually panics that is the contract the
			// caller chose, not a missing wireup we can detect lexically.
			hasSend = true
		}
		if !hasSend {
			return nil, ErrSendWireupMissing
		}
	}
	// R248-GO-2: warn when Capabilities and the legacy *Fn fields are both
	// set — Capabilities wins and the *Fn closures are silently ignored,
	// which is a common transition-period misuse. One-time slog.Warn at
	// constructor time, no hot-path cost.
	if cfg.Capabilities != nil && (cfg.SendFn != nil || cfg.TakeoverFn != nil || cfg.ReplyFooterFn != nil) {
		slog.Warn("dispatch: DispatcherConfig.Capabilities set; legacy SendFn/TakeoverFn/ReplyFooterFn ignored",
			"send_fn_set", cfg.SendFn != nil,
			"takeover_fn_set", cfg.TakeoverFn != nil,
			"reply_footer_fn_set", cfg.ReplyFooterFn != nil)
	}
	d := &Dispatcher{
		router:                router,
		platforms:             cfg.Platforms,
		agents:                cfg.Agents,
		agentCommands:         cfg.AgentCommands,
		scheduler:             cfg.Scheduler,
		projectMgr:            cfg.ProjectMgr,
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
	// Headless / test wirings may also leave the watchdog kill counters
	// unset. Production wiring sets them, but tests routinely build a
	// Dispatcher without these fields. The watchdog hot path calls
	// .Add(1) unconditionally; nil here would panic. (R227-CR-12)
	if d.watchdogNoOutputKills == nil {
		d.watchdogNoOutputKills = new(atomic.Int64)
	}
	if d.watchdogTotalKills == nil {
		d.watchdogTotalKills = new(atomic.Int64)
	}
	// BuildHandler's hot path calls d.dedup.Seen(...) unconditionally. The
	// caps / watchdog counters above already noop-fallback for headless
	// and test wiring; the same convention applies here. Without this, a
	// constructor missing cfg.Dedup would crash on the very first incoming
	// message (nil-pointer deref inside Seen). Default capacity matches
	// platform.NewDedup's own zero-cap fallback (10000). (R237-GO-12)
	if d.dedup == nil {
		d.dedup = platform.NewDedup(0)
	}
	if cfg.ImageReader != nil {
		d.imageReader = cfg.ImageReader
	} else {
		d.imageReader = osImageReader{}
	}
	return d, nil
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
		lg := slog.With(
			"platform", session.SanitizeLogAttr(msg.Platform),
			"user", session.SanitizeLogAttr(msg.UserID),
			"chat", session.SanitizeLogAttr(msg.ChatID),
		)
		trimmed := strings.TrimSpace(msg.Text)

		// Dispatch slash commands (/help, /new, /cron, /cd, /pwd, /project)
		if d.dispatchCommand(ctx, msg, trimmed, lg) {
			return
		}

		// Resolve agent from command prefix (e.g. "/review code" -> agent=code-reviewer, text="code")
		agentID, cleanText := session.ResolveAgent(trimmed, d.agentCommands)
		if cleanText == "" && len(msg.Images) == 0 {
			if agentID != "general" {
				d.replyText(ctx, msg, "请在指令后输入内容。", lg)
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
				d.replyText(ctx, msg, "未知命令: "+cmd+"\n输入 /help 查看可用命令，或直接发送消息。", lg)
				return
			}
		}

		// Count accepted messages (post-dedup, post-command-filter). Does not
		// include slash commands, ignored non-text items, or dedup hits.
		// Per-Dispatcher counter feeds /health; expvar mirror feeds
		// /debug/vars. R245-ARCH-36 (#892).
		d.messageCount.Add(1)
		dispatchMessageTotal.Add(1)

		// Determine session key and opts via KeyResolver — single source of
		// truth for project-binding precedence and aliasing-safe ExtraArgs
		// merge (see docs/rfc/key-resolver.md §3.1 and session/routing.go).
		// NewDispatcher always builds a resolver, so no nil-branch fallback
		// is needed.
		key, opts := d.resolver.ResolveForChat(msg.Platform, msg.ChatType, msg.ChatID, agentID)

		// Convert platform images to CLI image data
		var images []cli.ImageData
		for _, img := range msg.Images {
			images = append(images, cli.ImageData{Data: img.Data, MimeType: img.MimeType})
		}

		// Passthrough mode: direct dispatch — every message gets its own
		// goroutine. Ordering and merging handled by the CLI's commandQueue
		// plus the Process-level sendSlot FIFO. No naozhi-side coalesce.
		//
		// Fallback: if the session's protocol does not expose the
		// --replay-user-messages primitive (e.g. ACP), sendFn silently
		// downgrades to the legacy sendMu-serialized Send path. That loses
		// the passthrough merge optimization but preserves correctness: each
		// of N concurrent goroutines blocks on sendMu in arrival order.
		if d.queue != nil && d.queue.Mode() == ModePassthrough {
			lg.Info("message received (passthrough)", "agent", agentID, "text_len", len(cleanText), "images", len(images))
			// Detach from the platform handler ctx: webhook handlers return
			// in seconds while LLM turns take minutes. If we keep the caller
			// ctx, handler-return cancels it and SendPassthrough bails early,
			// leaking slots into the 5.5-min bail timer. Use WithoutCancel
			// to preserve values (log fields, auth) without the cancellation.
			sendCtx := context.WithoutCancel(ctx)
			go d.sendAndReply(WithPassthrough(sendCtx), key, cleanText, images, agentID, opts, msg, lg, true)
			// Ack arrival so the IM user sees a reaction/receipt. This is
			// cheap and does not depend on the turn completing.
			d.ackQueuedWithReaction(ctx, msg, lg)
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
						lg.Info("interrupt mode: aborted active turn to process follow-up",
							"key", key)
					case session.InterruptNoTurn:
						// Session is spawning or idle — the turn isn't active yet,
						// so nothing to interrupt. The follow-up will be drained
						// by the owner loop after the first turn completes.
						lg.Debug("interrupt mode: session idle or spawning, will process follow-up after current turn",
							"key", key)
					case session.InterruptNoSession:
						lg.Debug("interrupt mode: session not found, falling back to collect",
							"key", key)
					case session.InterruptUnsupported:
						lg.Debug("interrupt mode: protocol does not support stdin interrupt, falling back to collect",
							"key", key)
					case session.InterruptError:
						// Warn already emitted inside ManagedSession.InterruptViaControl;
						// keep a paired trace here to anchor the dispatch side.
						lg.Warn("interrupt mode: transport error, falling back to collect",
							"key", key)
					}
				}
				if enqueued {
					// Prefer an in-place reaction on the user's own message
					// (non-intrusive) over a new bot chat bubble. Fall back to
					// the text notice if the platform isn't Reactor-capable,
					// has no inbound MessageID, or the reaction call fails —
					// ShouldNotify still rate-limits the fallback.
					if !d.ackQueuedWithReaction(ctx, msg, lg) {
						if d.queue.ShouldNotify(key) {
							d.replyText(ctx, msg, "消息已收到，待当前回复完成后一并处理。", lg)
						}
					}
				} else {
					// Queue disabled (maxDepth<=0) — degrade to old drop behavior.
					if d.queue.ShouldNotify(key) {
						d.replyText(ctx, msg, "正在处理上一条消息，请稍候...", lg)
					}
				}
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

// discardQueue is a nil-safe helper to clear queued messages for a key.
// In passthrough mode it also fires ErrSessionReset to any in-flight
// SendPassthrough callers so the IM user sees the turn as cancelled rather
// than silently hanging.
func (d *Dispatcher) discardQueue(key string) {
	if d.queue != nil {
		d.queue.Discard(key)
	}
	if d.router != nil {
		if sess := d.router.GetSession(key); sess != nil {
			sess.DiscardPassthroughPending(cli.ErrSessionReset)
		}
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
	lg *slog.Logger,
) {
	// Enrich the logger once for the whole ownerLoop lifetime. Previously
	// sendAndReply re-did this `log.With` on every drained turn — a coalesced
	// burst of 5 follow-ups meant 5 identical handler-chain allocs. Lifting
	// it here costs exactly one alloc per ownerLoop regardless of drain
	// depth. R61-PERF-12.
	lg = lg.With("key", key, "agent", agentID)
	// Defer order matters here. Go runs deferred funcs LIFO, so the LAST
	// registered defer runs FIRST. We want this exit order on every path
	// (clean return AND panic):
	//   1. recover() runs first  — catches a panic from sendAndReply,
	//      logs it via handleOwnerLoopPanic, and stops it propagating.
	//   2. NotifyIdle runs second — marks the session idle only after
	//      panic recovery has logged context, so an external watcher
	//      reading "idle" never sees a state where a panic is still
	//      mid-flight. (R237-GO-8)
	//
	// This means NotifyIdle must be registered BEFORE the recover defer
	// (so it runs after, by LIFO). Reversing this ordering would let
	// NotifyIdle run while the panic is still propagating, which races
	// with anyone observing the idle signal as "turn complete".
	defer d.router.NotifyIdle()
	defer func() {
		if r := recover(); r != nil {
			// R230-CQ-11: pass the enriched ownerLoop logger so the panic
			// path inherits the same key/agent/platform attrs as the rest
			// of this turn's log lines. The recover trigger means the
			// loop's normal `log.Info("message replied", ...)` already
			// fired this turn or never will — operators grepping by key
			// see the panic stitched into the same context window.
			d.handleOwnerLoopPanic(key, msg, r, lg)
		}
	}()

	// Process first message.
	d.sendAndReply(ctx, key, first.Text, first.Images, agentID, opts, msg, lg, true)

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
		lg.Info("processing queued messages", "count", len(queued), "merged_len", len(text))
		d.sendAndReply(ctx, key, text, images, agentID, opts, msg, lg, false)
		// Drained queued messages were acknowledged with a queue reaction
		// when they arrived; clear those reactions now that their content
		// was processed. Best-effort — errors only log.
		d.clearQueuedReactions(ctx, msg.Platform, queued, lg)
		// Go 1.23+: Reset on a Timer whose channel was just consumed by the case arm above is race-free; no Stop+drain needed.
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
//
// R230-CQ-11: lg carries the ownerLoop's enriched key/agent attrs so the
// panic and reply-panic log lines share context with the rest of the turn.
// nil is tolerated for callers that don't have an ownerLoop logger handy
// (e.g. unit tests) — falls back to the package-level slog.
func (d *Dispatcher) handleOwnerLoopPanic(key string, msg platform.IncomingMessage, r any, lg *slog.Logger) {
	metrics.PanicRecoveredTotal.Add(1)
	if lg == nil {
		lg = slog.Default()
	}
	lg.Error("ownerLoop panic", "key", key, "panic", r, "stack", string(debug.Stack()))
	if d.queue != nil {
		d.queue.Discard(key)
	}
	func() {
		defer func() {
			if rr := recover(); rr != nil {
				lg.Error("ownerLoop reply panic recovered", "key", key, "panic", rr)
			}
		}()
		notifyCtx, cancel := context.WithTimeout(context.Background(), platformReplyTimeout)
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
	lg *slog.Logger,
	isFirst bool,
) {
	// Session-key + agent attrs are attached once in ownerLoop (R61-PERF-12)
	// so every Info/Warn/Error line below carries enough context for an
	// operator to grep a full turn end-to-end without paying a per-call
	// handler-chain alloc.

	// Takeover check only on first message for a key.
	//
	// RNEW-010: takeoverFn returns bool to indicate whether an external
	// Claude session was adopted. We intentionally ignore the result here:
	// success means the old process was killed and the session was
	// registered for resume — GetOrCreate below will rebuild with the
	// resumed SessionID. Failure (returns false) means no external session
	// was found, which is the common case; GetOrCreate still needs to run
	// to spawn a fresh one. Either way the caller behaviour is identical,
	// so we discard explicitly rather than branch on it.
	if isFirst {
		_ = d.caps.Takeover(ctx, session.ChatKey(msg.Platform, msg.ChatType, msg.ChatID), key, opts)
	}

	sess, sessStatus, err := d.router.GetOrCreate(ctx, key, opts)
	if err != nil {
		// Shutdown-path cancellation is expected noise, not an alarm;
		// downgrade to Info so ops dashboards don't light up on every
		// restart. Unexpected failures stay at Error.
		if errors.Is(err, context.Canceled) {
			lg.Info("get session cancelled during shutdown", "err", err)
		} else {
			lg.Error("get session", "err", err)
		}
		var errMsg string
		replyCtx := ctx
		switch {
		case errors.Is(err, session.ErrMaxProcs):
			errMsg = "当前处理已满，请稍后重试。"
		case errors.Is(err, session.ErrMaxExemptSessions):
			// R190-WRAP-M1: exempt-session cap means "too many projects/cron
			// workers"; user /new won't clear it because the exempt counter
			// is independent of user sessions. Tell the user explicitly so
			// they contact the operator instead of looping on /new.
			errMsg = "长时会话（planner/cron）已满，请联系管理员。"
		case errors.Is(err, session.ErrNoCLIWrapper):
			// R190-WRAP-M1: permanent config error; /new retry is hopeless.
			// Surface a clear "ask operator" so IM users don't spin on it.
			errMsg = "会话后端未配置，请联系管理员。"
		case errors.Is(err, context.Canceled):
			errMsg = "系统正在重启，请稍后重试。"
			// R188-CONC-M1: ctx is already Done on shutdown path; using it for
			// the user-facing error reply silently drops the notification at
			// the platform layer. Match the handleOwnerLoopPanic recovery
			// pattern and use a fresh Background ctx with short timeout so the
			// user actually sees the "restart, retry" message.
			notifyCtx, cancel := context.WithTimeout(context.Background(), shutdownReplyTimeout)
			defer cancel()
			replyCtx = notifyCtx
		default:
			errMsg = "会话创建失败，请发送 /new 重置后重试。"
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

	tracker := newIMEventTracker(ctx, p, msg.ChatID)
	defer tracker.stop()

	result, err := d.caps.Send(ctx, key, sess, text, images, tracker.onEvent)
	if err != nil {
		d.replyErrorCount.Add(1)
		dispatchReplyErrorTotal.Add(1)
		lg.Error("send to claude", "err", err)
		// IM path uses the timeout-aware helper (it renders the configured
		// no-output / total durations in Chinese) and prepends a clock
		// emoji for visibility on chat surfaces. Dashboard send path
		// (server/errors_usermsg.go) calls usermsg.ForSendError directly
		// so the timeout cases collapse to the generic "处理超时，请简化任务后重试。"
		// — it has no per-session timeout configured. R249-DISPATCH-1 (#419)
		// extracted usermsg.UserMessage so a new sentinel only registers
		// once, instead of two parallel switches with cross-package
		// "keep in sync" comments.
		// /clear early-return mirrors the prior behaviour: the user just
		// triggered the reset, so we suppress the extra "会话已重置" reply.
		if errors.Is(err, cli.ErrSessionReset) {
			return
		}
		// Watchdog counters stay in dispatch because they are owned by the
		// IM-side configuration; the shared helper only renders text.
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
		if _, err := platform.ReplyWithRetry(ctx, p, platform.OutgoingMessage{ChatID: msg.ChatID, Text: errMsg}, platformReplyMaxAttempts); err != nil {
			d.sendFailCount.Add(1)
			dispatchSendFailTotal.Add(1)
			lg.Warn("error reply also failed", "chat", msg.ChatID, "err", err)
		}
		return
	}

	lg.Info("message replied", "result_len", len(result.Text), "cost", result.CostUSD,
		"merged_count", result.MergedCount, "merged_with_head", result.MergedWithHead)

	// Passthrough merge fan-out: follower slots get MergedCount>1 and an
	// empty Text. The head slot for the merge group delivered the full
	// reply on its own bubble; followers should surface a short "合并" hint
	// on the user's original message instead of echoing the same text again.
	if result.MergedCount > 1 && result.Text == "" {
		d.ackMergedFollower(ctx, msg, result.MergedCount, lg)
		d.markReplySuccess()
		return
	}

	// Record turn success regardless of reply text length. A successful
	// sendFn with empty result (e.g. a turn that only produces tool calls
	// or whose text was stripped) still constitutes a healthy end-to-end
	// roundtrip; gating markReplySuccess on non-empty text previously made
	// /health's lastReplySuccess go stale on otherwise-healthy sessions.
	d.markReplySuccess()

	replyText := localizeAPIError(result.Text)
	// Head slot of a merge group: append a small chip so the user knows the
	// single bot bubble covers N messages.
	if result.MergedCount > 1 && replyText != "" {
		// R20260526-PERF-005: hot path on every merge-group head reply,
		// avoid fmt.Sprintf's reflect/format overhead for a single int.
		replyText += "\n\n*— 合并了 " + strconv.Itoa(result.MergedCount) + " 条消息的回复*"
	}
	// Per-session ReplyFooter: when sess is non-nil we resolve the tag from
	// sess.Backend(); when nil (cron edge case where the session has been
	// pruned but the reply path still fires) Capabilities.ReplyFooter
	// receives "" and the implementation falls back to the router default.
	// NoopCapabilities returns "" so an unwired host yields no footer (same
	// as the legacy nil-closure behaviour).
	{
		var backendID string
		if sess != nil {
			backendID = sess.Backend()
		}
		if footer := d.caps.ReplyFooter(backendID); footer != "" {
			replyText += "\n\n— " + footer
		}
	}
	var outImages []platform.Image
	imagePaths := cli.ExtractImagePaths(replyText)
	if len(imagePaths) > 0 {
		// One pass through replyText for all paths: previously the per-path
		// strings.ReplaceAll loop scanned the whole message text N times,
		// turning a multi-image reply into O(N * len(replyText)). Building
		// a single Replacer keeps it O(len(replyText)). Each path is always
		// added to replacePairs (even when ReadFile fails) so the user-visible
		// behaviour matches the prior strings.ReplaceAll loop where every
		// extracted path got rewritten to "[图片]" regardless of read success.
		replacePairs := make([]string, 0, 2*len(imagePaths))
		for _, path := range imagePaths {
			data, err := d.imageReader.ReadFile(path)
			if err == nil {
				outImages = append(outImages, platform.Image{Data: data, MimeType: cli.MimeFromPath(path)})
			}
			replacePairs = append(replacePairs, path, "[图片]")
		}
		replyText = strings.NewReplacer(replacePairs...).Replace(replyText)
	}

	tracker.waitReady(ctx)

	// AskUserQuestion suppression: when this turn surfaced an interactive
	// question card, `claude -p` also emits a bailout text ("I've asked you
	// two questions ...") because it auto-rejects the tool to unblock
	// headless mode. That text is redundant with the card and makes the
	// session look "finished" instead of "waiting for answer". Replace it
	// with a short wait-hint on the thinking banner so the user's next view
	// on the IM channel is the card + a single "waiting" line, nothing else.
	// The card itself stays rendered above; clicking it sends the answer.
	//
	// Dashboard is not affected: it already renders the card as a native
	// bubble separate from the reply stream, and suppressing the text
	// simply removes the duplicate final bubble.
	if tracker.askQuestionFired.Load() {
		if msgID := tracker.getThinkingMsgID(); msgID != "" {
			// Best-effort — if the banner edit fails, we log and move on;
			// there's no user-visible recovery better than "tried to clear".
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
		// R228-ARCH-1: fall back to the package-level default rather than a
		// floating literal so a bump in platform.DefaultMaxReplyLen is
		// picked up here automatically.
		maxLen = platform.DefaultMaxReplyLen
	}

	chunks := platform.SplitText(text, maxLen)
	total := len(chunks)
	for i, chunk := range chunks {
		if total > 1 {
			// R20260526-PERF-005: per-chunk on every multi-chunk reply,
			// strconv.Itoa avoids fmt.Sprintf's per-call alloc/format.
			chunk += "\n— [" + strconv.Itoa(i+1) + "/" + strconv.Itoa(total) + "]"
		}
		if _, err := platform.ReplyWithRetry(ctx, p, platform.OutgoingMessage{ChatID: chatID, Text: chunk}, platformReplyMaxAttempts); err != nil {
			d.sendFailCount.Add(1)
			dispatchSendFailTotal.Add(1)
			slog.Error("reply chunk failed after retries", "chat", chatID, "chunk", i+1, "err", err)
		} else {
			d.markReplySuccess()
		}
	}
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
		rctx, cancel := context.WithTimeout(context.Background(), platformReplyTimeout)
		defer cancel()

		if sender, ok := platform.AsQuestionCardSender(p); ok {
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
	rctx, cancel := context.WithTimeout(context.Background(), platformReplyTimeout)
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
