package session

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/history"
)

const (
	maxPersistedHistory = 500

	// maxPrevSessionIDs caps the session chain length so long-lived chats
	// don't grow storeEntry.PrevSessionIDs without bound (each "/new" or
	// workspace switch appends one). 32 retains enough chain for multi-day
	// context recovery while keeping sessions.json size bounded.
	maxPrevSessionIDs = 32
)

// ProcessSender is the send-path facet of processIface — the first
// incremental split of the long-flagged god-interface (R215-ARCH-P1-3 /
// R219-ARCH-7 / R224-ARCH-5 / R230C-ARCH-4 / R242-GO-12 [REPEAT-5]). It
// covers Send / SendPassthrough / Interrupt / passthrough lifecycle —
// the methods a caller needs to deliver one user turn and unwind it on
// abort. Lifecycle (Alive/Close/Kill) and event/introspection methods
// stay on the wider processIface for now and will be split into
// ProcessLifecycle / EventSource / Introspection facets in subsequent
// rounds. Defined as an exported type-level contract so downstream
// callers (cron, dispatch, upstream) can declare a narrower dependency
// over time without forcing a single-cut refactor.
//
// processIface embeds ProcessSender, so any concrete implementation of
// processIface (only *cli.Process in production; testutil.TestProcess
// in tests) automatically satisfies ProcessSender — no additional
// adapter required.
//
// Cross-ref: R242-ARCH-4 catalogues the planned full split into
// ProcessLifecycle / EventSource / ProcessSender / Introspection.
type ProcessSender interface {
	// Send delivers a user turn and streams events through onEvent
	// until the result entry arrives. Single-shot; serialised by
	// caller-side mutex (sendMu in ManagedSession).
	Send(ctx context.Context, text string, images []cli.ImageData, onEvent cli.EventCallback) (*cli.SendResult, error)
	// SendPassthrough is the passthrough-mode Send. Callers must
	// ensure the underlying protocol reports SupportsReplay()==true;
	// otherwise this returns an error. Unlike Send, multiple
	// goroutines may call this concurrently on the same process —
	// ordering is handled by the CLI's internal commandQueue plus a
	// naozhi-side sendSlot FIFO.
	SendPassthrough(ctx context.Context, text string, images []cli.ImageData, onEvent cli.EventCallback, priority string) (*cli.SendResult, error)
	// SupportsPassthrough reports whether this process's protocol
	// can operate in passthrough mode (i.e. Protocol.SupportsReplay()).
	// Dispatch uses this to fall back to legacy Send when the protocol
	// can't provide the replay events passthrough matching relies on.
	SupportsPassthrough() bool
	// PassthroughDepth returns the current pending-slot count for
	// dashboard / status display.
	PassthroughDepth() int
	// DiscardPassthroughPending cancels all in-flight passthrough
	// sends and fires the given error to each caller. Used on /new,
	// /clear, or forced session reset.
	DiscardPassthroughPending(reason error)
	// Interrupt requests OS-signal-style abort of the active turn.
	// Best-effort: protocol may not honour SIGINT.
	Interrupt()
	// InterruptViaControl asks the CLI to abort the active turn via
	// an in-band stream-json control_request (no SIGINT, no process
	// kill). Returns cli.ErrInterruptUnsupported for protocols
	// without this primitive.
	InterruptViaControl() error
}

