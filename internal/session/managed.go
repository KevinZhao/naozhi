package session

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/history"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/textutil"
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
	Alive() bool
	IsRunning() bool
	Close()
	Kill()
	// Dashboard introspection
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

	process    atomic.Pointer[processBox] // stores *processBox; use loadProcess/storeProcess
	sendMu     sync.Mutex                 // serializes messages to the same session
	historyMu  sync.RWMutex               // protects persistedHistory reads/writes (independent of sendMu)
	sendCancel atomic.Pointer[context.CancelFunc]
	// workspace is the effective cwd at spawn time. Writers hold r.mu in the
	// router (spawnSession / RegisterCronStub / SetWorkspace), but Snapshot()
	// is called from Hub handlers WITHOUT r.mu (see wshub.go:466, 520). Direct
	// string read there races the write — harmless today (word-sized assign),
	// but flagged by -race and future-unsafe if pointee ever grows. Go through
	// atomic.Pointer[string] to match the backend/cliName/cliVersion pattern
	// already established above.
	workspace atomic.Pointer[string]
	// backend/cliName/cliVersion are written at spawn time AND later by
	// reconnectShims under r.mu (write), but read by Snapshot() without
	// any lock (called via ListSessions which only holds RLock while
	// collecting refs). Using atomic.Pointer[string] keeps the read/write
	// race-free without round-tripping Snapshot through r.mu — type-safe
	// (unlike atomic.Value which accepts any interface value), and Load
	// returns nil when never stored so an explicit empty-string store is
	// distinguishable from "untouched".
	backend     atomic.Pointer[string] // backend ID ("claude" | "kiro"); empty = router default
	cliName     atomic.Pointer[string] // "claude-code", "kiro" — set at creation from Wrapper
	cliVersion  atomic.Pointer[string] // semver from --version
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

// Workspace returns the effective cwd recorded for this session. Lock-free;
// safe to call from Hub handlers and other call sites that don't hold r.mu.
func (s *ManagedSession) Workspace() string { return loadAtomicString(&s.workspace) }

// setWorkspace stores the workspace path atomically. Router-internal helper —
// all writers already hold r.mu, but we route through the helper so the string
// is always handed to the atomic.Pointer via one place (matches
// storeAtomicString convention for backend/cliName/cliVersion).
func (s *ManagedSession) setWorkspace(ws string) { storeAtomicString(&s.workspace, ws) }

// IsExempt returns whether this session is exempt from TTL and eviction.
func (s *ManagedSession) IsExempt() bool { return s.exempt }

// loadAtomicString and storeAtomicString are thin wrappers around the shared
// textutil.LoadAtomicString / textutil.StoreAtomicString helpers. Kept as
// package-private aliases so the surrounding accessor methods read cleanly.
// Behavioural contract — fast-path short-circuit on equal value,
// last-writer-wins — is documented on the textutil helpers; do not
// re-document it here to keep the two in sync.
//
// Naming follows the textutil canonical (action-type) so cli and session
// thin wrappers no longer have inverted word order. R219-CR-1 closed the
// duplication; this rename closes the naming inconsistency.
func loadAtomicString(v *atomic.Pointer[string]) string {
	return textutil.LoadAtomicString(v)
}

func storeAtomicString(v *atomic.Pointer[string], s string) {
	textutil.StoreAtomicString(v, s)
}

// loadTotalCost reads the float64 cumulative cost from an atomic.Uint64
// field, decoding the IEEE-754 bit pattern via math.Float64frombits.
// Returns 0 when the field has never been written (Load() → 0 maps to
// float64 zero, same default the plain-float64 field had).
//
// Cross-ref: textutil exposes LoadAtomicString / StoreAtomicString for the
// `atomic.Pointer[string]` mirror pattern (R219-CR-1) but does not yet
// cover the `atomic.Uint64`-encoded float64 case used here. These helpers
// stay package-local until a second call site emerges; lifting them into
// textutil now would invert the dependency (textutil is a leaf package
// that must not import session-specific contracts). R230-CQ-18.
func loadTotalCost(v *atomic.Uint64) float64 {
	return math.Float64frombits(v.Load())
}

// storeTotalCost writes a float64 cumulative cost via atomic.Uint64,
// encoding through math.Float64bits. Paired with loadTotalCost to keep the
// packing/unpacking convention in one place — R183-CONCUR-M2 made the
// field atomic to harden against future post-publication writers, and
// having a helper keeps call sites free of bit-level noise.
//
// See loadTotalCost for the textutil cross-reference.
func storeTotalCost(v *atomic.Uint64, cost float64) {
	v.Store(math.Float64bits(cost))
}

// Backend returns the backend ID ("" when the router default is in effect).
func (s *ManagedSession) Backend() string { return loadAtomicString(&s.backend) }

// SetBackend records the backend ID for this session. Called at spawn time
// and (rarely) by reconnectShims after a naozhi restart.
func (s *ManagedSession) SetBackend(id string) { storeAtomicString(&s.backend, id) }

// CLIName returns the CLI display name (e.g. "claude-code", "kiro").
func (s *ManagedSession) CLIName() string { return loadAtomicString(&s.cliName) }

// SetCLIName records the wrapper-provided CLI display name.
func (s *ManagedSession) SetCLIName(name string) { storeAtomicString(&s.cliName, name) }

// CLIVersion returns the detected CLI version string.
func (s *ManagedSession) CLIVersion() string { return loadAtomicString(&s.cliVersion) }

// SetCLIVersion records the wrapper-provided CLI version.
func (s *ManagedSession) SetCLIVersion(v string) { storeAtomicString(&s.cliVersion, v) }

// UserLabel returns the operator-set display label ("" when unset).
func (s *ManagedSession) UserLabel() string { return loadAtomicString(&s.userLabel) }

// SetUserLabel records an operator-set display label. Callers must have
// already validated length/charset; the empty string clears any prior label.
//
// Deprecated for daemon callers: prefer Router.SetUserLabelWithOrigin so the
// LabelOrigin field stays consistent. This bare setter is preserved for
// internal callers (router restore, tests) that already know the origin
// context they want to preserve.
func (s *ManagedSession) SetUserLabel(v string) { storeAtomicString(&s.userLabel, v) }

// LabelOrigin returns the recorded origin of the current UserLabel:
// "" (legacy / empty equivalent to "user") / "user" / "auto". Lock-free.
func (s *ManagedSession) LabelOrigin() string { return loadAtomicString(&s.labelOrigin) }

// setLabelOrigin records the origin of the current UserLabel. Unexported
// because the only legitimate writers are Router.SetUserLabelWithOrigin
// and ClearUserLabelOrigin, which run under r.mu so the re-read protocol
// (RFC §11.1) stays atomic with the userLabel update.
func (s *ManagedSession) setLabelOrigin(v string) { storeAtomicString(&s.labelOrigin, v) }

// Model returns the persisted last-known CLI model identifier ("" when
// not yet captured from system/init / SpawnOptions). UI Round 5 R5-3.
func (s *ManagedSession) Model() string { return loadAtomicString(&s.model) }

// SetModel records the latest known model id. Called by the readLoop
// snapshotter when proc.Model() flips from "" to a real value, AND by
// the store-restore path in NewRouter when seeding from sessions.json.
func (s *ManagedSession) SetModel(v string) { storeAtomicString(&s.model, v) }

// SetHistorySource installs the backend-specific disk-tier Source. Called
// by the router at session construction; safe to call after the session is
// published (atomic store) but callers should not rely on mid-flight
// swaps being observed by a pagination request already in progress.
// nil disables disk fallback (equivalent to history.Noop).
func (s *ManagedSession) SetHistorySource(src history.Source) {
	s.historySource.Store(&historySourceBox{src: src})
}

// loadHistorySource returns the installed Source, or nil when no source
// has been attached yet. Callers treat nil the same as history.Noop.
func (s *ManagedSession) loadHistorySource() history.Source {
	box := s.historySource.Load()
	if box == nil {
		return nil
	}
	return box.src
}

