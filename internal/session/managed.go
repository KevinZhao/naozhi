package session

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/history"
	"github.com/naozhi/naozhi/internal/session/runhistory"
)

const (
	maxPersistedHistory = 500

	// maxPrevSessionIDs caps the session chain (each "/new" or workspace
	// switch appends one) so sessions.json stays bounded while retaining
	// enough for multi-day context recovery.
	maxPrevSessionIDs = 32

	// Visible-aware initial-history tuning (ManagedSession.EventLastNVisibleCtx):
	// DefaultVisibleTarget ≈ one screenful of chat bubbles; maxVisibleTotal
	// caps the returned slice (== ring size and HTTP maxEventsPageLimit);
	// visibleDiskPageSize is entries per disk LoadBefore page; and
	// maxVisibleDiskPages bounds worst-case JSONL I/O on a slow filesystem.
	DefaultVisibleTarget = 30
	maxVisibleTotal      = 500
	visibleDiskPageSize  = 200
	maxVisibleDiskPages  = 5
)

// ProcessSender is the send-path facet of processIface: the methods a caller
// needs to deliver one user turn and unwind it on abort. Exported so
// downstream callers (cron, dispatch, upstream) can declare a narrower
// dependency; processIface embeds it, so *cli.Process and
// testutil.TestProcess satisfy it with no adapter.
type ProcessSender interface {
	// Send delivers a user turn and streams events through onEvent until
	// the result entry arrives. Single-shot; serialised by the caller-side
	// sendMu in ManagedSession.
	Send(ctx context.Context, text string, images []cli.Attachment, onEvent cli.EventCallback) (*cli.SendResult, error)
	// SendPassthrough is the passthrough-mode Send; errors unless the
	// protocol reports SupportsReplay()==true. Unlike Send, multiple
	// goroutines may call it concurrently — ordering is handled by the
	// CLI's commandQueue plus a naozhi-side sendSlot FIFO.
	SendPassthrough(ctx context.Context, text string, images []cli.Attachment, onEvent cli.EventCallback, priority string) (*cli.SendResult, error)
	// SupportsPassthrough reports whether the protocol can operate in
	// passthrough mode (Protocol.SupportsReplay()); dispatch falls back to
	// Send otherwise.
	SupportsPassthrough() bool
	// PassthroughDepth returns the current pending-slot count.
	PassthroughDepth() int
	// DiscardPassthroughPending cancels all in-flight passthrough sends and
	// fires the given error to each caller (/new, /clear, forced reset).
	DiscardPassthroughPending(reason error)
	// Interrupt requests OS-signal-style abort of the active turn.
	// Best-effort: protocol may not honour SIGINT.
	Interrupt()
	// InterruptViaControl aborts the active turn via an in-band stream-json
	// control_request (no SIGINT, no kill). Returns
	// cli.ErrInterruptUnsupported for protocols without this primitive.
	InterruptViaControl() error
}

// ProcessEventReader is the read-side facet of processIface over the
// in-process EventLog ring (#1319). processIface embeds it, so *cli.Process
// and testutil.TestProcess satisfy both; callers may narrow one site at a time.
type ProcessEventReader interface {
	// EventEntries returns a defensive copy of every entry currently in
	// the live EventLog ring (chronological order).
	EventEntries() []clievent.EventEntry
	// EventLastN returns the last N entries (chronological).
	EventLastN(n int) []clievent.EventEntry
	// EventEntriesSince returns entries with Time > afterMS.
	EventEntriesSince(afterMS int64) []clievent.EventEntry
	// EventEntriesSinceAppend appends matched entries into dst so 1Hz
	// pollers reuse capacity (#1740). Passing nil preserves the "nil when
	// empty" contract. The returned slice is owned by the caller; the
	// reader retains no reference.
	EventEntriesSinceAppend(dst []clievent.EventEntry, afterMS int64) []clievent.EventEntry
	// EventEntriesBefore returns up to `limit` entries with Time < beforeMS
	// from the live ring (chronological).
	EventEntriesBefore(beforeMS int64, limit int) []clievent.EventEntry
	// LastEventAt returns the wall-clock time of the most recent live event
	// appended to the EventLog, or zero when nothing has arrived yet.
	LastEventAt() time.Time
}

