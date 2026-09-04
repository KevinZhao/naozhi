package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/eventlog/persist"

	// History backends (claudejsonl / kirojsonl) are blank-imported in
	// internal/wireup, not here, so this package stays backend-agnostic; the
	// generic naozhilog tier is constructed via eventlog_bridge.go (#403, #567).
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/session/runhistory"
	"github.com/naozhi/naozhi/internal/session/workspacestore"
	"github.com/naozhi/naozhi/internal/sessionconst"
	"github.com/naozhi/naozhi/internal/tuningspec"
)

// ShutdownTimeout is the maximum time to wait for graceful shutdown
// of running sessions (Router) and HTTP connections (Server).
// Exported so both session and server packages use a single value.
const ShutdownTimeout = 30 * time.Second

// ErrMaxProcs is returned when all process slots are occupied.
var ErrMaxProcs = errors.New("max concurrent processes reached")

// ErrMaxExemptSessions is returned when the global cap on exempt (planner/
// cron) sessions — or a per-namespace sub-quota (cron / planner / sys) — is
// hit. Distinct from ErrMaxProcs so callers can apply different retry
// policies: exempt exhaustion is roughly permanent until an exempt session
// exits. The wrapped %d count is the cap that actually rejected.
var ErrMaxExemptSessions = errors.New("max exempt sessions reached")

// ErrNoCLIWrapper is returned when spawnSession is called but the router
// was constructed without a CLI wrapper (misconfiguration). This is
// permanent until the operator fixes config and restarts; retry loops
// should stop on this sentinel.
var ErrNoCLIWrapper = errors.New("no CLI wrapper configured")

// ErrNoActiveProcess is returned by ManagedSession.Send / SendPassthrough
// when the underlying process handle has been released (paused, reclaimed,
// or never spawned). Callers errors.Is this sentinel to distinguish
// "process needs to be spawned" from real CLI failures.
var ErrNoActiveProcess = errors.New("session has no active process")

// ErrRouterStopped is returned by spawnSession (and therefore GetOrCreate /
// Takeover / ResetAndRecreate) once Router.Shutdown has set the stopped gate,
// closing the spawn-after-snapshot leak window (#1822).
var ErrRouterStopped = errors.New("router is shutting down")

// Router defaults applied by NewRouter when the corresponding RouterConfig
// field is zero. The source of truth lives in internal/sessionconst so
// internal/config can read it without importing internal/session; these
// re-exports keep session.DefaultTTL etc. compiling.
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
	// maxExemptSessions caps the total alive exempt sessions (cron stubs +
	// project planners + sys daemon stubs). The per-namespace sub-quotas below
	// are the primary limit; this global cap is the relief valve so a future
	// exempt namespace added without sub-quota wiring still has a hard ceiling.
	// Sum of sub-quotas must stay ≤ maxExemptSessions (docs/design/exempt-quotas.md).
	maxExemptSessions = 20

	// maxCronExempt caps alive cron-stub exempt sessions so a noisy chat with
	// DefaultMaxJobsPerChat cron jobs cannot push planner / sys sessions out.
	maxCronExempt = 12

	// maxProjectExempt caps alive project-planner exempt sessions (one per
	// project by design; un-spawned projects sit dormant).
	maxProjectExempt = 5

	// maxSysExempt caps alive sys-daemon exempt sessions. 12 + 5 + 3 = 20 =
	// maxExemptSessions: sub-quotas fully partition the pool, so adding a new
	// exempt namespace MUST shrink an existing quota or bump maxExemptSessions.
	maxSysExempt = 3

	// historyLoadConcurrency limits parallel disk I/O goroutines during
	// startup session history loading.
	historyLoadConcurrency = 10

	// ProjectScanInterval is how often the project root is rescanned to
	// pick up added or removed subdirectories. Exported for use by server package.
	ProjectScanInterval = 60 * time.Second

	// shimReconnectTimeout bounds individual shim reconnect/spawn RPCs at
	// NewRouter time so a hung socket handshake cannot stall startup; on
	// timeout the iteration moves on (SIGUSR2 fallback for orphan shims, skip
	// for drifted shims, log+continue for spawn).
	shimReconnectTimeout = 15 * time.Second

	// shimReconnectGraceDelay is how long the deferred history-load path waits
	// for ReconnectShims' first pass before backfilling JSONL for a session that
	// shimManagedKeys() claimed at startup. Covers a shim that exits between the
	// two Discover() calls; hasInjectedHistory() gates the backfill so the happy
	// path pays only the wait + a read-lock check.
	shimReconnectGraceDelay = 5 * time.Second

	// knownIDsSaveInterval throttles knownIDs fsync to limit disk I/O.
	// A crash losing up to this much session-ID tracking costs one
	// discovery rescan cycle. Shared between Cleanup and saveIfDirty.
	knownIDsSaveInterval = 5 * time.Minute

	// sessionSaveInterval controls the cadence of the periodic sessions.json
	// flush in StartCleanupLoop. Shorter than knownIDsSaveInterval so a crash
	// loses at most this window of session-state updates.
	sessionSaveInterval = 30 * time.Second
)