// SnapshotChainIDs returns the session-ID chain (oldest → newest). The
// current session ID is appended only when non-empty — a just-spawned
// session that hasn't captured its first ID yet yields the prev chain
// alone, which matches how router.go builds the chain for JSONL loads
// today.
//
// Lock contract (R230C-GO-1): the authoritative invariant for
// prevSessionIDs is "writers hold r.mu; readers either hold r.mu or
// accept a stale-but-not-torn snapshot". All writers (registerStub /
// spawnSession.installFreshSessionLocked / RenameSession /
// router_core restore) write under r.mu.Lock(). This reader runs from
// cli.Wrapper.NewHistorySource factories which do NOT hold r.mu, so
// historyMu.RLock here is a defensive rope: it does not synchronise
// with the r.mu writers, but the slices.Clone in writers + the append
// pattern guarantee any value we observe was a complete prior snapshot
// (Go's memory model on slice header writes is per-word atomic on
// 64-bit). historyMu still serialises against the InjectHistory
// persistedHistory append path which lives next to prevSessionIDs in
// memory. A future cleanup is to take r.mu.RLock() here instead, but
// that requires plumbing the router pointer into ManagedSession and
// is not done as part of R230C-GO-1.
//
// Exported (Sprint 1a) so cli.Wrapper.NewHistorySource factories can
// pull the current chain at LoadBefore time without the cli package
// having to know about ManagedSession internals. Callers must not
// mutate the returned slice — the underlying append/clone defends
// against torn writes but not against caller-side modification.
func (s *ManagedSession) SnapshotChainIDs() []string {
	s.historyMu.RLock()
	defer s.historyMu.RUnlock()
	cur := s.getSessionID()
	n := len(s.prevSessionIDs)
	if cur != "" {
		n++
	}
	if n == 0 {
		return nil
	}
	out := make([]string, 0, n)
	out = append(out, s.prevSessionIDs...)
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// SetPrevSessionOrigins records the origin label for the most-recently
// appended chain segment (the trailing len(ids) entries of prevSessionIDs).
// Older origin entries are preserved with their existing value, defaulting
// to "manual" for any prefix that was set before origins tracking arrived.
//
// origin is one of: "manual" / "auto-spawn" / "auto-backfill" / "resume".
// Empty origin is rejected as a no-op (caller bug protection).
//
// Invariant (RFC v3 Arch-MINOR-1): prev_session_ids is append-only — every
// production write path (spawn chain rotation, auto-chain attach,
// RegisterCronStubWithChain replace, store restore) only grows the slice
// or replaces it wholesale. SetPrevSessionOrigins detects drift (origins
// longer than ids, or "ids" tail position negative) and rebuilds origins
// to all-"manual" rather than allowing a misaligned label to persist.
// The drift is metric-counted so a regression in a future writer is
// visible in production telemetry.
func (s *ManagedSession) SetPrevSessionOrigins(ids []string, origin string) {
	if origin == "" || len(ids) == 0 {
		return
	}
	s.historyMu.Lock()
	defer s.historyMu.Unlock()

	// Drift detection. start := len(ids in chain) - len(this batch).
	// Negative means the batch is longer than the chain — the batch
	// was not appended to the tail. Origins longer than IDs means a
	// past write left dangling labels. Both rebuild the parallel
	// slice from scratch with "manual" defaults so origin↔id never
	// misaligns silently.
	start := len(s.prevSessionIDs) - len(ids)
	driftLonger := len(s.prevSessionOrigins) > len(s.prevSessionIDs)
	if start < 0 || driftLonger {
		metrics.AutoChainOriginsLengthMismatch.Add(1)
		slog.Warn("auto-chain: prev_session_origins length drift; rebuilding to manual",
			"key", s.key,
			"prev_ids_len", len(s.prevSessionIDs),
			"prev_origins_len", len(s.prevSessionOrigins),
			"incoming_len", len(ids))
		rebuilt := make([]string, len(s.prevSessionIDs))
		for i := range rebuilt {
			rebuilt[i] = "manual"
		}
		s.prevSessionOrigins = rebuilt
		// Re-derive start with the now-clean baseline; if start was
		// negative the batch is meaningless against this chain — bail.
		if start < 0 {
			return
		}
	}

	// Grow origins to match the chain length, defaulting any older
	// untracked prefix to "manual" so the resulting slice is fully
	// populated.
	if len(s.prevSessionOrigins) < len(s.prevSessionIDs) {
		grown := make([]string, len(s.prevSessionIDs))
		copy(grown, s.prevSessionOrigins)
		for i := len(s.prevSessionOrigins); i < len(grown); i++ {
			grown[i] = "manual"
		}
		s.prevSessionOrigins = grown
	}

	// Stamp the trailing len(ids) entries with the supplied origin.
	for i := range ids {
		s.prevSessionOrigins[start+i] = origin
	}
}

// SnapshotPrevSessionOrigins returns a defensive copy of the parallel
// origins slice. Callers (storeStateLocked / dashboard introspection)
// must not mutate the result. Length is exactly len(prevSessionIDs);
// any unset entry is materialised as "manual" so consumers can always
// align positionally without nil-checks.
func (s *ManagedSession) SnapshotPrevSessionOrigins() []string {
	s.historyMu.RLock()
	defer s.historyMu.RUnlock()
	if len(s.prevSessionIDs) == 0 {
		return nil
	}
	out := make([]string, len(s.prevSessionIDs))
	for i := range out {
		if i < len(s.prevSessionOrigins) && s.prevSessionOrigins[i] != "" {
			out[i] = s.prevSessionOrigins[i]
		} else {
			out[i] = "manual"
		}
	}
	return out
}

// loadProcess returns the currently attached processIface, or nil when
// the session is detached (paused, reclaimed, or never spawned).
//
// Implementation note: s.process is an atomic.Pointer[processBox] — we
// wrap the iface in a one-field struct because Go's atomic.Pointer is
// generic over a concrete type and requires non-nil iface assertions to
// store directly. The "load box, dereference" indirection is the cost
// of getting lock-free read/write semantics for an interface value.
// Callers that only need liveness should prefer isAlive() over
// loadProcess() != nil to also catch dead-but-attached processes.
func (s *ManagedSession) loadProcess() processIface {
	if box := s.process.Load(); box != nil {
		return box.p
	}
	return nil
}

// storeProcess atomically replaces the attached process. Passing nil
// detaches; passing a non-nil iface re-wraps in a fresh processBox so
// concurrent loadProcess callers see a consistent (box, p) pair without
// torn reads. Must be paired with sendMu / spawnMu by the caller — this
// function only handles the atomic publication, not the lifecycle
// invariant that only one process is attached at a time.
func (s *ManagedSession) storeProcess(p processIface) {
	if p == nil {
		s.process.Store(nil)
	} else {
		s.process.Store(&processBox{p: p})
	}
}

// isAlive returns true only when a process is attached AND its Alive()
// reports the underlying handle has not exited. Lock-free; uses
// loadProcess() so it is safe to call from any goroutine. The dual
// nil + Alive check is required because the readLoop transitions
// process state to dead before storeProcess(nil) detaches.
func (s *ManagedSession) isAlive() bool {
	p := s.loadProcess()
	return p != nil && p.Alive()
}

// ReattachProcess safely injects a reconnected shim process into this session.
// Called by Router.reconnectShims after naozhi restart.
func (s *ManagedSession) ReattachProcess(proc processIface, sessionID string) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	snapshot := s.attachProcessAndSnapshotPersisted(proc)
	s.setSessionID(sessionID)
	storeAtomicString(&s.deathReason, "")
	s.lastActive.Store(time.Now().UnixNano())

	// attachProcessAndSnapshotPersisted returns nil snapshot when proc is nil,
	// so len(snapshot) > 0 already implies proc != nil. R231-CQ-3.
	if len(snapshot) > 0 {
		proc.InjectHistory(snapshot)
	}

	if s.onSessionID != nil && sessionID != "" {
		s.onSessionID(sessionID)
	}
}

// ReattachProcessNoCallback is like ReattachProcess but skips the onSessionID
// callback. Used when the caller already holds router.mu and will track the
// session ID directly (avoids deadlock since onSessionID acquires router.mu).
//
// Does NOT acquire sendMu: all operations here are atomic stores, and the
// caller already holds router.mu (write). Acquiring sendMu here would violate
// the documented lock ordering (sendMu → router.mu) and risk ABBA deadlock
// with Send() which holds sendMu then calls onSessionID → router.mu.
//
// SAFETY CONSTRAINT: this function must only be called when Send() cannot be
// in flight for this session (e.g., during ReconnectShims at startup, or while
// the session's process is known-dead). If Send() were concurrently executing,
// the deathReason.Store("") here could silently erase a diagnostic death reason
// that Send() just set. The lack of sendMu makes this a logical race on the
// deathReason value, even though each individual Store is atomic.
func (s *ManagedSession) ReattachProcessNoCallback(proc processIface, sessionID string) {
	snapshot := s.attachProcessAndSnapshotPersisted(proc)
	s.setSessionID(sessionID)
	storeAtomicString(&s.deathReason, "")
	s.lastActive.Store(time.Now().UnixNano())
	// attachProcessAndSnapshotPersisted returns nil snapshot when proc is nil,
	// so len(snapshot) > 0 already implies proc != nil. R231-CQ-3.
	if len(snapshot) > 0 {
		proc.InjectHistory(snapshot)
	}
}