// ProcessLifecycle is the liveness + teardown facet of processIface
// consumed by Router.Cleanup, evictOldest, Remove/Reset and the shim
// reconciler (#430). processIface embeds it, so *cli.Process and
// testutil.TestProcess satisfy both.
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

// HistoryInjector is the prev-session history replay facet of processIface
// used on spawn, plus the subagent-roster read for the turn-agent fan-out
// (#430). processIface embeds it.
type HistoryInjector interface {
	// InjectHistory replays prior-session EventEntries into the process's
	// EventLog so history renders continuously across a resume. Called
	// once, on the spawn path, before SetPersistSink flips sinkReady true.
	InjectHistory(entries []clievent.EventEntry)
	// TurnAgents returns the subagent roster observed this turn; empty for
	// backends without a subagent concept.
	TurnAgents() []cli.SubagentInfo
}

// processIface abstracts the CLI process methods used by session-aware code
// in internal/session, internal/server, internal/dispatch, internal/cron and
// internal/upstream (all via ManagedSession.loadProcess()). Keep new methods
// minimal — prefer the embedded facets (ProcessSender, ProcessLifecycle,
// ProcessEventReader, HistoryInjector). *cli.Process is the only production
// implementation; testutil.TestProcess is the test fake.
type processIface interface {
	ProcessSender
	ProcessLifecycle
	// Dashboard introspection. SessionID / State drop the `Get` prefix per
	// Go convention; TestProcessIfaceGetterRenamePlanned pins the names.
	SessionID() string
	State() cli.ProcessState
	// DeathReason returns the process-level reason recorded when the
	// shim-backed CLI exited (passive death). Empty while alive or when the
	// reason has not been classified yet.
	DeathReason() string
	TotalCost() float64
	EventEntries() []clievent.EventEntry
	EventLastN(n int) []clievent.EventEntry
	// EventLastNVisible returns a contiguous tail carrying at least
	// visibleTarget visible entries (or up to maxTotal), so an
	// internal-event flood can't blank the dashboard's first paint.
	EventLastNVisible(visibleTarget, maxTotal int) []clievent.EventEntry
	EventEntriesSince(afterMS int64) []clievent.EventEntry
	EventEntriesSinceAppend(dst []clievent.EventEntry, afterMS int64) []clievent.EventEntry
	EventEntriesBefore(beforeMS int64, limit int) []clievent.EventEntry
	LastActivitySummary() string
	// LastResponseSummary returns the summary of the most recent assistant
	// "text" entry; empty until one has streamed. Feeds
	// SessionSnapshot.LastResponse.
	LastResponseSummary() string
	// LastEventAt returns the wall-clock time of the most recent live event,
	// or zero when nothing has arrived yet. Router.Cleanup uses it as a
	// fallback activity signal so a long turn streaming tool_use / thinking
	// events is not misclassified as stuck once the session-level
	// lastActive (refreshed only at Send entry) has aged.
	LastEventAt() time.Time
	// UserTurnCount returns the cumulative count of "user" entries seen
	// since spawn. Feeds SessionSnapshot.MessageCount.
	UserTurnCount() int64
	ProtocolName() string
	SubscribeEvents() (<-chan struct{}, func())
	PID() int
	HistoryInjector
	// Normalize-layer accessors (docs/rfc/multi-backend.md §8.8). kiro fills
	// these from _kiro.dev/metadata; claude leaves zero values until an
	// estimator lands. Lock-free.
	ContextUsagePercent() float64
	TurnDurationMs() int64
	// SpawnDiags returns the spawn gates' drop/ignore decisions for this
	// process (#2532); nil when everything took effect.
	SpawnDiags() []cli.SpawnDiag
	MeteringUsage() []cli.MeteringEntry
	// MeteringGen versions MeteringUsage so Snapshot can cache its copy per
	// (process, gen) (#2345). Implementations that do not version their rows
	// must return 0, which disables the cache.
	MeteringGen() uint64
	// Effort returns the backend-reported thinking-effort tier (kiro:
	// low/medium/high/xhigh/max), "" when unreported.
	Effort() string
	// Model returns the spawn-time CLI model identifier or "" when
	// unconfigured.
	Model() string
	// LiveVersion returns the CLI binary version self-reported in the
	// process's system/init frame (claude_code_version), or "" before the
	// init frame arrives / on backends that don't self-report.
	LiveVersion() string
}