// ProcessEventReader is the second facet of the planned processIface split
// (R242-ARCH-4 / R20260527122801-ARCH-10, #1319). It exposes the read-side
// of the in-process EventLog ring — the methods Snapshot / dashboard
// pagination / cron history fan-out / discovery already consume but which
// are presently entangled with lifecycle / identity / metering on the wider
// processIface god-interface.
//
// Pure additive split. processIface embeds ProcessEventReader so the
// concrete *cli.Process implementation and testutil.TestProcess fakes keep
// satisfying both interfaces unchanged. Callers ready to narrow can switch
// from processIface (~25 methods) to ProcessEventReader (~5) one site at
// a time.
type ProcessEventReader interface {
	// EventEntries returns a defensive copy of every entry currently in
	// the live EventLog ring (chronological order). Used by the dashboard
	// "full history" path before pagination cut over to EventEntriesBefore.
	EventEntries() []cli.EventEntry
	// EventLastN returns the last N entries (chronological). Used by IM
	// reply rendering to assemble the trailing thinking / tool_use chain.
	EventLastN(n int) []cli.EventEntry
	// EventEntriesSince returns entries with Time > afterMS, used by the
	// dashboard 1Hz incremental poll path.
	EventEntriesSince(afterMS int64) []cli.EventEntry
	// EventEntriesBefore returns up to `limit` entries with Time < beforeMS
	// drawn from the live ring (chronological). Used by the dashboard
	// pagination handler when a tab scrolls back past the in-memory tail.
	EventEntriesBefore(beforeMS int64, limit int) []cli.EventEntry
	// LastEventAt returns the wall-clock time of the most recent live event
	// appended to the EventLog, or zero when nothing has arrived yet.
	LastEventAt() time.Time
}

// ProcessLifecycle is the third facet of the planned processIface split
// (R176-ARCH-M2 / R214-ARCH-8, #430), following the ProcessSender and
// ProcessEventReader precedent. It exposes the liveness + teardown subset
// that Router.Cleanup, evictOldest, Remove/Reset and the shim reconciler
// consume — the parts entangled with event-read / identity / metering on
// the wider processIface god-interface.
//
// Pure additive split. processIface embeds ProcessLifecycle so the concrete
// *cli.Process implementation and testutil.TestProcess fakes keep satisfying
// both interfaces unchanged. Callers ready to narrow can switch from
// processIface (~25 methods) to ProcessLifecycle (4) one site at a time —
// e.g. the ACP/Gemini backend onboarding the issue is gated on no longer
// needs to mock the full god-interface just to prove liveness behaviour.
type ProcessLifecycle interface {
	// Alive reports whether the underlying CLI/shim process handle is still
	// usable (not Closed/Killed). Lock-free poll.
	Alive() bool
	// IsRunning reports whether a turn is actively streaming. Distinct from
	// Alive: a session can be Alive (process up) but not Running (idle).
	IsRunning() bool
	// Close releases the process handle gracefully (drains the shim socket
	// on the normal path). Idempotent.
	Close()
	// Kill force-terminates the process. Used by the stuck-session watchdog
	// when Close's graceful path cannot reclaim the slot.
	Kill()
}