// adoptProcessAlreadySeeded publishes proc and marks the entire current
// persistedHistory as already-seeded into proc.EventLog. Used by Rename /
// takeover paths where the proc was running under a different ManagedSession
// and already has the matching entries in its ring; we must NOT re-inject
// (would duplicate every bubble) but we DO need persistedSeededLen aligned
// so the next InjectHistory tail still forwards.
//
// R231-CQ-5: the verb pair "adopt … AlreadySeeded" vs
// "attach … AndSnapshotPersisted" intentionally diverges to encode the two
// distinct semantics — adopt = "treat persistedHistory as if proc has it
// already, do not return a snapshot"; attach = "publish proc + return the
// persistedHistory slice so the caller can re-seed". A blanket rename to
// match the styles would lose that signal at the call site, so this godoc
// pins the contrast instead. See attachProcessAndSnapshotPersisted's godoc
// for the symmetric path.
func (s *ManagedSession) adoptProcessAlreadySeeded(proc processIface) {
	s.historyMu.Lock()
	s.storeProcess(proc)
	s.persistedSeededLen = len(s.persistedHistory)
	s.historyMu.Unlock()
}

// attachProcessAndSnapshotPersisted publishes proc as the session's live
// process and atomically snapshots the persistedHistory prefix that the new
// proc still needs to be seeded with. The two writes share historyMu so
// concurrent InjectHistory calls observe a consistent (process, seededLen)
// pair: an InjectHistory that wins the lock first sees seededLen=0 and the
// old (likely nil) process, appends to persistedHistory, and forwards to the
// fresh process if one is already attached; an InjectHistory that loses the
// race sees seededLen == len(persistedHistory) so its forwarding loop only
// pushes the truly new tail (no double-injection).
//
// Returns a defensive copy because proc.InjectHistory consumes the slice and
// runs after we release historyMu — handing it the live persistedHistory
// backing array would race with subsequent appends.
//
// R231-CQ-5: the verb pair "attach … AndSnapshotPersisted" vs
// "adopt … AlreadySeeded" intentionally diverges — attach returns the
// persistedHistory slice so the caller re-seeds; adopt treats the slice as
// already in proc.EventLog and returns nothing. See adoptProcessAlreadySeeded
// for the symmetric path.
func (s *ManagedSession) attachProcessAndSnapshotPersisted(proc processIface) []cli.EventEntry {
	s.historyMu.Lock()
	if proc == nil {
		// R231-CQ-2: nil parameter = "session is now process-less" (detach
		// path used by ResetAndRecreate / Cleanup / Remove). The decision to
		// also reset persistedSeededLen=0 is deliberate: when a fresh process
		// later attaches via this same function, it MUST be re-seeded with
		// the full persistedHistory snapshot, otherwise the dashboard
		// renders empty until new live events arrive. Leaving seededLen at
		// its prior non-zero value would make the next attach skip the
		// snapshot and the new proc's EventLog would start blank against
		// the persisted history that the session still remembers.
		//
		// persistedHistory itself is NOT cleared — the chat key + workspace
		// stay the same across the detach/reattach pair, so its content is
		// still valid. Only the "what proc has been seeded with" pointer
		// resets. Mirrors adoptProcessAlreadySeeded's symmetric contract:
		// adopt = "proc already has the events"; attach(nil) = "no proc,
		// next attach must re-seed from scratch".
		s.storeProcess(nil)
		s.persistedSeededLen = 0
		s.historyMu.Unlock()
		return nil
	}
	s.storeProcess(proc)
	n := len(s.persistedHistory)
	var snapshot []cli.EventEntry
	if n > 0 {
		snapshot = make([]cli.EventEntry, n)
		copy(snapshot, s.persistedHistory)
	}
	s.persistedSeededLen = n
	s.historyMu.Unlock()
	return snapshot
}

// LastActive returns the last active time.
func (s *ManagedSession) LastActive() time.Time {
	return time.Unix(0, s.lastActive.Load())
}

// touchLastActive updates the last active timestamp.
func (s *ManagedSession) touchLastActive() {
	s.lastActive.Store(time.Now().UnixNano())
}

// initCreatedAtIfUnset stamps createdAt to now when it has not been set yet.
// Idempotent: a non-zero value is left alone, so Rename / loadStore paths that
// preload the original creation timestamp keep sidebar order stable.
func (s *ManagedSession) initCreatedAtIfUnset() {
	if s.createdAt.Load() == 0 {
		s.createdAt.Store(time.Now().UnixNano())
	}
}

// createdAtMillis returns the createdAt instant in unix milliseconds for the
// dashboard payload. Zero stays zero so the JSON omitempty check fires for
// sessions that somehow never received a stamp.
func (s *ManagedSession) createdAtMillis() int64 {
	v := s.createdAt.Load()
	if v == 0 {
		return 0
	}
	return v / int64(time.Millisecond)
}

// SendPassthrough is the concurrent-capable Send for passthrough mode.
// Unlike Send, it does NOT serialise the entire turn under sendMu — the
// CLI's internal commandQueue plus the Process-level sendSlot FIFO
// provide ordering, and serialising at this layer would defeat
// passthrough's whole point (instant dispatch, tool-boundary mid-turn
// injection).
//
// sendMu is still acquired briefly around the first-turn session-ID
// capture inner-check (see R215-GO-P2-2); the lock window is bounded
// to that critical section and does not span the round-trip.
//
// Callers must verify SupportsPassthrough() before invoking. For protocols
// that don't support replay, the dispatcher should fall back to the legacy
// Send path. Calling SendPassthrough on an unsupported protocol just returns
// an error; it does not hang.
//
// `priority` is one of "", "now", "next", "later". Empty lets the CLI default
// ("next") win. "now" aborts the in-flight turn (see docs/rfc/
// passthrough-mode.md §5.6, validation V2).
func (s *ManagedSession) SendPassthrough(ctx context.Context, text string, images []cli.ImageData, onEvent cli.EventCallback, priority string) (*cli.SendResult, error) {
	s.touchLastActive()

	prompt := textutil.TruncateRunes(text, 120)
	if len(images) > 0 {
		prompt += " [+" + strconv.Itoa(len(images)) + " image(s)]"
	}
	storeAtomicString(&s.lastPrompt, prompt)

	proc := s.loadProcess()
	if proc == nil {
		return nil, fmt.Errorf("session %s: %w", s.key, ErrNoActiveProcess)
	}

	result, err := proc.SendPassthrough(ctx, text, images, onEvent, priority)
	if err != nil {
		s.mapSendError(proc, err)
		return nil, err
	}
	if result.SessionID != "" && s.getSessionID() == "" {
		// Double-check the session-ID capture (R215-GO-P2-2):
		//
		//   1. The outer atomic-Load `s.getSessionID() == ""` is a fast-path
		//      filter — once any prior turn has captured an ID, every later
		//      turn skips the lock entirely (the steady-state cost is one
		//      atomic load).
		//   2. The inner re-check under sendMu enforces correctness when two
		//      concurrent passthrough turns both observe empty on the outer
		//      check: only the first to take sendMu calls onSessionID
		//      (which writes r.sessionIDToKey under r.mu).
		//
		// Without the inner re-check, the second turn could double-invoke
		// onSessionID with a stale-but-equal ID and (in tests) double-count
		// router-side maps. Without the outer check, every passthrough turn
		// would pay sendMu even after the ID is captured.
		//
		// Lock ordering: sendMu → r.mu (top-of-router.go contract). sendMu is
		// only held around the short CAS — it does not serialise the
		// passthrough turn itself, which is the whole point of passthrough.
		s.sendMu.Lock()
		if s.getSessionID() == "" {
			s.setSessionID(result.SessionID)
			if s.onSessionID != nil {
				s.onSessionID(result.SessionID)
			}
		}
		s.sendMu.Unlock()
	}
	return result, nil
}

// SupportsPassthrough exposes the underlying process's passthrough capability
// so the dispatcher can pick between passthrough and legacy Send per session
// (ACP-backed sessions fall back; Claude-backed sessions use passthrough).
func (s *ManagedSession) SupportsPassthrough() bool {
	proc := s.loadProcess()
	if proc == nil {
		return false
	}
	return proc.SupportsPassthrough()
}

