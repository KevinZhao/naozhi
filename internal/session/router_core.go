package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/eventlog/persist"

	// Note: blank-imports for claudejsonl / kirojsonl history backends
	// have moved to internal/wireup (R239-ARCH-B). cmd/naozhi imports
	// internal/wireup at startup so cli.RegisterHistoryFactory is
	// populated before any Router is constructed; tests that need a
	// specific backend import its package directly. internal/session
	// is now backend-agnostic at the import graph level — adding a
	// new backend only requires editing internal/wireup.
	"github.com/naozhi/naozhi/internal/history/naozhilog"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/sessionconst"
)

// ShutdownTimeout is the maximum time to wait for graceful shutdown
// of running sessions (Router) and HTTP connections (Server).
// Exported so both session and server packages use a single value.
const ShutdownTimeout = 30 * time.Second

// ErrMaxProcs is returned when all process slots are occupied.
var ErrMaxProcs = errors.New("max concurrent processes reached")

// ErrMaxExemptSessions is returned when the global cap on exempt (planner/
// cron) sessions is hit. Distinct from ErrMaxProcs so callers can apply
// different retry policies: exempt exhaustion means "too many projects
// configured" and is roughly permanent until an exempt session exits;
// ErrMaxProcs means "user sessions full" and clears faster.
//
// R242-ARCH-2: this sentinel now also fires when a per-namespace
// sub-quota (cron / planner / sys) is hit, even if the global pool
// still has room. Callers Is-checking on this sentinel keep working;
// the wrapped %d count matches the namespace cap that actually
// rejected so logs surface which sub-quota is exhausted.
var ErrMaxExemptSessions = errors.New("max exempt sessions reached")

// ErrNoCLIWrapper is returned when spawnSession is called but the router
// was constructed without a CLI wrapper (misconfiguration). This is
// permanent until the operator fixes config and restarts; retry loops
// should stop on this sentinel.
var ErrNoCLIWrapper = errors.New("no CLI wrapper configured")

// ErrNoActiveProcess is returned by ManagedSession.Send / SendPassthrough
// when the underlying process handle has been released (paused, reclaimed,
// or never spawned). Callers can errors.Is this sentinel to distinguish
// "process needs to be spawned" from real CLI failures, avoiding the
// "处理失败，请 /new 重置" fallback in dispatch.mapSendError.
var ErrNoActiveProcess = errors.New("session has no active process")

// exemptKeyPrefixes lists the session-key namespaces that are exempt from
// TTL expiry, LRU eviction, and the active-process counter. Derived from
// keyNamespaces (key.go) so the reserved + exempt lists share a single
// source of truth — see R239-ARCH-L for the prior drift between the two
// independently-maintained slices.
//
// To toggle a namespace's exempt status (or add a new exempt namespace),
// edit the matching `exempt` flag in keyNamespaces in key.go; this slice
// is rebuilt at package init from that table.
//
// Scratch keys are deliberately NOT exempt — they are short-lived and
// should pay the normal TTL / eviction cost (ScratchPool manages its own
// lifetime on top of that). SysKeyPrefix is exempt: system daemon stubs
// (when daemons opt to register one — see docs/rfc/system-session.md)
// must outlive the regular TTL/LRU pressure. Phase 1 daemons typically
// don't register stubs at all (Runner path), but the prefix is reserved
// here to keep the policy consistent with future stub-using daemons.
var exemptKeyPrefixes = func() []string {
	out := make([]string, 0, len(keyNamespaces))
	for _, ns := range keyNamespaces {
		if ns.exempt {
			out = append(out, ns.prefix)
		}
	}
	return out
}()