// Router manages session key -> ManagedSession mapping.
//
// Lock ordering: s.sendMu -> r.mu. The onSessionID callback acquires r.mu
// while sendMu is held (Send → onSessionID → trackSessionID). Code that
// holds r.mu (write) must NEVER acquire sendMu — release r.mu first.
// s.historyMu protects persistedHistory independently; never held with sendMu or r.mu.
// Read-only operations (ListSessions, SessionFor, Stats, Version) use RLock.
//
// Every field carries a 读写 (access-set) annotation listing the router_*.go
// files touching it (tools/check-router-fields); block PRs adding a field without one.
type Router struct {
	// 读写: core (lock primitive itself), all router_*.go (acquired by methods)
	mu sync.RWMutex
	// 读写: core, lifecycle, cleanup, capacity (waitForCapacity broadcast/wait), tuning (respawn broadcast)
	shutdownCond *sync.Cond // signaled when process state changes; conditioned on mu (write lock)
	// ss is the session-table facet (#383): sessions + byChat/keyhash/idToKey
	// indices, activeCount, dirty, gen (sessionStore, store.go). No lock of its
	// own — read/written ONLY under r.mu. INVARIANT: sessions + byChat +
	// keyhash + idToKey mutate together inside ONE r.mu write critical section;
	// indexAdd/indexDel are the keyhash+byChat funnel. activeCount / gen are
	// atomic for lock-free readers. The annotation below is the UNION of all
	// domains; the lint recurses one level into sessionStore's own annotations.
	// 读写: core (init/Stats/Version/indexAdd/Del/resolver), lifecycle (spawn/reset/rename/install/unregister/countActive/evict/BumpVersion), shim (reconnect), cleanup (remove/cleanup/saveIfDirty/reconcile/BumpVersion), discovery (takeover/register/RegisterForResume/BumpVersion), capacity (reconcile active-gauge scan/evictOldest), workspace (evictWorkspaceOverrideLocked byChat live-session check), backend (BackendModelManifest live-proc manifest scan), tuning (SetSessionTuning lookup/record)
	ss sessionStore
	// bkStore is the backend/policy facet (#383): read-only-after-NewRouter
	// config fields plus the mutable backendOverrides map (router_backend.go).
	// No lock of its own — mutations ONLY under r.mu write lock, reads under
	// RLock. The annotation below is the UNION of all domains; the lint
	// recurses one level into backendStore's own annotations.
	// 读写: core (init), backend (wrapperFor/managerFor/BackendIDs/BackendWrapper/DefaultBackend/CLIName/CLIVersion/CLIPath/backendDefaultsFor/Set/GetSessionBackend), lifecycle (spawn/resolveSpawnParams/unregisterSessionLocked/RenameSession), shim (shimManagers)
	bkStore backendStore
	// accessProfiles is the named auth/upstream overlay registry (RFC
	// project-access-profile). Nil/empty ⇒ every session runs on the global
	// baseline. Copy-on-write: AddAccessProfile swaps the whole map under the
	// write lock, so readers outside r.mu snapshot the pointer under RLock (#2494).
	// 读写: core (init), access_profile (AddAccessProfile swap), lifecycle (resolveSpawnParamsLocked read-only), spawn_layers (accessProfileDefaultModel RLock)
	accessProfiles map[string]AccessProfile
	// defaultAccessProfile is applied when a session resolves to no explicit
	// profile (lowest precedence); "" = global-baseline fallthrough. Read-only after NewRouter.
	// 读写: core (init), lifecycle (resolveSpawnParamsLocked read-only)
	defaultAccessProfile string
	// 读写: core (init), lifecycle (countActive/evictOldest)
	maxProcs int
	// 读写: core (init), cleanup (shouldPrune)
	ttl time.Duration
	// 读写: core (init), cleanup (shouldPrune)
	pruneTTL time.Duration
	// 读写: core (init/DefaultWorkspace), lifecycle (GetWorkspace fallback), workspace (resolveWorkspaceLocked/WorkspaceRoots fallback)
	//
	// Named defaultCWD (not "workspace") to disambiguate from node identity
	// (Config.Workspace), remote nodes (Config.Workspaces) and per-chat
	// overrides (wsStore.overrides): this is purely the fallback cwd handed
	// to CLI processes when a session has no per-chat override (#732).
	defaultCWD string // default cwd for CLI processes
	// 读写: core (init), lifecycle (attachHistorySource), discovery (attachHistorySource via RegisterForResume / RegisterCronStubWithChain / Takeover), shim (reconnect)
	claudeDir string // ~/.claude dir for loading session history
	// kiroSessionsDir is the kiro session-state root, plumbed into
	// cli.HistoryWiring at attachHistorySource time for the kirojsonl factory.
	// 读写: core (init), lifecycle (attachHistorySource), discovery (attachHistorySource via Register* / Takeover)
	kiroSessionsDir string

	// codexSessionsDir is the codex session-state root (~/.codex/sessions),
	// plumbed into cli.HistoryWiring for the codexjsonl factory.
	// 读写: core (init), lifecycle (attachHistorySource), discovery (attachHistorySource via Register* / Takeover)
	codexSessionsDir string

	// wsStore is the per-chat workspace-override facet (#383, #2495): overrides
	// map + LRU seq, dirty flag and gen, in internal/session/workspacestore.
	// No lock of its own — every method is called under r.mu because override
	// mutations must be atomic with session mutations (#2342) and eviction
	// reads r.ss.byChat. Zero value is usable.
	// 读写: core (init/load), lifecycle (ResetChat/RenameSession/spawn-resolver), cleanup (save), discovery (Takeover), workspace (SetWorkspace/resolve/Roots)
	wsStore workspacestore.Store

	// pp is the process-pool / shim-reconciler facet (#805): pendingSpawns,
	// spawningKeys, shimStuckOnReset, removeWg (processPool, process_pool.go).
	// No lock of its own — read/written ONLY under r.mu (no atomics).
	//
	// removeWg is a sync.WaitGroup and therefore NON-COPYABLE; embedding by
	// value is sound ONLY because Router is always &Router{} (go vet copylocks
	// enforces). Do NOT add any method/func taking Router or processPool by
	// value. spawningKeys done-channel pairing and pendingSpawns RAII balance
	// stay funneled through this struct under r.mu; lint recurses into processPool.
	// 读写: core (init/acquire-release RAII helpers), lifecycle (spawnSession write/close/Reset/ResetAndRecreate/GetOrCreate), shim (reconnect read), cleanup (RemoveAsync Add/Done/finishRemoveCleanup write), test helpers (removeWg.Wait)
	pp processPool

	// 读写: core (init), cleanup (saveIfDirty)
	storePath string

	// sessionRuns persists per-run wall-clock timing. Constructed in NewRouter
	// from filepath.Dir(storePath), injected into every ManagedSession; nil when
	// StorePath is empty. Closed in Shutdown to flush the async write worker.
	// 读写: core (init/spawn-config injection), lifecycle (spawn-config injection), discovery (takeover/register injection), cleanup (Invalidate/Close), runhistory (List/Stats read)
	sessionRuns *runhistory.Store

	// kid is the known-session-IDs facet (#600): IDs set, insertion-order
	// slice, dirty flag, gen and the gen-memoised sorted cache (knownIDsStore,
	// store.go). No lock of its own — read/written only under r.mu. gen /
	// sortedGen are PLAIN uint64: they have no lock-free reader. The annotation
	// below is the UNION of all domains; the lint recurses into knownIDsStore.
	// 读写: core (init/load), discovery (trackSessionID), cleanup (Cleanup/saveIfDirty/Shutdown save), store (snapshotKnownIDsSortedLocked)
	kid knownIDsStore

	// 读写: core (init), lifecycle (spawn config), shim (reconnect spawn config)
	noOutputTimeout time.Duration
	// 读写: core (init), lifecycle (spawn config), shim (reconnect spawn config), cleanup (Cleanup grace)
	totalTimeout time.Duration

	// onChange is an atomic.Pointer so notifyChange can load it lock-free on
	// the stream-event hot path (after every result event); set once at startup.
	// onChangeHolder makes the "function value through atomic pointer" idiom
	// explicit instead of `&fn` on a parameter copy.
	// 读写: core (SetOnChange/notifyChange)
	onChange atomic.Pointer[onChangeHolder]

	// onKeyRetired fires after Reset/Remove finish; lets side-indices keyed
	// on the session key (e.g. dispatch.MessageQueue) drop their entries.
	// 读写: core (SetOnKeyRetired/notifyKeyRetired), lifecycle (Reset), cleanup (Remove)
	onKeyRetired atomic.Pointer[onKeyRetiredHolder]

	// onSessionRetired mirrors onKeyRetired but exposes the session UUID
	// captured before teardown cleared r.ss.sessions[key]; see SetOnSessionRetired.
	// 读写: core (SetOnSessionRetired/notifyKeyRetired), lifecycle (Reset), cleanup (Remove)
	onSessionRetired atomic.Pointer[onSessionRetiredHolder]

	// historyWg tracks startup history-loading goroutines so Shutdown waits for them.
	// 读写: core (init Add/Done), cleanup (Shutdown Wait), lifecycle (loadResumeHistoryOnSpawn Add/Done)
	historyWg sync.WaitGroup
	// historyWgMu serialises the "check historyCtx.Err() then historyWg.Add(1)"
	// pair against Shutdown's historyCancel() (#2186): a cancel landing between
	// the nil-Err check and the Add could re-add to a WaitGroup already drained
	// to 0 with a Wait in flight ("WaitGroup is reused before previous Wait has
	// returned"). Shutdown takes this lock around historyCancel() (NOT around
	// Wait), so a producer that passed the check completes its Add before the
	// cancel is observable, and any later producer sees Err()!=nil and bails.
	// 读写: core (runHistoryTask), lifecycle (loadResumeHistoryOnSpawn), cleanup (Shutdown cancel)
	historyWgMu sync.Mutex

	// historyCtx is cancelled on Shutdown so in-flight LoadHistory*Ctx calls
	// abort promptly instead of blocking the drain on slow filesystems.
	// Paired with historyCancel (set by NewRouter, called from Shutdown).
	// 读写: core (init), lifecycle (attachHistorySource), cleanup (Shutdown cancel), shim (ReconnectShims parent ctx)
	historyCtx context.Context
	// 读写: core (init), cleanup (Shutdown cancel)
	historyCancel context.CancelFunc

	// shutdownOnce guards Shutdown against re-entry: a double call would race
	// the broadcast timer, re-cancel historyCtx and double-detach shim processes.
	// 读写: cleanup (Shutdown)
	shutdownOnce sync.Once

	// startOnce guards startBackgroundLifecycle against re-entry (NewRouter and
	// Start() both call it): a second run would overwrite r.attachmentTracker
	// (leaking the first tracker's goroutine) and schedule a redundant orphan sweep.
	// 读写: core (startBackgroundLifecycle)
	startOnce sync.Once

	// stopped is set true under r.mu inside Shutdown immediately before the
	// session snapshot is taken, and gates spawnSession: a spawn arriving after
	// the snapshot is rejected with ErrRouterStopped instead of installing a
	// shim+CLI the snapshot missed (leaking the subtree). Setting it under the
	// SAME r.mu hold as the snapshot makes gate and snapshot mutually
	// exclusive (no TOCTOU). Set once, never cleared — a Router is not reusable
	// after Shutdown. atomic.Bool so readers need only the r.mu they already hold (#1822).
	// 读写: cleanup (Shutdown Store under r.mu), lifecycle (spawnSession Load under r.mu)
	stopped atomic.Bool

	// eventLogDir is where per-session event log files live. Empty disables
	// event log persistence (tests / opt-out); non-empty wires eventLogPersister
	// for writes and naozhilog.Source for reads.
	// 读写: core (init), lifecycle (attachHistorySource), cleanup (dropEventLog)
	eventLogDir string
	// 读写: core (init), lifecycle (installPersistSink), cleanup (Shutdown)
	eventLogPersister *persist.Persister

	// cliDebugDir, when non-empty, is where each spawned Claude CLI writes its
	// `--debug-file` log. Set only when NAOZHI_CLI_DEBUG opts in at construction;
	// empty keeps every spawn bit-identical. spawn() derives a per-session path.
	// 读写: core (init only — immutable after NewRouter), lifecycle (spawn read)
	cliDebugDir string

	// naozhiSettingsFile mirrors RouterConfig.NaozhiSettingsFile ("" = legacy
	// `--setting-sources user`). Immutable after NewRouter; the router_shim
	// arg-drift check must pass the SAME value or --settings sessions look drifted.
	// 读写: core (init only), lifecycle (spawn read), router_shim (drift read)
	naozhiSettingsFile string
	// mcpConfigFile mirrors RouterConfig.MCPConfigFile ("" omits `--mcp-config`).
	// Immutable after NewRouter; the shim arg-drift comparison must mirror the spawn argv exactly.
	// 读写: core (init only), lifecycle (spawn read), router_shim (drift read)
	mcpConfigFile string

	// attachmentTracker is the refcount tracker that bridges event-log persist
	// events to .meta sidecar updates. nil when eventLogDir is unset (no event
	// source). See docs/rfc/attachment-refcount.md.
	// 读写: core (init/stopAttachmentTracker), lifecycle (installPersistSink), cleanup (clearAttachmentTrackerRefs / Shutdown stop)
	attachmentTracker *attachmentTracker

	// historyLoader loads a session's persisted JSONL history tail across a
	// prev_session_ids chain; tests inject a fixture (#458). Never nil and
	// read-only after NewRouter.
	// 读写: core (init default), lifecycle (LoadHistoryChainTail reader), shim (reconnect LoadHistoryChainTail reader)
	historyLoader HistoryLoader

	// resolver is the shared KeyResolver exposed via Resolver() so Dispatcher /
	// Hub / upstream wiring read one instance instead of drifting copies (#604).
	// nil when the caller did not opt in. Read-only after NewRouter; KeyResolver
	// is immutable post-construction so concurrent readers are safe.
	// 读写: core (init), Resolver() (read-only accessor)
	resolver *KeyResolver
}