// DiscardPassthroughPending delegates to the process's pending-slot cleanup.
// Called on /new, /clear, and forced session reset.
func (s *ManagedSession) DiscardPassthroughPending(reason error) {
	proc := s.loadProcess()
	if proc == nil {
		return
	}
	proc.DiscardPassthroughPending(reason)
}

// PassthroughDepth is a read-only view of pending slots for dashboard /
// status display.
func (s *ManagedSession) PassthroughDepth() int {
	proc := s.loadProcess()
	if proc == nil {
		return 0
	}
	return proc.PassthroughDepth()
}

// mapSendError translates Process-level errors into ManagedSession
// deathReason bookkeeping. Shared between Send and SendPassthrough so new
// error sentinels live in one place.
func (s *ManagedSession) mapSendError(proc processIface, err error) {
	switch {
	case errors.Is(err, cli.ErrNoOutputTimeout):
		storeAtomicString(&s.deathReason, "no_output_timeout")
	case errors.Is(err, cli.ErrTotalTimeout):
		storeAtomicString(&s.deathReason, "total_timeout")
	case errors.Is(err, cli.ErrProcessExited):
		reason := "process_exited"
		if dr := proc.DeathReason(); dr != "" {
			reason = dr
		}
		storeAtomicString(&s.deathReason, reason)
	}
}

// Send delivers a message to the claude process and returns the result.
// Messages to the same session are serialized via sendMu.
func (s *ManagedSession) Send(ctx context.Context, text string, images []cli.ImageData, onEvent cli.EventCallback) (*cli.SendResult, error) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	ctx, cancel := context.WithCancel(ctx)
	s.sendCancel.Store(&cancel)
	defer func() {
		s.sendCancel.Store(nil)
		cancel()
	}()

	s.touchLastActive()

	// Cache the user prompt for Snapshot (matches how process.go logs user events).
	prompt := textutil.TruncateRunes(text, 120)
	if len(images) > 0 {
		prompt += " [+" + strconv.Itoa(len(images)) + " image(s)]"
	}
	storeAtomicString(&s.lastPrompt, prompt)

	proc := s.loadProcess()
	if proc == nil {
		return nil, fmt.Errorf("session %s: %w", s.key, ErrNoActiveProcess)
	}

	// lastActivity tracking is handled lock-free by EventLog.Append via its
	// cached lastActivitySummary; Snapshot() reads that value when the process
	// is alive. Passing onEvent directly (no wrapper closure) avoids a per-Send
	// heap allocation on the nil-callback path (cron/connector) and one less
	// indirect call per event on the Send path.
	result, err := proc.Send(ctx, text, images, onEvent)
	if err != nil {
		s.mapSendError(proc, err)
		return nil, err
	}

	// Capture session ID from first successful send
	if s.getSessionID() == "" && result.SessionID != "" {
		s.setSessionID(result.SessionID)
		if s.onSessionID != nil {
			s.onSessionID(result.SessionID)
		}
	}
	return result, nil
}

// Interrupt sends SIGINT to the CLI process and cancels the current Send context.
// This is the equivalent of pressing Escape in Claude Code.
//
// proc.Interrupt() is called BEFORE cancel() to ensure the interrupted flag is
// set before a new Send() can start. proc.Interrupt() only acquires shimWMu
// (not sendMu), so there is no deadlock risk. The subsequent cancel() unblocks
// any in-flight Send() waiting on ctx.Done(), allowing it to release sendMu.
//
// If cancel() were called first, a new Send could race in before proc.Interrupt()
// sets the interrupted flag, causing drainStaleEvents to miss stale events from
// the interrupted turn — the old result would then be returned as the new turn's
// response.
func (s *ManagedSession) Interrupt() bool {
	proc := s.loadProcess()
	if proc == nil || !proc.Alive() {
		// Still cancel in case Send is blocked on ctx.Done().
		if cancel := s.sendCancel.Load(); cancel != nil {
			(*cancel)()
		}
		return false
	}

	proc.Interrupt()

	if cancel := s.sendCancel.Load(); cancel != nil {
		(*cancel)()
	}
	return true
}

// InterruptOutcome describes what happened on an InterruptViaControl call.
// Callers use this instead of a bare bool so log messages can reflect the
// actual state (e.g. don't claim "aborted turn" when nothing was running).
type InterruptOutcome int

const (
	// InterruptSent — a control_request reached the CLI; the active turn
	// will produce a final result shortly and the next Send() will drain it.
	InterruptSent InterruptOutcome = iota
	// InterruptNoSession — session does not exist or has no live process.
	InterruptNoSession
	// InterruptNoTurn — session is alive but idle; nothing was interrupted.
	InterruptNoTurn
	// InterruptUnsupported — protocol does not support stdin-level interrupt
	// (e.g. ACP). Callers may fall back to Interrupt() for SIGINT semantics.
	InterruptUnsupported
	// InterruptError — transport failure (shim socket dead, write broke);
	// the process-level settle flags have been rolled back. Callers should
	// log this as an error.
	InterruptError
)

// String renders an InterruptOutcome as a stable lowercase tag so slog
// attribute values stay grep-friendly across callers (cron / router /
// dashboard) instead of leaking the iota integer.
func (o InterruptOutcome) String() string {
	switch o {
	case InterruptSent:
		return "sent"
	case InterruptNoSession:
		return "no_session"
	case InterruptNoTurn:
		return "no_turn"
	case InterruptUnsupported:
		return "unsupported"
	case InterruptError:
		return "error"
	default:
		return fmt.Sprintf("unknown(%d)", int(o))
	}
}

// InterruptViaControl asks the CLI to abort the active turn by writing an
// in-band control_request to stdin. Unlike Interrupt, this does NOT cancel
// the Send() context — the in-flight Send will see the CLI's interrupted
// result event arrive naturally and return normally, so the owner loop can
// proceed to drain and send the coalesced follow-up messages on the same
// live process.
//
// Transport failures are logged at Warn here (rather than silently returned)
// so operators do not need every caller to plumb their own error log; the
// outcome return value still lets callers tune their user-facing text.
//
// Callers that need to inspect the underlying error (e.g. to errors.Is
// against a specific transport sentinel for triage) should call
// InterruptViaControlDetail instead — see R249-GO-18 (#916).
func (s *ManagedSession) InterruptViaControl() InterruptOutcome {
	outcome, _ := s.InterruptViaControlDetail()
	return outcome
}

// InterruptViaControlDetail mirrors InterruptViaControl but additionally
// returns the underlying error so callers can errors.Is against transport
// sentinels (e.g. distinguish a write-broken socket from a generic protocol
// error). Returned error semantics:
//
//   - InterruptSent       → nil
//   - InterruptNoSession  → nil (no live process to fail against)
//   - InterruptNoTurn     → cli.ErrNoActiveTurn
//   - InterruptUnsupported → cli.ErrInterruptUnsupported
//   - InterruptError      → the wrapped transport error (non-nil)
//
// R249-GO-18 (#916): pre-fix, InterruptError was opaque so cron / dispatch
// could not distinguish "shim socket dead, retry useless" from "stdin
// write returned EAGAIN, retry safe". Adding the err return lets each
// caller errors.Is on specific sentinels without breaking the existing
// outcome-only callers (those keep using InterruptViaControl, which now
// delegates to this method).
func (s *ManagedSession) InterruptViaControlDetail() (InterruptOutcome, error) {
	proc := s.loadProcess()
	if proc == nil || !proc.Alive() {
		return InterruptNoSession, nil
	}
	err := proc.InterruptViaControl()
	if err == nil {
		return InterruptSent, nil
	}
	switch {
	case errors.Is(err, cli.ErrNoActiveTurn):
		return InterruptNoTurn, err
	case errors.Is(err, cli.ErrInterruptUnsupported):
		// Caller decides whether to fall back; do not escalate to SIGINT
		// silently because that would couple two different semantics.
		return InterruptUnsupported, err
	default:
		// Transport / write error. Process.InterruptViaControl has already
		// rolled back the settle flags, so the next Send() will not spin
		// on the 500ms settle timeout. Surface at Warn so the failure mode
		// is visible even to callers that treat non-Sent as "fall back".
		slog.Warn("session interrupt via control_request failed",
			"key", s.key, "err", err)
		return InterruptError, err
	}
}