// Compile-time pins: each facet must stay a strict subset of processIface so
// callers can narrow without an adapter; a signature drift on either side
// fails the build instead of silently desyncing.
var _ ProcessEventReader = (processIface)(nil)
var _ ProcessLifecycle = (processIface)(nil)
var _ HistoryInjector = (processIface)(nil)

// processBox wraps processIface for use with atomic.Pointer (which
// requires a concrete type). Not atomic.Value: it panics on a Store of a
// different dynamic type, and tests inject several fakes. Not sync.Pool:
// loadProcess callers retain the *processBox across goroutine boundaries,
// so recycling it would be a use-after-free. storeProcess runs only on cold
// paths, so the 16-byte alloc per call is negligible.
type processBox struct{ p processIface }

// cancelBox binds a Send()'s context cancel func to the process pointer that
// Send loaded for that turn, so Interrupt() only fires cancel when the live
// process still matches the in-flight Send's target (#381). Send stores the
// cancel func before loadProcess(); if a concurrent spawnSession replaces
// the process in that window, a bare cancel would target the old ctx and
// silently no-op. Interrupt skips a stale box and reports failure instead.
// nil proc means "not yet bound" — Interrupt still fires it because that
// turn is about to run on the current live process.
type cancelBox struct {
	cancel context.CancelFunc
	proc   processIface
}