// spawnerFunc is the signature panicSafeSpawnFn executes; tests inject a
// function that panics instead of constructing a real cli.Wrapper. Production
// wraps (*cli.Wrapper).Spawn in a closure at the call site.
type spawnerFunc func(context.Context, cli.SpawnOptions) (*cli.Process, error)

// pendingSpawnSlot is a one-shot RAII token returned by
// (*Router).acquirePendingSpawnSlotLocked. It guards r.pp.pendingSpawns against
// a stranded ++ on any panic / error path between increment and decrement.
// release() is idempotent: happy-path callers decrement explicitly at the
// original site and a `defer token.release()` absorbs any unexpected exit.
type pendingSpawnSlot struct {
	r        *Router
	released bool
}

// acquirePendingSpawnSlotLocked increments r.pp.pendingSpawns and returns a
// slot token whose release method can be called from any lock state.
//
// LOCK: caller must hold r.mu (write).
func (r *Router) acquirePendingSpawnSlotLocked() *pendingSpawnSlot {
	r.pp.pendingSpawns++
	return &pendingSpawnSlot{r: r}
}

// releaseLocked decrements pendingSpawns; caller must hold r.mu for writing.
// Idempotent — a second call (e.g. from defer) is a no-op.
func (s *pendingSpawnSlot) releaseLocked() {
	if s == nil || s.released {
		return
	}
	s.r.pp.pendingSpawns--
	s.released = true
}