// getSessionID returns the session ID lock-free via atomic.Pointer[string].
//
// R230C-CR-5: there are three SessionID-shaped accessors across two
// packages — keep them in mind so future refactors don't accidentally
// drop one or introduce a fourth:
//
//   - ManagedSession.getSessionID — package-private; canonical lock-free
//     read of the session-level atomic. Used internally inside this file.
//   - ManagedSession.SessionID — public alias of getSessionID; satisfies
//     the cli.HistorySessionView interface (Wrapper.NewHistorySource
//     factory wiring) and is the right entry point for cross-package
//     callers in internal/server / internal/dispatch.
//   - cli.Process.GetSessionID — different layer entirely. Reads the CLI
//     subprocess's most-recently-observed session ID off the live event
//     stream. The two layers may briefly disagree during a /resume
//     handshake or first-Send capture; callers picking between them
//     should choose by intent: "what does naozhi remember for this chat
//     key" → ManagedSession.SessionID; "what does the CLI think the
//     active session is right now" → Process.GetSessionID.
func (s *ManagedSession) getSessionID() string {
	return loadAtomicString(&s.sessionID)
}

// SessionID returns the current CLI session ID, lock-free. Public alias
// for getSessionID used by the cli.HistorySessionView interface
// (Sprint 1a, Wrapper.NewHistorySource factory wiring) and any future
// caller that needs the current ID without taking r.mu. See
// getSessionID's godoc for the relationship with cli.Process.GetSessionID
// (R230C-CR-5).
func (s *ManagedSession) SessionID() string { return s.getSessionID() }

// setSessionID stores the session ID atomically.
func (s *ManagedSession) setSessionID(id string) {
	storeAtomicString(&s.sessionID, id)
}

// parseKeyParts lazily parses the immutable session key into cached components.
// Hand-rolled split avoids the []string allocation that strings.SplitN would
// produce — every new session triggers exactly one parseKeyParts on its first
// Snapshot, and dashboards poll dozens of sessions per second. (R227-PERF-13)
func (s *ManagedSession) parseKeyParts() {
	s.keyOnce.Do(func() {
		k := s.key
		idx := strings.IndexByte(k, ':')
		if idx < 0 {
			s.keyPlatform = k
			return
		}
		s.keyPlatform = k[:idx]
		k = k[idx+1:]
		idx = strings.IndexByte(k, ':')
		if idx < 0 {
			s.keyChatType = k
			return
		}
		s.keyChatType = k[:idx]
		k = k[idx+1:]
		idx = strings.IndexByte(k, ':')
		if idx < 0 {
			s.keyChatID = k
			return
		}
		s.keyChatID = k[:idx]
		s.keyAgentID = k[idx+1:]
	})
}

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

// HasProcess reports whether a process is currently attached to this
// session, regardless of liveness. Returns true even for processes
// that have exited but not yet been detached by the readLoop cleanup.
// Callers needing liveness should use isAlive() (private) or check
// State() == "ready"/"busy" via the Snapshot path. Lock-free read of
// the atomic.Pointer[processBox] backing field.
func (s *ManagedSession) HasProcess() bool {
	return s.loadProcess() != nil
}

// State returns just the live process state ("ready" / "busy" / etc.)
// without performing the SetModel mirror or building a full
// SessionSnapshot. Lock-free hot path for high-frequency observers
// (R230C-PERF-1: connector_subscribe ticks per agent_message_chunk
// event ~10-50/s and only needs State + DeathReason). Returns "ready"
// when no process is attached, mirroring Snapshot's no-proc branch.
func (s *ManagedSession) State() string {
	proc := s.loadProcess()
	if proc == nil {
		return "ready"
	}
	return proc.GetState().String()
}

// DeathReason returns the recorded death cause string ("" when the
// session is healthy or has not died yet). Companion to State() for
// connector_subscribe's session_state push so the change-detection
// branch can avoid a full Snapshot. R230C-PERF-1.
func (s *ManagedSession) DeathReason() string {
	return loadAtomicString(&s.deathReason)
}

// Snapshot returns a point-in-time view of this session.
//
// Side effect (R230C-CR-Diag / R229-GO-2): when a live process reports a
// non-empty Model() that disagrees with the persisted s.model field,
// Snapshot writes the live value back via SetModel before returning the
// view. This keeps the dashboard's model chip in sync with what the CLI
// is actually using (kiro reports the model only after session/new
// completes, not at spawn time). Callers that need a strictly read-only
// snapshot should not rely on this path; a future SnapshotReadOnly
// variant is tracked under R229-GO-2 once the dashboard polling cadence
// is moved to a dedicated mirror.
//
// Performance contract (R214-PERF-2 #411): Snapshot MUST NOT copy
// persistedHistory or any other O(N) backing structure. Dashboards poll
// at 1Hz × N tabs × M sessions, and at 500 entries × ~400 B each the
// per-call copy would burn ~200 KB of allocation per session per second.
// Scalar fields are cached via atomic.Pointer[string] (lastPrompt,
// lastActivity, lastResponse, deathReason, model) so the snapshot is
// O(1) regardless of session age. Callers that need the full event log
// must call EventEntries / EventLastN / EventEntriesSince explicitly,
// which is the cheap-rare-call path versus this hot poll path.
// snapshot_no_history_copy_test.go pins the contract.
func (s *ManagedSession) Snapshot() SessionSnapshot {
	s.parseKeyParts()
	snap := SessionSnapshot{
		Key:         s.key,
		Platform:    s.keyPlatform,
		ChatType:    s.keyChatType,
		ChatID:      s.keyChatID,
		Agent:       s.keyAgentID,
		SessionID:   s.getSessionID(),
		LastActive:  s.LastActive().UnixMilli(),
		CreatedAt:   s.createdAtMillis(),
		Workspace:   s.Workspace(),
		Backend:     s.Backend(),
		CLIName:     s.CLIName(),
		CLIVersion:  s.CLIVersion(),
		UserLabel:   s.UserLabel(),
		LabelOrigin: s.LabelOrigin(),
		// UI Round 5 R5-3: seed Model from persisted ManagedSession; the
		// proc-bearing branch below will overwrite if live proc has a
		// fresher value. No-proc snapshots (evicted / pre-spawn) keep
		// the persisted value so dashboard doesn't blink to
		// "(模型未配置)" during restart-reattach.
		Model: s.Model(),
	}
	snap.DeathReason = loadAtomicString(&s.deathReason)

	proc := s.loadProcess()
	sessCost := loadTotalCost(&s.totalCost)
	if proc == nil {
		snap.TotalCost = sessCost
		snap.State = "ready"
	} else {
		snap.State = proc.GetState().String()
		snap.Protocol = proc.ProtocolName()
		// UI Round 5 R5-3: model resolution priority
		//   1. live proc.Model() (claude system/init or kiro SpawnOptions)
		//   2. persisted s.Model() (post-restart, before next init)
		// When proc reports a model and it differs from / is more
		// recent than what we persisted, mirror it back so the next
		// saveStore tick captures it. Empty live → keep persisted.
		//
		// R226-CR-13: this SetModel is an intentional read-side write —
		// dashboard polls Snapshot at 1Hz and proc-reported model is the
		// authoritative source we need to ship into sessions.json.
		// Snapshot is otherwise read-only; if a future caller needs a
		// pure-read variant, factor a SnapshotReadOnly that skips this
		// mirror rather than dropping it (skipping silently regresses to
		// the symptom Round 5 R5-3 fixed: dashboard "model 未配置" blink
		// after spawn until the first result event triggered a save).
		liveModel := proc.Model()
		if liveModel != "" {
			// SetModel internally short-circuits when the value is unchanged,
			// so the outer equality check would just duplicate that work.
			s.SetModel(liveModel)
			snap.Model = liveModel
		} else {
			snap.Model = s.Model()
		}
		// Prefer whichever is larger: a freshly resumed process reports 0
		// until the first `result` event arrives, but s.totalCost carries
		// the historical cumulative value restored from sessions.json.
		// Claude CLI's total_cost_usd under --resume is cumulative, so once
		// the next result lands, proc.TotalCost() will be >= s.totalCost
		// and the display won't regress.
		if pc := proc.TotalCost(); pc > sessCost {
			snap.TotalCost = pc
		} else {
			snap.TotalCost = sessCost
		}
		snap.Subagents = proc.TurnAgents()
		// Prefer the EventLog-maintained summary (updated lock-free on every
		// event) so we don't need a wrapper closure around Send just to track
		// lastActivity.
		snap.LastActivity = proc.LastActivitySummary()
		// R110-P1: live process is the authoritative source for the most-recent
		// assistant text reply. Empty when no text block has streamed yet
		// (post-spawn pre-result window); the s.lastResponse fallback below
		// covers the post-restart / pre-replay case via scanLastSummaries seed.
		snap.LastResponse = proc.LastResponseSummary()
		// MessageCount is the cumulative user turn count observed by the
		// current Process since its last spawn. proc==nil branch leaves the
		// field at zero so UI code can gate visibility on `> 0` and skip the
		// chip for brand-new sessions that haven't yet received a prompt.
		snap.MessageCount = proc.UserTurnCount()

		// Normalize layer (docs/rfc/multi-backend.md §8.8). Process getters
		// return zero values for fields the backend never reports, so
		// `> 0` gating in dashboard.js works for both claude (most fields
		// zero today) and kiro (all fields populated).
		snap.ContextUsagePercent = proc.ContextUsagePercent()
		snap.TurnDurationMs = proc.TurnDurationMs()
		snap.MeteringUsage = proc.MeteringUsage()
	}

	// CostUnit is derived from backend even when proc is nil so an evicted
	// session still renders the right cost label until pruning. claude is
	// the default for legacy stores predating the Backend field.
	snap.CostUnit = costUnitForBackend(snap.Backend)

	// UI Round 5 R5-4: when CostUnit is "credits" (kiro family) the
	// dashboard's header cost cell should show the SESSION-level total,
	// not per-turn. claude path keeps snap.TotalCost from CLI's own
	// running total (USD). For kiro we derive it from the accumulated
	// MeteringUsage (Process.applyMetadata is now session-level).
	if snap.CostUnit == "credits" && len(snap.MeteringUsage) > 0 {
		var credits float64
		for _, m := range snap.MeteringUsage {
			if m.Unit == "credit" || m.Unit == "credits" {
				credits += m.Value
			}
		}
		// Only override when we found a credit-typed entry; if kiro ever
		// emits a non-credit unit (token / cost) under cost_unit=credits,
		// don't silently zero the running total.
		if credits > 0 {
			snap.TotalCost = credits
		}
	}

	// Read cached values instead of copying the full event log.
	if lp := loadAtomicString(&s.lastPrompt); lp != "" {
		snap.LastPrompt = lp
	}
	if snap.LastActivity == "" {
		if la := loadAtomicString(&s.lastActivity); la != "" {
			snap.LastActivity = la
		}
	}
	// R110-P1: only fall back to the cached lastResponse when the live process
	// hasn't yet reported one. Mirrors the LastPrompt/LastActivity priority
	// (live wins, cache survives restart). Empty cache + empty live leaves the
	// field unset → JSON omitempty hides the dim line on brand-new sessions.
	if snap.LastResponse == "" {
		if lr := loadAtomicString(&s.lastResponse); lr != "" {
			snap.LastResponse = lr
		}
	}

	return snap
}