// ManagedSession wraps a claude CLI process with session metadata.
type ManagedSession struct {
	key string

	// sessionID is written once during the first successful Send and read
	// by Snapshot lock-free. Load returns nil when never stored, distinct
	// from a stored empty string.
	sessionID atomic.Pointer[string]

	// onSessionID is called when a session ID is first captured from Send().
	// Set by the Router to track known IDs for history exclusion.
	onSessionID func(string)

	// runStore records per-run timing for the dashboard. Shared across all
	// sessions (owned by the Router); nil in tests / no-persist configs and
	// every call site is nil-safe.
	runStore *runhistory.Store

	// lastActive stores time.UnixNano atomically to avoid data races
	// between Send() (under sendMu) and Cleanup/evictOldest (under r.mu).
	lastActive atomic.Int64

	// createdAt anchors the session's sidebar position: set once at
	// construction (or carried over via Rename) and never touched again.
	// On store load, missing values fall back to LastActive.
	createdAt atomic.Int64

	// lastPrompt caches the most recent user message summary (atomic for lock-free Snapshot reads).
	lastPrompt atomic.Pointer[string]

	// lastActivity caches the most recent tool_use/thinking summary.
	lastActivity atomic.Pointer[string]

	// lastResponse caches the most recent assistant text summary for the
	// sidebar preview line. Live updates flow from proc.LastResponseSummary;
	// the cache exists for dead/suspended sessions whose process is gone.
	lastResponse atomic.Pointer[string]

	// Cached key parts, parsed once via keyOnce. Key is immutable.
	keyOnce     sync.Once
	keyPlatform string
	keyChatType string
	keyChatID   string
	keyAgentID  string

	process atomic.Pointer[processBox] // stores *processBox; use loadProcess/storeProcess
	// meteringCache is Snapshot's last MeteringUsage view keyed by
	// (process, MeteringGen); see managed_metering_cache.go (#2345).
	meteringCache atomic.Pointer[meteringCache]
	sendMu        sync.Mutex   // serializes messages to the same session
	historyMu     sync.RWMutex // protects persistedHistory reads/writes (independent of sendMu)
	// costMu serializes the read-modify-write of costSpent/lastCumulativeCost
	// in finishRun, which runs OUTSIDE sendMu on both paths (Send releases
	// sendMu before returning; passthrough is lock-free), so this mutex is
	// the only serializer for the per-turn delta and required for
	// race-freedom.
	costMu sync.Mutex
	// sendCancel holds the in-flight Send()'s cancel func bound to the process
	// it targets (see cancelBox), so Interrupt() can skip a cancel whose
	// process has been replaced by a concurrent spawnSession (#381).
	sendCancel atomic.Pointer[cancelBox]
	// workspace is the effective cwd at spawn time. Writers hold r.mu in the
	// router, but Snapshot() is called from Hub handlers WITHOUT r.mu, so
	// the read must be atomic.
	workspace atomic.Pointer[string]
	// cliIdentity packs backend / cliName / cliVersion (always written
	// together) so Snapshot reads all three with one Load. Updaters use
	// updateCLIIdentity (CAS loop) so single-field setters compose safely.
	cliIdentity atomic.Pointer[cliIdentityBox]
	deathReason atomic.Pointer[string] // why process died, empty if alive
	// overlayDrift is the reconcile-computed per-field diff between the live
	// shim's argv and a fresh spawn under current config (#2543). Written by
	// reconnectShims outside r.mu, read lock-free by snapshot(); nil = none.
	overlayDrift atomic.Pointer[[]OverlayFieldDrift]
	// userLabel is an operator-set display name overriding summary/last_prompt
	// in the dashboard. Empty = unset.
	userLabel atomic.Pointer[string]
	// labelOrigin records who set userLabel: ""/"user" (operator) or "auto"
	// (sysession daemon). Once a human writes, daemons must leave the session
	// alone unless ClearUserLabelOrigin resets it (docs/rfc/system-session.md
	// §7.3). Writes must go through Router.SetUserLabelWithOrigin so the r.mu
	// re-read closes the daemon-vs-user race (RFC §11.1).
	labelOrigin atomic.Pointer[string]
	// model is the most-recent CLI model identifier (system/init for claude,
	// SpawnOptions.Model for kiro), persisted to sessions.json as the
	// fallback for restart / pre-init windows; live proc.Model() wins.
	model atomic.Pointer[string]
	// tuningModel / tuningEffort are the operator's per-session overrides
	// (docs/rfc/dashboard-model-effort-control.md §4.3); "" = none. They top
	// resolveSpawnParamsLocked's precedence and persist to sessions.json.
	// Unlike `model` (REPORTED) they record what the operator DEMANDED;
	// tuningspec-validated on write and store load so a hand-edited
	// sessions.json cannot inject argv (§4.6).
	tuningModel  atomic.Pointer[string]
	tuningEffort atomic.Pointer[string]
	// totalCost is the cost inherited from a previous process incarnation,
	// written at construction; Snapshot() falls back to it before the live
	// process reports a result (no $0.00 flash after resume). Atomic so a
	// future post-publication writer cannot introduce a torn read; access
	// via loadTotalCost/storeTotalCost (math.Float64bits packing).
	totalCost atomic.Uint64

	// costSpent is the genuine cumulative spend (sum of per-turn deltas, see
	// runhistory.TurnCostDelta): monotonic across process replacements,
	// unlike totalCost which RESETS on resume. lastCumulativeCost is the
	// previous raw CLI reading the next delta diffs against. Both are
	// Float64bits-packed and written only from accountTurnCost.
	costSpent          atomic.Uint64
	lastCumulativeCost atomic.Uint64
	// lastCumulative is the full per-incarnation baseline (USD, per-model
	// rows, backend metering) the next turn differences against; spent is the
	// monotonic total across incarnations. modelsBaselineUnknown marks a
	// store-restored session whose per-model baseline was not persisted, so
	// the first turn's model drill-down is withheld. All three under costMu.
	lastCumulative        costledger.Cumulative
	spent                 costledger.Totals
	modelsBaselineUnknown bool
	// costAcct is the router-wide ledger sink; nil in tests that don't wire one.
	costAcct *costAccounting

	// persistedHistory stores event entries that survive process restarts.
	// Populated by InjectHistory and carried over when the process is replaced.
	persistedHistory []clievent.EventEntry

	// persistedUserTurns caches the Type=="user" count in persistedHistory,
	// recomputed under historyMu on every mutation so the proc==nil snapshot
	// branch fills MessageCount lock-free without an O(n) scan (#1644).
	persistedUserTurns atomic.Int64

	// persistedSeededLen is the prefix length of persistedHistory already
	// forwarded into the current proc.EventLog; reset whenever a fresh proc
	// is published so InjectHistory forwards only the unseeded tail.
	// Read/written under historyMu in sync with persistedHistory; see
	// attachProcessAndSnapshotPersisted for the publish/snapshot ordering.
	persistedSeededLen int

	// persistedHistorySorted is true when persistedHistory is known to be
	// Time-ascending, letting EventEntriesSince skip the stable sort on the
	// 1Hz dashboard hot path. Zero value (false) means readers sort once and
	// flip it; InjectHistory maintains it against the existing tail.
	// Maintained under historyMu in lockstep with persistedHistory.
	persistedHistorySorted bool

	// prevSessionIDs tracks previous session IDs for this key (oldest →
	// newest), used on startup to load the conversation chain from JSONL.
	// Capped at maxPrevSessionIDs; overflow drops the oldest entries.
	prevSessionIDs []string

	// prevSessionOrigins is parallel to prevSessionIDs ("manual" default,
	// "auto-spawn", "auto-backfill", "resume"); read/written under historyMu
	// in lockstep. Contract: len(origins) <= len(ids), missing tail reads as
	// "manual" (relies on ids being append-only, RFC §4.6); a length drift is
	// detected, metric'd and rebuilt to all-"manual", never left misaligned.
	prevSessionOrigins []string

	// prevHistoryGen increments on every mutation of prevSessionIDs /
	// prevSessionOrigins (under historyMu) so equalStoreEntry compares an
	// O(1) counter instead of slices.Equal (#2346). Atomic so readers holding
	// only historyMu.RLock observe a torn-free value.
	prevHistoryGen atomic.Uint64

	// exempt marks this session as exempt from TTL cleanup, eviction, and activeCount.
	// Used for planner sessions that should persist indefinitely.
	exempt bool

	// historySource backs EventEntriesBeforeCtx's disk-tier fallback (claude
	// gets claudejsonl.Source; other backends history.Noop so no nil-check).
	// Atomic because SetHistorySource is exported and may race with in-flight
	// pagination reads after the session is reachable.
	historySource atomic.Pointer[historySourceBox]

	// storeMarshalCache memoizes the last (storeEntry → JSON) result so
	// saveStore skips re-marshalling unchanged sessions (#1523). Keyed on the
	// storeEntry value via equalStoreEntry, so any persisted-field change
	// invalidates it without per-site bookkeeping. Atomic because Shutdown's
	// final saveStore can overlap an in-flight periodic save.
	storeMarshalCache atomic.Pointer[storeEntryCache]
}

