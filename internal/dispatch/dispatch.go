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

// platformReplyMaxAttempts moved to internal/limits.PlatformReplyMaxAttempts
// (R20260527-ARCH-8) so internal/cron's notifyTarget path and internal/dispatch's
// reply paths share one source-of-truth constant instead of mirrored copies.

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
//
// R250-ARCH-7 (#1170): rediscovered the same collapse argument with new
// framing — "MessageQueue already exposes SessionGuard's surface plus
// more, the interface is a strict subset". Reaffirmed: the collapse
// proposal still ignores fakeGuard's role. The structural-subset claim
// is true but conflates implementation surface with consumer surface;
// dispatch consumes 3 methods, while MessageQueue's full API
// (Enqueue/DoneOrDrain/Discard/Cleanup/Depth/...) is irrelevant to
// dispatch's busy-gating contract. Documenting the rejection here so
// the next rediscovery has a single durable rebuttal to point at.
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
	// scheduler is the cron-side consumer surface dispatch slash-commands
	// need (CronCommands, cron_consumer.go). Production wiring passes the
	// server-side cronDispatchAdapter; tests inject a fake without
	// constructing a real Scheduler + tempdir. R250-ARCH-17 (#1178),
	// projection-typed since R250-ARCH-1 (#1164). nil when cron is
	// disabled at the operator level — every call site already gates on
	// `d.scheduler != nil`, preserving the no-cron-feature exit path.
	// (See dispatchCommand in commands.go for the gate.)
	scheduler CronCommands
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
	//
	// ARCH-DISP-1 (#457): typed as the ProjectStore consumer interface
	// (consumer.go) rather than *project.Manager so slash-command handler
	// tests can inject a fake binding store. *project.Manager satisfies it
	// implicitly; NewDispatcher assigns cfg.ProjectMgr directly. nil when
	// projects.root is unconfigured — every handler gates on
	// `d.projectMgr == nil` first (see handleProjectCommand).
	projectMgr ProjectStore
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

	// stopCtx is the long-lived process-shutdown signal context. The
	// passthrough send branch detaches the per-webhook ctx (handlers
	// return in seconds while LLM turns take minutes) but must still
	// observe SIGTERM-driven graceful shutdown — without this binding the
	// detached goroutine has no path to abort early during shutdown and
	// only stops on its internal totalTimeout (5min). NewDispatcher seeds
	// stopCtx from cfg.StopCtx (or context.Background() if nil) so call
	// sites can dereference unconditionally. (#1320)
	stopCtx context.Context

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
	// Scheduler is the cron consumer surface (CronCommands). Production
	// wiring passes the server-side cronDispatchAdapter; test wiring may
	// inject a fake. nil disables /cron commands at runtime.
	// R250-ARCH-17 (#1178), projection-typed since R250-ARCH-1 (#1164).
	Scheduler  CronCommands
	ProjectMgr *project.Manager
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

	// StopCtx is the long-lived process-shutdown signal context. Passed
	// into Dispatcher so the passthrough goroutine launched per inbound
	// message can observe SIGTERM-driven graceful shutdown rather than
	// living its full totalTimeout independent of process lifecycle.
	// Optional — when nil, NewDispatcher falls back to context.Background()
	// (preserving the legacy never-cancels behaviour for headless / test
	// wiring). Production wiring (server.Start) passes the long-lived
	// service ctx so the passthrough send aborts on shutdown. (#1320)
	StopCtx context.Context

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