// hasInjectedHistory reports whether persistedHistory contains any entries.
// Used by the startup history loader (R53-ARCH-001 fix) to decide whether
// the deferred JSONL backfill path is needed: if ReconnectShims already
// injected history via proc.InjectHistory → s.InjectHistory's
// persistedHistory append, the flag is set and we skip the redundant FS
// read. Read-only, no copy — callers just need a boolean.
func (s *ManagedSession) hasInjectedHistory() bool {
	s.historyMu.RLock()
	defer s.historyMu.RUnlock()
	return len(s.persistedHistory) > 0
}

// EventEntries returns the event log entries for this session.
// Returns persisted history when the process is nil or dead.
func (s *ManagedSession) EventEntries() []cli.EventEntry {
	proc := s.loadProcess()
	if proc != nil {
		return proc.EventEntries()
	}
	s.historyMu.RLock()
	out := make([]cli.EventEntry, len(s.persistedHistory))
	copy(out, s.persistedHistory)
	s.historyMu.RUnlock()
	return out
}

// SubagentLinker returns the SubagentLinker owned by the live *cli.Process,
// or nil when the session is not backed by a live Claude-CLI process (fake
// test process, dead process, ACP protocol, etc.). Callers must guard the
// nil return — the agent-team UI endpoints downgrade to 404 in that case.
//
// Intentionally type-asserts rather than widening processIface so the fake
// processes in router/managed tests don't need to implement the full Linker
// surface. The downside — a test process that wants real linker behaviour
// must wrap *cli.Process directly — is acceptable because the linker's own
// unit tests in internal/cli/subagent_link_test.go are the canonical spot
// for that coverage.
//
// R239-ARCH-I: the consumer-facing interface lives at
// internal/session/agentlink.AgentLinker — server stores wired linkers
// keyed on that interface. ManagedSession still returns the concrete
// *cli.SubagentLinker so callers that need the full linker surface
// (SeedFromHistory / Resolve / SetContext / ConfigureForTest, used by
// the cli package itself plus its tests) keep working without an extra
// type assertion. The interface widens only at the server boundary.
//
// TODO: introduce AgentIntrospector interface when a second backend needs
// agent-view support. Tracked in docs/TODO.md (R214-CODE-6 / R217-ARCH-2 /
// R219-ARCH-3 — the lifecycle question, distinct from the consumer-side
// interface R239-ARCH-I now solves). The three live anchors above cover
// the same root; the orphan-id reference (R245-CR-008) was retired in
// favour of pointing at the live anchors directly.
func (s *ManagedSession) SubagentLinker() *cli.SubagentLinker {
	if real := s.loadCliProcess(); real != nil {
		return real.Linker()
	}
	return nil
}

// AgentEventLog exposes the live *cli.EventLog so the server-side tailer
// registry can install its task_done hook. nil for fake processes / dead
// sessions, same policy as SubagentLinker above.
func (s *ManagedSession) AgentEventLog() *cli.EventLog {
	if real := s.loadCliProcess(); real != nil {
		return real.EventLog()
	}
	return nil
}

// loadCliProcess returns the live *cli.Process when the session is backed by
// one, nil otherwise (fake test process, dead session, ACP protocol, etc.).
func (s *ManagedSession) loadCliProcess() *cli.Process {
	proc := s.loadProcess()
	if proc == nil {
		return nil
	}
	real, ok := proc.(*cli.Process)
	if !ok {
		return nil
	}
	return real
}

// EventLastN returns the most recent n event entries.
func (s *ManagedSession) EventLastN(n int) []cli.EventEntry {
	proc := s.loadProcess()
	if proc != nil {
		return proc.EventLastN(n)
	}
	s.historyMu.RLock()
	defer s.historyMu.RUnlock()
	if n <= 0 || n >= len(s.persistedHistory) {
		out := make([]cli.EventEntry, len(s.persistedHistory))
		copy(out, s.persistedHistory)
		return out
	}
	start := len(s.persistedHistory) - n
	out := make([]cli.EventEntry, n)
	copy(out, s.persistedHistory[start:])
	return out
}

// sortEntriesByTimeStable sorts entries in-place by Time ascending using a
// stable sort so that entries sharing the same Time keep their insertion
// order (matters for InjectHistory batches where a whole chain replay may
// collapse to a single default timestamp). Callers of EventEntriesSince /
// EventEntriesBefore depend on chronological output — the ring buffer and
// persistedHistory themselves don't guarantee strict ordering because
// (a) InjectHistory may interleave segments from multiple session chains
// and (b) AppendBatch assigns a single wall-clock to zero-Time entries
// while older entries might still arrive with real earlier timestamps
// from resume paths.
func sortEntriesByTimeStable(entries []cli.EventEntry) {
	if len(entries) < 2 {
		return
	}
	slices.SortStableFunc(entries, func(a, b cli.EventEntry) int {
		return cmp.Compare(a.Time, b.Time)
	})
}

// EventEntriesSince returns the event log entries with Time > afterMS in
// chronological order.
//
// Live-process branch: proc.EventEntriesSince is backed by cli.EventLog's
// ring buffer, which records entries in strict append order. Append stamps
// zero-Time entries with now and AppendBatch uses a single now for the
// batch, so Time is weakly monotonic by construction and no re-sort is
// needed. This is the WS push hot path (wshub.go emits on every notify
// tick), so avoiding an O(n)+sort here matters.
//
// Dead-session branch: persistedHistory is NOT guaranteed sorted because
// InjectHistory may interleave segments from multiple session chains
// (startup backfill replays prev-session IDs in reverse-chain order).
// We do a full linear scan + stable sort so paginated fetches see
// chronological output.
func (s *ManagedSession) EventEntriesSince(afterMS int64) []cli.EventEntry {
	proc := s.loadProcess()
	if proc != nil {
		return proc.EventEntriesSince(afterMS)
	}
	// Skip the stable sort once the maintained invariant says
	// persistedHistory is already in Time order. Steady-state dashboard
	// polling (1Hz × N tabs × M dead sessions) used to pay this every
	// call; the in-place sort under historyMu also blocks concurrent
	// readers. While the flag is still false (initial state, or after an
	// out-of-order InjectHistory) we promote to the write lock once, sort
	// in place, set the flag, and downgrade — subsequent reads then take
	// the cheap RLock-only path. R237-PERF-12.
	s.historyMu.RLock()
	if !s.persistedHistorySorted {
		s.historyMu.RUnlock()
		s.historyMu.Lock()
		// Re-check under the write lock — another reader may have already
		// sorted between the unlock and re-acquire.
		if !s.persistedHistorySorted {
			sortEntriesByTimeStable(s.persistedHistory)
			s.persistedHistorySorted = true
		}
		s.historyMu.Unlock()
		s.historyMu.RLock()
	}
	var out []cli.EventEntry
	for _, e := range s.persistedHistory {
		if e.Time > afterMS {
			out = append(out, e)
		}
	}
	s.historyMu.RUnlock()
	return out
}