// processIface abstracts the CLI process lifecycle methods used across the
// session-aware code paths. Despite the package name, callers extend beyond
// internal/session itself: internal/server (Hub broadcast + dashboard
// snapshots), internal/dispatch (turn coalescing), internal/cron (cron Send
// with ack), and internal/upstream (reverse-node RPC) all reach a Process
// through ManagedSession.loadProcess() and consume this interface. Keep new
// methods minimal — the surface has already been flagged as a god-interface
// (R215-ARCH-P1-3 / R219-ARCH-7 / R224-ARCH-5) pending a future split into
// ProcessLifecycle / EventSource / ProcessSender facets.
//
// First facet split landed in R242-GO-12: ProcessSender covers the
// send-path subset (Send / SendPassthrough / Interrupt / passthrough
// lifecycle). processIface embeds it so existing call sites keep working
// untouched while new narrow consumers can depend on ProcessSender alone.
//
// *cli.Process is the only production implementation; testutil.TestProcess is
// the test fake.
// R230-CQ-16.
type processIface interface {
	// Send / SendPassthrough / Interrupt / InterruptViaControl /
	// PassthroughDepth / SupportsPassthrough / DiscardPassthroughPending
	// live on ProcessSender (R242-GO-12, first facet split). Embed it
	// rather than duplicate.
	ProcessSender
	// Alive / IsRunning / Close / Kill live on ProcessLifecycle (R176-ARCH-M2
	// facet split, #430). Embed it rather than duplicate.
	ProcessLifecycle
	// Dashboard introspection.
	//
	// GetSessionID / GetState are flagged as unidiomatic by R219-CR-9
	// (#665) — Go convention drops the `Get` prefix on accessors. The
	// rename to SessionID() / State() is breaking (~12 callsites + the
	// fakeProcess / TestProcess fakes + cli.Process itself) and is
	// tracked for a coordinated cross-package change. Until it lands the
	// names are pinned by TestProcessIfaceGetterRenamePlanned so a
	// piecemeal rename of just one side cannot slip through unnoticed.
	GetSessionID() string
	GetState() cli.ProcessState
	// DeathReason returns the process-level reason string recorded when the
	// shim-backed CLI exited (passive death). Empty while alive or when the
	// reason has not been classified yet.
	DeathReason() string
	TotalCost() float64
	EventEntries() []cli.EventEntry
	EventLastN(n int) []cli.EventEntry
	EventEntriesSince(afterMS int64) []cli.EventEntry
	EventEntriesBefore(beforeMS int64, limit int) []cli.EventEntry
	LastActivitySummary() string
	// LastResponseSummary returns the summary of the most recent assistant
	// "text" entry the process's EventLog has seen. Used by Snapshot to feed
	// SessionSnapshot.LastResponse for the sidebar 30-rune dim preview line
	// (R110-P1). Empty until at least one assistant text block has streamed.
	LastResponseSummary() string
	// LastEventAt returns the wall-clock time of the most recent live event
	// appended to the process's EventLog, or zero Time when nothing has
	// arrived yet. Router.Cleanup uses it as a fallback activity signal so
	// a long-running turn that streams tool_use / thinking events is not
	// misclassified as stuck when the session-level lastActive timestamp
	// (only refreshed at Send entry) has aged past the stuck threshold.
	LastEventAt() time.Time
	// UserTurnCount returns the cumulative count of "user" entries the
	// process's EventLog has seen since spawn. Feeds SessionSnapshot.MessageCount.
	UserTurnCount() int64
	ProtocolName() string
	SubscribeEvents() (<-chan struct{}, func())
	PID() int
	InjectHistory(entries []cli.EventEntry)
	TurnAgents() []cli.SubagentInfo
	// Normalize-layer accessors (docs/rfc/multi-backend.md §8.8). kiro fills
	// these from _kiro.dev/metadata; claude leaves zero values until an
	// estimator lands. Lock-free.
	ContextUsagePercent() float64
	TurnDurationMs() int64
	MeteringUsage() []cli.MeteringEntry
	// Model returns the spawn-time CLI model identifier (e.g.
	// "claude-opus-4.7", "claude-sonnet-4.6") or "" when unconfigured.
	// UI Round 5 R5-3.
	Model() string
}

// Compile-time guarantee that ProcessEventReader is a strict subset of
// processIface — every concrete implementation of processIface (production
// *cli.Process, test fakes) automatically satisfies ProcessEventReader so
// callers can narrow without an adapter. Pinning this with a var keeps
// drift from creeping in if either interface evolves. R20260527122801-ARCH-10
// (#1319): future facet rounds (ProcessLifecycle / ProcessIdentity) will
// follow this same pattern — define the narrow facet, prove subset by
// satisfaction var, narrow consumers one site at a time.
var _ ProcessEventReader = (processIface)(nil)

// Compile-time guarantee that ProcessLifecycle is a strict subset of
// processIface (R176-ARCH-M2 facet split, #430). Mirrors the
// ProcessEventReader pin above so a future edit that drops one of
// Alive/IsRunning/Close/Kill from processIface — or changes a signature on
// either side — fails the package build instead of silently desyncing the
// facet from the god-interface.
var _ ProcessLifecycle = (processIface)(nil)

// processBox wraps processIface for use with atomic.Pointer (which
// requires a concrete type).
//
// R242-ARCH-30 considered replacing the per-storeProcess alloc with
// either atomic.Value or a sync.Pool. Both options were rejected after
// analysis:
//
//   - atomic.Value rejects a Store of a different dynamic type than the
//     first non-nil Store (panic on inconsistent type). Production has
//     a single dynamic type (*cli.Process), but tests inject several
//     fakes (TestProcess, hookCloseProc, fakeProcess). atomic.Value
//     would force every test to use the production concrete type, an
//     unreasonable test-fake constraint.
//
//   - sync.Pool would let storeProcess re-use boxes, but loadProcess
//     handlers may retain the *processBox pointer across goroutine
//     boundaries (Hub broadcast, dispatch turn coalescing, cron Send).
//     Putting the box back to the pool while a reader still holds it
//     would be a use-after-free racing the next Get + Store. Tracking
//     the readers' lifetimes requires reference counting we don't have.
//
// storeProcess is only called on cold paths (spawn, attach, kill, /new,
// /clear) — typically once per minute per active session — so the
// 16-byte alloc per call is dwarfed by the surrounding goroutine and
// process-lifecycle work. The wrapper stays.
type processBox struct{ p processIface }