// resolveOrFabricateKeyResolver returns the live KeyResolver Dispatcher
// must hold. Precedence (single track — drift here = bug, no inline copy
// elsewhere is permitted; see #543 R215-CR-P2-3):
//
//  1. cfg.Resolver — explicit caller-supplied singleton.
//  2. cfg.Router.Resolver() — the Router-attached singleton from
//     session.RouterConfig.Resolver (R237-ARCH-12 / #604) so Dispatcher /
//     Hub / upstream see the same agents-config snapshot.
//  3. Fabricate a fresh resolver from cfg.Agents and a project data
//     source derived from cfg.ProjectMgr (nil-safe — NewKeyResolver and
//     project.NewDataSource both accept nil inputs).
//
// All three branches return a non-nil *KeyResolver, so call sites
// downstream of NewDispatcher can dereference d.resolver without a guard.
// Adding a fourth branch (or copying this fallback chain into a
// caller) is the legacy-double-track failure mode this helper exists
// to prevent.
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
	resolver := resolveOrFabricateKeyResolver(cfg)
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
	// Defend against the Go typed-nil-interface trap: a caller that boxes
	// a nil concrete pointer into the CronCommands interface produces a
	// value that is not == nil. Every slash-command gate uses
	// `d.scheduler != nil`, so we collapse the typed-nil here exactly
	// once. R250-ARCH-17 (#1178). Production now passes a struct adapter
	// value (#1164) so the pointer case is defensive, not load-bearing.
	scheduler := cfg.Scheduler
	if scheduler != nil {
		v := reflect.ValueOf(scheduler)
		if v.Kind() == reflect.Pointer && v.IsNil() {
			scheduler = nil
		}
	}
	// ARCH-DISP-1 (#457): same typed-nil-interface trap for ProjectStore.
	// cfg.ProjectMgr is a concrete *project.Manager; a nil pointer boxed
	// into the ProjectStore field is != nil, which would defeat every
	// `d.projectMgr == nil` gate (handleProjectCommand / /cd / /new).
	// Collapse it to a true nil interface here, mirroring scheduler above.
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
	// stopCtx defaults to context.Background() so headless / test wiring
	// that omits cfg.StopCtx behaves like the legacy WithoutCancel branch
	// (never cancels). Production wiring (server.Start) passes the long-
	// lived service ctx so the passthrough goroutine aborts on shutdown.
	// (#1320)
	if cfg.StopCtx != nil {
		d.stopCtx = cfg.StopCtx
	} else {
		d.stopCtx = context.Background()
	}
	return d, nil
}

// fallbackDedupKey builds a composite dedup key for messages whose
// adapter left EventID empty. The shape "fallback:<platform>:<chatID>:
// <messageID>:<unixMinute>" gives platform retries within the same
// minute a stable identity so platform.Dedup.Seen short-circuits them
// rather than passing through (Seen("") returns false, never records).
//
// The "fallback:" prefix segregates the fallback namespace from real
// EventIDs so a legitimate EventID that happens to look like a colon-
// joined tuple cannot collide with a fallback. now is plumbed in so
// tests can drive deterministic minute boundaries. (#1310)
func fallbackDedupKey(msg platform.IncomingMessage, now time.Time) string {
	return "fallback:" + msg.Platform + ":" + msg.ChatID + ":" + msg.MessageID + ":" + strconv.FormatInt(now.Unix()/60, 10)
}

// preparedInbound carries the resolved per-message state produced by
// prepareInbound and consumed by the dispatch-strategy tail of BuildHandler.
// R20260531A-ARCH-3 (#1527): bundling these into one value keeps the front-
// matter extraction (dedup → group-gate → command → agent-resolve → key/opts
// → image-convert) in a single named helper without widening the strategy
// switch's parameter list.
type preparedInbound struct {
	lg        *slog.Logger
	agentID   string
	cleanText string
	key       string
	opts      session.AgentOpts
	images    []cli.ImageData
}