// EventEntriesBefore returns up to `limit` entries with Time < beforeMS
// drawn from the in-memory log (live process ring or persistedHistory).
// Entries are returned in chronological order.
//
// Scope: memory-tier only. Does NOT consult the backend's disk-tier
// history.Source — callers that need complete historical coverage should
// use EventEntriesBeforeCtx which falls back to disk when memory is
// exhausted. This split preserves the legacy call sites (tests, internal
// helpers) that can't easily thread a context through.
//
// The live-process branch relies on EventLog's insertion-order ring which
// is already chronological (Append/AppendBatch assign monotonic Time to
// zero-Time entries), so it returns without re-sorting. Only the
// persistedHistory branch pays for a stable sort because startup chain
// replay may interleave segments.
//
// beforeMS <= 0 is treated as "no upper bound" — equivalent to the tail
// of the log, matching EventLastN semantics. limit <= 0 returns nil.
func (s *ManagedSession) EventEntriesBefore(beforeMS int64, limit int) []cli.EventEntry {
	if limit <= 0 {
		return nil
	}
	proc := s.loadProcess()
	if proc != nil {
		return proc.EventEntriesBefore(beforeMS, limit)
	}
	out := s.persistedHistoryBefore(beforeMS, limit)
	sortEntriesByTimeStable(out)
	return out
}

// EventEntriesBeforeCtx extends EventEntriesBefore with a disk-tier
// fallback. When the in-memory log has no entries strictly older than
// beforeMS, the session's history.Source is consulted. This is the path
// the dashboard pagination handler takes; legacy non-ctx callers still
// use the memory-only variant.
//
// The two tiers are never merged: the memory tier is authoritative for
// any range it covers (since it includes naozhi-synthesized events like
// LogSystemEvent that never reach disk), and falling through to disk
// only when memory is empty keeps the result strictly chronological
// without a deduplication step. The trade-off is one extra round trip
// on the page that straddles the memory-bottom; on all subsequent pages
// memory returns empty and disk is queried directly.
func (s *ManagedSession) EventEntriesBeforeCtx(ctx context.Context, beforeMS int64, limit int) []cli.EventEntry {
	if limit <= 0 {
		return nil
	}
	if mem := s.EventEntriesBefore(beforeMS, limit); len(mem) > 0 {
		return mem
	}
	src := s.loadHistorySource()
	if src == nil {
		return nil
	}
	entries, err := src.LoadBefore(ctx, beforeMS, limit)
	if err != nil {
		// Treat as end-of-history — logging (not propagating) matches the
		// existing JSONL load sites in router.go which also degrade silently
		// on read errors.
		slog.Warn("history source load failed", "key", s.key, "err", err)
		return nil
	}
	sortEntriesByTimeStable(entries)
	return entries
}