// cancelBox binds a Send()'s context cancel func to the process pointer that
// Send loaded for that turn. Interrupt() consults it so it only fires cancel
// when the live process still matches the one the in-flight Send targets.
//
// SM3 (#381): Send() stores the cancel func before loadProcess(); in the
// narrow window where a concurrent spawnSession replaces the process pointer,
// a bare cancel func could target the previous process's ctx — cancelling it
// is a no-op against the new live process, silently weakening Interrupt. By
// recording the bound process here, Interrupt skips a stale cancel (whose
// proc no longer matches the live process) and reports failure instead of a
// misleading success. nil proc means "not yet bound to a process" (Send has
// stored the box but not reached loadProcess yet) — Interrupt still fires it
// because that turn is about to run on the current live process.
type cancelBox struct {
	cancel context.CancelFunc
	proc   processIface
}

// ManagedSession wraps a claude CLI process with session metadata.
type ManagedSession struct {
	key string

	// sessionID stores the CLI session ID atomically.
	// Written once during first successful Send, read by Snapshot lock-free.
	// atomic.Pointer[string] is type-safe: Load returns *string (nil when never
	// stored, distinct from a stored empty string).
	sessionID atomic.Pointer[string]

	// onSessionID is called when a session ID is first captured from Send().
	// Set by the Router to track known IDs for history exclusion.
	onSessionID func(string)

	// lastActive stores time.UnixNano atomically to avoid data races
	// between Send() (under sendMu) and Cleanup/evictOldest (under r.mu).
	lastActive atomic.Int64

	// createdAt anchors the session's sidebar position. Set once at construction
	// (or carried over via Rename) and never touched again, so neither activity
	// nor process state changes shift the row. Persisted via storeEntry; on
	// load, missing values fall back to LastActive so older sessions retain
	// their relative order across the upgrade.
	createdAt atomic.Int64

	// lastPrompt caches the most recent user message summary (atomic for lock-free Snapshot reads).
	lastPrompt atomic.Pointer[string]

	// lastActivity caches the most recent tool_use/thinking summary.
	lastActivity atomic.Pointer[string]

	// lastResponse caches the most recent assistant text summary so the
	// sidebar can render a 30-rune dim preview line under the prompt
	// (R110-P1). Mirrors lastPrompt's lock-free Snapshot read pattern.
	// Live updates flow from proc.LastResponseSummary; the cache exists for
	// dead/suspended sessions whose process is gone (parallels lastPrompt's
	// extractLastPromptFromProcess seeding path).
	lastResponse atomic.Pointer[string]

	// Cached key parts, parsed once via keyOnce. Key is immutable.
	keyOnce     sync.Once
	keyPlatform string
	keyChatType string
	keyChatID   string
	keyAgentID  string

	process   atomic.Pointer[processBox] // stores *processBox; use loadProcess/storeProcess
	sendMu    sync.Mutex                 // serializes messages to the same session
	historyMu sync.RWMutex               // protects persistedHistory reads/writes (independent of sendMu)
	// sendCancel holds the in-flight Send()'s cancel func bound to the process
	// it targets (see cancelBox). Bound so Interrupt() can skip a cancel whose
	// process has been replaced by a concurrent spawnSession (SM3 / #381).
	sendCancel atomic.Pointer[cancelBox]
	// workspace is the effective cwd at spawn time. Writers hold r.mu in the
	// router (spawnSession / RegisterCronStub / SetWorkspace), but Snapshot()
	// is called from Hub handlers WITHOUT r.mu (see wshub.go:466, 520). Direct
	// string read there races the write — harmless today (word-sized assign),
	// but flagged by -race and future-unsafe if pointee ever grows. Go through
	// atomic.Pointer[string] to match the backend/cliName/cliVersion pattern
	// already established above.
	workspace atomic.Pointer[string]
	// cliIdentity packs backend / cliName / cliVersion into a single
	// atomic.Pointer so Snapshot() (1 Hz × N tabs × N sessions hot path —
	// see R215-ARCH-P2-7 / R219-PERF-3 / R222-PERF-7) reads all three with
	// one atomic.Load instead of three sequential Loads. The fields are
	// written together at every spawn / reconnect / restore site (each
	// triple is "snapshot of the wrapper that owns this session"), so
	// packing them is also closer to the actual invariant. Updaters use
	// updateCLIIdentity (CAS loop) so partial updates from the legacy
	// SetBackend / SetCLIName / SetCLIVersion paths still compose
	// safely. Read paths go through Backend() / CLIName() / CLIVersion()
	// for callers that need only one field, or loadCLIIdentity() for
	// Snapshot which needs all three.
	cliIdentity atomic.Pointer[cliIdentityBox]
	deathReason atomic.Pointer[string] // why process died, empty if alive
	// userLabel is an operator-set display name that overrides summary/last_prompt
	// in the dashboard sidebar and header. Empty = unset, fall back to
	// summary → last_prompt. Lock-free reads from Snapshot() mirror the
	// backend/cliName/cliVersion pattern.
	userLabel atomic.Pointer[string]
	// labelOrigin records who set userLabel: "" / "user" (operator-driven)
	// or "auto" (sysession daemon). The empty/"user" cases are equivalent
	// (legacy compatibility); only "auto" identifies a daemon-written label
	// that may be overwritten by future daemon ticks. Once a human writes
	// (origin="user"), daemons must permanently leave that session alone
	// unless ClearUserLabelOrigin resets it back to "". See
	// docs/rfc/system-session.md §7.3. Read paths are lock-free; writes
	// must go through Router.SetUserLabelWithOrigin so the r.mu-protected
	// re-read closes the daemon-vs-user race window (RFC §11.1).
	labelOrigin atomic.Pointer[string]
	// model is the most-recent CLI model identifier captured from
	// system/init (claude) or SpawnOptions.Model (kiro), persisted to
	// sessions.json so it survives naozhi restart even when the next
	// turn hasn't re-emitted init yet. Live process value (from
	// proc.Model()) wins over this when both are available; this field
	// is the fallback for restart / pre-init windows. UI Round 5 R5-3.
	model atomic.Pointer[string]
	// totalCost is the cumulative cost carried over from a previous process
	// incarnation: written at construction (either in NewRouter() when
	// restoring from store, or in spawnSession() when inheriting from the
	// replaced session) and effectively read-only thereafter. Snapshot()
	// falls back to this value when the live process hasn't yet reported a
	// result event — this avoids the $0.00 flash after resume/reconnect.
	//
	// R183-CONCUR-M2: stored as atomic.Uint64 holding math.Float64bits()
	// pack of the float64 value, mirroring the pattern the other 9 atomic
	// fields use. The struct's lifecycle guarantees saveStore snapshots the
	// *ManagedSession pointer under r.mu before reading cost, but the
	// plain-float64 layout left the type-level contract "implicit sync-only"
	// — any future refactor that adds a post-publication writer (e.g. a
	// live-cost updater) would silently introduce a torn-read race. Making
	// the field atomic at the type level prevents that regression.
	// Read/write via loadTotalCost/storeTotalCost to avoid spreading the
	// math.Float64bits incantation across call sites.
	totalCost atomic.Uint64

	// persistedHistory stores event entries that survive process restarts.
	// Populated by InjectHistory and carried over when the process is replaced.
	persistedHistory []cli.EventEntry

	// persistedSeededLen is the prefix length of persistedHistory that has
	// already been forwarded into the current proc.EventLog. Reset whenever a
	// fresh proc is published, so InjectHistory only forwards the unseeded
	// tail rather than re-injecting the already-seeded prefix. Read/written
	// under historyMu in sync with persistedHistory. See
	// attachProcessAndSnapshotPersisted for the publish/snapshot ordering.
	// R231-CQ-6.
	persistedSeededLen int

	// persistedHistorySorted is true when persistedHistory is known to be
	// sorted ascending by Time. Default false (zero value) keeps the legacy
	// "sort on every read" behaviour for test fixtures that assign the
	// slice directly; production mutations go through InjectHistory which
	// computes monotonicity vs the existing tail and only flips the flag
	// to true once a reader has paid the one-off sort. EventEntriesSince
	// (1Hz dashboard push hot path × N tabs × M dead sessions) checks the
	// flag and skips the stable sort once it lands true. Maintained under
	// historyMu in lockstep with persistedHistory mutations. R237-PERF-12.
	persistedHistorySorted bool

	// prevSessionIDs tracks previous session IDs for this key (oldest → newest).
	// Used on startup to load the full conversation chain from JSONL files.
	// Capped at maxPrevSessionIDs to bound long-lived session memory and
	// sessions.json size. Overflow drops oldest entries; history still loads
	// from the retained tail which carries the most recent context.
	prevSessionIDs []string

	// prevSessionOrigins is parallel to prevSessionIDs and records who
	// added each entry: "manual" (default for legacy / /clear / chain
	// rotation), "auto-spawn" (auto-workspace-chain feature, new spawn
	// path), "auto-backfill" (auto-workspace-chain startup backfill),
	// or "resume" (RegisterForResume). Read/written under historyMu in
	// lockstep with prevSessionIDs.
	//
	// Length contract: len(prevSessionOrigins) <= len(prevSessionIDs).
	// Missing tail entries default to "manual" on read. The append-only
	// invariant on prevSessionIDs (auto-workspace-chain RFC §4.6) lets
	// SetPrevSessionOrigins extend the slice in lockstep without
	// re-deriving positions across the whole chain. A length drift is
	// detected, metric'd, and bounce-rebuilt to all-"manual" rather than
	// allowed to silently misalign origin labels with their session IDs.
	prevSessionOrigins []string

	// exempt marks this session as exempt from TTL cleanup, eviction, and activeCount.
	// Used for planner sessions that should persist indefinitely.
	exempt bool

	// historySource backs EventEntriesBeforeCtx's disk-tier fallback. Set by
	// the router at session construction based on the backend: claude sessions
	// get a claudejsonl.Source; other backends currently get history.Noop so
	// the call site never has to nil-check.
	//
	// Atomic because SetHistorySource is exported and can race with in-flight
	// pagination reads: the router attaches the source before publishing the
	// session to r.sessions, but tests and potential future reconfig paths
	// may reset it after the session is reachable. atomic.Pointer makes the
	// hand-off race-free without requiring historyMu on every read.
	historySource atomic.Pointer[historySourceBox]
}