// storeEntryCache is the per-session memo behind storeMarshalCache: entry
// produced data, a standalone JSON object (NOT array-wrapped) ready to be
// concatenated into the sessions.json array.
type storeEntryCache struct {
	entry storeEntry
	data  []byte
}

// historySourceBox wraps history.Source so atomic.Pointer (which requires a
// concrete type) can store it.
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
	// AccessProfile is the access-profile ID this session spawned under
	// ("" = global default). Label/colour live in the /api/access-profiles
	// registry, NOT here — the snapshot never carries env values or secrets
	// (RFC project-access-profile §8.3).
	AccessProfile string `json:"access_profile,omitempty"`
	// Model is the CLI model identifier (live process value, else the
	// persisted spawn-time value). Empty when the operator did not configure
	// one; the dashboard renders "(模型未配置)". For ACP backends the runtime
	// model from session/new is not read back (see docs/TODO.md), so this
	// reflects the configured value.
	Model      string `json:"model,omitempty"`
	LastActive int64  `json:"last_active"` // unix ms
	// CreatedAt anchors sidebar order (ascending, so new rows land at the
	// bottom and rows never shift on activity). unix ms; 0 only if loadStore
	// couldn't infer one (treated as "very old").
	CreatedAt    int64   `json:"created_at,omitempty"`
	TotalCost    float64 `json:"total_cost"`
	Workspace    string  `json:"workspace,omitempty"`
	DeathReason  string  `json:"death_reason,omitempty"`
	ChatType     string  `json:"chat_type,omitempty"`
	ChatID       string  `json:"chat_id,omitempty"`
	Node         string  `json:"node,omitempty"`
	LastPrompt   string  `json:"last_prompt,omitempty"`   // most recent user message
	LastActivity string  `json:"last_activity,omitempty"` // most recent tool/thinking status
	// LastResponse is the truncated summary of the most recent assistant text
	// reply for the sidebar preview: live proc.LastResponseSummary, falling
	// back to the s.lastResponse cache for suspended/dead sessions.
	LastResponse string `json:"last_response,omitempty"`
	Summary      string `json:"summary,omitempty"`    // Claude-generated session title
	UserLabel    string `json:"user_label,omitempty"` // operator-set override for sidebar/header title
	// LabelOrigin records who set UserLabel: "" / "user" (human) or "auto"
	// (sysession daemon); drives the bot icon and "restore auto naming"
	// action (docs/rfc/system-session.md §7.3 / §9.3).
	LabelOrigin     string             `json:"label_origin,omitempty"`
	Project         string             `json:"project,omitempty"`          // project name (filled by server)
	ProjectFallback bool               `json:"project_fallback,omitempty"` // true when Project is a workspace-basename fallback, not a registered project
	IsPlanner       bool               `json:"is_planner,omitempty"`       // true for project planner sessions
	Subagents       []cli.SubagentInfo `json:"subagents,omitempty"`        // active sub-agent types in current turn
	// MessageCount is the cumulative "user" turn count: from the live Process
	// event log since spawn, else the persistedHistory count. Not persisted;
	// InjectHistory → EventLog.AppendBatch rebuilds it on reconnect.
	MessageCount int64 `json:"message_count,omitempty"`

	// Normalized cross-backend status fields (docs/rfc/multi-backend.md §8.8)
	// so dashboard / IM / cron never parse backend-private events.
	//
	// CostUnit is "USD" for claude-class backends and the backend-reported
	// unit for ACP-class (kiro: "credits"). Empty when no known backend.
	CostUnit string `json:"cost_unit,omitempty"`
	// ContextUsagePercent is 0-100 context utilisation (kiro only; claude 0).
	ContextUsagePercent float64 `json:"context_usage_percent,omitempty"`
	// TurnDurationMs is the last completed turn's duration (kiro only; claude 0).
	TurnDurationMs int64 `json:"turn_duration_ms,omitempty"`
	// MeteringUsage carries backend-reported per-turn billing rows (kiro).
	// READ-ONLY, shared across snapshots: while MeteringGen is unchanged
	// every Snapshot returns the same backing array (#2345). Consumers,
	// SnapshotEnricher hooks included, must copy before mutating.
	MeteringUsage []cli.MeteringEntry `json:"metering_usage,omitempty"`
	// Effort is the backend's thinking-effort tier for the latest turn
	// (low/medium/high/xhigh/max on kiro). Empty for backends that report
	// none, evicted sessions, and before the first metadata frame; the
	// dashboard hides the tag. Not persisted, so it resets across restarts
	// (docs/rfc/kiro-effort-visibility.md).
	Effort string `json:"effort,omitempty"`
	// SpawnDiags is what the spawn gates dropped/ignored for the live process
	// (#2532). Always serialised — an empty array, not undefined, so the
	// dashboard can index it unconditionally. Runtime observation like
	// Effort: empty for evicted sessions and across restarts.
	SpawnDiags []cli.SpawnDiag `json:"spawn_diags"`
	// OverlayDrift lists the argv-bearing fields whose live value differs
	// from what a fresh spawn under the current config would use (#2543);
	// remedy is restarting the session. Same always-an-array contract as
	// SpawnDiags.
	OverlayDrift []OverlayFieldDrift `json:"overlay_drift"`
}