// persistedHistoryBefore collects up to `limit` entries from persistedHistory
// strictly older than beforeMS. Returns entries in insertion order — the
// caller is responsible for the final sort. Only relevant when proc is nil;
// live-process sessions go through proc.EventEntriesBefore directly.
func (s *ManagedSession) persistedHistoryBefore(beforeMS int64, limit int) []cli.EventEntry {
	if limit <= 0 {
		return nil
	}
	s.historyMu.RLock()
	defer s.historyMu.RUnlock()
	if len(s.persistedHistory) == 0 {
		return nil
	}
	// Walk backward collecting up to `limit` entries strictly older than
	// beforeMS. persistedHistory is not guaranteed to be sorted, so a full
	// linear walk is the conservative choice.
	out := make([]cli.EventEntry, 0, limit)
	for i := len(s.persistedHistory) - 1; i >= 0 && len(out) < limit; i-- {
		e := s.persistedHistory[i]
		if beforeMS > 0 && e.Time >= beforeMS {
			continue
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil
	}
	// Order does not matter: the only caller (EventEntriesBefore) pipes
	// this through sortEntriesByTimeStable, which overrides whatever
	// order we produce here. The prior code reversed `out` to restore
	// insertion order, but stable-sort-by-Time then re-orders by Time
	// making the reversal pure waste. Leave the reverse-walk order.
	return out
}

// SubscribeEvents subscribes to event log notifications for this session.
// If the session has no process, returns a closed channel and a no-op unsubscribe.
func (s *ManagedSession) SubscribeEvents() (<-chan struct{}, func()) {
	proc := s.loadProcess()
	if proc == nil {
		ch := make(chan struct{})
		close(ch)
		return ch, func() {}
	}
	return proc.SubscribeEvents()
}

// LogSystemEvent appends a single "system"-typed EventEntry with the given
// summary text to this session's event log and notifies subscribers. Used by
// off-main-path writers (e.g. upstream/connector's async Send goroutine)
// that would otherwise lose errors to log.Warn while the primary has
// already told the UI "accepted". Dashboard renders system events as
// esc(e.summary), so the text is safe to contain arbitrary error messages.
//
// Semantics:
//   - proc != nil: appends to the live EventLog; push-subscribers (WS
//     eventPushLoop) wake immediately.
//   - proc == nil (suspended session): appends to persistedHistory so the
//     entry shows up on the next subscribe/snapshot. Still bounded by
//     maxPersistedHistory; the oldest entry is dropped if full.
//
// Empty summary is rejected (no-op) to avoid polluting the log with blank
// system lines on programmer error. R49-REL-CONNECTOR-SEND-RESULT-LOSS.
func (s *ManagedSession) LogSystemEvent(summary string) {
	if summary == "" {
		return
	}
	entry := cli.EventEntry{
		Time:    time.Now().UnixMilli(),
		Type:    "system",
		Summary: summary,
	}
	// Reuse InjectHistory so proc/persistedHistory routing stays in one
	// place and subscribers wake via the existing notifySubscribers path.
	s.InjectHistory([]cli.EventEntry{entry})
}

// InjectHistory pre-populates the event log with historical entries.
// Entries are saved to persistedHistory so they survive process restarts.
func (s *ManagedSession) InjectHistory(entries []cli.EventEntry) {
	if len(entries) > maxPersistedHistory {
		slog.Debug("inject history: batch exceeds cap, truncating oldest",
			"key", s.key,
			"batch_len", len(entries),
			"cap", maxPersistedHistory,
			"dropped", len(entries)-maxPersistedHistory)
		entries = entries[len(entries)-maxPersistedHistory:]
	}
	// Scan the injected batch for prompt/activity summaries outside the lock:
	// the scan operates on the caller-supplied slice only (not persistedHistory),
	// and the only side-effects are atomic.Pointer[string] Store calls. Keeping
	// it out of historyMu lets concurrent readers (EventEntries / EventEntriesSince
	// / EventEntriesBefore) proceed during 500-entry JSONL replays at startup.
	// R61-PERF-9.
	prompt, activity, response := scanLastSummaries(entries)

	// Mutate persistedHistory AND read s.process under the same historyMu
	// hold so a concurrent attachProcessAndSnapshotPersisted (also serialised
	// on historyMu) cannot stamp seededLen between our load-process and our
	// forward-decision: either it ran first (we observe the new proc and
	// seededLen=full snapshot, forward only genuinely-new tail) or it runs
	// after (we observe proc=nil, defer forwarding to the upcoming attach,
	// which will snapshot our just-appended entries).
	//
	// proc.InjectHistory itself is invoked AFTER releasing historyMu — it
	// takes proc.eventLog.mu and we never want two long locks held at once.
	// R191-GO-M1's "reload proc after unlock" concern is no longer relevant:
	// a fresh proc replacing the current one happens through attach helpers
	// that share historyMu, so the in-lock loadProcess() is the authoritative
	// snapshot for this caller.
	//
	// Stale proc note (R231-CQ-7): if the proc captured here was already
	// orphaned by a concurrent storeProcess(nil) during ResetChat / Remove,
	// proc.InjectHistory below still mutates that orphan's EventLog ring,
	// but no one calls EventEntries() on an orphan — Router.loadProcess()
	// returns the new pointer and dashboards/cron snapshot through that.
	// The orphan ring is GC'd when the last reference (this closure)
	// drops, so the extra append is a harmless no-op rather than a leak.
	s.historyMu.Lock()
	// Monotonicity check (R237-PERF-12): when persistedHistory is empty
	// or already known sorted AND the appended batch is internally sorted
	// w.r.t. the existing tail, the flag stays/becomes true and
	// dead-session readers can skip the per-call stable sort. Out-of-order
	// entries leave the flag false, falling back to the lazy sort-on-read
	// path that EventEntriesSince still implements. Steady-state
	// Send/result append is monotonic by construction (Append/AppendBatch
	// stamp now), so the common path costs only this O(batch) scan.
	if s.persistedHistorySorted || len(s.persistedHistory) == 0 {
		monotonic := true
		var prevTime int64
		if n := len(s.persistedHistory); n > 0 {
			prevTime = s.persistedHistory[n-1].Time
		}
		for _, e := range entries {
			if e.Time < prevTime {
				monotonic = false
				break
			}
			prevTime = e.Time
		}
		if monotonic {
			s.persistedHistorySorted = true
		} else {
			s.persistedHistorySorted = false
		}
	}
	s.persistedHistory = append(s.persistedHistory, entries...)
	if trimmed := len(s.persistedHistory) - maxPersistedHistory; trimmed > 0 {
		s.persistedHistory = s.persistedHistory[trimmed:]
		// Cap-trim shifts the prefix backwards; clamp seededLen so it keeps
		// pointing at "tail-end of what proc has already seen" rather than
		// past the new start.
		//
		// R231-CQ-9 — degrade-to-reseed semantic: when trimmed > seededLen
		// the clamp lands on 0, which means the next forward span below will
		// re-emit the entire post-trim ring (including some entries the proc
		// has already observed). This is intentional: after a cap-trim the
		// "exact already-seen prefix" is no longer recoverable (its leading
		// entries were dropped), so we choose duplicate forwarding over data
		// loss. The duplication only fires when the injected batch by itself
		// exceeds maxPersistedHistory minus what proc already saw — i.e.
		// boot-time JSONL replay of >cap entries; steady-state Send/result
		// flow stays well under the cap and preserves the no-duplicate
		// guarantee.
		if s.persistedSeededLen >= trimmed {
			s.persistedSeededLen -= trimmed
		} else {
			s.persistedSeededLen = 0
		}
	}
	proc := s.loadProcess()
	// R237-PERF-6 (#667): capture only the bounds of the forward window
	// under historyMu; defer the make+copy to AFTER Unlock so concurrent
	// EventEntries / EventEntriesSince RLockers do not stall on a
	// 500-entry replay's allocation+memcpy. Safety: subsequent
	// InjectHistory calls cannot mutate slots the captured `tail` slice
	// points at — append only writes past the current len, and cap-trim
	// merely reslices the header. A reallocating append leaves the old
	// backing array referenced by `tail` alive for GC; element data at
	// [seededLen..end) is never overwritten in place anywhere in the
	// codebase (verified by Grep on persistedHistory[ writes — only
	// `s.persistedHistory = …` reslices the header). seededLen is
	// committed under the lock so no second InjectHistory can re-forward
	// the same entries.
	var tail []cli.EventEntry
	if proc != nil && s.persistedSeededLen < len(s.persistedHistory) {
		tail = s.persistedHistory[s.persistedSeededLen:]
		s.persistedSeededLen = len(s.persistedHistory)
	}
	s.historyMu.Unlock()

	if len(tail) > 0 {
		// Defensive copy outside historyMu: proc.InjectHistory consumes
		// the slice and may outlive this call, while the caller's
		// entries slice and `tail`'s backing array are owned by us — a
		// fresh allocation severs both ties cleanly.
		forward := make([]cli.EventEntry, len(tail))
		copy(forward, tail)
		proc.InjectHistory(forward)
	}

	// Update cached snapshot values only if not yet set by Send. Each Store
	// is atomic so no lock is needed; the "only set if empty" check is a
	// benign TOCTOU — a concurrent Send writing the same field races, but
	// both values are "most recent" views and whichever lands is acceptable.
	if prompt != "" && loadAtomicString(&s.lastPrompt) == "" {
		storeAtomicString(&s.lastPrompt, prompt)
	}
	if activity != "" && loadAtomicString(&s.lastActivity) == "" {
		storeAtomicString(&s.lastActivity, activity)
	}
	// R110-P1: seed lastResponse alongside lastPrompt/lastActivity. Same
	// "only set if empty" guard so a concurrent live Send that already
	// stamped a fresher response doesn't get clobbered by historical replay.
	if response != "" && loadAtomicString(&s.lastResponse) == "" {
		storeAtomicString(&s.lastResponse, response)
	}
}

// extractLastPromptFromProcess scans the attached process's event log to populate
// lastPrompt, lastActivity, and lastResponse when they haven't been set yet
// (e.g. after shim reconnect where events were injected directly into the
// process, bypassing InjectHistory).
func (s *ManagedSession) extractLastPromptFromProcess() {
	if loadAtomicString(&s.lastPrompt) != "" &&
		loadAtomicString(&s.lastActivity) != "" &&
		loadAtomicString(&s.lastResponse) != "" {
		return
	}
	p := s.loadProcess()
	if p == nil {
		return
	}
	prompt, activity, response := scanLastSummaries(p.EventEntries())
	if prompt != "" && loadAtomicString(&s.lastPrompt) == "" {
		storeAtomicString(&s.lastPrompt, prompt)
	}
	if activity != "" && loadAtomicString(&s.lastActivity) == "" {
		storeAtomicString(&s.lastActivity, activity)
	}
	if response != "" && loadAtomicString(&s.lastResponse) == "" {
		storeAtomicString(&s.lastResponse, response)
	}
}

// scanLastSummaries walks entries in reverse, returning the most-recent
// user-prompt summary, activity summary, and assistant response summary.
// Stops early once all three are found. Used by InjectHistory and
// extractLastPromptFromProcess to seed the atomic caches after replay.
//
// R110-P1: response capture extends the existing prompt/activity scan so
// suspended/dead sessions still surface a sidebar second-line preview after
// shim reconnect (which replays history into a fresh EventLog).
func scanLastSummaries(entries []cli.EventEntry) (prompt, activity, response string) {
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if prompt == "" && e.Type == "user" {
			prompt = e.Summary
		}
		if activity == "" && cli.IsActivityType(e.Type) {
			activity = e.Summary
		}
		if response == "" && e.Type == "text" {
			response = e.Summary
		}
		if prompt != "" && activity != "" && response != "" {
			break
		}
	}
	return prompt, activity, response
}

// costUnitForBackend returns the SessionSnapshot.CostUnit value for a given
// backend. claude-class backends report cost in USD via Process.TotalCost();
// ACP-class kiro reports per-turn metering in credits via _kiro.dev/metadata.
// Empty backend (legacy stores predating the Backend field) defaults to USD
// because such stores are necessarily claude-only.
//
// The actual unit string lives on backend.Profile.CostUnit, looked up via
// backend.Get. Adding a new backend means setting CostUnit on its profile —
// no edit here required (R225-CR-4 / R224-ARCH-1). The dashboard reads this
// value as the source of truth for cost-cell formatting (see
// docs/rfc/multi-backend.md §8.3 D5).
func costUnitForBackend(backendID string) string {
	// Legacy stores predating the Backend field — claude-only.
	if backendID == "" {
		backendID = "claude"
	}
	// Lazy bootstrap pattern (matches server.replyTagForBackend): production
	// wires backend.RegisterDefaults() in cmd/naozhi/main.go before any
	// session is constructed. Tests that build a Snapshot without calling
	// RegisterDefaults would otherwise see backend.Get return false and lose
	// the unit — costUnitForBackendOnce ensures one-shot lazy registration so
	// tests stay green. Guard with a registry-empty check so we cooperate
	// with sibling tests (server pkg withDefaultBackends) that already
	// pre-registered, rather than panicking on duplicate Register.
	costUnitForBackendOnce.Do(func() {
		if len(backend.All()) == 0 {
			backend.RegisterDefaults()
		}
	})
	if p, ok := backend.Get(backendID); ok {
		return p.CostUnit
	}
	// Unregistered backend ID (e.g. config typo, in-progress backend not
	// yet wired into RegisterDefaults). Returning "" makes the dashboard
	// hide the cost cell rather than render a misleading unit.
	return ""
}

var costUnitForBackendOnce sync.Once