// release is the lock-agnostic counterpart used from defer. It acquires r.mu
// only when the slot has not yet been released, so the happy path (which calls
// releaseLocked() inline) pays no extra lock acquisition. Idempotent.
func (s *pendingSpawnSlot) release() {
	if s == nil || s.released {
		return
	}
	s.r.mu.Lock()
	if !s.released {
		s.r.pp.pendingSpawns--
		s.released = true
	}
	s.r.mu.Unlock()
}

// panicSafeSpawn invokes the runner's Spawn inside a deferred recover so a
// panic from the spawn path cannot leave pendingSpawns stranded in
// spawnSession (which would make every subsequent GetOrCreate fail with
// ErrMaxProcs until restart). The panic becomes a regular error so the
// caller's standard "spawn process: %w" wrap applies.
//
// Takes cli.Runner (the placement seam) rather than *cli.Wrapper so sandbox
// placements reuse the same protection; (*cli.Wrapper)(nil).Runner() returns
// nil, which is checked here.
func panicSafeSpawn(
	ctx context.Context,
	r cli.Runner,
	opts cli.SpawnOptions,
	key, backendID string,
) (*cli.Process, error) {
	if r == nil {
		// Wrap ErrNoCLIWrapper so error classification treats a nil runner
		// identically to the nil-wrapper guard upstream.
		return nil, fmt.Errorf("no runner for backend %q: %w", backendID, ErrNoCLIWrapper)
	}
	return panicSafeSpawnFn(ctx, r.Spawn, opts, key, backendID)
}

