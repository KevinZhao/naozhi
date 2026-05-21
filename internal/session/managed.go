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

// processIface abstracts the CLI process lifecycle methods used by the router
// and session layer. *cli.Process satisfies this interface.
type processIface interface {
	Alive() bool
	IsRunning() bool
	Close()
	Kill()
	Interrupt()
	// InterruptViaControl asks the CLI to abort the active turn via an
	// in-band stream-json control_request (no SIGINT, no process kill).
	// Returns cli.ErrInterruptUnsupported for protocols without this primitive.
	InterruptViaControl() error
	Send(ctx context.Context, text string, images []cli.ImageData, onEvent cli.EventCallback) (*cli.SendResult, error)
	// SendPassthrough is the passthrough-mode Send. Callers must ensure the
	// underlying protocol reports SupportsReplay()==true; otherwise this
	// returns an error. Unlike Send, multiple goroutines may call this
	// concurrently on the same process — ordering is handled by the CLI's
	// internal commandQueue plus a naozhi-side sendSlot FIFO.
	SendPassthrough(ctx context.Context, text string, images []cli.ImageData, onEvent cli.EventCallback, priority string) (*cli.SendResult, error)
	// DiscardPassthroughPending cancels all in-flight passthrough sends and
	// fires the given error to each caller. Used on /new, /clear, or forced
	// session reset.
	DiscardPassthroughPending(reason error)
	// PassthroughDepth returns the current pending-slot count for dashboard/
	// status display.
	PassthroughDepth() int
	// SupportsPassthrough reports whether this process's protocol can operate
	// in passthrough mode (i.e. Protocol.SupportsReplay()). Dispatch uses
	// this to fall back to legacy Send when the protocol can't provide the
	// replay events passthrough matching relies on.
	SupportsPassthrough() bool
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

// processBox wraps processIface for use with atomic.Pointer (which requires a concrete type).
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

	// lastPrompt caches the most recent user message summary (atomic for lock-free Snapshot reads).
	lastPrompt atomic.Pointer[string]

	// lastActivity caches the most recent tool_use/thinking summary.
	lastActivity atomic.Pointer[string]

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
	// already been forwarded into the *current* proc.EventLog via the
	// ReattachProcess catch-up. It is reset to 0 on every storeProcess(new)
	// (under historyMu) so a fresh proc starts with seededLen=0 and gets the
	// full persistedHistory snapshot at attach time.
	//
	// Why this exists: without it, kiro (and any non-claude backend) sessions
	// that get reconnected via ReconnectShims after a naozhi restart end up
	// with an empty proc.EventLog while persistedHistory holds the real
	// pre-restart conversation — naozhilog tier1 already wrote it but the
	// proc didn't exist yet. Subsequent dashboard polls hit the live proc
	// branch of EventEntries / EventEntriesBefore, see the empty ring, and
	// the user "loses" their history after the first message lands in the
	// new proc.
	//
	// InjectHistory uses this to decide whether each batch is "new tail" that
	// must be forwarded to proc, vs. a re-injection of the already-seeded
	// prefix that would double-render the same entries. Read/written under
	// historyMu so it stays in sync with persistedHistory append/snapshot.
	persistedSeededLen int

	// prevSessionIDs tracks previous session IDs for this key (oldest → newest).
	// Used on startup to load the full conversation chain from JSONL files.
	// Capped at maxPrevSessionIDs to bound long-lived session memory and
	// sessions.json size. Overflow drops oldest entries; history still loads
	// from the retained tail which carries the most recent context.
	prevSessionIDs []string

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
func loadTotalCost(v *atomic.Uint64) float64 {
	return math.Float64frombits(v.Load())
}

// storeTotalCost writes a float64 cumulative cost via atomic.Uint64,
// encoding through math.Float64bits. Paired with loadTotalCost to keep the
// packing/unpacking convention in one place — R183-CONCUR-M2 made the
// field atomic to harden against future post-publication writers, and
// having a helper keeps call sites free of bit-level noise.
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

// SnapshotChainIDs returns the session-ID chain (oldest → newest) under
// historyMu so concurrent mutation of prevSessionIDs doesn't produce a
// torn slice. The current session ID is appended only when non-empty —
// a just-spawned session that hasn't captured its first ID yet yields
// the prev chain alone, which matches how router.go builds the chain
// for JSONL loads today.
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

func (s *ManagedSession) loadProcess() processIface {
	if box := s.process.Load(); box != nil {
		return box.p
	}
	return nil
}

func (s *ManagedSession) storeProcess(p processIface) {
	if p == nil {
		s.process.Store(nil)
	} else {
		s.process.Store(&processBox{p: p})
	}
}

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

	if proc != nil && len(snapshot) > 0 {
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
	if proc != nil && len(snapshot) > 0 {
		proc.InjectHistory(snapshot)
	}
}

// adoptProcessAlreadySeeded publishes proc and marks the entire current
// persistedHistory as already-seeded into proc.EventLog. Used by Rename /
// takeover paths where the proc was running under a different ManagedSession
// and already has the matching entries in its ring; we must NOT re-inject
// (would duplicate every bubble) but we DO need persistedSeededLen aligned
// so the next InjectHistory tail still forwards.
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
func (s *ManagedSession) attachProcessAndSnapshotPersisted(proc processIface) []cli.EventEntry {
	s.historyMu.Lock()
	if proc == nil {
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

// SendPassthrough is the concurrent-capable Send for passthrough mode.
// Unlike Send, this does NOT acquire sendMu — the CLI's internal commandQueue
// plus the Process-level sendSlot FIFO provide ordering, and serializing at
// this layer would defeat passthrough's whole point (instant dispatch, tool-
// boundary mid-turn injection).
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
func (s *ManagedSession) InterruptViaControl() InterruptOutcome {
	proc := s.loadProcess()
	if proc == nil || !proc.Alive() {
		return InterruptNoSession
	}
	err := proc.InterruptViaControl()
	if err == nil {
		return InterruptSent
	}
	switch {
	case errors.Is(err, cli.ErrNoActiveTurn):
		return InterruptNoTurn
	case errors.Is(err, cli.ErrInterruptUnsupported):
		// Caller decides whether to fall back; do not escalate to SIGINT
		// silently because that would couple two different semantics.
		return InterruptUnsupported
	default:
		// Transport / write error. Process.InterruptViaControl has already
		// rolled back the settle flags, so the next Send() will not spin
		// on the 500ms settle timeout. Surface at Warn so the failure mode
		// is visible even to callers that treat non-Sent as "fall back".
		slog.Warn("session interrupt via control_request failed",
			"key", s.key, "err", err)
		return InterruptError
	}
}

// getSessionID returns the session ID lock-free via atomic.Pointer[string].
func (s *ManagedSession) getSessionID() string {
	return loadAtomicString(&s.sessionID)
}

// SessionID returns the current CLI session ID, lock-free. Public alias
// for getSessionID used by the cli.HistorySessionView interface
// (Sprint 1a, Wrapper.NewHistorySource factory wiring) and any future
// caller that needs the current ID without taking r.mu.
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
	Model        string  `json:"model,omitempty"`
	LastActive   int64   `json:"last_active"` // unix ms
	TotalCost    float64 `json:"total_cost"`
	Workspace    string  `json:"workspace,omitempty"`
	DeathReason  string  `json:"death_reason,omitempty"`
	ChatType     string  `json:"chat_type,omitempty"`
	ChatID       string  `json:"chat_id,omitempty"`
	Node         string  `json:"node,omitempty"`
	LastPrompt   string  `json:"last_prompt,omitempty"`   // most recent user message
	LastActivity string  `json:"last_activity,omitempty"` // most recent tool/thinking status
	Summary      string  `json:"summary,omitempty"`       // Claude-generated session title
	UserLabel    string  `json:"user_label,omitempty"`    // operator-set override for sidebar/header title
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

func (s *ManagedSession) HasProcess() bool {
	return s.loadProcess() != nil
}

// Snapshot returns a point-in-time view of this session.
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
// TODO(RFC v4 phase 3+, tracked in docs/TODO.md R214-CODE-6 / R217-ARCH-2 /
// R219-ARCH-3): if a second backend needs internal agent-view support
// (e.g. ACP / Kiro),
// abstract via:
//
//	type AgentIntrospector interface {
//	    Linker() *cli.SubagentLinker
//	    EventLog() *cli.EventLog
//	}
//
// and have *cli.Process implement it, then switch this to a type-safe
// assertion. Today only stream-json-backed Claude writes the on-disk
// transcript, so the cost of the abstraction is not warranted yet.
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
	var out []cli.EventEntry
	s.historyMu.RLock()
	for _, e := range s.persistedHistory {
		if e.Time > afterMS {
			out = append(out, e)
		}
	}
	s.historyMu.RUnlock()
	sortEntriesByTimeStable(out)
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
		slog.Debug("InjectHistory: batch exceeds cap, truncating oldest",
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
	prompt, activity := scanLastSummaries(entries)

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
	s.historyMu.Lock()
	s.persistedHistory = append(s.persistedHistory, entries...)
	if trimmed := len(s.persistedHistory) - maxPersistedHistory; trimmed > 0 {
		s.persistedHistory = s.persistedHistory[trimmed:]
		// Cap-trim shifts the prefix backwards; clamp seededLen so it keeps
		// pointing at "tail-end of what proc has already seen" rather than
		// past the new start.
		if s.persistedSeededLen >= trimmed {
			s.persistedSeededLen -= trimmed
		} else {
			s.persistedSeededLen = 0
		}
	}
	proc := s.loadProcess()
	var forward []cli.EventEntry
	if proc != nil && s.persistedSeededLen < len(s.persistedHistory) {
		// Defensive copy: the slice escapes the lock and proc.InjectHistory
		// outlives historyMu, so handing it persistedHistory's backing array
		// would race with subsequent appends.
		tail := s.persistedHistory[s.persistedSeededLen:]
		forward = make([]cli.EventEntry, len(tail))
		copy(forward, tail)
		s.persistedSeededLen = len(s.persistedHistory)
	}
	s.historyMu.Unlock()

	if len(forward) > 0 {
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
}

// extractLastPromptFromProcess scans the attached process's event log to populate
// lastPrompt and lastActivity when they haven't been set yet (e.g. after shim reconnect
// where events were injected directly into the process, bypassing InjectHistory).
func (s *ManagedSession) extractLastPromptFromProcess() {
	if loadAtomicString(&s.lastPrompt) != "" && loadAtomicString(&s.lastActivity) != "" {
		return
	}
	p := s.loadProcess()
	if p == nil {
		return
	}
	prompt, activity := scanLastSummaries(p.EventEntries())
	if prompt != "" && loadAtomicString(&s.lastPrompt) == "" {
		storeAtomicString(&s.lastPrompt, prompt)
	}
	if activity != "" && loadAtomicString(&s.lastActivity) == "" {
		storeAtomicString(&s.lastActivity, activity)
	}
}

// scanLastSummaries walks entries in reverse, returning the most-recent
// user-prompt summary and the most-recent activity summary. Stops early once
// both are found. Used by InjectHistory and extractLastPromptFromProcess.
func scanLastSummaries(entries []cli.EventEntry) (prompt, activity string) {
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if prompt == "" && e.Type == "user" {
			prompt = e.Summary
		}
		if activity == "" && cli.IsActivityType(e.Type) {
			activity = e.Summary
		}
		if prompt != "" && activity != "" {
			break
		}
	}
	return prompt, activity
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