// isExemptKey reports whether key belongs to an exempt namespace. Callers
// that already have a ManagedSession should prefer reading s.exempt —
// this helper exists for the construction path and for external callers
// that know the key but not the session.
//
// Note: ScratchKeyPrefix is intentionally NOT an exempt namespace — scratch
// sessions are ephemeral and MUST remain subject to the regular TTL /
// eviction policy so an abandoned scratch conversation eventually releases
// its process slot. ScratchPool manages its own lifetime on top of that.
func isExemptKey(key string) bool {
	for _, prefix := range exemptKeyPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// exemptKind classifies an exempt session key into one of three buckets:
// "cron", "project", "sys", or "" if the key is not exempt. Used by the
// per-namespace sub-quota gate in spawnSession so a noisy cron chat
// can't starve planner / sys exempt sessions (R242-ARCH-2).
//
// R239-ARCH-L: derived from keyNamespaces (key.go) so a new exempt
// namespace registers its kind label in one place. Iteration is bounded
// by len(keyNamespaces) (4 entries today) so the linear scan stays
// cheap relative to the strings.HasPrefix calls a switch would make
// anyway.
func exemptKind(key string) string {
	for _, ns := range keyNamespaces {
		if !ns.exempt {
			continue
		}
		if strings.HasPrefix(key, ns.prefix) {
			return ns.kind
		}
	}
	return ""
}

// exemptCapFor returns the sub-quota cap for a given exempt kind. Unknown
// kinds return maxExemptSessions (the pre-R242-ARCH-2 global cap) so a
// future exempt namespace added to exemptKeyPrefixes without wiring up
// a sub-quota still has a defined limit and never reaches a "missing
// case ⇒ unlimited" state.
func exemptCapFor(kind string) int {
	switch kind {
	case "cron":
		return maxCronExempt
	case "project":
		return maxProjectExempt
	case "sys":
		return maxSysExempt
	default:
		return maxExemptSessions
	}
}

// Router defaults applied by NewRouter when the corresponding RouterConfig
// field is zero. Exported so other packages (tests, config validation, CLI
// flag defaults) can reference the single source of truth instead of
// re-typing the literal and drifting out of sync. R70-ARCH-H5.
//
// R222-ARCH-3: the source of truth lives in internal/sessionconst so
// internal/config can read it without reverse-importing internal/session.
// This file re-exports the names so existing call sites (session.DefaultTTL
// etc.) keep compiling unchanged.
const (
	// DefaultMaxProcs is the concurrent-process cap applied when
	// RouterConfig.MaxProcs is not set.
	DefaultMaxProcs = sessionconst.DefaultMaxProcs
	// DefaultTTL is the idle-session eviction threshold applied when
	// RouterConfig.TTL is not set.
	DefaultTTL = sessionconst.DefaultTTL
	// DefaultPruneTTL is the "keep metadata for long-idle session" threshold
	// applied when RouterConfig.PruneTTL is not set. Entries older than this
	// are pruned from the store even when exempt.
	DefaultPruneTTL = sessionconst.DefaultPruneTTL
)

const (
	// maxExemptSessions caps the total number of alive exempt sessions
	// (cron stubs + project planners + sys daemon stubs) to prevent
	// unbounded growth when many projects / cron jobs are configured.
	//
	// R242-ARCH-2: this used to be the only cap, which let a noisy cron
	// chat starve project planners (BL2 acknowledged). Per-namespace
	// sub-quotas below are now the primary limit — the global cap stays
	// as a belt-and-braces ceiling so a future exempt namespace
	// (planner / quick session / etc.) added without sub-quota wiring
	// still has a hard upper bound.
	//
	// Sum of sub-quotas should stay ≤ maxExemptSessions so the global
	// check is the relief valve (never the primary trigger) — see
	// docs/design/exempt-quotas.md if that ever changes.
	maxExemptSessions = 20

	// maxCronExempt caps the alive cron-stub exempt sessions. R242-ARCH-2
	// hard isolation: a noisy chat that configures DefaultMaxJobsPerChat
	// (10 today) cron jobs can no longer push planner / sys exempt
	// sessions out of the pool. Sized so the typical "1-2 busy chats ×
	// few jobs" deployment fits comfortably while leaving room for
	// planner + sys quotas to coexist.
	maxCronExempt = 12

	// maxProjectExempt caps the alive project-planner exempt sessions.
	// One per project is the design contract; this cap doubles as an
	// implicit ceiling on the active project count for planner-spawn
	// purposes (the project count itself isn't capped — un-spawned
	// projects sit dormant).
	maxProjectExempt = 5

	// maxSysExempt caps the alive sys-daemon exempt sessions. Phase 1
	// sysession daemons typically don't register stubs (they use a
	// transient claude -p Runner instead), so this cap is small;
	// future stub-using daemons can request a bump via a follow-up
	// review rather than silently consuming the cron quota.
	//
	// 12 + 5 + 3 = 20 = maxExemptSessions; sub-quotas fully partition
	// the global pool. Adding a new exempt namespace MUST shrink an
	// existing quota or bump maxExemptSessions in tandem, otherwise
	// the relief-valve check in spawnSession is a soft-fail surprise.
	maxSysExempt = 3

	// historyLoadConcurrency limits parallel disk I/O goroutines during
	// startup session history loading.
	historyLoadConcurrency = 10

	// ProjectScanInterval is how often the project root is rescanned to
	// pick up added or removed subdirectories. Exported for use by server package.
	ProjectScanInterval = 60 * time.Second

	// shimReconnectTimeout bounds individual shim reconnect/spawn RPCs at
	// NewRouter time. A hung socket handshake cannot stall startup past
	// this budget — on timeout the iteration moves on (SIGUSR2 fallback
	// for orphan shims, skip for drifted shims, log+continue for spawn).
	shimReconnectTimeout = 15 * time.Second

	// shimReconnectGraceDelay is how long the deferred history-load path
	// waits for ReconnectShims to complete its first pass before backfilling
	// JSONL for a session that shimManagedKeys() claimed at startup.
	// R53-ARCH-001: a shim that appears in the first Discover() but has
	// exited by the second (ReconnectShims') Discover() would previously
	// leave the session with no history at all. 5s comfortably exceeds a
	// normal ReconnectShims pass (per-key budget bounded by
	// shimReconnectTimeout=15s but most keys connect in < 500ms) on any
	// realistic deployment; the backfill is gated by hasInjectedHistory()
	// so the happy path (ReconnectShims succeeded) pays only the 5s wait
	// + a read-lock check, no FS I/O.
	shimReconnectGraceDelay = 5 * time.Second

	// knownIDsSaveInterval throttles knownIDs fsync to limit disk I/O.
	// A crash losing up to this much session-ID tracking costs one
	// discovery rescan cycle. Shared between Cleanup and saveIfDirty.
	knownIDsSaveInterval = 5 * time.Minute

	// sessionSaveInterval controls the cadence of the periodic
	// sessions.json flush in StartCleanupLoop. Kept shorter than
	// knownIDsSaveInterval so a crash loses at most this window of
	// session-state updates, while still limiting fsync churn.
	sessionSaveInterval = 30 * time.Second
)

// Router manages session key -> ManagedSession mapping.
//
// Lock ordering: s.sendMu -> r.mu. The onSessionID callback acquires r.mu
// while sendMu is held (Send → onSessionID → trackSessionID). Code that
// holds r.mu (write) must NEVER acquire sendMu — release r.mu first.
// s.historyMu protects persistedHistory independently; never held with sendMu or r.mu.
// Read-only operations (ListSessions, GetSession, Stats, Version) use RLock.
//
// Maintenance rule (router-split refactor): every field below carries a
// `// 读写: <files>` annotation listing which router_*.go files access it.
// Reviewers MUST block PRs that add a new field without this annotation —
// the annotation is the only mechanism that keeps fields-vs-methods coupling
// visible after the file split. See docs/design/router-split-design.md.
//
// NEEDS-DESIGN (R245-ARCH-48): the `// 读写:` annotation is human-
// maintained and silently rots when a field's actual access set
// drifts from the comment (e.g., a refactor pulls a getter into a
// new router_*.go file but forgets to update the comment). Plan:
// add tools/check-router-fields.go that parses each field's
// annotation, greps the listed files for the field name, and
// fails CI when any router_*.go reads/writes a field without
// being listed (or vice versa). Deferred until the router-split
// refactor stabilises (multiple in-flight changes still moving
// fields between files would generate false positives until
// quiescent).
type Router struct {
	// 读写: core (lock primitive itself), all router_*.go (acquired by methods)
	mu sync.RWMutex
	// 读写: core, lifecycle, cleanup
	shutdownCond *sync.Cond // signaled when process state changes; conditioned on mu (write lock)
	// 读写: core (init), lifecycle (spawn/reset/rename), shim (reconnect), cleanup (remove/cleanup), discovery (takeover/register)
	sessions map[string]*ManagedSession
	// sessionsByChat is a secondary index: chat key → set of session keys.
	// Enables O(k) ResetChat instead of O(n) full scan (k = agents per chat, typically 1-3).
	// Inner type is a set (map[string]struct{}) so indexAdd does O(1) dedupe
	// and indexDel does O(1) removal — the prior []string variant scanned the
	// slice on every Add/Del. R225-PERF-18.
	// Nil in test-created routers; all helpers below are nil-safe.
	// 读写: core (indexAdd/Del helpers), lifecycle (ResetChat/install/unregister), cleanup, discovery
	sessionsByChat map[string]map[string]struct{}
	// 读写: core (init), backend (wrapperFor), lifecycle (spawn)
	wrapper *cli.Wrapper // default (legacy single-backend) wrapper
	// 读写: core (init), backend (wrapperFor/managerFor/BackendIDs), lifecycle, shim
	wrappers map[string]*cli.Wrapper // backend ID → wrapper (nil in legacy mode)
	// 读写: core (init), backend (DefaultBackend, wrapperFor)
	defaultBackend string // backend ID used when AgentOpts.Backend is empty
	// backendIDs caches the dashboard-stable ordering returned by BackendIDs:
	// default backend first, remaining IDs sorted ascending. wrappers is
	// constructed once in NewRouter and never mutated, so this slice is
	// computed once at construction and read-only thereafter — saves a
	// per-call O(N) sort + 2 small allocations on the dashboard /api/sessions
	// hot path.
	// 读写: core (init), backend (BackendIDs)
	backendIDs []string
	// 读写: core (init), lifecycle (countActive/evictOldest)
	maxProcs int
	// 读写: core (init), cleanup (shouldPrune)
	ttl time.Duration
	// 读写: core (init), cleanup (shouldPrune)
	pruneTTL time.Duration
	// 读写: core (init), lifecycle (resolveSpawnParams)
	model string
	// 读写: core (init), lifecycle (resolveSpawnParams)
	extraArgs []string
	// backendModels / backendExtraArgs optionally override model and args
	// per backend ID. Read-only after NewRouter.
	// 读写: core (init), lifecycle (resolveSpawnParams)
	backendModels map[string]string
	// 读写: core (init), lifecycle (resolveSpawnParams)
	backendExtraArgs map[string][]string
	// 读写: core (init/DefaultWorkspace), lifecycle (GetWorkspace fallback)
	workspace string // default cwd for CLI processes
	// 读写: core (init), lifecycle (attachHistorySource), discovery (attachHistorySource via RegisterForResume / RegisterCronStubWithChain / Takeover), shim (reconnect)
	claudeDir string // ~/.claude dir for loading session history
	// kiroSessionsDir is the kiro session-state root. Plumbed into
	// cli.HistoryWiring at attachHistorySource time so the kirojsonl
	// factory can read per-session JSONL from this path. Wired from
	// RouterConfig.KiroSessionsDir in cmd/naozhi/main.go.
	// 读写: core (init), lifecycle (attachHistorySource), discovery (attachHistorySource via Register* / Takeover)
	kiroSessionsDir string

	// workspaceOverrides stores per-chat workspace overrides.
	// Key format: "platform:chatType:chatID" (3-segment chat key —
	// distinct from the 4-segment session key used in r.sessions).
	//
	// Two-key invariant: every chatKey present in sessionsByChat may
	// have a workspaceOverrides entry; ResetChat clears both maps.
	// SetWorkspace creates only the override entry (no session yet),
	// and Reset(key)/evictOldest must NOT touch this map — it is
	// driven by user intent (SetWorkspace) rather than the session
	// lifecycle.
	// 读写: core (init/load), lifecycle (SetWorkspace/GetWorkspace), cleanup (save)
	workspaceOverrides map[string]string

	// backendOverrides stores per-session backend preferences picked by
	// the dashboard at session-creation time. Keyed by full session key
	// (including agent suffix) so two sessions on the same chat can run
	// against different backends.
	// 读写: core (init), backend (Set/GetSessionBackend), lifecycle (unregisterSessionLocked)
	backendOverrides map[string]string

	// activeCount tracks currently alive processes (non-exempt only).
	// Writes happen under r.mu (write lock); atomic access lets Stats()
	// read lock-free so the dashboard /api/sessions hot path does not
	// take a second r.mu RLock right after ListSessions() released one.
	// R58-PERF-F1.
	// 读写: core (Stats lock-free read), lifecycle (countActive/evict/install)
	activeCount atomic.Int64

	// pendingSpawns tracks Spawn() calls in progress (lock released during spawn)
	// 读写: lifecycle (spawnSession)
	pendingSpawns int

	// spawningKeys records keys whose spawnSession is in flight. ReconnectShims
	// consults this set before declaring a discovered shim "orphan": a shim may
	// have written its state file after we dropped r.mu for wrapper.Spawn() but
	// before the new ManagedSession is installed, and without this set a
	// concurrent reconcile would shut the fresh shim down as an orphan.
	//
	// The map value is a per-spawn done-channel that spawnSession close()s
	// from its defer. GetOrCreate's wait loop selects on this channel
	// instead of polling, so the second caller wakes the instant the
	// winner finishes (success or failure) rather than after the next
	// 20ms tick. ReconnectShims still reads only the key set, so its
	// presence check is unaffected by the value type. R243-ARCH-4.
	// 读写: core (init), lifecycle (spawnSession write/close), shim (reconnect read)
	spawningKeys map[string]chan struct{}

	// 读写: core (init), cleanup (saveIfDirty)
	storePath string
	// 读写: lifecycle (spawn/Reset/Rename mutations), shim (reconnect post-attach), discovery (label/register/takeover), cleanup (saveIfDirty consume)
	storeDirty bool // true when sessions changed since last save
	// storeGen increments on each mutation. Writes happen under r.mu (write
	// lock) but atomic.Uint64 also lets Version() read lock-free — the
	// dashboard polls Version() every few seconds from the /api/sessions
	// hot path, and the previous RLock layered on top of ListSessions'
	// RLock made each poll take two contended trips through r.mu.
	// 读写: core (Version lock-free), lifecycle / cleanup / discovery (BumpVersion)
	storeGen atomic.Uint64
	// 读写: lifecycle (SetWorkspace / ResetChat / RenameSession), discovery (Takeover), cleanup (saveIfDirty consume)
	wsOverridesDirty bool // true when workspace overrides changed since last save
	// 读写: lifecycle (SetWorkspace / ResetChat / RenameSession), discovery (Takeover), cleanup (snapshot/check during save)
	wsOverridesGen atomic.Uint64 // increments on each ws-override mutation, mirrors storeGen pattern

	// knownIDs tracks ALL session IDs ever used by naozhi, including
	// sessions that have been removed/reset/evicted. Used by the
	// discovered-session scanner to match CLI processes to naozhi keys,
	// and as a secondary filter for filesystem-based recent sessions.
	// 读写: core (init), discovery (trackSessionID/Discovery*), cleanup (saveIfDirty)
	knownIDs map[string]bool
	// knownIDsOrder preserves insertion order so overflow eviction drops the
	// oldest (FIFO) rather than picking randomly via map iteration — random
	// eviction could drop a still-active session ID, causing discovery to
	// misclassify its CLI process as an external (non-naozhi) session.
	// 读写: core (init), discovery (trackSessionID)
	knownIDsOrder []string
	// 读写: discovery, cleanup
	knownIDsDirty bool
	// 读写: discovery, cleanup
	knownIDsGen uint64 // incremented on each knownIDs mutation (add/evict)
	// 读写: cleanup (Cleanup/saveIfDirty)
	knownIDsSavedAt time.Time // last successful saveKnownIDs; throttles fsync to 5min

	// sessionIDToKey is a reverse index from session ID to session key.
	// Used by RegisterForResume for O(1) deduplication instead of O(n) scan.
	// Maintained under r.mu by setSessionIDIndex/clearSessionIDIndex.
	// 读写: core (init), lifecycle (install/unregister), discovery (RegisterForResume)
	sessionIDToKey map[string]string

	// 读写: core (init), lifecycle (spawn config)
	noOutputTimeout time.Duration
	// 读写: core (init), lifecycle (spawn config)
	totalTimeout time.Duration

	// onChange is stored via atomic.Pointer so notifyChange can load it
	// lock-free on the stream-event hot path (called after every result
	// event via Process.SetOnTurnDone). The previous RLock on r.mu added
	// contention with session-mutation paths for a field that is set once
	// at startup and never replaced in practice.
	//
	// The wrapper struct exists to make the "store a function value through
	// an atomic pointer" idiom explicit. A bare `atomic.Pointer[func()]` +
	// `Store(&fn)` works — Go escapes `fn`'s parameter copy to the heap —
	// but the address-of-a-parameter pattern is easy to break during
	// future refactors. Wrapping `fn` in a named struct makes the heap
	// escape obvious and the dereference pattern unambiguous. R59-GO-M3.
	// 读写: core (SetOnChange/notifyChange)
	onChange atomic.Pointer[onChangeHolder]

	// onKeyRetired fires after Reset/Remove finish; lets side-indices keyed
	// on the session key (e.g. dispatch.MessageQueue) drop their entries.
	// 读写: core (SetOnKeyRetired/notifyKeyRetired), lifecycle (Reset), cleanup (Remove)
	onKeyRetired atomic.Pointer[onKeyRetiredHolder]

	// onSessionRetired mirrors onKeyRetired but exposes the session UUID
	// captured before teardown cleared r.sessions[key]. Used by the
	// dashboard history-sort path; see SetOnSessionRetired godoc for why
	// it is split from onKeyRetired.
	// 读写: core (SetOnSessionRetired/notifyKeyRetired), lifecycle (Reset), cleanup (Remove)
	onSessionRetired atomic.Pointer[onSessionRetiredHolder]

	// historyWg tracks startup history-loading goroutines so Shutdown waits for them.
	// 读写: core (init Add/Done), cleanup (Shutdown Wait)
	historyWg sync.WaitGroup

	// historyCtx is cancelled on Shutdown so in-flight LoadHistory*Ctx calls
	// abort promptly instead of blocking the drain on slow filesystems.
	// Paired with historyCancel (set by NewRouter, called from Shutdown).
	// 读写: core (init), lifecycle (attachHistorySource), cleanup (Shutdown cancel)
	historyCtx    context.Context
	historyCancel context.CancelFunc

	// shutdownOnce guards Shutdown against re-entry. Production flow invokes
	// Shutdown exactly once from the signal handler, but future code paths
	// (test teardown, hot-restart) might call it again; a double call would
	// race the broadcast timer, re-close historyCtx via historyCancel (safe
	// on its own but noisy) and double-detach shim processes. R49-REL-SHUTDOWN-ONCE.
	// 读写: cleanup (Shutdown)
	shutdownOnce sync.Once

	// eventLogDir is the directory naozhi's per-session event log files
	// live under. Empty disables the event log persistence entirely —
	// useful for tests and for deployments that explicitly opt out via
	// configuration. When non-empty, the Router uses eventLogPersister
	// to spool cli.EventEntry batches to disk and naozhilog.Source to
	// read them back on restart / pagination.
	// 读写: core (init), lifecycle (attachHistorySource), cleanup (dropEventLog)
	eventLogDir string
	// 读写: core (init), lifecycle (installPersistSink), cleanup (Shutdown)
	eventLogPersister *persist.Persister

	// attachmentTracker is the refcount tracker that bridges
	// event-log persist events to .meta sidecar updates. nil when
	// eventLogDir is unset (refcount tracking has no source of
	// events in that case). See docs/rfc/attachment-refcount.md.
	// 读写: core (init/stopAttachmentTracker), lifecycle (installPersistSink), cleanup (clearAttachmentTrackerRefs / Shutdown stop)
	attachmentTracker *attachmentTracker

	// autoChainPolicy decides whether and how aggressively to attach
	// prev_session_ids drawn from the same workspace's other JSONL
	// files (auto-workspace-chain feature; docs/rfc/auto-workspace-chain.md).
	// Never nil — defaults to disabledAutoChainPolicy when RouterConfig
	// leaves the field empty, so the feature is opt-in even on a freshly
	// constructed Router. Read-only after NewRouter.
	autoChainPolicy AutoChainPolicy

	// excluders aggregates SessionIDExcluder implementations registered
	// via AddSessionIDExcluder (cron Scheduler, sysession Manager, future
	// subsystems that own internal CLI sessionIDs). Read by the auto-chain
	// spawn / backfill paths to guarantee no internal sessionID is folded
	// into a user-facing chain (RFC §4.3 Arch-B2). Atomic copy-on-write so
	// the read path stays lock-free under r.mu.
	//
	// NEEDS-DESIGN (R242-ARCH-16): excluders is read-as-nil-pool during
	// the cmd-startup window between NewRouter and the first call to
	// AddSessionIDExcluder. Today auto-chain readers fall back to "no
	// exclusions" — a benign opening because cron Scheduler / sysession
	// Manager have not yet installed any internal sessionIDs (they
	// register themselves *after* their stub keys exist), so a Phase-2
	// candidate scan during that window has nothing to exclude. The
	// fallback's drift cost is that adding a third subsystem (e.g.
	// planner) which owns sessionIDs from t=0 would silently leak them
	// into auto-chain candidates until its registration call lands.
	// Plan: SetPendingExcluders(true) at NewRouter, flip to false in
	// the cmd-wire step that awaits cron + sysession + planner
	// registration; auto-chain reads block on the gate so the leak
	// window is closed by construction. Deferred until a third
	// excluder owner ships, since the current two-owner topology has
	// no exposure.
	excluders atomic.Pointer[[]SessionIDExcluder]

	// autoChainListJSONL is the listJSONL function injected into
	// pickWorkspaceChain. Production wires discovery.ListWorkspaceJSONL;
	// tests inject a fixture function that returns synthetic
	// WorkspaceJSONL slices without touching disk. Read-only after
	// NewRouter (config-time injection); not subject to lock contention.
	autoChainListJSONL func(workspace string) []discovery.WorkspaceJSONL

	// testHookBeforeSpawnPhase3 / testHookBeforeBackfillPhase3 are
	// test-only TOCTOU race-pinning hooks (RFC §5.3.1). Production
	// builds leave them nil. Trigger contracts and helper methods
	// live in router_testhooks.go so this struct stays focused on
	// production state. Tests set the fields directly. R245-ARCH-29
	// (#882).
	testHookBeforeSpawnPhase3    func()
	testHookBeforeBackfillPhase3 func()
}

// spawnerFunc is the signature panicSafeSpawnFn executes; abstracting it lets
// tests inject a function that panics instead of constructing a real
// cli.Wrapper (whose Spawn path has no panic-injection seam). Production
// wraps (*cli.Wrapper).Spawn in a closure at the call site.
type spawnerFunc func(context.Context, cli.SpawnOptions) (*cli.Process, error)

// pendingSpawnSlot is a one-shot RAII token returned by
// (*Router).acquirePendingSpawnSlotLocked. It guards r.pendingSpawns against
// stranded ++ on any panic / new error path between increment and the matching
// decrement. release() is idempotent: explicit happy-path callers decrement at
// the original site (preserving the existing lock-state contract) and a
// `defer token.release()` absorbs any unexpected exit (panic, future early
// return added without a manual --). R215-ARCH-P1-2.
type pendingSpawnSlot struct {
	r        *Router
	released bool
}

// acquirePendingSpawnSlotLocked increments r.pendingSpawns under r.mu (caller
// must hold r.mu for writing). It returns a slot token whose release method
// can be called from any lock state — release acquires r.mu itself if needed.
//
// LOCK: caller must hold r.mu (write).
func (r *Router) acquirePendingSpawnSlotLocked() *pendingSpawnSlot {
	r.pendingSpawns++
	return &pendingSpawnSlot{r: r}
}

// releaseLocked decrements pendingSpawns assuming the caller already holds
// r.mu for writing. Idempotent — a second call (e.g. from defer after the
// happy-path explicit release) is a no-op.
func (s *pendingSpawnSlot) releaseLocked() {
	if s == nil || s.released {
		return
	}
	s.r.pendingSpawns--
	s.released = true
}

// release is the lock-agnostic counterpart used from defer. It acquires r.mu
// only when the slot has not yet been released, so the common happy-path
// (which calls releaseLocked() inline) pays no extra lock acquisition.
// Idempotent.
func (s *pendingSpawnSlot) release() {
	if s == nil || s.released {
		return
	}
	s.r.mu.Lock()
	if !s.released {
		s.r.pendingSpawns--
		s.released = true
	}
	s.r.mu.Unlock()
}

// panicSafeSpawn invokes wrapper.Spawn inside a deferred recover so a panic
// from the wrapper (shim exec crash, bogus protocol Init, etc.) cannot leave
// pendingSpawns stranded in spawnSession. A stranded counter would make the
// router refuse every subsequent GetOrCreate with ErrMaxProcs until restart.
// The recovered panic is translated into a regular error so the surrounding
// control flow runs the standard "spawn process: %w" wrap + early return
// without special-casing panic. RES1.
func panicSafeSpawn(
	ctx context.Context,
	w *cli.Wrapper,
	opts cli.SpawnOptions,
	key, backendID string,
) (*cli.Process, error) {
	return panicSafeSpawnFn(ctx, w.Spawn, opts, key, backendID)
}

// panicSafeSpawnFn is the testable core: tests inject a spawnerFunc that
// panics to verify the recover path without a real wrapper. Production calls
// go through panicSafeSpawn above.
func panicSafeSpawnFn(
	ctx context.Context,
	spawn spawnerFunc,
	opts cli.SpawnOptions,
	key, backendID string,
) (proc *cli.Process, err error) {
	defer func() {
		if r := recover(); r != nil {
			// R172-ARCH-D10: counter sits inside the recover arm so it is
			// incremented exactly once per absorbed panic, paired with the
			// slog.Error record below. Operators watching
			// naozhi_spawn_panic_recovered_total see a non-zero value and can
			// grep journalctl for the paired record to pinpoint root cause.
			metrics.SpawnPanicRecoveredTotal.Add(1)
			slog.Error("spawnSession: wrapper.Spawn panicked",
				"key", key, "backend", backendID, "panic", r,
				"stack", string(debug.Stack()))
			// RNEW-009: caller at line 1656 wraps with "spawn process: %w".
			// Keep this message unprefixed so logs read
			// "spawn process: panic: <value>" instead of the doubled
			// "spawn process: spawn process: panic: ...".
			// R249-GO-15: preserve errors.Is/As chain when the panic value
			// itself is an error (e.g. nil pointer deref wraps runtime.Error).
			if e, ok := r.(error); ok {
				err = fmt.Errorf("panic: %w", e)
			} else {
				err = fmt.Errorf("panic: %v", r)
			}
		}
	}()
	return spawn(ctx, opts)
}

// chatKeyFor strips the last ":agentID" segment from a session key to get the chat key.
func chatKeyFor(key string) string {
	if idx := strings.LastIndexByte(key, ':'); idx >= 0 {
		return key[:idx]
	}
	return key
}

// isENOENTErr reports whether err (or any error it wraps) ultimately
// carries syscall.ENOENT. The helper exists primarily to make the intent
// explicit at call sites and to spell out why we must NOT match the
// strerror text ("no such file or directory") — it is locale-dependent
// (e.g. LANG=zh_CN.UTF-8 returns a Chinese translation) and silently
// regresses under non-English containers. errors.Is already walks the
// %w chain through *os.PathError / *os.SyscallError transparently, so
// a single call suffices.
func isENOENTErr(err error) bool {
	return err != nil && errors.Is(err, syscall.ENOENT)
}

// claudeProjectSlug maps a CWD to the directory name Claude CLI uses under
// ~/.claude/projects/. Thin wrapper over discovery.ClaudeProjectSlug so the
// two call sites (session + discovery) can never drift: if Claude's naming
// scheme ever changes, the single implementation in internal/discovery is
// the one to edit. TestClaudeProjectSlug_MatchesDiscovery pins the behaviour.
// RNEW-002.
func claudeProjectSlug(cwd string) string {
	return discovery.ClaudeProjectSlug(cwd)
}

// resolveResumeID returns resumeID if the corresponding jsonl conversation
// file exists under claudeDir (i.e. Claude CLI's --resume will actually find
// it), or "" to downgrade the spawn to a fresh session.
//
// Motivating failure: a cron job whose work_dir is edited after first run
// stores its jsonl under the original workspace's slug; subsequent ticks
// compute the new slug and --resume hits a path that does not exist, so
// Claude CLI prints "No conversation found with session ID: <id>" to stderr
// and exits 1 in ~1.7s. Upstream sees cron_job completed with result_len=0
// and no recorded error. Same failure mode fires when the prior CLI process
// died before flushing any turn — shim captured the init event's session_id
// but no jsonl was ever produced, so every subsequent tick keeps generating
// fresh-but-unsaved ids in a loop.
//
// Skipped when claudeDir or workspace are empty (test harness / misconfig):
// without both we can't build a meaningful path, and preserving legacy
// behavior keeps unrelated unit tests independent of filesystem layout.
// On stat errors other than ErrNotExist (permission denied, I/O failure)
// we also downgrade — a broken claudeDir would otherwise manifest as the
// same silent exit-1 loop the primary fix targets.
func resolveResumeID(claudeDir, workspace, key, resumeID string) string {
	if resumeID == "" || claudeDir == "" || workspace == "" {
		return resumeID
	}
	jsonlPath := filepath.Join(claudeDir, "projects",
		claudeProjectSlug(workspace), resumeID+".jsonl")
	if _, err := os.Stat(jsonlPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Warn("resume target missing, starting fresh session",
				"key", key,
				"resume_id", resumeID,
				"workspace", workspace,
				"expected_path", jsonlPath)
		} else {
			slog.Warn("resume target stat failed, starting fresh session",
				"key", key,
				"resume_id", resumeID,
				"expected_path", jsonlPath,
				"err", err)
		}
		return ""
	}
	return resumeID
}