// panicSafeSpawnFn is the testable core: tests inject a spawnerFunc that
// panics to verify the recover path without a real wrapper.
func panicSafeSpawnFn(
	ctx context.Context,
	spawn spawnerFunc,
	opts cli.SpawnOptions,
	key, backendID string,
) (proc *cli.Process, err error) {
	defer func() {
		if r := recover(); r != nil {
			// Counter and slog record are paired inside the recover arm so
			// naozhi_spawn_panic_recovered_total counts exactly one per absorbed panic.
			metrics.SpawnPanicRecoveredTotal.Add(1)
			slog.Error("spawnSession: wrapper.Spawn panicked",
				"key", key, "backend", backendID, "panic", r,
				"stack", string(debug.Stack()))
			// Unprefixed: the caller wraps with "spawn process: %w". %w when the
			// panic value is an error preserves the errors.Is/As chain.
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

// indexAdd adds key to the chat→sessions index. No-op when index is nil.
// Must be called under r.mu.
func (r *Router) indexAdd(key string) {
	// keyhash → key fast-path for the attachment tracker resolver (#1646);
	// independent of byChat (nil in some test routers).
	if r.ss.keyhash != nil {
		r.ss.keyhash[persist.KeyHash(key)] = key
	}
	if r.ss.byChat == nil {
		return
	}
	ck := chatKeyFor(key)
	set := r.ss.byChat[ck]
	if set == nil {
		set = make(map[string]struct{})
		r.ss.byChat[ck] = set
	}
	set[key] = struct{}{}
}

// indexDel removes key from the chat→sessions index. No-op when index is nil.
// Must be called under r.mu.
func (r *Router) indexDel(key string) {
	// Only delete the keyhash entry when the stored key matches, keeping the
	// invariant exact under a (theoretical) hash collision (#1646).
	if r.ss.keyhash != nil {
		kh := persist.KeyHash(key)
		if r.ss.keyhash[kh] == key {
			delete(r.ss.keyhash, kh)
		}
	}
	if r.ss.byChat == nil {
		return
	}
	ck := chatKeyFor(key)
	set := r.ss.byChat[ck]
	if set == nil {
		return
	}
	delete(set, key)
	if len(set) == 0 {
		delete(r.ss.byChat, ck)
	}
}

// HistoryLoader abstracts loading a session's persisted JSONL history tail
// across a prev_session_ids chain, so unit tests can inject a fixture without
// wiring the whole discovery chain (#458). Production: discoveryHistoryLoader.
type HistoryLoader interface {
	// LoadHistoryChainTail walks the JSONL files for ids (newest→oldest)
	// under claudeDir/cwd and returns up to limit entries. ctx cancellation
	// aborts the load promptly.
	LoadHistoryChainTail(ctx context.Context, claudeDir string, ids []string, cwd string, limit int) []clievent.EventEntry
}

// discoveryHistoryLoader is the production HistoryLoader backed by the
// discovery package. Stateless; the zero value is ready to use.
type discoveryHistoryLoader struct{}

func (discoveryHistoryLoader) LoadHistoryChainTail(ctx context.Context, claudeDir string, ids []string, cwd string, limit int) []clievent.EventEntry {
	return discovery.LoadHistoryChainTailCtx(ctx, claudeDir, ids, cwd, limit)
}

// RouterConfig holds configuration for the session router.
//
// Every field is read at NewRouter construction time; treat the struct as
// immutable afterwards. Changing any of these at runtime requires a naozhi
// restart, except `~/.claude/settings.json`, which cc re-reads on every spawn
// via `--setting-sources user` (docs/rfc/direct-user-settings.md).
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
	// BackendModels / BackendExtraArgs override Model / ExtraArgs per backend.
	// BackendExtraArgs REPLACES (does not append to) the router-level ExtraArgs
	// for that backend; per-session AgentOpts.ExtraArgs is appended on top. An
	// operator wanting to keep a router-wide flag like `--setting-sources ""`
	// must re-specify it in every override — additive semantics would make it
	// impossible to drop a default flag for one backend.
	BackendModels    map[string]string
	BackendExtraArgs map[string][]string
	// BackendEfforts is the resolved thinking-effort tier per backend ID. There
	// is deliberately no router-wide counterpart: the composition root folds
	// cli.effort in AND drops it for backends whose protocol cannot accept a
	// tier; a router-level default would resurrect it and desync arg-drift
	// detection from the real spawn. docs/rfc/kiro-effort-control.md
	BackendEfforts map[string]string
	// BackendModelLists is the operator-declared model manifest per backend ID
	// (cli.backends[].models), the fallback tier below agent-reported manifests
	// in BackendModelManifest. Entries were validateModelString-gated at config load.
	BackendModelLists map[string][]string
	// AccessProfiles is the named auth/upstream overlay registry keyed by
	// profile ID (RFC project-access-profile). Nil/empty ⇒ every session runs
	// on the global settings.json baseline.
	AccessProfiles map[string]AccessProfile
	// DefaultAccessProfile is applied to a session that resolves to no explicit
	// profile (lowest precedence, below opts / override / resume lock). "" =
	// global-baseline fallthrough. Must be a key in AccessProfiles (validated at config load).
	DefaultAccessProfile string
	Workspace            string
	StorePath            string
	NoOutputTimeout      time.Duration
	TotalTimeout         time.Duration
	ClaudeDir            string
	// NaozhiSettingsFile, when non-empty, is the naozhi-owned Claude settings
	// file (RFC naozhi-owned-settings-v3): every ClaudeProtocol spawn then runs
	// `--setting-sources "" --settings <file>` instead of `--setting-sources
	// user`. ACP backends ignore it.
	NaozhiSettingsFile string
	// MCPConfigFile, when non-empty, is an operator-owned MCP server definition
	// file (RFC cli-mcp-config): every ClaudeProtocol spawn runs `--mcp-config
	// <file>`; empty omits the flag. ACP / codex backends ignore it. Router-global
	// on purpose: it does NOT travel through the per-backend maps (which would
	// push it onto backends that cannot accept the flag), and MCP definitions
	// are a high-privilege operator decision with no per-agent / per-project override.
	//
	// The caller MUST have validated the file (exists, parses as JSON, has an
	// `mcpServers` object) — cc refuses to start otherwise, turning a typo into
	// a total spawn outage; cmd/naozhi's resolveMCPConfigFile passes "" on failure.
	MCPConfigFile string
	// KiroSessionsDir is the kiro CLI's session-state root (~/.kiro/sessions/cli).
	// Empty disables kiro history fallback.
	KiroSessionsDir string
	// CodexSessionsDir is the codex CLI's session-state root (~/.codex/sessions).
	// Empty disables codex history fallback.
	CodexSessionsDir string
	// EventLogDir is where per-session event log files live. Empty DISABLES
	// event log persistence (Claude CLI JSONL becomes the sole history source);
	// non-empty spins up a persist.Persister, wires every session's cli.EventLog
	// to it and installs a merged.Source (naozhilog + claudejsonl) as history
	// fallback. Usually next to StorePath. docs/rfc/event-log-persistence.md §4.
	EventLogDir string
	// EventLogGenerator tags every new <keyhash>.log header with the naozhi
	// build identifier. Optional.
	EventLogGenerator string
	// EventLogDevMode enables Persister's panic-on-replay-phase guard (RFC
	// §3.2.3): test / CI builds set it so a SetPersistSink ordering regression
	// panics immediately; production drops + counts instead.
	EventLogDevMode bool

	// EventLogPersister, when non-nil, is used as the event-log sink instead of
	// constructing one from EventLogDir/EventLogGenerator/EventLogDevMode, so
	// callers owning the Persister lifecycle can share one instance across
	// routers or pre-configure observers/codecs. nil + non-empty EventLogDir ⇒
	// the router constructs the Persister itself.
	EventLogPersister *persist.Persister

	// HistoryLoader loads a session's persisted JSONL history tail across a
	// prev_session_ids chain. nil ⇒ discovery.LoadHistoryChainTailCtx; tests
	// inject a fixture (#458).
	HistoryLoader HistoryLoader

	// Resolver is the shared KeyResolver. When set, callers (Dispatcher, Hub,
	// upstream wiring) should fetch it via Router.Resolver() instead of building
	// their own from cfg.Agents, which drifted across construction sites (#604).
	// nil leaves Router.Resolver() returning nil.
	Resolver *KeyResolver
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
	// for code that still reads r.bkStore.wrapper directly (mostly tests).
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
		// Pick deterministically: Go map iteration is randomised, so without
		// sorting a multi-backend deployment with no explicit DefaultBackend
		// would flip its default on every process start.
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
		maxProcs:         cfg.MaxProcs,
		ttl:              cfg.TTL,
		pruneTTL:         cfg.PruneTTL,
		defaultCWD:       cfg.Workspace,
		claudeDir:        cfg.ClaudeDir,
		kiroSessionsDir:  cfg.KiroSessionsDir,
		codexSessionsDir: cfg.CodexSessionsDir,
		storePath:        cfg.StorePath,
		noOutputTimeout:  cfg.NoOutputTimeout,
		totalTimeout:     cfg.TotalTimeout,
		eventLogDir:      cfg.EventLogDir,
		historyLoader:    cfg.HistoryLoader,
		resolver:         cfg.Resolver,
	}
	// Value facets (ss / kid / pp / bkStore) have no lock of their own and are
	// not composite-literal initialised, so their maps are allocated here.
	// wsStore is zero-value usable (maps allocated lazily on first write).
	r.ss.sessions = make(map[string]*ManagedSession)
	r.ss.byChat = make(map[string]map[string]struct{})
	r.ss.keyhash = make(map[string]string)
	r.ss.idToKey = make(map[string]string)
	r.kid.ids = make(map[string]bool)
	r.pp.spawningKeys = make(map[string]chan struct{})
	r.pp.shimStuckOnReset = make(map[string]bool)
	r.bkStore.wrapper = defaultWrapper
	r.bkStore.wrappers = wrappers
	r.bkStore.defaultBackend = defaultBackend
	r.bkStore.model = cfg.Model
	r.bkStore.extraArgs = cfg.ExtraArgs
	r.bkStore.backendModels = cfg.BackendModels
	r.bkStore.backendExtraArgs = cfg.BackendExtraArgs
	r.bkStore.backendEfforts = cfg.BackendEfforts
	r.bkStore.configuredModelLists = cfg.BackendModelLists
	r.bkStore.modelManifests = make(map[string][]cli.ModelInfo)
	r.bkStore.backendOverrides = make(map[string]string)
	r.bkStore.accessProfileOverrides = make(map[string]string)
	r.accessProfiles = cfg.AccessProfiles
	r.defaultAccessProfile = cfg.DefaultAccessProfile
	// Run-history store is rooted next to the session store (its own config,
	// NOT cron's). Empty StorePath disables persistence (no-op store).
	if cfg.StorePath != "" {
		r.sessionRuns = runhistory.NewStore(filepath.Dir(cfg.StorePath), 0, 0)
	}

	// nil HistoryLoader → production discovery-backed implementation so the
	// rest of the router can call r.historyLoader unconditionally (#458).
	if r.historyLoader == nil {
		r.historyLoader = discoveryHistoryLoader{}
	}
	// Spin up the event-log persister BEFORE touching the session store; the
	// startup load path needs a live sink for restored ManagedSessions. A
	// caller-owned Persister wins; otherwise EventLogDir triggers in-router construction.
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
	// CLI debug capture (opt-in via NAOZHI_CLI_DEBUG), read once at startup.
	// Data root = event-log dir's parent, so debug capture stays off when the
	// event log is disabled. resolveCLIDebugDir returns "" on any failure so a
	// debug-dir problem never blocks spawning.
	r.cliDebugDir = resolveCLIDebugDir(cfg.EventLogDir)
	r.naozhiSettingsFile = cfg.NaozhiSettingsFile
	r.mcpConfigFile = cfg.MCPConfigFile
	r.shutdownCond = sync.NewCond(&r.mu)
	// historyCtx is cancelled only by Shutdown so startup history loads and
	// reconnect-time JSONL parses abort promptly on slow filesystems.
	r.historyCtx, r.historyCancel = context.WithCancel(context.Background())

	// Load every session ID ever used. Insertion order is lost on reload (persisted
	// as an unordered list); seed the order slice from the map so FIFO eviction
	// resumes after the first (arbitrary-order) post-restart overflow.
	if loaded := loadKnownIDs(r.storePath); loaded != nil {
		r.kid.ids = loaded
		r.kid.order = make([]string, 0, len(loaded))
		for id := range loaded {
			r.kid.order = append(r.kid.order, id)
		}
	}

	// Load persisted workspace overrides (/cd settings)
	r.wsStore.Seed(loadWorkspaceOverrides(r.storePath))

	// Restore sessions from store
	if restored := loadStore(r.storePath); restored != nil {
		for key, entry := range restored {
			// SECURITY: reject sys: entries even though saveStore already skips
			// them (RFC v2.1 §3.4). A sys: entry on disk means a tampered
			// sessions.json; resurrecting it would let an attacker pre-seed a
			// ManagedSession with chosen label_origin etc. Daemons re-register
			// stubs at startup, so dropping the persisted copy is safe.
			if IsSysKey(key) {
				slog.Warn("session store: dropping unexpected sys: entry",
					"key", key,
					"hint", "sys entries should never persist; possible sessions.json tampering")
				continue
			}
			r.restoreSessionFromEntry(key, entry)
		}
	}

	// Sidebar is driven purely by sessions.json (and live activity); filesystem-
	// discovered sessions go to the separate "history" panel so Remove is a
	// durable delete until the user explicitly resumes an entry.

	// Strip auto-spawn / auto-backfill segments from persisted prev_session_ids
	// chains (RFC project-stable-session-key §9.2), keeping only the real
	// sessionID-rotation chain. Must run BEFORE the Tier 1 / Tier 2 history
	// loaders so they observe the cleaned chain.
	r.retireAutoChainOnce()

	// Async-load history for all restored sessions so the dashboard shows
	// conversation history without waiting for the next message.
	r.startBackgroundHistoryLoaders()

	// Orphan sweep + attachment tracker are background side effects funnelled
	// through startBackgroundLifecycle (startOnce-guarded; Start() shares it).
	r.startBackgroundLifecycle()

	r.bkStore.backendIDs = computeBackendIDs(r.bkStore.wrapper, r.bkStore.wrappers, r.bkStore.defaultBackend)

	return r
}

// restoreSessionFromEntry rebuilds a single persisted ManagedSession from its
// on-disk storeEntry and publishes it into the router's maps + indexes. The
// caller owns the IsSysKey skip guard and the loadStore range.
//
// LOCK: must be invoked from NewRouter under construction (no concurrent
// r.ss.sessions writers); publishSessionLocked + the idToKey write assume
// exclusive access, which the publish-after-construct contract guarantees.
func (r *Router) restoreSessionFromEntry(key string, entry *storeEntry) {
	// Resolve the wrapper that owned this session's backend so the snapshot
	// carries the correct CLI identity after a pure restore (no shim reconnect).
	// Pre-multi-backend entries have empty Backend → router default.
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
		runStore:           r.sessionRuns,
	}
	storeTotalCost(&s.totalCost, entry.TotalCost)
	// Legacy stores (predating cost_spent) seed costSpent from TotalCost so the
	// established total keeps showing; lastCumulativeCost stays 0 there, which
	// is safe — the first post-upgrade raw cumulative is a fresh delta against 0.
	if entry.CostSpent > 0 {
		storeTotalCost(&s.costSpent, entry.CostSpent)
	} else {
		storeTotalCost(&s.costSpent, entry.TotalCost)
	}
	storeTotalCost(&s.lastCumulativeCost, entry.LastCumulativeCost)
	s.setWorkspace(entry.Workspace)
	s.SetBackend(restoreBackendID)
	// Restore the recorded access profile so a resume relocks the same auth
	// chain rather than re-resolving from a since-changed project binding (RFC §7).
	s.SetAccessProfile(entry.AccessProfile)
	s.SetCLIName(cliName)
	s.SetCLIVersion(cliVersion)
	if entry.UserLabel != "" {
		s.SetUserLabel(entry.UserLabel)
	}
	// Empty LabelOrigin in pre-v2.1 stores means "user" to daemons (RFC §7.3),
	// so no default is synthesised here.
	if entry.LabelOrigin != "" {
		s.setLabelOrigin(entry.LabelOrigin)
	}
	// Seed model from the store so the dashboard renders it on post-restart
	// reattach, before the first new turn re-emits system/init.
	if entry.Model != "" {
		s.SetModel(entry.Model)
	}
	// SECURITY: these values feed --model/--effort argv on the next spawn and
	// sessions.json is hand-editable, so re-validate with the same validators
	// as SetSessionTuning / config; drop-with-warn so one corrupt entry cannot
	// block the whole store load (docs/rfc/dashboard-model-effort-control.md §4.3).
	if entry.TuningModel != "" {
		if err := tuningspec.ValidateModel("stored tuning_model", entry.TuningModel); err != nil {
			slog.Warn("dropping invalid persisted tuning_model", "key", entry.Key, "err", err)
		} else {
			s.SetTuningModel(entry.TuningModel)
		}
	}
	if entry.TuningEffort != "" {
		if err := tuningspec.ValidateEffort("stored tuning_effort", entry.TuningEffort); err != nil {
			slog.Warn("dropping invalid persisted tuning_effort", "key", entry.Key, "err", err)
		} else {
			s.SetTuningEffort(entry.TuningEffort)
		}
	}
	s.setSessionID(entry.SessionID)
	if entry.LastActive != 0 {
		s.lastActive.Store(entry.LastActive)
	}
	// Sidebar order anchor: prefer persisted CreatedAt, fall back to LastActive
	// for pre-feature stores; if both are zero stamp now so the entry still
	// gets a stable comparator key.
	switch {
	case entry.CreatedAt != 0:
		s.createdAt.Store(entry.CreatedAt)
	case entry.LastActive != 0:
		s.createdAt.Store(entry.LastActive)
	default:
		s.initCreatedAtIfUnset()
	}
	// publishSessionLocked funnels attachHistorySource + map insert + index
	// update so the triple-index invariant is a property of the publish step.
	r.publishSessionLocked(key, s, false)
	r.trackSessionID(entry.SessionID)
	if entry.SessionID != "" {
		r.ss.idToKey[entry.SessionID] = key
	}
}