// historySourceBox wraps history.Source so atomic.Pointer can store it.
// atomic.Pointer[T] requires a concrete type; an interface-typed field
// can't be stored directly.
type historySourceBox struct{ src history.Source }

// SessionKey returns the immutable session key.
func (s *ManagedSession) SessionKey() string { return s.key }

// SessionSnapshot is a point-in-time view of a session for the dashboard API.
type SessionSnapshot struct {
	Key        string `json:"key"`
	Platform   string `json:"platform"`
	Agent      string `json:"agent"`
	SessionID  string `json:"session_id"`
	State      string `json:"state"`
	Protocol   string `json:"protocol"`
	Backend    string `json:"backend,omitempty"`     // "claude", "kiro", ...
	CLIName    string `json:"cli_name,omitempty"`    // "claude-code", "kiro"
	CLIVersion string `json:"cli_version,omitempty"` // e.g. "2.1.92"
	// Model is the spawn-time CLI model identifier resolved from
	// cli.backends[].model → SpawnOptions.Model → Process.Model. Empty
	// when the operator did not configure one. The dashboard renders
	// "(模型未配置)" in that case (UI Round 5 R5-3).
	//
	// Backend-specific behaviour:
	//   - claude (stream-json): the configured model flows through as-is;
	//     the value matches what the operator set in cli.backends[].model.
	//   - kiro / ACP: this field still reflects the spawn-time configured
	//     value (or empty). The real model id reported by ACP
	//     `session/new`'s `result.models.currentModelId` is currently NOT
	//     read back into Process.Model — see R225-ACP-MODEL-INIT in
	//     docs/TODO.md. Until that lands, dashboards consuming Snapshot
	//     for ACP backends should expect the configured value, not the
	//     in-effect runtime model. R225-CR-8.
	Model      string `json:"model,omitempty"`
	LastActive int64  `json:"last_active"` // unix ms
	// CreatedAt anchors sidebar order: the dashboard sorts sessions by this
	// value ascending so newly-created rows always land at the bottom and
	// existing rows never shift on activity. unix ms, 0 only if the loadStore
	// fallback couldn't infer one (treated as "very old" by the comparator).
	CreatedAt    int64   `json:"created_at,omitempty"`
	TotalCost    float64 `json:"total_cost"`
	Workspace    string  `json:"workspace,omitempty"`
	DeathReason  string  `json:"death_reason,omitempty"`
	ChatType     string  `json:"chat_type,omitempty"`
	ChatID       string  `json:"chat_id,omitempty"`
	Node         string  `json:"node,omitempty"`
	LastPrompt   string  `json:"last_prompt,omitempty"`   // most recent user message
	LastActivity string  `json:"last_activity,omitempty"` // most recent tool/thinking status
	// LastResponse is the truncated summary of the most recent assistant
	// text reply, used by the dashboard sidebar's 30-rune dim second-line
	// preview (R110-P1). Sourced from proc.LastResponseSummary when a live
	// process is attached; falls back to s.lastResponse atomic cache for
	// suspended/dead sessions, mirroring LastPrompt's resolution path.
	LastResponse string `json:"last_response,omitempty"`
	Summary      string `json:"summary,omitempty"`    // Claude-generated session title
	UserLabel    string `json:"user_label,omitempty"` // operator-set override for sidebar/header title
	// LabelOrigin records who set UserLabel: "" / "user" (human) or "auto"
	// (sysession daemon). Frontend uses this to show a small bot icon on
	// auto-labeled sessions and to enable the "restore auto naming"
	// action (POST /api/system/labels/clear-origin). See
	// docs/rfc/system-session.md §7.3 / §9.3.
	LabelOrigin     string             `json:"label_origin,omitempty"`
	Project         string             `json:"project,omitempty"`          // project name (filled by server)
	ProjectFallback bool               `json:"project_fallback,omitempty"` // true when Project is a workspace-basename fallback, not a registered project
	IsPlanner       bool               `json:"is_planner,omitempty"`       // true for project planner sessions
	Subagents       []cli.SubagentInfo `json:"subagents,omitempty"`        // active sub-agent types in current turn
	// MessageCount is the cumulative "user" turn count observed by the live
	// Process event log since the current spawn. Zero when no process is
	// attached (persistedHistory only sessions). Not persisted to sessions.json:
	// after shim reconnect, InjectHistory → EventLog.AppendBatch re-applies
	// the historical user entries so the counter rebuilds to the historical
	// value as part of normal startup.
	MessageCount int64 `json:"message_count,omitempty"`

	// Normalized cross-backend status fields (docs/rfc/multi-backend.md §8.8).
	// Filled by Snapshot from Process accessors so dashboard / IM / cron all
	// consume normalized data without parsing backend-private events.
	//
	// CostUnit is "USD" for claude-class backends and the unit reported by
	// the backend for ACP-class (kiro reports "credits"). Empty when no
	// known backend (dashboard hides the cell).
	CostUnit string `json:"cost_unit,omitempty"`
	// ContextUsagePercent is 0-100 conversation context utilisation. kiro
	// reports a real value via _kiro.dev/metadata; claude leaves 0 until
	// estimator lands.
	ContextUsagePercent float64 `json:"context_usage_percent,omitempty"`
	// TurnDurationMs is the duration of the most recently completed turn.
	// kiro: from _kiro.dev/metadata. claude: 0 until estimator wires up.
	TurnDurationMs int64 `json:"turn_duration_ms,omitempty"`
	// MeteringUsage carries backend-reported per-turn billing rows when
	// available (kiro). Each entry is one billing dimension, e.g.
	// {value: 0.024, unit: "credit"}.
	MeteringUsage []cli.MeteringEntry `json:"metering_usage,omitempty"`
}