// indexAdd adds key to the chat→sessions index. No-op when index is nil.
// Must be called under r.mu.
func (r *Router) indexAdd(key string) {
	if r.sessionsByChat == nil {
		return
	}
	ck := chatKeyFor(key)
	set := r.sessionsByChat[ck]
	if set == nil {
		set = make(map[string]struct{})
		r.sessionsByChat[ck] = set
	}
	set[key] = struct{}{}
}

// indexDel removes key from the chat→sessions index. No-op when index is nil.
// Must be called under r.mu.
func (r *Router) indexDel(key string) {
	if r.sessionsByChat == nil {
		return
	}
	ck := chatKeyFor(key)
	set := r.sessionsByChat[ck]
	if set == nil {
		return
	}
	delete(set, key)
	if len(set) == 0 {
		delete(r.sessionsByChat, ck)
	}
}

// RouterConfig holds configuration for the session router.
//
// Lifecycle: every field below is read at NewRouter construction time
// (or via legacy fallback paths that snapshot the value once). After
// NewRouter returns the receiver should treat the struct as
// immutable — fields like Wrapper / Wrappers / Workspace / StorePath
// / ClaudeDir are sampled into r.* state and never re-read. R230-ARCH-8:
// changing any of these at runtime requires `systemctl restart naozhi`
// (with the deliberate exception of `~/.claude/settings.json` which
// the per-spawn RefreshSettings hook re-loads each turn). Operators
// editing config.yaml should expect the changes to take effect only on
// the next process start; see docs/ops/naozhi-deploy-skill.md.
type RouterConfig struct {
	// Wrapper is the legacy single-backend field. If Wrappers is nil/empty
	// this wrapper is used for every session.
	Wrapper *cli.Wrapper
	// Wrappers maps backend ID → wrapper. When set, new sessions are
	// routed to the wrapper matching AgentOpts.Backend, with DefaultBackend
	// (or Wrapper) as a fallback.
	Wrappers map[string]*cli.Wrapper
	// DefaultBackend names the backend ID used when AgentOpts.Backend is
	// empty. Ignored when Wrappers is empty.
	DefaultBackend string
	MaxProcs       int
	TTL            time.Duration
	PruneTTL       time.Duration
	Model          string
	ExtraArgs      []string
	// BackendModels / BackendExtraArgs override Model / ExtraArgs per
	// backend (e.g. kiro-specific model flags).
	//
	// BackendExtraArgs semantics: REPLACE, not append. When a backend has
	// a non-empty entry here, spawnSession uses exactly those args instead
	// of the router-level ExtraArgs. Per-session AgentOpts.ExtraArgs is
	// then appended on top. An operator who wants to keep a router-wide
	// flag like `--setting-sources ""` must re-specify it in every backend
	// override — additive semantics would otherwise make it impossible to
	// drop a default flag for a specific backend. R53-ARCH-002.
	BackendModels    map[string]string
	BackendExtraArgs map[string][]string
	Workspace        string
	StorePath        string
	NoOutputTimeout  time.Duration
	TotalTimeout     time.Duration
	ClaudeDir        string
	// KiroSessionsDir is the kiro CLI's session-state root, typically
	// ~/.kiro/sessions/cli. Empty disables kiro history fallback; non-
	// empty enables the kirojsonl factory (registered via blank import
	// in this file's import block). Set by cmd/naozhi/main.go from
	// config. R228-CR-P3-4.
	KiroSessionsDir string
	// EventLogDir is where naozhi's per-session event log files live.
	// When empty, event log persistence is DISABLED and the router
	// falls back to Claude CLI JSONL as the sole history source. When
	// non-empty, the router spins up a persist.Persister on startup,
	// wires every session's cli.EventLog to it, and installs a
	// merged.Source (naozhilog + claudejsonl) as the history fallback.
	//
	// Common layout places this next to StorePath — for example
	// "/home/user/.naozhi/events" alongside "/home/user/.naozhi/sessions.json".
	// The cmd/naozhi wiring sets both values together.
	//
	// See docs/rfc/event-log-persistence.md §4 for the full startup
	// sequence.
	EventLogDir string
	// EventLogGenerator tags every new <keyhash>.log file's header
	// with the naozhi build identifier so operators running `jq` on
	// the file can tell which build produced it. Optional; empty
	// produces files with a blank generator field.
	EventLogGenerator string
	// EventLogDevMode enables Persister's panic-on-replay-phase
	// guard (RFC §3.2.3). Test / CI builds set this true so any
	// SetPersistSink ordering regression surfaces as an immediate
	// panic; production sets it false so the sink drops + counters
	// instead.
	EventLogDevMode bool

	// EventLogPersister, when non-nil, is used as the router's event-log
	// sink instead of constructing one from EventLogDir/EventLogGenerator/
	// EventLogDevMode. This decouples SessionStore wiring from event-log
	// persistence ownership: callers that own the Persister lifecycle
	// (tests, alternative startups) can inject the same instance shared
	// across multiple routers, or pre-configure observers and codecs that
	// the bare RouterConfig fields don't expose. R239-ARCH-N.
	//
	// When this field is nil and EventLogDir is non-empty the router
	// keeps the legacy behaviour of constructing the Persister itself,
	// which preserves wire compatibility with existing cmd/naozhi
	// callers.
	EventLogPersister *persist.Persister

	// AutoChainPolicy drives the workspace auto-chain feature
	// (docs/rfc/auto-workspace-chain.md). nil disables the feature
	// (Router falls back to disabledAutoChainPolicy). Production
	// wires GlobalAutoChainPolicy from cfg.Session.AutoChain.
	AutoChainPolicy AutoChainPolicy

	// AutoChainListJSONL is the discovery.ListWorkspaceJSONL hook used
	// by the auto-chain feature. nil falls back to discovery.ListWorkspaceJSONL.
	// Tests inject a fixture function so unit cases can synthesise
	// JSONL listings without touching disk.
	AutoChainListJSONL func(workspace string) []discovery.WorkspaceJSONL
}