// startBackgroundLifecycle launches the background side effects of
// construction: runOrphanSweep (reaps <keyhash>.log files for sessions with no
// live entry, RFC event-log-persistence §4.4) and startAttachmentTracker
// (refcount worker driven by OnPersistedEntry events). startOnce-guarded;
// both also guard-check r.eventLogDir.
//
// retireAutoChainOnce is intentionally NOT here: it must run synchronously
// BEFORE the Tier 1 / Tier 2 history goroutines spawn so they observe the
// cleaned prev_session_ids chain. Treat it as construction, not a side effect.
func (r *Router) startBackgroundLifecycle() {
	r.startOnce.Do(func() {
		r.runOrphanSweep()
		r.startAttachmentTracker()
	})
}

// startBackgroundHistoryLoaders launches the tier 1 / tier 2 history-load
// goroutines for every restored session. Tier 1 (naozhilog, when
// r.eventLogPersister is set) preserves Images / AskQuestion / agent-team
// linkage Claude JSONL cannot represent. Tier 2 (Claude CLI JSONL) skips
// sessions tier 1 filled; shim-managed sessions wait shimReconnectGraceDelay
// so ReconnectShims can inject first, then backfill only if still empty. One
// historyLoadSem bounds total history I/O across both tiers. Both finish
// BEFORE the process's PersistSink is installed, so replayed entries are
// tagged replayPhase=true and dropped. LOCK: NewRouter-only — ranges over
// r.ss.sessions unlocked under the publish-after-construct contract.
func (r *Router) startBackgroundHistoryLoaders() {
	historyLoadSem := make(chan struct{}, historyLoadConcurrency)

	// Tier 1: naozhilog (in-process per-session log).
	if r.eventLogPersister != nil {
		sem := historyLoadSem
		for _, s := range r.ss.sessions {
			r.historyWg.Add(1)
			go func() {
				defer r.historyWg.Done()
				select {
				case sem <- struct{}{}:
				case <-r.historyCtx.Done():
					return
				}
				defer func() { <-sem }()
				src := newEventLogLocalSource(r.eventLogDir, s.key)
				entries, err := src.LoadLatest(r.historyCtx, maxPersistedHistory)
				if err != nil || len(entries) == 0 {
					return
				}
				// InjectHistoryIfEmpty atomically guards against a concurrent
				// ReconnectShims / Tier 2 loader having already filled the
				// session; a separate check-then-inject would double-inject (#1812).
				if !s.InjectHistoryIfEmpty(entries) {
					return
				}
				slog.Info("loaded session history from naozhi event log",
					"key", s.key, "entries", len(entries))
				r.notifyChange()
			}()
		}
	}

	// Tier 2: Claude CLI JSONL.
	if r.claudeDir == "" {
		return
	}
	shimKeys := r.shimManagedKeys()
	sem := historyLoadSem
	for _, s := range r.ss.sessions {
		if s.getSessionID() == "" {
			continue
		}
		deferred := shimKeys[s.key]
		r.historyWg.Add(1)
		go func() {
			defer r.historyWg.Done()
			if deferred {
				// Wait for ReconnectShims' first pass; historyCtx cancel aborts.
				// NewTimer + Stop (not time.After) so a fast shutdown does not
				// leak a timer per goroutine for the whole grace window.
				graceTimer := time.NewTimer(shimReconnectGraceDelay)
				select {
				case <-graceTimer.C:
					// Fired — no Stop needed, channel already drained.
				case <-r.historyCtx.Done():
					if !graceTimer.Stop() {
						<-graceTimer.C
					}
					return
				}
				if s.hasInjectedHistory() {
					return
				}
				// Counter sits AFTER the hasInjectedHistory short-circuit so
				// only the fallback branch (short-lived-shim race) increments.
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

			// Skip when tier 1 already filled the session — otherwise a deploy
			// with both sources would double-inject the first ~500 entries.
			if s.hasInjectedHistory() {
				return
			}

			// Ordered chain (prev + current) via SnapshotChainIDs() — a clone
			// under historyMu — because this goroutine holds neither r.mu nor
			// historyMu while a concurrent cron stub refresh may reassign the
			// slice header under r.mu (#2055). LoadHistoryChainTail walks
			// newest→oldest and stops at maxPersistedHistory entries.
			ids := s.SnapshotChainIDs()

			allEntries := r.historyLoader.LoadHistoryChainTail(
				r.historyCtx, r.claudeDir, ids, s.Workspace(), maxPersistedHistory,
			)
			if len(allEntries) == 0 {
				return
			}
			// The hasInjectedHistory() checks above only skip the expensive
			// read; the inject itself must be atomic, so InjectHistoryIfEmpty
			// does the final "still empty?" check and the append under one
			// historyMu hold (#1812).
			if !s.InjectHistoryIfEmpty(allEntries) {
				return
			}
			slog.Info("loaded session history on startup", "key", s.key, "entries", len(allEntries), "chain", len(ids), "deferred", deferred)
			r.notifyChange()
		}()
	}
}

// Start exposes the background-lifecycle hook so callers can defer the side
// effects; NewRouter still invokes it eagerly. ctx is accepted for
// forward-compat — sweepers currently honour r.historyCtx.
func (r *Router) Start(_ context.Context) {
	r.startBackgroundLifecycle()
}

// onChangeHolder wraps a callback so the atomic pointer Store site is an
// explicit composite literal rather than `&fn` (address of a parameter copy),
// which is easy to break when inlining / renaming the parameter.
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

// onSessionRetiredHolder mirrors onKeyRetiredHolder but carries the session
// UUID alongside the routing key, so the sessionID-keyed RetiredStore path
// need not reverse-lookup the UUID after teardown cleared r.ss.sessions[key].
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
// teardown completes, receiving the routing key and the session UUID captured
// before teardown cleared r.ss.sessions[key]. sessionID may be empty when the
// session retired before the CLI ever returned a UUID; callbacks must tolerate
// that. Independent of SetOnKeyRetired; both fire on the same teardown event.
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
// r.ss.sessions[key] is already gone by the time we reach this hook.
func (r *Router) notifyKeyRetired(key, sessionID string) {
	if h := r.onKeyRetired.Load(); h != nil {
		h.fn(key)
	}
	if h := r.onSessionRetired.Load(); h != nil {
		h.fn(key, sessionID)
	}
}

// NotifyIdle wakes the Shutdown wait loop so it can re-check running sessions.
// Call after a message send completes (session transitions running → ready).
//
// r.mu MUST be held around Broadcast: Shutdown re-checks "running" between
// Wait() calls, so a Broadcast landing between that check and Wait() would be
// lost and Shutdown would only wake from the 30s AfterFunc safety net.
// Holding r.mu blocks NotifyIdle until Shutdown is parked in Wait(). Callers
// are end-of-turn only, so the extra lock round-trip is free.
func (r *Router) NotifyIdle() {
	if r.shutdownCond == nil {
		return
	}
	r.mu.Lock()
	r.shutdownCond.Broadcast()
	r.mu.Unlock()
}

// ChatKey builds a chat-level key (without agent suffix) for workspace
// overrides. SECURITY: components are sanitized with the same rule as
// SessionKey so a malicious chat ID with C0/ANSI bytes or Unicode bidi
// overrides cannot inject fabricated slog.TextHandler log lines.
func ChatKey(platform, chatType, chatID string) string {
	return sanitizeKeyComponent(platform) + ":" + sanitizeKeyComponent(chatType) + ":" + sanitizeKeyComponent(chatID)
}

// DefaultWorkspace returns the router's default working directory.
func (r *Router) DefaultWorkspace() string {
	return r.defaultCWD
}

// Version returns a monotonic counter incremented on every session mutation;
// the dashboard polls it from /api/sessions to skip full JSON comparison.
// Lock-free (atomic).
//
// The same counter serves two audiences: data version (session map changed;
// bumped under r.mu) and render version (BumpVersion from non-session
// mutations such as project favorite toggles). A Version() change therefore
// does NOT guarantee ListSessions() returns new data; the cost is one
// redundant debounced saveStore.
func (r *Router) Version() uint64 {
	return r.ss.gen.Load()
}

// BumpVersion forces a version increment + onChange broadcast even when no
// session mutation occurred, for non-session state the dashboard surfaces via
// /api/sessions (e.g. project favorite toggle): without the bump the poll-time
// version gate skips the re-render; without notifyChange the live WebSocket
// push is skipped. It does NOT set storeDirty — UI-refresh signal only, never
// use it when session state must be persisted.
func (r *Router) BumpVersion() {
	r.ss.gen.Add(1)
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
// cannot publish active = N+1 against a pre-spawn total = N (active > total on
// the dashboard). activeCount stays atomic for the lock-free spawn-admission path.
func (r *Router) Stats() (active, total int) {
	r.mu.RLock()
	total = len(r.ss.sessions)
	active = int(r.ss.activeCount.Load())
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

// listRefsPool reuses the *ManagedSession slice ListSessions captures under
// r.mu; at 1 Hz × N tabs × hundreds of sessions the per-call alloc dominates.
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
// The SessionSnapshot slice is freshly allocated, never pooled: it escapes to
// the caller and is JSON-marshalled across goroutine boundaries, so a pooled
// entry could be reused while a previous handler is still in flight. Only the
// *ManagedSession refs slice is pooled (cleared before Put).
func (r *Router) ListSessions() []SessionSnapshot {
	snaps, _ := r.ListSessionsWithVersion()
	return snaps
}

// ListSessionsWithVersion returns the session snapshot slice paired with the
// gen value sampled in the same r.mu.RLock epoch, so /api/sessions tags data
// with exactly the version that produced it (separate Version() +
// ListSessions() reads could publish data with a stale version, #726).
// Writers do `r.mu.Lock(); ...; gen.Add(1); r.mu.Unlock()`, so a reader
// holding RLock observes an atomically produced (sessions, gen) pair.
func (r *Router) ListSessionsWithVersion() ([]SessionSnapshot, uint64) {
	refsPtr := listRefsPool.Get().(*[]*ManagedSession)
	refs := (*refsPtr)[:0]
	r.mu.RLock()
	if cap(refs) < len(r.ss.sessions) {
		// Grow once to the new max instead of the append growth path; the
		// grown array is written back to the pool before Put below
		// (regression guard: listrefspool_grow_2309_test.go).
		refs = make([]*ManagedSession, 0, len(r.ss.sessions))
	}
	for _, s := range r.ss.sessions {
		refs = append(refs, s)
	}
	version := r.ss.gen.Load()
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
	return snapshots, version
}

// ListSessionsIfChanged is the gen-gated variant of ListSessionsWithVersion
// for the /api/sessions REST poll path (#1886): when gen has not advanced since
// sinceVersion it returns (nil, sinceVersion, false) WITHOUT touching
// r.ss.sessions or building snapshots, so the handler can answer
// {version, unchanged:true} and skip the marshal. Sound because writers bump
// gen under r.mu.Lock (see ListSessionsWithVersion); changed==true reuses
// ListSessionsWithVersion so the (snapshots, version) pair stays atomic.
func (r *Router) ListSessionsIfChanged(sinceVersion uint64) (snapshots []SessionSnapshot, version uint64, changed bool) {
	if cur := r.ss.gen.Load(); cur == sinceVersion {
		return nil, cur, false
	}
	snaps, v := r.ListSessionsWithVersion()
	return snaps, v, true
}

// SessionFor returns the session for the given key, or nil.
func (r *Router) SessionFor(key string) *ManagedSession {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.ss.sessions[key]
}

// DiscardPassthroughPending fires reason to any in-flight passthrough sends for
// the keyed session (no-op when absent), so consumers such as
// dispatch.discardQueue go through the router seam rather than the concrete
// *ManagedSession (#1612).
func (r *Router) DiscardPassthroughPending(key string, reason error) {
	if sess := r.SessionFor(key); sess != nil {
		sess.DiscardPassthroughPending(reason)
	}
}

// runHistoryTask launches fn in a goroutine tracked by r.historyWg, parented
// on r.historyCtx. Returns false (no goroutine) when historyCtx is already
// cancelled, guarding the late Add(1) race against historyWg.Wait(). Router
// still owns historyCtx/historyCancel/historyWg directly (#748); inline sites
// that also need a semaphore + per-task timeout stay in place.
//
// LOCK: callers must hold r.mu (read or write) when invoking, OR call outside
// the lock when historyCtx is guaranteed live (NewRouter init, early Start).
// The historyWg.Add(1) must be visible to Shutdown before the goroutine
// begins observable work.
func (r *Router) runHistoryTask(fn func(ctx context.Context)) bool {
	if r.historyCtx == nil {
		// Test routers built by struct literal (skip NewRouter) get a
		// never-cancelled background; production Router always wires
		// historyCtx in NewRouter before any caller can reach here.
		r.historyWg.Add(1)
		go func() {
			defer r.historyWg.Done()
			fn(context.Background())
		}()
		return true
	}
	// Decide spawn-or-refuse BEFORE Add(1): an Add-then-compensating-Done shape
	// lets Shutdown's Wait() observe the transient +1 and return, after which a
	// later Add re-adds to a drained WaitGroup (#1655). The Err() check and the
	// Add(1) must be one critical section vs Shutdown's historyCancel() (#2186);
	// see historyWgMu.
	r.historyWgMu.Lock()
	if r.historyCtx.Err() != nil {
		r.historyWgMu.Unlock()
		return false
	}
	r.historyWg.Add(1)
	r.historyWgMu.Unlock()
	go func() {
		defer r.historyWg.Done()
		fn(r.historyCtx)
	}()
	return true
}