// prepareInbound runs the message front-matter common to every dispatch
// strategy: dedup, group-mention gate, log-attr sanitisation, slash-command
// dispatch, agent resolution, unknown-command echo, accounting, key/opts
// resolution, and platform→CLI image conversion. It returns (prepared, true)
// when the caller should proceed to a dispatch strategy, or (_, false) when the
// message was fully handled / dropped here (dedup hit, gated, command consumed,
// empty body, unknown command). Extracted verbatim from BuildHandler
// (R20260531A-ARCH-3 / #1527) — behaviour-preserving.
func (d *Dispatcher) prepareInbound(ctx context.Context, msg platform.IncomingMessage) (preparedInbound, bool) {
	// Dedup check at the top prevents duplicate processing from platform
	// retries (e.g., Feishu webhook timeout → re-delivery with same event_id).
	// Note: if guard fails below, the eventID is still consumed. This means
	// a platform retry during guard contention won't be re-processed. In
	// practice this is benign — the handler responds fast enough that
	// platforms don't retry, and the user is told to resend.
	//
	// Empty EventID fallback (#1310): some adapters (older Feishu webhook
	// shapes, raw HTTP test clients) leave EventID empty. platform.Dedup.Seen
	// treats "" as "not seen" and never records — meaning a platform retry
	// of the same message_id would call BuildHandler N times: token double-
	// charge, queue noise ("正在处理上一条消息"), LLM N-fold dispatch. Build
	// a composite fallback key from (Platform, ChatID, MessageID, minute-
	// bucketed wall clock). The minute bucket bounds collision risk for the
	// degenerate "no MessageID either" case to a single replay window.
	dedupID := msg.EventID
	if dedupID == "" {
		dedupID = fallbackDedupKey(msg, time.Now())
	}
	if d.dedup.Seen(dedupID) {
		return preparedInbound{}, false
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
		return preparedInbound{}, false
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
		return preparedInbound{}, false
	}

	// Resolve agent from command prefix (e.g. "/review code" -> agent=code-reviewer, text="code")
	agentID, cleanText := session.ResolveAgent(trimmed, d.agentCommands)
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
			// R20260527122801-CR-15: sanitize the user-controlled cmd
			// before echoing — IM renderers may interpret embedded ANSI
			// escape sequences or control bytes as formatting / injected
			// log fields. SanitizeForLog scrubs C0/C1, DEL, and the
			// bidi/LS-PS rune classes; the cap also bounds reply size
			// against an attacker stuffing a 4KB "/" prefix into chat.
			safeCmd := osutil.SanitizeForLog(cmd, 64)
			d.replyText(ctx, msg, "未知命令: "+safeCmd+"\n输入 /help 查看可用命令，或直接发送消息。", lg)
			return preparedInbound{}, false
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
	if len(msg.Images) > 0 {
		images = make([]cli.ImageData, 0, len(msg.Images))
		for _, img := range msg.Images {
			images = append(images, cli.ImageData{Data: img.Data, MimeType: img.MimeType})
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
// Extracted from BuildHandler (R20260531A-ARCH-3 / #1527) — behaviour-
// preserving. shouldInterrupt / enqueued / evictedID come from queue.Enqueue.
// A non-empty evictedID means this enqueue dropped the oldest queued message
// for the key (queue-full backpressure); its dangling HOURGLASS reaction is
// cleared so the evicted user is not left with a permanent "still queued"
// indicator (#1945).
func (d *Dispatcher) handleQueuedNonOwner(ctx context.Context, msg platform.IncomingMessage, p preparedInbound, shouldInterrupt, enqueued bool, evictedID string) {
	lg, key := p.lg, p.key
	// #1945: an evicted message's queued reaction must be removed — it never
	// enters a DoneOrDrain batch, so ownerLoop's clearQueuedReactions never
	// touches it. Without this its HOURGLASS hangs until the platform reaction
	// cache TTL (feishu: 12h) GCs it, falsely telling the dropped user "still
	// queued". The evicted message shares this inbound msg.Platform (same chat).
	if evictedID != "" {
		d.clearQueuedReaction(ctx, msg.Platform, evictedID, lg)
	}
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
			// leaking slots into the 5.5-min bail timer.
			//
			// R20260527122801-CR-6 (#1320): the original context.WithoutCancel
			// dropped the cancellation source entirely — including the long-
			// lived service ctx whose cancel signals graceful shutdown. The
			// passthrough goroutine therefore had no path to abort on
			// SIGTERM and only stopped on its internal totalTimeout (5min),
			// pushing systemd TimeoutStopSec breaches at restart. Instead
			// merge stopCtx (cancel source) with the webhook ctx (values
			// source) so log attrs survive while shutdown still aborts the
			// send.
			sendCtx := mergeStopAndValues(d.stopCtx, ctx)
			// Ack arrival BEFORE spawning the turn goroutine, matching the
			// /urgent path (commands.go: ack then goSendAndReply). The ack's
			// AddReaction stores the reaction_id synchronously; the goroutine's
			// defer clearQueuedReaction (goSendAndReply :1067) is the only clear
			// on this detached path. R20260608-133914-LB-3 (#1963): with the
			// pre-fix order (spawn then ack) a fast-fail turn (GetOrCreate/Send
			// early-return) could run the goroutine's clear — a LoadAndDelete
			// no-op against the not-yet-stored reaction — before this ack landed
			// its AddReaction, leaving a permanent HOURGLASS that no later clear
			// removes. Acking first establishes the AddReaction-then-clear order.
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

// discardQueue is a nil-safe helper to clear queued messages for a key.
// In passthrough mode it also fires ErrSessionReset to any in-flight
// SendPassthrough callers so the IM user sees the turn as cancelled rather
// than silently hanging.
func (d *Dispatcher) discardQueue(key string) {
	if d.queue != nil {
		d.queue.Discard(key)
	}
	if d.router != nil {
		d.router.DiscardPassthroughPending(key, cli.ErrSessionReset)
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
		// R247-ARCH-10 (#632): NotifyCtx centralises the "detach from
		// parent because the turn ctx is already Done" pattern.
		notifyCtx, cancel := NotifyCtx(nil, NotifyKindOwnerLoopPanic, platformReplyTimeout)
		defer cancel()
		d.replyText(notifyCtx, msg, "处理异常，请稍后重试。", nil)
	}()
}

// goSendAndReply spawns sendAndReply in its own goroutine with a deferred
// panic recover. The passthrough and /urgent paths detach each inbound
// message into a bare goroutine; without recover, a panic anywhere in
// sendAndReply (platform SDK, CLI wrapper, reply path) would crash the whole
// process rather than failing just that one turn. #1773. We reuse
// handleOwnerLoopPanic so the detached path gets the same log + queue-discard
// + "请稍后重试" reply behaviour as the ownerLoop recover.
func (d *Dispatcher) goSendAndReply(
	ctx context.Context,
	key, text string,
	images []cli.ImageData,
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
		// #1946: clear the HOURGLASS the passthrough / /urgent ack added once
		// this turn finishes. The detached goroutine never enters ownerLoop's
		// drain loop (the only other caller of the reaction-clear path), so
		// without this the queued reaction hangs until the platform's reaction
		// cache TTL GCs it (feishu: 12h), falsely showing "still processing".
		// WithoutCancel: the turn's ctx often carries a per-turn deadline that
		// has elapsed by reply time, and on shutdown it is Canceled — either
		// would make RemoveReaction fail fast. Strip cancellation/deadline but
		// keep request-scoped values; clearQueuedReaction re-bounds with its
		// own reactionAckTimeout.
		defer d.clearQueuedReaction(context.WithoutCancel(ctx), msg.Platform, msg.MessageID, lg)
		d.sendAndReply(ctx, key, text, images, agentID, opts, msg, lg, isFirst)
	}()
}

// resolveReplyCtx returns a context safe for an end-of-turn reply: when
// ctx is already Done because of a shutdown-style cancellation
// (context.Canceled), it returns a fresh NotifyCtx with the
// shutdownReplyTimeout budget so the platform.Reply call can actually
// land. Otherwise it returns ctx unchanged with a no-op cleanup.
//
// Centralising this swap removes the per-branch shutdown-ctx replacement
// the previous sendAndReply / handleGetOrCreateError variants each
// re-implemented (R242-GO-4, #550). Callers MUST defer cleanup() before
// dispatching the reply or leak the timer goroutine on the swap path.
//
// Only context.Canceled triggers the swap. context.DeadlineExceeded is
// treated as a legitimate per-turn timeout the caller asked for and is
// not auto-extended — that would be silently lengthening a configured
// budget. Pure ctx.Err() == nil short-circuits to the cheap "no swap"
// branch so the helper costs nothing on the happy path.
func resolveReplyCtx(ctx context.Context) (replyCtx context.Context, cleanup func()) {
	if ctx == nil {
		// nil parent: caller already lost its turn ctx. Mint a fresh
		// shutdown-budget ctx so the reply can still land.
		return NotifyCtx(nil, NotifyKindShutdown, shutdownReplyTimeout)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		// Happy path: cleanup is nil so callers using the
		// `if cleanup != nil { defer cleanup() }` idiom skip the defer.
		return ctx, nil
	}
	notifyCtx, cancel := NotifyCtx(ctx, NotifyKindShutdown, shutdownReplyTimeout)
	return notifyCtx, cancel
}

// handleGetOrCreateError maps a router.GetOrCreate failure into the
// user-facing reply ctx, an optional ctx.cancel cleanup, and a Chinese
// error message. Pulled out of sendAndReply so the seven-stage main
// path stays focused on the happy-path turn flow (R245-ARCH-39 / #894).
// The sentinel → Chinese-message mapping is delegated to
// usermsg.ForSendError (R20260531-ARCH-1) so it cannot drift from the
// WS send_ack path; this helper only owns the log-level and reply-ctx
// (shutdown-swap) policy, which unit tests can target directly without
// standing up a full Dispatcher.
//
// cleanup is non-nil when this returns a fresh background ctx (the
// shutdown / context.Canceled branch). Callers MUST defer it before
// using replyCtx, or the timeout goroutine leaks.
//
// Error logging policy: shutdown-path cancellation is expected noise on
// every restart and downgrades to Info so ops dashboards don't light up;
// every other GetOrCreate failure stays at Error.
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
	// R20260531-ARCH-1 (#754 follow-up): the per-sentinel switch here
	// duplicated usermsg.ForSendError's mapping for ErrMaxProcs /
	// ErrMaxExemptSessions / ErrNoCLIWrapper / context.Canceled, drifting
	// from it over time. Delegate to the shared classifier so a new
	// session-side sentinel only needs registering once (in usermsg).
	// The empty key keeps the regular (non-cron) phrasing; GetOrCreate
	// failures are not the cron-namespace ErrNoActiveProcess case that
	// would want the key. Unknown errors fall through to ForSendError's
	// generic "/new 重置" hint (was a near-identical literal here).
	errMsg = usermsg.ForSendError(err, "")
	// R242-GO-4 (#550): the shutdown-ctx swap is one helper, not a
	// per-branch repeat. resolveReplyCtx returns ctx unchanged when no
	// swap is needed (cheap), and a fresh NotifyCtx + cancel when ctx
	// was canceled — without it the user-facing reply would silently
	// drop at the platform layer (R188-CONC-M1).
	replyCtx, cleanup = resolveReplyCtx(ctx)
	return replyCtx, cleanup, errMsg
}

// handleSendError maps a Capabilities.Send failure into the user-facing
// error reply, watchdog counter bumps, and metrics increments. Pulled
// out of sendAndReply so the seven-stage main path stays focused on the
// happy turn flow (R237-GO-4 / #624). The per-sentinel switch is the
// part most likely to grow as new send-side failure modes are added
// (e.g. backend disabled, model deprecated) and now has a single home
// that unit tests can target without standing up a full Dispatcher.
//
// Caller has already entered the err != nil branch and consumed the
// Send result; this method does NOT signal whether a reply was actually
// delivered — failure to land the error reply is logged at Warn but
// not surfaced.
func (d *Dispatcher) handleSendError(
	ctx context.Context,
	err error,
	key string,
	msg platform.IncomingMessage,
	p platform.Platform,
	lg *slog.Logger,
) {
	// /clear early-return mirrors the prior behaviour: the user just
	// triggered the reset, so we suppress the extra "会话已重置" reply.
	// R260528-GO-2: ErrSessionReset is a control-flow signal from the
	// user (/new /clear), not an error — bail before bumping the
	// /health error counters so idle sessions don't pollute reply-error
	// metrics.
	if errors.Is(err, cli.ErrSessionReset) {
		return
	}
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
	// R242-GO-4 (#550): on shutdown the inbound ctx is already Done; the
	// user-facing error reply must still land. resolveReplyCtx swaps in a
	// fresh NotifyCtx with the shutdown budget when applicable; otherwise
	// returns ctx unchanged with nil cleanup. Identical to the swap done
	// in handleGetOrCreateError so the two error-reply paths share one
	// truth.
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

	tracker := newIMEventTracker(ctx, p, msg.ChatID, msg.ChatType)
	defer tracker.stop()

	result, err := d.caps.Send(ctx, key, sess, text, images, tracker.onEvent)
	if err != nil {
		d.handleSendError(ctx, err, key, msg, p, lg)
		return
	}

	lg.Info("message replied", "result_len", len(result.Text), "cost", result.CostUSD,
		"merged_count", result.MergedCount, "merged_with_head", result.MergedWithHead)

	// Passthrough merge fan-out: follower slots get MergedCount>1 and an
	// empty Text. The head slot for the merge group delivered the full
	// reply on its own bubble; followers should surface a short "合并" hint
	// on the user's original message instead of echoing the same text again.
	if result.MergedCount > 1 && result.Text == "" {
		d.ackMergedFollower(ctx, msg, key, result.MergedCount, lg)
		d.markReplySuccess()
		return
	}

	// Record turn success regardless of reply text length. A successful
	// sendFn with empty result (e.g. a turn that only produces tool calls
	// or whose text was stripped) still constitutes a healthy end-to-end
	// roundtrip; gating markReplySuccess on non-empty text previously made
	// /health's lastReplySuccess go stale on otherwise-healthy sessions.
	d.markReplySuccess()

	// R219-CR-7 (#656): decorateReplyText folds the localize step, the
	// merge-group chip, and the per-session ReplyFooter into one helper so
	// sendAndReply isn't a 240-line linear stack of post-processing
	// passes. Pure function on (result, sess) — easy to unit-test without
	// spinning up a Dispatcher / Router / platform.
	replyText := d.decorateReplyText(result, sess)
	var outImages []platform.Image
	imagePaths := cli.ExtractImagePaths(replyText)
	if len(imagePaths) > 0 {
		// R112714-PERF-8: use strings.ReplaceAll loop instead of
		// strings.NewReplacer. ExtractImagePaths returns at most a handful of
		// paths per reply (typically 1-2), so the per-call trie allocation in
		// strings.NewReplacer costs more than N simple verbatim scans. Each
		// path is always replaced (even when ReadFile fails) so user-visible
		// behaviour is unchanged: every extracted path becomes "[图片]".
		for _, path := range imagePaths {
			data, err := d.imageReader.ReadFile(path)
			if err == nil {
				outImages = append(outImages, platform.Image{Data: data, MimeType: cli.MimeFromPath(path)})
			}
			replyText = strings.ReplaceAll(replyText, path, "[图片]")
		}
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

	// #1959: outImages is derived from replyText (same source). When
	// askQuestionFired suppresses replyText, its associated images must
	// also be suppressed — otherwise orphaned /tmp image bubbles are sent
	// after the AskUserQuestion card, confusing the user.
	if !tracker.askQuestionFired.Load() {
		for _, img := range outImages {
			if _, err := p.Reply(ctx, platform.OutgoingMessage{
				ChatID: msg.ChatID,
				Images: []platform.Image{img},
			}); err != nil {
				slog.Warn("send image failed", "err", err)
			}
		}
	}
}

// decorateReplyText post-processes the raw CLI result text for IM
// delivery: localises Anthropic API errors to Chinese, appends the
// merge-group chip when the head slot covers N messages, and appends
// the per-session ReplyFooter (resolved from sess.Backend(), or the
// router default when sess is nil — a cron edge case where the session
// has been pruned but the reply path still fires).
//
// Extracted from sendAndReply for R219-CR-7 (#656). Keeping this as a
// method on *Dispatcher (rather than a free function) gives the helper
// access to d.caps.ReplyFooter without pushing the Capabilities
// dependency through a parameter list.
//
// Returns the empty string when the input result.Text is empty AND no
// footer applies — callers typically gate on `replyText != ""` before
// dispatching to the platform, so an empty return is the existing
// "nothing to send" sentinel.
func (d *Dispatcher) decorateReplyText(result *cli.SendResult, sess *session.ManagedSession) string {
	// R103901-CODE-1: scrub well-known credential token shapes (sk-ant-, ghp_,
	// AKIA, …) BEFORE localising the API error, mirroring the cron notify
	// path (scheduler_run.go: sanitise → localize, privacy-first ordering).
	// Without this a Claude reply that echoes a plaintext token would land
	// verbatim on the IM channel.
	// R20260602-091302-ARCH-1 (#1571): redactor now lives in the leaf package
	// internal/textutil; dispatch no longer couples this security-critical
	// path to the cron domain package for scrubbing.
	replyText := localizeAPIError(textutil.RedactSecrets(result.Text))
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
	var backendID string
	if sess != nil {
		backendID = sess.Backend()
	}
	// #1985: guard on replyText != "" (mirroring the merge-chip branch above)
	// so an empty-text turn returns the empty "nothing to send" sentinel
	// instead of an orphan "— cc" footer bubble. Without this guard a healthy
	// empty result (e.g. error_max_turns) plus the default footer would emit a
	// lone footer message to the IM channel.
	if footer := d.caps.ReplyFooter(backendID); footer != "" && replyText != "" {
		replyText += "\n\n— " + footer
	}
	return replyText
}

// pageSuffixRuneWidth returns the rune width of the worst-case page suffix
// "\n— [i/total]" for a reply split into total chunks. i can be at most
// total, so the widest "i" has the same digit count as total. The fixed
// glyphs are: '\n' + '—' + ' ' + '[' + '/' + ']' = 6 runes (the em dash is a
// single rune); plus the two numbers. #2008.
func pageSuffixRuneWidth(total int) int {
	if total < 1 {
		total = 1
	}
	digits := len(strconv.Itoa(total))
	return 6 + 2*digits
}

// upperBoundChunks returns a ceiling estimate of how many chunks SplitText
// would produce for runeCount runes at the given split width. It never
// under-estimates (SplitText may produce fewer when it breaks early at a
// newline, but never more than ceil), so reserving the suffix budget for
// this count is always safe. #2008.
func upperBoundChunks(runeCount, splitWidth int) int {
	if splitWidth <= 0 {
		return runeCount + 1
	}
	return (runeCount + splitWidth - 1) / splitWidth
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

	// #2008: when the reply needs splitting we append a "\n— [i/N]" page
	// suffix to each chunk. Splitting at the raw platform limit yields a
	// full chunk of exactly maxLen runes; the suffix then pushes it to
	// maxLen+len(suffix), which on platforms with a hard API ceiling (e.g.
	// Discord, MaxReplyLen=2000 with zero headroom) is rejected outright
	// (400 BASE_TYPE_MAX_LENGTH) rather than truncated — and ReplyWithRetry
	// blindly re-sends the same oversized payload, losing the chunk. Reserve
	// room for the worst-case suffix before splitting so every emitted
	// message stays within the platform limit.
	//
	// The suffix width depends on the digit count of total, which in turn
	// depends on the split width — a mild circularity. We break it with a
	// conservative upper bound on the chunk count (rune length / split
	// width, rounded up) computed at the reduced width, then reserve for the
	// worst-case suffix of that count. Reserving at the upper-bound count can
	// only over-reserve, never under-reserve, so every emitted chunk stays
	// within maxLen.
	splitLen := maxLen
	if runeCount := utf8.RuneCountInString(text); runeCount > maxLen {
		// First-pass reservation assuming a 1-digit count, then widen the
		// reservation to the worst-case suffix for the resulting chunk count.
		reserved := maxLen - pageSuffixRuneWidth(upperBoundChunks(runeCount, maxLen-pageSuffixRuneWidth(1)))
		if reserved > 0 {
			splitLen = reserved
		}
	}

	chunks := platform.SplitText(text, splitLen)
	total := len(chunks)
	for i, chunk := range chunks {
		if total > 1 {
			// R20260526-PERF-005: per-chunk on every multi-chunk reply,
			// strconv.Itoa avoids fmt.Sprintf's per-call alloc/format.
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