// NewRouter creates a session router.
func NewRouter(cfg RouterConfig) *Router {
	if cfg.MaxProcs <= 0 {
		cfg.MaxProcs = DefaultMaxProcs
	}
	if cfg.TTL <= 0 {
		cfg.TTL = DefaultTTL
	}
	if cfg.PruneTTL <= 0 {
		cfg.PruneTTL = DefaultPruneTTL
	}

	// Normalize wrappers. Accept either a Wrappers map or a single Wrapper;
	// when both are set, Wrappers wins and Wrapper is kept as a compat alias
	// for code that still reads r.wrapper directly (mostly tests).
	wrappers := cfg.Wrappers
	defaultBackend := cfg.DefaultBackend
	if len(wrappers) == 0 && cfg.Wrapper != nil {
		id := cfg.Wrapper.BackendID
		if id == "" {
			id = "claude"
		}
		wrappers = map[string]*cli.Wrapper{id: cfg.Wrapper}
		if defaultBackend == "" {
			defaultBackend = id
		}
	}
	defaultWrapper := cfg.Wrapper
	if defaultWrapper == nil && defaultBackend != "" {
		defaultWrapper = wrappers[defaultBackend]
	}
	if defaultWrapper == nil {
		// Pick deterministically: Go map iteration is randomised, so
		// without sorting a multi-backend deployment without an explicit
		// DefaultBackend would flip its default on every process start.
		ids := make([]string, 0, len(wrappers))
		for id := range wrappers {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		if len(ids) > 0 {
			id := ids[0]
			defaultWrapper = wrappers[id]
			if defaultBackend == "" {
				defaultBackend = id
			}
		}
	}

	r := &Router{
		sessions:           make(map[string]*ManagedSession),
		sessionsByChat:     make(map[string]map[string]struct{}),
		wrapper:            defaultWrapper,
		wrappers:           wrappers,
		defaultBackend:     defaultBackend,
		maxProcs:           cfg.MaxProcs,
		ttl:                cfg.TTL,
		pruneTTL:           cfg.PruneTTL,
		model:              cfg.Model,
		extraArgs:          cfg.ExtraArgs,
		backendModels:      cfg.BackendModels,
		backendExtraArgs:   cfg.BackendExtraArgs,
		workspace:          cfg.Workspace,
		claudeDir:          cfg.ClaudeDir,
		kiroSessionsDir:    cfg.KiroSessionsDir,
		workspaceOverrides: make(map[string]string),
		backendOverrides:   make(map[string]string),
		storePath:          cfg.StorePath,
		knownIDs:           make(map[string]bool),
		sessionIDToKey:     make(map[string]string),
		spawningKeys:       make(map[string]chan struct{}),
		noOutputTimeout:    cfg.NoOutputTimeout,
		totalTimeout:       cfg.TotalTimeout,
		eventLogDir:        cfg.EventLogDir,
		autoChainPolicy:    cfg.AutoChainPolicy,
		autoChainListJSONL: cfg.AutoChainListJSONL,
	}
	// Auto-chain defaults: nil policy → safely-disabled stub so the
	// rest of the code can call r.autoChainPolicy.Enabled(...) without
	// nil checks. nil listJSONL → production discovery hook.
	if r.autoChainPolicy == nil {
		r.autoChainPolicy = disabledAutoChainPolicy{}
	}
	if r.autoChainListJSONL == nil {
		// Capture claudeDir in a closure so the auto-chain decision
		// signature stays workspace-only. discovery.ListWorkspaceJSONL
		// requires both claudeDir and workspace; the router knows
		// claudeDir from config and pickWorkspaceChain doesn't.
		claudeDir := r.claudeDir
		r.autoChainListJSONL = func(workspace string) []discovery.WorkspaceJSONL {
			return discovery.ListWorkspaceJSONL(claudeDir, workspace)
		}
	}
	// Spin up the event-log persister BEFORE we touch the session
	// store; the startup load path needs a live sink to attach
	// to spawned ManagedSessions as they get restored.
	//
	// Two wire paths:
	//   1. cfg.EventLogPersister != nil — caller-owned Persister wins.
	//      Lets callers split SessionStore wiring from event-log
	//      lifecycle (R239-ARCH-N).
	//   2. cfg.EventLogPersister == nil && cfg.EventLogDir != "" —
	//      legacy in-router construction; preserved for cmd/naozhi
	//      and existing tests that pass only EventLogDir.
	switch {
	case cfg.EventLogPersister != nil:
		r.eventLogPersister = cfg.EventLogPersister
	case cfg.EventLogDir != "":
		p, err := persist.NewPersister(persist.Options{
			Dir:       cfg.EventLogDir,
			Generator: cfg.EventLogGenerator,
			DevMode:   cfg.EventLogDevMode,
			Observer:  eventLogMetricsObserver{},
		})
		if err != nil {
			slog.Error("event log persister init failed; disabling event log persistence",
				"dir", cfg.EventLogDir, "err", err)
			r.eventLogDir = ""
		} else {
			r.eventLogPersister = p
		}
	}
	r.shutdownCond = sync.NewCond(&r.mu)
	// historyCtx is cancelled by Shutdown so startup history loads and
	// reconnect-time JSONL parses abort promptly on slow filesystems.
	// Parent is Background because NewRouter has no caller-supplied ctx;
	// Shutdown is the sole cancel trigger.
	r.historyCtx, r.historyCancel = context.WithCancel(context.Background())

	// Load historical session IDs (all IDs ever used by naozhi).
	// Insertion order is lost on reload (persistence writes as an unordered
	// list); seed the order slice from the map so FIFO eviction resumes.
	// On the first overflow post-restart the eviction order is arbitrary,
	// but subsequent eviction is FIFO again.
	if loaded := loadKnownIDs(r.storePath); loaded != nil {
		r.knownIDs = loaded
		r.knownIDsOrder = make([]string, 0, len(loaded))
		for id := range loaded {
			r.knownIDsOrder = append(r.knownIDsOrder, id)
		}
	}

	// Load persisted workspace overrides (/cd settings)
	if loaded := loadWorkspaceOverrides(r.storePath); loaded != nil {
		for k, v := range loaded {
			r.workspaceOverrides[k] = v
		}
	}

	// Restore sessions from store
	if restored := loadStore(r.storePath); restored != nil {
		for key, entry := range restored {
			// SECURITY:  reject sys: entries here even though saveStore
			// already skips them (RFC v2.1 §3.4 / Sec-HIGH-1).  Treat
			// any sys: entry on disk as evidence of a tampered
			// sessions.json — the legitimate naozhi binary never
			// writes them, and resurrecting one would let an attacker
			// pre-seed a synthetic ManagedSession with chosen
			// label_origin etc.  Daemons re-register stubs at startup
			// if they need them, so dropping the persisted copy is
			// safe.
			if IsSysKey(key) {
				slog.Warn("session store: dropping unexpected sys: entry",
					"key", key,
					"hint", "sys entries should never persist; possible sessions.json tampering")
				continue
			}
			// Resolve the wrapper that owned this session's backend so the
			// snapshot carries the correct CLI identity even after a pure
			// restore (no shim reconnect). Pre-multi-backend entries have
			// empty Backend and fall back to the router default.
			restoreWrapper, restoreBackendID := r.wrapperFor(entry.Backend)
			cliName, cliVersion := r.CLIName(), r.CLIVersion()
			if restoreWrapper != nil {
				cliName = restoreWrapper.CLIName
				cliVersion = restoreWrapper.CLIVersion
			}
			s := &ManagedSession{
				key:                key,
				prevSessionIDs:     entry.PrevSessionIDs,
				prevSessionOrigins: entry.PrevSessionOrigins,
				exempt:             isExemptKey(key),
			}
			storeTotalCost(&s.totalCost, entry.TotalCost)
			s.setWorkspace(entry.Workspace)
			s.SetBackend(restoreBackendID)
			s.SetCLIName(cliName)
			s.SetCLIVersion(cliVersion)
			if entry.UserLabel != "" {
				s.SetUserLabel(entry.UserLabel)
			}
			// LabelOrigin restore: empty in pre-v2.1 stores is treated as
			// "user" by daemons (RFC §7.3 / §13), so we don't synthesise a
			// default here — leaving the field at "" preserves the legacy
			// "human-set" semantics. Only persist explicit non-empty origin.
			if entry.LabelOrigin != "" {
				s.setLabelOrigin(entry.LabelOrigin)
			}
			// UI Round 5 R5-3: seed model from persisted store so the
			// dashboard immediately renders "claude-opus-4.7" / etc on
			// post-restart reattach, before the first new turn re-emits
			// system/init.
			if entry.Model != "" {
				s.SetModel(entry.Model)
			}
			s.setSessionID(entry.SessionID)
			if entry.LastActive != 0 {
				s.lastActive.Store(entry.LastActive)
			}
			// Sidebar order anchor: prefer the persisted CreatedAt, fall back
			// to LastActive for pre-feature stores so the upgraded binary keeps
			// older sessions in roughly their previous relative order. If both
			// are zero (very first save loop after a brand-new key), stamp now
			// so the entry still gets a stable comparator key.
			switch {
			case entry.CreatedAt != 0:
				s.createdAt.Store(entry.CreatedAt)
			case entry.LastActive != 0:
				s.createdAt.Store(entry.LastActive)
			default:
				s.initCreatedAtIfUnset()
			}
			// R215-ARCH-P2-2: publishSessionLocked funnels the
			// attachHistorySource + map insert + index update so the
			// invariant is a property of the publish step, not five
			// copy-paste sites.
			r.publishSessionLocked(key, s, false)
			r.trackSessionID(entry.SessionID)
			if entry.SessionID != "" {
				r.sessionIDToKey[entry.SessionID] = key
			}
		}
	}

	// Sidebar is driven purely by sessions.json (and live IM / dashboard
	// activity). Filesystem-discovered sessions are surfaced via the separate
	// "history" panel so that Remove is a durable delete — the user must
	// explicitly resume an entry before it re-enters the sidebar.

	// Auto-workspace-chain backfill: synchronously fill prev_session_ids
	// on every restored session that qualifies BEFORE we launch the Tier 1 /
	// Tier 2 history-loading goroutines. Tier 2 reads the chain (via
	// LoadHistoryChainTailCtx) — if the chain were still empty here, the
	// loader would only see the current sessionID's JSONL, and the
	// dashboard's "load earlier" button would stop at that boundary even
	// though older JSONL files exist in the workspace. Pinned by
	// TestNewRouter_AutoChainPrecedesTier2Loaders. See
	// docs/rfc/auto-workspace-chain.md §4.4-B.
	r.runAutoChainBackfillOnce()

	// Async-load history for all suspended sessions so the dashboard
	// shows conversation history without waiting for the next message.
	//
	// Tier 1: naozhilog (naozhi-native per-session log). When the
	// event log persister is configured (r.eventLogDir != "") we
	// LoadLatest from the local .log file. This tier preserves
	// Images / ImagePaths / AskQuestion / agent-team linkage fields
	// that Claude JSONL cannot provide.
	//
	// Tier 2: Claude CLI JSONL. Used when the local tier returns
	// nothing (fresh deploy, user cleared events/). The walk is the
	// same chain walker the reconnect path uses.
	//
	// Both tiers complete BEFORE the corresponding process's
	// PersistSink is installed (via spawnSession / ReconnectShims),
	// so replayed entries are tagged replayPhase=true and dropped by
	// the Persister rather than re-persisted.
	//
	// historyLoadSem is shared across tier 1 and tier 2 so the cap
	// expresses "total concurrent history-load disk I/O", not "10 per
	// tier". Without this share the worst case was ~2× cap on a deploy
	// that triggered both tiers (e.g. event-log persister enabled but
	// some sessions only have Claude JSONL). R215-GO-P2-1.
	historyLoadSem := make(chan struct{}, historyLoadConcurrency)

	if r.eventLogPersister != nil {
		sem := historyLoadSem
		for _, s := range r.sessions {
			r.historyWg.Add(1)
			go func() {
				defer r.historyWg.Done()
				select {
				case sem <- struct{}{}:
				case <-r.historyCtx.Done():
					return
				}
				defer func() { <-sem }()
				src := naozhilog.New(r.eventLogDir, s.key)
				entries, err := src.LoadLatest(r.historyCtx, maxPersistedHistory)
				if err != nil || len(entries) == 0 {
					return
				}
				// hasInjectedHistory guards against a concurrent
				// ReconnectShims having already filled the session —
				// we'd double-inject otherwise.
				if s.hasInjectedHistory() {
					return
				}
				s.InjectHistory(entries)
				slog.Info("loaded session history from naozhi event log",
					"key", s.key, "entries", len(entries))
				r.notifyChange()
			}()
		}
	}

	// Tier 2 (Claude CLI JSONL) — runs unconditionally; the
	// hasInjectedHistory check inside each goroutine skips work when
	// tier 1 already populated the session.
	//
	// Two sub-paths (unchanged from pre-eventlog behaviour):
	//   1. Non-shim-managed sessions (default): load immediately.
	//   2. Shim-managed sessions (shimKeys[key]==true): defer for
	//      shimReconnectGraceDelay to let ReconnectShims inject its own
	//      replay + JSONL history first; then backfill only if the session
	//      is still empty. This guards against R53-ARCH-001 — a short-lived
	//      shim that appears in shimManagedKeys() at startup but has
	//      exited by the time ReconnectShims runs its second Discover,
	//      previously leaving the session with no history (skipped by
	//      path #1, missed by ReconnectShims) until the user sent a
	//      message. The deferred backfill checks hasInjectedHistory()
	//      so successful ReconnectShims runs do not get duplicated.
	if r.claudeDir != "" {
		shimKeys := r.shimManagedKeys()
		// Shared with tier 1 above — see historyLoadSem rationale at the
		// top of this block (R215-GO-P2-1: cap = total disk I/O, not per
		// tier).
		sem := historyLoadSem
		for _, s := range r.sessions {
			if s.getSessionID() == "" {
				continue
			}
			deferred := shimKeys[s.key]
			r.historyWg.Add(1)
			go func() {
				defer r.historyWg.Done()
				if deferred {
					// Wait for ReconnectShims to complete its first pass.
					// historyCtx cancel (Shutdown) aborts the wait cleanly.
					// R175-P3: use NewTimer + Stop instead of time.After —
					// on fast shutdown (within shimReconnectGraceDelay) the
					// time.After variant leaks a runtime timer per goroutine
					// for the full grace window, and at startup we can have
					// up to historyLoadConcurrency * #deferred-sessions
					// goroutines parked here.
					graceTimer := time.NewTimer(shimReconnectGraceDelay)
					select {
					case <-graceTimer.C:
						// Fired — no Stop needed, timer channel already drained.
					case <-r.historyCtx.Done():
						if !graceTimer.Stop() {
							<-graceTimer.C
						}
						return
					}
					// If ReconnectShims already populated history (happy
					// path), skip the JSONL load to avoid duplicate entries.
					if s.hasInjectedHistory() {
						return
					}
					// Otherwise fall through: the shim disappeared between
					// shimManagedKeys() and ReconnectShims' Discover, so we
					// must backfill directly or the dashboard shows empty
					// history until the next message.
					// R172-ARCH-D10: counter sits AFTER the hasInjectedHistory
					// short-circuit, so only the fallback branch increments it.
					// A non-zero value flags the short-lived-shim race from
					// R53-ARCH-001 — ReconnectShims' happy path must not move
					// this number, or the signal inverts.
					metrics.ShimReconnectGraceBackfillTotal.Add(1)
					slog.Info("shim-managed session missing history after reconnect grace, falling back to JSONL load",
						"key", s.key)
				}
				select {
				case sem <- struct{}{}:
				case <-r.historyCtx.Done():
					return
				}
				defer func() { <-sem }()

				// Skip when tier 1 (naozhilog) already filled the
				// session. Without this, a deploy with BOTH event-log
				// persistence and a populated Claude JSONL would
				// double-inject the first ~500 entries.
				if s.hasInjectedHistory() {
					return
				}

				// Build ordered list of all session IDs: prev chain + current.
				// LoadHistoryChainTailCtx walks from newest→oldest and stops
				// as soon as maxPersistedHistory entries are collected, so a
				// 32-link chain typically opens only 1-2 JSONL files.
				ids := make([]string, 0, len(s.prevSessionIDs)+1)
				ids = append(ids, s.prevSessionIDs...)
				ids = append(ids, s.getSessionID())

				allEntries := discovery.LoadHistoryChainTailCtx(
					r.historyCtx, r.claudeDir, ids, s.Workspace(), maxPersistedHistory,
				)
				if len(allEntries) == 0 {
					return
				}
				// Final check for the deferred path: ReconnectShims may have
				// raced us between the grace timer and LoadHistory returning.
				// InjectHistory appends (not replaces), so a double-inject
				// shows duplicates in the sidebar.
				if deferred && s.hasInjectedHistory() {
					return
				}
				s.InjectHistory(allEntries)
				slog.Info("loaded session history on startup", "key", s.key, "entries", len(allEntries), "chain", len(ids), "deferred", deferred)
				r.notifyChange()
			}()
		}
	}

	// Reap <keyhash>.log files that don't correspond to any restored
	// session AND are older than orphanSweepAge. See §4.4 of
	// docs/rfc/event-log-persistence.md for the rationale; in short,
	// DropKey failures + sessions.json rewrites can leave stranded
	// logs that never get reclaimed otherwise.
	r.runOrphanSweep()

	// Attachment refcount tracker. See docs/rfc/attachment-refcount.md.
	// Must be started AFTER r.sessions is populated so the resolver
	// closure can see them; first OnPersistedEntry callback arrives
	// when a live CLI produces a new EventEntry which cannot happen
	// until callers call GetOrCreate, which can't happen until
	// NewRouter returns.
	r.startAttachmentTracker()

	r.backendIDs = computeBackendIDs(r.wrapper, r.wrappers, r.defaultBackend)

	return r
}

// onChangeHolder wraps a callback so the atomic pointer Store site is an
// explicit composite literal rather than `&fn` (address of a parameter copy).
// Both forms are correct — Go's escape analysis heap-allocates either way —
// but the wrapper makes the "function-value through atomic pointer" idiom
// unmistakable to future readers and is harder to break when inlining /
// renaming the parameter. R59-GO-M3.
type onChangeHolder struct{ fn func() }

// SetOnChange registers a callback invoked when the session list changes.
// Replaces any previous callback; nil fn clears the callback.
func (r *Router) SetOnChange(fn func()) {
	if fn == nil {
		r.onChange.Store(nil)
		return
	}
	r.onChange.Store(&onChangeHolder{fn: fn})
}

// notifyChange calls the onChange callback if set. Must be called outside
// r.mu. Lock-free so stream-event callbacks (fired per result event) don't
// contend r.mu with session mutations.
func (r *Router) notifyChange() {
	if h := r.onChange.Load(); h != nil {
		h.fn()
	}
}

// onKeyRetiredHolder mirrors onChangeHolder for the key-retirement hook.
type onKeyRetiredHolder struct{ fn func(key string) }

// onSessionRetiredHolder mirrors onKeyRetiredHolder but carries the
// session's UUID alongside the routing key. Wired separately so the
// sessionID-keyed RetiredStore path doesn't have to reverse-lookup
// the UUID after teardown has already cleared r.sessions[key].
type onSessionRetiredHolder struct{ fn func(key, sessionID string) }

// SetOnKeyRetired registers a callback fired from Reset/Remove AFTER the
// session teardown completes. Typical wiring: dispatch.MessageQueue.Cleanup
// so it does not accumulate empty entries Discard retains for gen-monotonicity.
func (r *Router) SetOnKeyRetired(fn func(key string)) {
	if fn == nil {
		r.onKeyRetired.Store(nil)
		return
	}
	r.onKeyRetired.Store(&onKeyRetiredHolder{fn: fn})
}

// SetOnSessionRetired registers a callback fired from Reset/Remove AFTER
// the session teardown completes, receiving both the routing key and the
// session UUID captured before teardown cleared r.sessions[key]. Used by
// the dashboard history-drawer sort path to stamp a retired_at timestamp
// onto the corresponding RecentSession entry. sessionID may be empty when
// the session was retired before the CLI ever returned a UUID; callbacks
// must tolerate that and skip recording.
//
// Independent of SetOnKeyRetired so the existing FIFO-cleanup wiring in
// dispatch.MessageQueue.Cleanup is not disturbed; both fire on the same
// teardown event.
func (r *Router) SetOnSessionRetired(fn func(key, sessionID string)) {
	if fn == nil {
		r.onSessionRetired.Store(nil)
		return
	}
	r.onSessionRetired.Store(&onSessionRetiredHolder{fn: fn})
}

// notifyKeyRetired invokes both the onKeyRetired and onSessionRetired
// callbacks (when set). Call outside r.mu. sessionID is captured from
// the session before its teardown ran, so it remains valid even though
// r.sessions[key] is already gone by the time we reach this hook.
func (r *Router) notifyKeyRetired(key, sessionID string) {
	if h := r.onKeyRetired.Load(); h != nil {
		h.fn(key)
	}
	if h := r.onSessionRetired.Load(); h != nil {
		h.fn(key, sessionID)
	}
}

// NotifyIdle wakes the Shutdown wait loop so it can re-check running sessions.
// Call this after a message send completes (session transitions from running to ready).
//
// R183-REL-H1: acquire r.mu before Broadcast. sync.Cond.Broadcast technically
// accepts being called without the associated lock held, but Shutdown's loop
// re-checks "running" between each Wait() — if NotifyIdle fires in the window
// between Shutdown clearing `running` and entering Wait(), the signal is lost
// and Shutdown only wakes from the 30s AfterFunc safety net. Holding r.mu
// around Broadcast blocks NotifyIdle until Shutdown is actually parked in
// Wait() (which re-releases r.mu internally), eliminating the missed-wakeup
// race. Every other Broadcast site in this file acquires r.mu first; this
// was the sole exception. All callers of NotifyIdle are off the hot path
// (end-of-turn only, not per-event) so the extra lock round-trip is free.
func (r *Router) NotifyIdle() {
	if r.shutdownCond == nil {
		return
	}
	r.mu.Lock()
	r.shutdownCond.Broadcast()
	r.mu.Unlock()
}

// ChatKey builds a chat-level key (without agent suffix) for workspace
// overrides. Components are sanitized with the same rule that SessionKey uses
// so a malicious IM chat ID containing C0/ANSI bytes or Unicode bidi overrides
// cannot flow through the chat_key attr into slog.TextHandler output and
// inject fabricated log lines. R58-GO-H1 / R58-SEC-L1.
func ChatKey(platform, chatType, chatID string) string {
	return sanitizeKeyComponent(platform) + ":" + sanitizeKeyComponent(chatType) + ":" + sanitizeKeyComponent(chatID)
}

// DefaultWorkspace returns the router's default working directory.
func (r *Router) DefaultWorkspace() string {
	return r.workspace
}

// Version returns a monotonic counter incremented on every session
// mutation. The dashboard polls it from the /api/sessions hot path
// to skip full JSON comparison when nothing changed. storeGen is
// atomic so this is lock-free.
//
// R230C-ARCH-18 / R229-ARCH-20: Version() is read by two distinct
// audiences and the same uint64 has been ambiguously serving both:
//
//  1. **Data version** — "session map content changed, persistence layer
//     should re-save and any consumer caching the result must invalidate."
//     Bumped on add/remove/rename/reset/snapshot mutations under r.mu.
//  2. **Render version** — "UI must re-fetch even though the session map
//     didn't change." Bumped via BumpVersion() from non-session mutations
//     (project favorite toggle, agent registry changes, etc.).
//
// Today both audiences read storeGen.Load() so a render-only bump still
// makes the persistence layer think the session map mutated. The cost
// is one redundant saveStore (debounced, IO-cheap) and is acceptable
// for the audiences we have. Splitting into two counters is tracked
// under R229-ARCH-20; until it lands callers must be aware that a
// Version() change does NOT necessarily mean ListSessions() returns
// new data — it may be a render-only signal.
func (r *Router) Version() uint64 {
	return r.storeGen.Load()
}

// BumpVersion forces a version increment + onChange broadcast even when no
// session mutation occurred. Use this from non-session state changes that
// the dashboard surfaces through /api/sessions (e.g. project favorite
// toggle): without the bump, the frontend's poll-time version gate
// short-circuits the re-render; without the notifyChange, the live
// WebSocket `sessions_update` push is skipped and the UI only refreshes
// on the next 5s poll tick.
//
// R230C-ARCH-18: BumpVersion is the "Render version" half of the
// Version() ambiguity. It does NOT set storeDirty (so persistence layer
// won't enqueue a save), but it DOES advance storeGen so cache-keyed
// consumers downstream of Version() invalidate as if data changed.
//
// BumpVersion does NOT set storeDirty. It is a UI-refresh signal only and
// must not be used when session state needs to be persisted to disk.
// R68-GO-M1 / R68-SEC-L1.
func (r *Router) BumpVersion() {
	r.storeGen.Add(1)
	r.notifyChange()
}

// MaxProcs returns the maximum number of concurrent CLI processes.
func (r *Router) MaxProcs() int {
	return r.maxProcs
}

// Stats returns current session statistics.
// active = sessions with a live process (ready or running, excluding exempt);
// total = all sessions in the map including suspended ones.
//
// Both reads happen inside the same RLock epoch so a concurrent spawnSession
// landing between them cannot publish `active = N+1` against a pre-spawn
// `total = N`, which would surface as `active > total` on the dashboard.
// activeCount is still atomic for the lock-free fast path in spawn admission
// checks; here we trade the lock-free read for observational consistency —
// the RLock is uncontended with other readers and Load() is wait-free, so
// the added cost is a pointer-level memory read. R59-GO-H1.
func (r *Router) Stats() (active, total int) {
	r.mu.RLock()
	total = len(r.sessions)
	active = int(r.activeCount.Load())
	r.mu.RUnlock()
	return active, total
}

// HealthCheck performs a lightweight liveness check by testing that the
// router's RWMutex is not permanently held (deadlock detection).
// Returns true if the lock can be acquired, false if it appears stuck.
func (r *Router) HealthCheck() bool {
	if !r.mu.TryRLock() {
		return false
	}
	r.mu.RUnlock()
	return true
}

// listRefsPool reuses the *ManagedSession slice that ListSessions allocates
// to capture session pointers under r.mu. The slice is short-lived (single
// poll) but at 1 Hz × N tabs × hundreds of sessions the per-call alloc is the
// dominant cost on the dashboard's session list path. R222-PERF-10.
var listRefsPool = sync.Pool{
	New: func() any {
		s := make([]*ManagedSession, 0, 64)
		return &s
	},
}

// ListSessions returns a snapshot of all sessions for the dashboard.
// Collects references under r.mu, then releases before snapshotting
// to avoid blocking the router while getSessionID() waits on sendMu.
//
// R229-PERF-10: the SessionSnapshot slice itself is freshly allocated
// each call (not pooled). Pooling here is unsafe because the slice
// escapes to caller (handleList etc.) which JSON-marshals it across
// goroutine boundaries; a sync.Pool entry could be returned to the pool
// while a previous caller's handler is still in flight, leaking
// snapshot fields into a different request's response. The
// listRefsPool above only holds *ManagedSession pointers (caller never
// retains the slice — we clear it before Put), and that's the
// correct boundary for pooling. ~50 sessions × ~280 B SessionSnapshot
// = ~14 KB / call; 1 Hz × N tabs is acceptable.
func (r *Router) ListSessions() []SessionSnapshot {
	refsPtr := listRefsPool.Get().(*[]*ManagedSession)
	refs := (*refsPtr)[:0]
	r.mu.RLock()
	if cap(refs) < len(r.sessions) {
		// Pool slice too small for this poll — drop it (next caller will
		// pull a fresh one) and grow once instead of paying the append
		// growth path.
		refs = make([]*ManagedSession, 0, len(r.sessions))
	}
	for _, s := range r.sessions {
		refs = append(refs, s)
	}
	r.mu.RUnlock()

	snapshots := make([]SessionSnapshot, len(refs))
	for i, s := range refs {
		snapshots[i] = s.Snapshot()
	}
	// Clear pointers before returning to pool so a stuck pool entry does
	// not pin Sessions past their last legitimate use.
	for i := range refs {
		refs[i] = nil
	}
	*refsPtr = refs[:0]
	listRefsPool.Put(refsPtr)
	return snapshots
}

// GetSession returns the session for the given key, or nil.
func (r *Router) GetSession(key string) *ManagedSession {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sessions[key]
}
