package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/history"
	"github.com/naozhi/naozhi/internal/history/claudejsonl"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/shim"
)

// R188-SEC-M2: model identifiers flow into the `--model` argv of the CLI child
// process. An authenticated dashboard user (or a malicious IM planner reply)
// could inject additional flags via a whitespace-containing model string. The
// project package's plannerModelRe enforces the same pattern for planner
// config; keep the regex in sync if either changes.
const maxModelBytes = 128

var modelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/\-]*$`)

// validateModel returns nil for empty (use router default) or any string
// matching modelRe under the byte cap; otherwise returns ErrInvalidModel.
func validateModel(model string) error {
	if model == "" {
		return nil
	}
	if len(model) > maxModelBytes {
		return fmt.Errorf("%w: exceeds %d bytes", ErrInvalidModel, maxModelBytes)
	}
	if !modelRe.MatchString(model) {
		return fmt.Errorf("%w: must match %s", ErrInvalidModel, modelRe)
	}
	return nil
}

// ErrInvalidModel is returned when AgentOpts.Model fails validateModel.
// Callers should map it to an HTTP 400 or IM error reply.
var ErrInvalidModel = errors.New("invalid model identifier")

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
var ErrMaxExemptSessions = errors.New("max exempt sessions reached")

// ErrNoCLIWrapper is returned when spawnSession is called but the router
// was constructed without a CLI wrapper (misconfiguration). This is
// permanent until the operator fixes config and restarts; retry loops
// should stop on this sentinel.
var ErrNoCLIWrapper = errors.New("no CLI wrapper configured")

// exemptKeyPrefixes lists the session-key namespaces that are exempt from
// TTL expiry, LRU eviction, and the active-process counter. Centralising
// the list keeps the policy one line away from anyone adding a new
// long-lived session type (e.g. a future "planner:" family) — previously
// the predicate was inlined at the single construction site while three
// separate skip branches read `s.exempt`. Keep the list sorted for grep.
//
// R176-ARCH-M1: references the canonical prefix constants in key.go so
// there is one source of truth for reserved namespaces. Scratch keys are
// deliberately NOT exempt — they are short-lived and should pay the
// normal TTL / eviction cost.
var exemptKeyPrefixes = []string{CronKeyPrefix, ProjectKeyPrefix}

// isExemptKey reports whether key belongs to an exempt namespace. Callers
// that already have a ManagedSession should prefer reading s.exempt —
// this helper exists for the construction path and for external callers
// that know the key but not the session.
func isExemptKey(key string) bool {
	for _, prefix := range exemptKeyPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// Router defaults applied by NewRouter when the corresponding RouterConfig
// field is zero. Exported so other packages (tests, config validation, CLI
// flag defaults) can reference the single source of truth instead of
// re-typing the literal and drifting out of sync. R70-ARCH-H5.
const (
	// DefaultMaxProcs is the concurrent-process cap applied when
	// RouterConfig.MaxProcs is not set.
	DefaultMaxProcs = 3
	// DefaultTTL is the idle-session eviction threshold applied when
	// RouterConfig.TTL is not set.
	DefaultTTL = 30 * time.Minute
	// DefaultPruneTTL is the "keep metadata for long-idle session" threshold
	// applied when RouterConfig.PruneTTL is not set. Entries older than this
	// are pruned from the store even when exempt.
	DefaultPruneTTL = 72 * time.Hour
)

const (
	// maxExemptSessions caps the number of alive exempt (planner) sessions
	// to prevent unbounded growth when many projects are configured.
	maxExemptSessions = 20

	// historyLoadConcurrency limits parallel disk I/O goroutines during
	// startup session history loading.
	historyLoadConcurrency = 10

	// ProjectScanInterval is how often the project root is rescanned
	// for CLAUDE.md changes. Exported for use by server package.
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
)

// Router manages session key -> ManagedSession mapping.
//
// Lock ordering: s.sendMu -> r.mu. The onSessionID callback acquires r.mu
// while sendMu is held (Send → onSessionID → trackSessionID). Code that
// holds r.mu (write) must NEVER acquire sendMu — release r.mu first.
// s.historyMu protects persistedHistory independently; never held with sendMu or r.mu.
// Read-only operations (ListSessions, GetSession, Stats, Version) use RLock.
type Router struct {
	mu           sync.RWMutex
	shutdownCond *sync.Cond // signaled when process state changes; conditioned on mu (write lock)
	sessions     map[string]*ManagedSession
	// sessionsByChat is a secondary index: chat key → session keys.
	// Enables O(k) ResetChat instead of O(n) full scan (k = agents per chat, typically 1-3).
	// Nil in test-created routers; all helpers below are nil-safe.
	sessionsByChat map[string][]string
	wrapper        *cli.Wrapper            // default (legacy single-backend) wrapper
	wrappers       map[string]*cli.Wrapper // backend ID → wrapper (nil in legacy mode)
	defaultBackend string                  // backend ID used when AgentOpts.Backend is empty
	maxProcs       int
	ttl            time.Duration
	pruneTTL       time.Duration
	model          string
	extraArgs      []string
	// backendModels / backendExtraArgs optionally override model and args
	// per backend ID. Read-only after NewRouter.
	backendModels    map[string]string
	backendExtraArgs map[string][]string
	workspace        string // default cwd for CLI processes
	claudeDir        string // ~/.claude dir for loading session history

	// workspaceOverrides stores per-chat workspace overrides.
	// Key format: "platform:chatType:chatID"
	workspaceOverrides map[string]string

	// backendOverrides stores per-session backend preferences picked by
	// the dashboard at session-creation time. Keyed by full session key
	// (including agent suffix) so two sessions on the same chat can run
	// against different backends.
	backendOverrides map[string]string

	// activeCount tracks currently alive processes (non-exempt only).
	// Writes happen under r.mu (write lock); atomic access lets Stats()
	// read lock-free so the dashboard /api/sessions hot path does not
	// take a second r.mu RLock right after ListSessions() released one.
	// R58-PERF-F1.
	activeCount atomic.Int64

	// pendingSpawns tracks Spawn() calls in progress (lock released during spawn)
	pendingSpawns int

	// spawningKeys records keys whose spawnSession is in flight. ReconnectShims
	// consults this set before declaring a discovered shim "orphan": a shim may
	// have written its state file after we dropped r.mu for wrapper.Spawn() but
	// before the new ManagedSession is installed, and without this set a
	// concurrent reconcile would shut the fresh shim down as an orphan.
	spawningKeys map[string]struct{}

	storePath  string
	storeDirty bool // true when sessions changed since last save
	// storeGen increments on each mutation. Writes happen under r.mu (write
	// lock) but atomic.Uint64 also lets Version() read lock-free — the
	// dashboard polls Version() every few seconds from the /api/sessions
	// hot path, and the previous RLock layered on top of ListSessions'
	// RLock made each poll take two contended trips through r.mu.
	storeGen         atomic.Uint64
	wsOverridesDirty bool          // true when workspace overrides changed since last save
	wsOverridesGen   atomic.Uint64 // increments on each ws-override mutation, mirrors storeGen pattern

	// knownIDs tracks ALL session IDs ever used by naozhi, including
	// sessions that have been removed/reset/evicted. Used by the
	// discovered-session scanner to match CLI processes to naozhi keys,
	// and as a secondary filter for filesystem-based recent sessions.
	knownIDs map[string]bool
	// knownIDsOrder preserves insertion order so overflow eviction drops the
	// oldest (FIFO) rather than picking randomly via map iteration — random
	// eviction could drop a still-active session ID, causing discovery to
	// misclassify its CLI process as an external (non-naozhi) session.
	knownIDsOrder   []string
	knownIDsDirty   bool
	knownIDsGen     uint64    // incremented on each knownIDs mutation (add/evict)
	knownIDsSavedAt time.Time // last successful saveKnownIDs; throttles fsync to 5min

	// sessionIDToKey is a reverse index from session ID to session key.
	// Used by RegisterForResume for O(1) deduplication instead of O(n) scan.
	// Maintained under r.mu by setSessionIDIndex/clearSessionIDIndex.
	sessionIDToKey map[string]string

	noOutputTimeout time.Duration
	totalTimeout    time.Duration

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
	onChange atomic.Pointer[onChangeHolder]

	// historyWg tracks startup history-loading goroutines so Shutdown waits for them.
	historyWg sync.WaitGroup

	// historyCtx is cancelled on Shutdown so in-flight LoadHistory*Ctx calls
	// abort promptly instead of blocking the drain on slow filesystems.
	// Paired with historyCancel (set by NewRouter, called from Shutdown).
	historyCtx    context.Context
	historyCancel context.CancelFunc

	// shutdownOnce guards Shutdown against re-entry. Production flow invokes
	// Shutdown exactly once from the signal handler, but future code paths
	// (test teardown, hot-restart) might call it again; a double call would
	// race the broadcast timer, re-close historyCtx via historyCancel (safe
	// on its own but noisy) and double-detach shim processes. R49-REL-SHUTDOWN-ONCE.
	shutdownOnce sync.Once
}

// spawnerFunc is the signature panicSafeSpawnFn executes; abstracting it lets
// tests inject a function that panics instead of constructing a real
// cli.Wrapper (whose Spawn path has no panic-injection seam). Production
// wraps (*cli.Wrapper).Spawn in a closure at the call site.
type spawnerFunc func(context.Context, cli.SpawnOptions) (*cli.Process, error)

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
			slog.Error("spawnSession: wrapper.Spawn panicked",
				"key", key, "backend", backendID, "panic", r,
				"stack", string(debug.Stack()))
			err = fmt.Errorf("spawn process: panic: %v", r)
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

// claudeProjectSlug maps a CWD to the directory name Claude CLI uses under
// ~/.claude/projects/. Mirrors the transformation in internal/discovery
// (projDirName): every "/" becomes "-", so "/home/user/proj" → "-home-user-proj".
// Duplicated here rather than imported to keep discovery's internal helpers
// package-private; the two must stay in sync if Claude's scheme ever changes.
func claudeProjectSlug(cwd string) string {
	return strings.ReplaceAll(cwd, "/", "-")
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

// cliNameDefault returns the CLI display name from the wrapper, or empty if no wrapper.
// Kept as an internal helper (not inlined into CLIName) because snapshot
// builders in this file already route through it.
func (r *Router) cliNameDefault() string {
	if r.wrapper != nil {
		return r.wrapper.CLIName
	}
	return ""
}

// cliVersionDefault returns the CLI version from the wrapper, or empty if no wrapper.
func (r *Router) cliVersionDefault() string {
	if r.wrapper != nil {
		return r.wrapper.CLIVersion
	}
	return ""
}

// CLIName exposes the wrapper's CLI display name for status endpoints.
func (r *Router) CLIName() string { return r.cliNameDefault() }

// CLIVersion exposes the wrapper's detected CLI version for status endpoints.
func (r *Router) CLIVersion() string { return r.cliVersionDefault() }

// wrapperFor selects the wrapper for the requested backend ID.
// Empty backend picks the router default. Returns (wrapper, effectiveID).
// Callers must treat a nil wrapper as "no backend available" and fail fast.
func (r *Router) wrapperFor(backend string) (*cli.Wrapper, string) {
	if len(r.wrappers) == 0 {
		id := backend
		if id == "" && r.wrapper != nil {
			id = r.wrapper.BackendID
		}
		return r.wrapper, id
	}
	if backend != "" {
		if w, ok := r.wrappers[backend]; ok {
			return w, backend
		}
	}
	if r.defaultBackend != "" {
		if w, ok := r.wrappers[r.defaultBackend]; ok {
			return w, r.defaultBackend
		}
	}
	// Last-resort fallback: return r.wrapper paired with its own
	// BackendID (not r.defaultBackend) so callers never see a non-empty
	// ID paired with a nil wrapper — that combination produced confusing
	// error messages like `spawn process (backend "claude"): no wrapper`.
	if r.wrapper != nil {
		return r.wrapper, r.wrapper.BackendID
	}
	return nil, ""
}

// managerFor returns the shim.Manager associated with the given backend ID.
// Empty backend picks the router default via wrapperFor's fallback rules.
// Used by reconnectShims's ENOENT-cleanup path (F6) to purge zombies
// without having to thread a manager reference through every call site.
// Returns nil when no wrapper/manager is configured, so callers must guard.
func (r *Router) managerFor(backend string) *shim.Manager {
	w, _ := r.wrapperFor(backend)
	if w == nil {
		return nil
	}
	return w.ShimManager
}

// BackendIDs returns the list of backend IDs the router can spawn against,
// with the default backend first. Suitable for UI enumeration.
func (r *Router) BackendIDs() []string {
	if len(r.wrappers) == 0 {
		if r.wrapper != nil {
			id := r.wrapper.BackendID
			if id == "" {
				id = "claude"
			}
			return []string{id}
		}
		return nil
	}
	out := make([]string, 0, len(r.wrappers))
	if r.defaultBackend != "" {
		if _, ok := r.wrappers[r.defaultBackend]; ok {
			out = append(out, r.defaultBackend)
		}
	}
	for id := range r.wrappers {
		if id == r.defaultBackend {
			continue
		}
		out = append(out, id)
	}
	return out
}

// DefaultBackend returns the backend ID used when no explicit backend is
// requested. May be empty for test-only routers without a wrapper.
func (r *Router) DefaultBackend() string {
	if r.defaultBackend != "" {
		return r.defaultBackend
	}
	if r.wrapper != nil {
		return r.wrapper.BackendID
	}
	return ""
}

// BackendWrapper returns the wrapper registered for the given backend ID,
// or nil if the router has no matching backend. Intended for callers that
// need read-only metadata (CLIName, CLIVersion, CLIPath) per backend.
func (r *Router) BackendWrapper(id string) *cli.Wrapper {
	if len(r.wrappers) == 0 {
		if id == "" || r.wrapper == nil || r.wrapper.BackendID == id || (id == "claude" && r.wrapper.BackendID == "") {
			return r.wrapper
		}
		return nil
	}
	if id == "" {
		id = r.defaultBackend
	}
	return r.wrappers[id]
}

// indexAdd adds key to the chat→sessions index. No-op when index is nil.
// Must be called under r.mu.
func (r *Router) indexAdd(key string) {
	if r.sessionsByChat == nil {
		return
	}
	ck := chatKeyFor(key)
	for _, k := range r.sessionsByChat[ck] {
		if k == key {
			return
		}
	}
	r.sessionsByChat[ck] = append(r.sessionsByChat[ck], key)
}

// indexDel removes key from the chat→sessions index. No-op when index is nil.
// Must be called under r.mu.
func (r *Router) indexDel(key string) {
	if r.sessionsByChat == nil {
		return
	}
	ck := chatKeyFor(key)
	keys := r.sessionsByChat[ck]
	for i, k := range keys {
		if k == key {
			last := len(keys) - 1
			keys[i] = keys[last]
			r.sessionsByChat[ck] = keys[:last]
			if len(r.sessionsByChat[ck]) == 0 {
				delete(r.sessionsByChat, ck)
			}
			return
		}
	}
}

// RouterConfig holds configuration for the session router.
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
		for id, w := range wrappers {
			defaultWrapper = w
			if defaultBackend == "" {
				defaultBackend = id
			}
			break
		}
	}

	r := &Router{
		sessions:           make(map[string]*ManagedSession),
		sessionsByChat:     make(map[string][]string),
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
		workspaceOverrides: make(map[string]string),
		backendOverrides:   make(map[string]string),
		storePath:          cfg.StorePath,
		knownIDs:           make(map[string]bool),
		sessionIDToKey:     make(map[string]string),
		spawningKeys:       make(map[string]struct{}),
		noOutputTimeout:    cfg.NoOutputTimeout,
		totalTimeout:       cfg.TotalTimeout,
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
			// Resolve the wrapper that owned this session's backend so the
			// snapshot carries the correct CLI identity even after a pure
			// restore (no shim reconnect). Pre-multi-backend entries have
			// empty Backend and fall back to the router default.
			restoreWrapper, restoreBackendID := r.wrapperFor(entry.Backend)
			cliName, cliVersion := r.cliNameDefault(), r.cliVersionDefault()
			if restoreWrapper != nil {
				cliName = restoreWrapper.CLIName
				cliVersion = restoreWrapper.CLIVersion
			}
			s := &ManagedSession{
				key:            key,
				totalCost:      entry.TotalCost,
				prevSessionIDs: entry.PrevSessionIDs,
				exempt:         isExemptKey(key),
			}
			s.setWorkspace(entry.Workspace)
			s.SetBackend(restoreBackendID)
			s.SetCLIName(cliName)
			s.SetCLIVersion(cliVersion)
			if entry.UserLabel != "" {
				s.SetUserLabel(entry.UserLabel)
			}
			s.setSessionID(entry.SessionID)
			if entry.LastActive != 0 {
				s.lastActive.Store(entry.LastActive)
			}
			r.attachHistorySource(s)
			r.sessions[key] = s
			r.indexAdd(key)
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

	// Async-load JSONL history for all suspended sessions so the dashboard
	// shows conversation history without waiting for the next message.
	// Loads the full session chain (prev → current) to restore history
	// that accumulated across multiple CLI session IDs.
	//
	// Two load paths:
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
		sem := make(chan struct{}, historyLoadConcurrency) // limit concurrent disk I/O
		for _, s := range r.sessions {
			s := s
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
					case <-r.historyCtx.Done():
						graceTimer.Stop()
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
					slog.Info("shim-managed session missing history after reconnect grace, falling back to JSONL load",
						"key", s.key)
				}
				select {
				case sem <- struct{}{}:
				case <-r.historyCtx.Done():
					return
				}
				defer func() { <-sem }()

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

	return r
}

// attachHistorySource picks the right history.Source for a session based on
// its backend ID and installs it. Called immediately after every
// ManagedSession allocation in this file so EventEntriesBeforeCtx's disk
// fallback is live before the first pagination request can arrive.
//
// Selection:
//   - "claude" (and legacy empty-string backend rows restored from
//     sessions.json) → claudejsonl.Source, reading ~/.claude/projects
//   - anything else → history.Noop placeholder so the call site doesn't
//     have to nil-check. Per-backend implementations can slot in here
//     as they land without touching ManagedSession.
//
// Missing claudeDir (empty-string config or test harness) degrades to Noop
// regardless of backend — we can't read JSONL without a root directory.
func (r *Router) attachHistorySource(s *ManagedSession) {
	if s == nil {
		return
	}
	backend := s.Backend()
	if backend == "" {
		backend = r.defaultBackend
	}
	if backend == "claude" && r.claudeDir != "" {
		s.SetHistorySource(claudejsonl.New(r.claudeDir, s.Workspace(), s.snapshotChainIDs))
		return
	}
	s.SetHistorySource(history.Noop{})
}

// shimManagedKeys returns the set of session keys that have a surviving shim
// process. Called by NewRouter to skip async JSONL loading for sessions that
// will be fully restored by ReconnectShims (replay + JSONL user entries).
func (r *Router) shimManagedKeys() map[string]bool {
	managers := r.shimManagers()
	if len(managers) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	for _, mgr := range managers {
		states, err := mgr.Discover()
		if err != nil {
			continue
		}
		for _, s := range states {
			seen[s.Key] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	return seen
}

// shimManagers returns the distinct ShimManager instances across wrappers.
// Shared managers are deduplicated so a combined deployment (all backends
// reusing the same state dir) only scans once.
func (r *Router) shimManagers() []*shim.Manager {
	var out []*shim.Manager
	seen := make(map[*shim.Manager]bool)
	add := func(w *cli.Wrapper) {
		if w == nil || w.ShimManager == nil || seen[w.ShimManager] {
			return
		}
		seen[w.ShimManager] = true
		out = append(out, w.ShimManager)
	}
	for _, w := range r.wrappers {
		add(w)
	}
	add(r.wrapper)
	return out
}

// ReconnectShims discovers surviving shim processes and reconnects sessions.
// Called after NewRouter to restore sessions that were active before naozhi restart.
// Accepts a context so reconcile-loop callers can propagate shutdown cancellation
// into per-shim timeout contexts; pass context.Background() from startup paths
// where no shutdown signal is available yet.
func (r *Router) ReconnectShims() {
	r.reconnectShims(context.Background())
}

// ReconnectShimsCtx is the context-aware variant used by the reconcile loop so
// SIGTERM during a 15 s handshake aborts promptly instead of waiting per session.
func (r *Router) ReconnectShimsCtx(ctx context.Context) {
	r.reconnectShims(ctx)
}

func (r *Router) reconnectShims(parentCtx context.Context) {
	managers := r.shimManagers()
	if len(managers) == 0 {
		return
	}

	// Aggregate states across all managers and dedupe on key, as each shim
	// is uniquely identified by the session key regardless of backend.
	seenKey := make(map[string]bool)
	var states []shim.State
	for _, mgr := range managers {
		ss, err := mgr.Discover()
		if err != nil {
			slog.Warn("shim discovery failed", "err", err)
			continue
		}
		for _, s := range ss {
			if seenKey[s.Key] {
				continue
			}
			seenKey[s.Key] = true
			states = append(states, s)
		}
	}
	slog.Info("shim discovery complete", "found", len(states))

	reconnected := 0
	for _, state := range states {
		r.mu.Lock()
		sess, ok := r.sessions[state.Key]
		var hasLiveProcess bool
		if ok && sess.isAlive() {
			hasLiveProcess = true
		}
		_, spawning := r.spawningKeys[state.Key]
		r.mu.Unlock()

		// A spawnSession is in flight for this key: the new shim may have
		// already written its state file while ManagedSession is not yet
		// installed in r.sessions. Skip this round — the next tick will
		// find either a live session or a real orphan.
		if spawning {
			continue
		}

		// Resolve the wrapper recorded at shim startup so reconnect uses
		// the matching Protocol and binary. An empty Backend in the state
		// file predates multi-backend support and falls back to the
		// router default.
		recWrapper, recBackendID := r.wrapperFor(state.Backend)

		if !ok {
			slog.Info("orphan shim found, shutting down", "key", state.Key)
			// Connect briefly to send shutdown. Bound the reconnect so a
			// hung shim socket cannot stall NewRouter startup — we fall
			// through to SIGUSR2 if the timeout fires. IIFE + defer so
			// rcancel always runs even if Reconnect or Shutdown panics
			// (R32-REL1).
			func() {
				rctx, rcancel := context.WithTimeout(parentCtx, shimReconnectTimeout)
				defer rcancel()
				var (
					handle  *shim.ShimHandle
					connErr error
				)
				if recWrapper != nil && recWrapper.ShimManager != nil {
					handle, connErr = recWrapper.ShimManager.Reconnect(rctx, state.Key, 0)
				} else {
					connErr = fmt.Errorf("no shim manager for backend %q", state.Backend)
				}
				if connErr == nil {
					handle.Shutdown()
				} else {
					syscall.Kill(state.ShimPID, syscall.SIGUSR2) //nolint:errcheck
				}
			}()
			continue
		}

		// Skip if session already has a live process
		if hasLiveProcess {
			continue
		}

		if recWrapper == nil {
			slog.Warn("shim reconnect skipped: no wrapper for backend",
				"key", state.Key, "backend", state.Backend)
			continue
		}

		// CLI args drift check: if config changed (model, args), shut down old shim
		// and let the next message create a new session with updated config.
		// Strip --resume <id> from stored args since it's session-specific, not config.
		storedBase := stripResumeArgs(state.CLIArgs)
		driftModel := r.model
		if m, ok := r.backendModels[recBackendID]; ok && m != "" {
			driftModel = m
		}
		driftArgs := r.extraArgs
		if a, ok := r.backendExtraArgs[recBackendID]; ok && len(a) > 0 {
			driftArgs = a
		}
		currentArgs := recWrapper.Protocol.BuildArgs(cli.SpawnOptions{
			Model:     driftModel,
			ExtraArgs: driftArgs,
		})
		if len(storedBase) > 0 && !slices.Equal(storedBase, currentArgs) {
			slog.Info("shim config drifted, shutting down old shim",
				"key", state.Key,
				"old_args_len", len(storedBase),
				"new_args_len", len(currentArgs))
			// IIFE + defer ensures rcancel runs even on panic (R32-REL1).
			func() {
				rctx, rcancel := context.WithTimeout(parentCtx, shimReconnectTimeout)
				defer rcancel()
				handle, err := recWrapper.ShimManager.Reconnect(rctx, state.Key, 0)
				if err == nil {
					handle.Shutdown()
				}
			}()
			// After killing the old shim the session becomes suspended until the
			// next user message spawns a fresh process. NewRouter's async JSONL
			// load loop skips this key because shimManagedKeys() already claimed
			// it, so without an explicit backfill here the dashboard panel stays
			// blank until the user sends something. Load JSONL directly into
			// persistedHistory (InjectHistory is proc-nil safe) so the sidebar
			// shows the last conversation while the session waits for revival.
			if r.claudeDir != "" && state.SessionID != "" {
				ids := make([]string, 0, len(sess.prevSessionIDs)+1)
				ids = append(ids, sess.prevSessionIDs...)
				ids = append(ids, state.SessionID)
				histCtx, histCancel := context.WithTimeout(parentCtx, shimReconnectTimeout)
				histEntries := discovery.LoadHistoryChainTailCtx(
					histCtx, r.claudeDir, ids, sess.Workspace(), maxPersistedHistory,
				)
				histCancel()
				if len(histEntries) > 0 {
					sess.InjectHistory(histEntries)
					sess.extractLastPromptFromProcess()
					slog.Info("drifted shim: backfilled JSONL history",
						"key", state.Key, "entries", len(histEntries))
				}
			}
			continue
		}

		// Reconnect. Timeout-bounded so a stuck shim handshake cannot stall
		// NewRouter indefinitely; on timeout we log and keep iterating.
		lastSeq := int64(0) // full replay on restart
		spawnCtx, spawnCancel := context.WithTimeout(parentCtx, shimReconnectTimeout)
		proc, replays, err := recWrapper.SpawnReconnect(
			spawnCtx, state.Key, lastSeq, recWrapper.Protocol,
			r.noOutputTimeout, r.totalTimeout,
		)
		spawnCancel()
		if err != nil {
			// ENOENT on the socket path = zombie shim (live PID, missing
			// filesystem entry). Discover's F4 check will prune it on the
			// next 30s tick, but that means 30s of WARN spam AND every
			// dashboard retry in between also fails. Eagerly clean up so
			// the next user message spawns a fresh shim instead of hitting
			// the same dead path. errors.Is(err, syscall.ENOENT) is the
			// canonical check, but the error wraps through fmt.Errorf("%w")
			// layers in SpawnReconnect → Reconnect → net.Dial, so we also
			// match the textual "no such file or directory" suffix as a
			// belt-and-braces fallback.
			if errors.Is(err, syscall.ENOENT) ||
				strings.Contains(err.Error(), "no such file or directory") {
				slog.Warn("shim reconnect: socket missing, cleaning up zombie",
					"key", state.Key, "pid", state.ShimPID, "err", err)
				if mgr := r.managerFor(recBackendID); mgr != nil {
					mgr.ForceCleanupZombie(state)
				}
				continue
			}
			slog.Warn("shim reconnect failed", "key", state.Key, "err", err)
			continue
		}

		// Install the turn-done callback before any history/JSONL work
		// completes so result events arriving during the JSONL-load window
		// (the readLoop is already running inside SpawnReconnect) do not
		// fire the nil-callback path and leave the dashboard stuck on a
		// "running" spinner until the next unrelated broadcast.
		proc.SetOnTurnDone(func() { r.notifyChange() })

		// Restore dashboard history from JSONL only.
		//
		// Replay events are intentionally NOT injected into persistedHistory:
		// they originate from the shim stdout ring buffer, which has no native
		// per-event timestamp, so EventEntryFromEvent stamps them all with
		// time.Now() at reconnect moment — this breaks chronological ordering
		// against user entries loaded from JSONL (which carry real ts).
		//
		// Replay is still useful for runtime state (isMidTurn detection inside
		// SpawnReconnect, and any live bytes readLoop picks up post-reconnect).
		// For long-term history, JSONL is authoritative — it records both
		// user input (stdin) and assistant output with accurate timestamps.
		//
		// Tradeoff: if naozhi restarts within seconds of the last turn, the
		// current session's JSONL may not yet be flushed to disk; assistant
		// entries for that turn are transiently absent from the dashboard
		// until the next live event repopulates them. Self-healing.
		if r.claudeDir != "" {
			ids := make([]string, 0, len(sess.prevSessionIDs)+1)
			ids = append(ids, sess.prevSessionIDs...)
			if state.SessionID != "" {
				ids = append(ids, state.SessionID)
			}
			// Use parentCtx (reconcile loop / startup ctx) rather than
			// r.historyCtx: historyCtx is cancelled as Shutdown's FIRST
			// action, so a reconcile tick that fires during the 30s drain
			// window would see ctx.Canceled and load zero entries, leaving
			// the reconnected session's dashboard panel empty.
			// Bounded budget (maxPersistedHistory) and the inner
			// shimReconnectTimeout still protect against hung storage.
			histCtx, histCancel := context.WithTimeout(parentCtx, shimReconnectTimeout)
			histEntries := discovery.LoadHistoryChainTailCtx(
				histCtx, r.claudeDir, ids, sess.Workspace(), maxPersistedHistory,
			)
			histCancel()
			if len(histEntries) > 0 {
				proc.InjectHistory(histEntries)
			}
		}

		// TOCTOU guard: re-check under lock that the session hasn't been replaced
		// by a concurrent spawnSession while we were replaying history (lock was
		// released). Then atomically attach the process under the same lock hold
		// to eliminate the race window where a concurrent GetOrCreate could see
		// isAlive()==false between check and ReattachProcess.
		r.mu.Lock()
		currentSess := r.sessions[state.Key]
		if currentSess != sess || (currentSess != nil && currentSess.isAlive()) {
			r.mu.Unlock()
			proc.Close()
			slog.Info("shim reconnect aborted: session replaced concurrently", "key", state.Key)
			continue
		}
		// ReattachProcess calls onSessionID which tries to r.mu.Lock(),
		// but we already hold the lock here. Do the tracking directly
		// to avoid deadlock (sync.RWMutex is not reentrant).
		// (onTurnDone was already bound before the JSONL-load window
		// to avoid missing early result events.)
		sess.ReattachProcessNoCallback(proc, state.SessionID)
		// Record the backend + wrapper-provided CLI identity so the
		// dashboard snapshot reflects the actual backend post-reconnect,
		// even for sessions restored from a pre-multi-backend store.
		// Writes go through atomic.Pointer[string] so the lock-free Snapshot()
		// in ListSessions remains race-free.
		if recBackendID != "" {
			sess.SetBackend(recBackendID)
		}
		if recWrapper.CLIName != "" {
			sess.SetCLIName(recWrapper.CLIName)
		}
		if recWrapper.CLIVersion != "" {
			sess.SetCLIVersion(recWrapper.CLIVersion)
		}
		if state.SessionID != "" {
			r.trackSessionID(state.SessionID)
			r.sessionIDToKey[state.SessionID] = state.Key
		}
		if !sess.exempt {
			r.activeCount.Add(1)
		}
		// Mark store dirty so the next Cleanup/saveIfDirty cycle persists
		// the reconnected session's backend/CLI identity and active flag.
		// Without this, a naozhi crash within the (up to) 60-second gap
		// before the next save would lose the shim-reconnect state even
		// though the shim itself kept the CLI process alive. Every other
		// storeGen.Add site pairs with storeDirty = true for this reason.
		r.storeDirty = true
		r.storeGen.Add(1)
		r.mu.Unlock()

		// Extract lastPrompt/lastActivity from replay + JSONL entries so the
		// sidebar shows a meaningful label instead of "(no prompt)".
		sess.extractLastPromptFromProcess()

		reconnected++
		slog.Info("session reconnected via shim",
			"key", state.Key,
			"session_id", state.SessionID,
			"replayed", len(replays))
	}

	if reconnected > 0 {
		r.notifyChange()
		slog.Info("shim reconnect complete", "count", reconnected)
	}
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

// maxWorkspaceOverrides caps the workspaceOverrides map size. Same rationale
// as maxBackendOverrides (R55-SEC-001): authenticated callers can POST unique
// chat keys to /api/sessions/send and each valid call grows the map by one
// entry with no natural pruning. 1024 comfortably exceeds realistic operator
// usage (one override per chat, typical deployment < 50 chats).
const maxWorkspaceOverrides = 1024

// SetWorkspace sets the working directory override for a chat. Bounded by
// maxWorkspaceOverrides to prevent DoS via unique-chat-key flooding (R58-SEC-H1).
func (r *Router) SetWorkspace(chatKey, path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Allow existing keys to be updated without bumping against the cap;
	// only reject brand-new keys once the limit is hit. Mirrors the
	// SetSessionBackend gating pattern.
	if _, existing := r.workspaceOverrides[chatKey]; !existing && len(r.workspaceOverrides) >= maxWorkspaceOverrides {
		slog.Warn("workspaceOverrides at capacity; dropping override",
			"chat_key", chatKey, "cap", maxWorkspaceOverrides)
		return
	}
	r.workspaceOverrides[chatKey] = path
	r.wsOverridesDirty = true
	r.wsOverridesGen.Add(1)
}

// GetWorkspace returns the effective workspace for a chat key.
func (r *Router) GetWorkspace(chatKey string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if ws, ok := r.workspaceOverrides[chatKey]; ok {
		return ws
	}
	return r.workspace
}

// maxBackendOverrides caps the per-key backend override map so an
// authenticated dashboard user cannot exhaust memory by POSTing unique keys.
// backendOverrides entries are cleared on first spawnSession / Reset /
// Remove / ResetChat for the key; abandoned picks (key chosen then never
// spawned) would otherwise accumulate indefinitely — the 30/min send-limiter
// bounds burst rate but not cumulative growth. Pick a limit that comfortably
// exceeds realistic outstanding picks (a single operator seldom has >100
// unresolved picks) while making the DoS surface trivially bounded.
const maxBackendOverrides = 1024

// SetSessionBackend remembers the backend the dashboard picked for a new
// session keyed by its full session key (including agent suffix). Only
// applied the next time spawnSession runs — existing live sessions are not
// migrated. Empty backend clears the override.
func (r *Router) SetSessionBackend(key, backend string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if backend == "" {
		delete(r.backendOverrides, key)
		return
	}
	// Allow existing keys to be updated without bumping against the cap
	// (operator changing their mind mid-flow) — only reject when inserting
	// a brand-new key after the limit is hit.
	if _, existing := r.backendOverrides[key]; !existing && len(r.backendOverrides) >= maxBackendOverrides {
		slog.Warn("backendOverrides at capacity; dropping override",
			"key", key, "cap", maxBackendOverrides)
		return
	}
	r.backendOverrides[key] = backend
}

// GetSessionBackend returns the backend override for key, or "" if none.
func (r *Router) GetSessionBackend(key string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.backendOverrides[key]
}

// ResetChat resets all sessions belonging to a chat (all agents).
func (r *Router) ResetChat(chatKeyPrefix string) {
	r.mu.Lock()
	var toClose []processIface
	var closedActive int
	if r.sessionsByChat != nil {
		// O(k) path via index (k = agents per chat, typically 1-3).
		for _, key := range r.sessionsByChat[chatKeyPrefix] {
			s := r.sessions[key]
			if s == nil {
				continue
			}
			if p := s.loadProcess(); p != nil && p.Alive() {
				toClose = append(toClose, p)
				if !s.exempt {
					closedActive++
				}
			}
			if id := s.getSessionID(); id != "" {
				delete(r.sessionIDToKey, id)
			}
			delete(r.sessions, key)
			// Drop any per-session backend pick queued via SetSessionBackend.
			// Without this, an abandoned dashboard "choose backend" pick for a
			// key that is then reset leaks an entry into backendOverrides that
			// is only cleared by a later spawnSession for the same key, which
			// may never happen.
			delete(r.backendOverrides, key)
		}
		delete(r.sessionsByChat, chatKeyPrefix)
	} else {
		// Fallback O(n) scan for test-created routers without index.
		var toDelete []string
		for key, s := range r.sessions {
			if len(key) > len(chatKeyPrefix) && key[:len(chatKeyPrefix)+1] == chatKeyPrefix+":" {
				toDelete = append(toDelete, key)
				if p := s.loadProcess(); p != nil && p.Alive() {
					toClose = append(toClose, p)
					if !s.exempt {
						closedActive++
					}
				}
			}
		}
		for _, key := range toDelete {
			if s := r.sessions[key]; s != nil {
				if id := s.getSessionID(); id != "" {
					delete(r.sessionIDToKey, id)
				}
			}
			delete(r.sessions, key)
			delete(r.backendOverrides, key)
		}
	}
	if closedActive > 0 {
		newCount := r.activeCount.Add(-int64(closedActive))
		if newCount < 0 {
			r.activeCount.Store(0)
		}
	}
	if _, existed := r.workspaceOverrides[chatKeyPrefix]; existed {
		delete(r.workspaceOverrides, chatKeyPrefix)
		// Without wsOverridesDirty, the delete is only written back when some
		// other code path bumps the flag; a crash before that would reload
		// the override on restart and silently undo the user's reset.
		r.wsOverridesDirty = true
		r.wsOverridesGen.Add(1)
	}
	r.storeDirty = true
	r.storeGen.Add(1)
	r.mu.Unlock()

	for _, proc := range toClose {
		proc.Close()
	}
	if r.shutdownCond != nil {
		r.shutdownCond.Broadcast()
	}

	r.notifyChange()
}

// AgentOpts provides per-agent overrides for session creation.
type AgentOpts struct {
	Model     string
	ExtraArgs []string
	Workspace string // override workspace (empty = use default/chat override)
	Backend   string // backend ID ("claude" / "kiro" / …); empty = router default
	Exempt    bool   // exempt from TTL, eviction, and activeCount (planner sessions)
}

// SessionStatus indicates how a session was obtained.
type SessionStatus int

const (
	SessionExisting SessionStatus = iota // reused a live session
	SessionResumed                       // resumed a suspended session
	SessionNew                           // created a brand new session
)

// GetOrCreate returns an existing session or creates a new one.
// AgentOpts overrides the router defaults for model and args.
func (r *Router) GetOrCreate(ctx context.Context, key string, opts AgentOpts) (*ManagedSession, SessionStatus, error) {
	// R188-SEC-M2: flag-injection guard on the per-request Model override.
	// Router-global r.model is operator-configured in config.yaml and trusted;
	// opts.Model originates from dashboard WS messages, upstream RPC, or
	// planner project config and must be validated at the router boundary.
	if err := validateModel(opts.Model); err != nil {
		return nil, 0, err
	}
	r.mu.Lock()

	if s, ok := r.sessions[key]; ok {
		if s.isAlive() {
			s.touchLastActive()
			r.mu.Unlock()
			return s, SessionExisting, nil
		}
		slog.Info("session process exited, resuming", "key", key, "session_id", s.getSessionID())
		s, err := r.spawnSession(ctx, key, s.getSessionID(), opts)
		if err != nil {
			return nil, 0, fmt.Errorf("session %s: %w", key, err)
		}
		return s, SessionResumed, nil
	}

	// Debug (not Info): spawnSession will emit "session spawned" at Info
	// with the same key + active count moments later; a preceding Info at
	// the same key would double the per-spawn row in the systemd journal
	// with no additional signal. Keep Debug for the "brand-new vs resume"
	// distinction when operators opt into verbose logging.
	slog.Debug("creating new session", "key", key)
	s, err := r.spawnSession(ctx, key, "", opts)
	if err != nil {
		return nil, 0, fmt.Errorf("session %s: %w", key, err)
	}
	return s, SessionNew, nil
}

// spawnSession creates a new process, optionally resuming an existing session.
// Caller must hold r.mu. Releases r.mu during Spawn() to avoid blocking other
// goroutines during potentially slow protocol init (e.g., ACP handshake).
func (r *Router) spawnSession(ctx context.Context, key string, resumeID string, opts AgentOpts) (*ManagedSession, error) {
	// Mark this key as spawning so ReconnectShims does not mistake the freshly
	// started shim's state file for an orphan. Every return path below leaves
	// r.mu unlocked, so the defer reacquires it to delete the marker. Lazy
	// init tolerates test-only Routers constructed with &Router{...}.
	if r.spawningKeys == nil {
		r.spawningKeys = make(map[string]struct{})
	}
	r.spawningKeys[key] = struct{}{}
	defer func() {
		r.mu.Lock()
		delete(r.spawningKeys, key)
		r.mu.Unlock()
	}()

	// Exempt sessions (planners) bypass maxProcs capacity check but have their own limit
	if !opts.Exempt {
		// Fast path: the incremental activeCount is accurate under normal operation
		// (Reset/Remove/evictOldest/Cleanup maintain it). Avoid the O(n) countActive
		// scan on every spawn. Only recount when we appear to be at capacity, to
		// detect drift from undetected process exits (OOM, SIGKILL) before refusing.
		// All three checks run under r.mu (write lock); storing the Load into a
		// local keeps the comparison in int64 (so no 32-bit wrap on exotic cross
		// builds) and avoids re-issuing the atomic read between the rechecks.
		// R62-PERF-7 / R62-SEC-4.
		maxProcs64 := int64(r.maxProcs)
		pending64 := int64(r.pendingSpawns)
		if r.activeCount.Load()+pending64 >= maxProcs64 {
			r.countActive()
		}
		if r.activeCount.Load()+pending64 >= maxProcs64 {
			if !r.evictOldest() {
				r.mu.Unlock()
				return nil, fmt.Errorf("%w (%d), all busy", ErrMaxProcs, r.maxProcs)
			}
			if r.activeCount.Load()+pending64 >= maxProcs64 {
				r.mu.Unlock()
				return nil, fmt.Errorf("%w (%d), all busy", ErrMaxProcs, r.maxProcs)
			}
		}
	} else {
		// Guard against unbounded exempt session growth (e.g., many projects)
		exemptCount := r.countExempt()
		if exemptCount >= maxExemptSessions {
			r.mu.Unlock()
			return nil, fmt.Errorf("%w (%d)", ErrMaxExemptSessions, maxExemptSessions)
		}
	}

	// Pick the wrapper for the requested backend. AgentOpts.Backend wins
	// over the stored per-key override (set by the dashboard when the user
	// picked a backend during new-session). Unknown IDs fall back silently
	// to the router default; callers that need strict validation should
	// pre-check via BackendIDs().
	//
	// The override is consumed on first spawn so a later Reset→spawn for
	// the same key doesn't silently carry the old backend pick. The map
	// is also wiped in Reset/Remove; deleting here as well guards the
	// spawn path even when those callers missed cleanup.
	reqBackend := opts.Backend
	// Skip map ops entirely when no overrides are registered — the common
	// single-backend case. The `delete` is a no-op but still hashes the
	// key, so gating reduces the per-spawn hot path to a single len() call.
	if len(r.backendOverrides) > 0 {
		if reqBackend == "" {
			reqBackend = r.backendOverrides[key]
		}
		delete(r.backendOverrides, key)
	}
	wrapper, backendID := r.wrapperFor(reqBackend)

	// Merge agent opts with router defaults. Backend-scoped values
	// (backendModels / backendExtraArgs) take precedence over the global
	// fallbacks so operators can set a Kiro-specific model without
	// affecting Claude sessions.
	model := r.model
	if bm, ok := r.backendModels[backendID]; ok && bm != "" {
		model = bm
	}
	if opts.Model != "" {
		model = opts.Model
	}
	baseArgs := r.extraArgs
	if ba, ok := r.backendExtraArgs[backendID]; ok && len(ba) > 0 {
		baseArgs = ba
	}
	args := make([]string, len(baseArgs))
	copy(args, baseArgs)
	args = append(args, opts.ExtraArgs...)

	// Determine workspace: opts override > per-chat override > old session workspace > default
	workspace := r.workspace
	workspaceOverridden := false
	if opts.Workspace != "" {
		workspace = opts.Workspace
		workspaceOverridden = true
	} else if chatKey := chatKeyFor(key); chatKey != key {
		if ws, ok := r.workspaceOverrides[chatKey]; ok {
			workspace = ws
			workspaceOverridden = true
		}
	}
	// When resuming after restart and no workspace override exists, fall back to
	// the old session's stored workspace so --resume finds the session in the
	// correct project directory (Claude stores sessions under ~/.claude/projects/<sha256(cwd)>/).
	if !workspaceOverridden && resumeID != "" {
		if old := r.sessions[key]; old != nil {
			if ws := old.Workspace(); ws != "" {
				workspace = ws
			}
		}
	}

	// Guard against "resume target missing": drop resumeID when the jsonl
	// Claude CLI would try to read does not exist, so the spawn falls
	// through to a fresh session instead of exit-1'ing on "No conversation
	// found". See resolveResumeID for rationale and edge-cases.
	resumeID = resolveResumeID(r.claudeDir, workspace, key, resumeID)

	spawnOpts := cli.SpawnOptions{
		Key:             key,
		Model:           model,
		ResumeID:        resumeID,
		ExtraArgs:       args,
		WorkingDir:      workspace,
		NoOutputTimeout: r.noOutputTimeout,
		TotalTimeout:    r.totalTimeout,
	}

	// ── Lock release 1: Spawn may block (ACP Init handshake, process startup).
	// We release r.mu to avoid holding it during I/O. pendingSpawns prevents
	// a concurrent Cleanup from pruning slots we're about to fill.
	r.pendingSpawns++
	r.mu.Unlock()
	if wrapper == nil {
		r.mu.Lock()
		r.pendingSpawns--
		r.mu.Unlock()
		return nil, fmt.Errorf("spawn process (backend %q): %w", backendID, ErrNoCLIWrapper)
	}
	// Panic-safe Spawn: if wrapper.Spawn panics (shim exec failure, protocol
	// Init crash, etc.) pendingSpawns must still be decremented or this
	// router permanently refuses new sessions with ErrMaxProcs until the
	// process restarts. Extracted to panicSafeSpawn so tests can exercise
	// the recover path directly (wrapper itself has no panic injection
	// seam). RES1.
	proc, err := panicSafeSpawn(ctx, wrapper, spawnOpts, key, backendID)
	r.mu.Lock()
	r.pendingSpawns--
	if err != nil {
		r.mu.Unlock()
		return nil, fmt.Errorf("spawn process: %w", err)
	}

	// ── TOCTOU guard 1: Defends against concurrent spawnSession for the same key.
	// While we were unlocked for Spawn(), another goroutine may have completed
	// spawnSession and installed a live session. If so, discard our process.
	if existing, ok := r.sessions[key]; ok && existing.isAlive() {
		r.mu.Unlock()
		proc.Close() // discard the redundant process
		return existing, nil
	}

	// ── Lock release 2: Copy old session history under historyMu only (not r.mu).
	// Holding both r.mu and historyMu would violate lock ordering (historyMu is
	// acquired independently by event injection). The old reference is safe to
	// read because sessions are never mutated after creation, only replaced.
	old := r.sessions[key]
	// Snapshot fields that are written under r.mu elsewhere (e.g.
	// RegisterCronStub writes old.workspace under r.mu) before releasing
	// the lock; reading them after the release races those writers.
	// Round 49 concurrency finding.
	var oldPrevIDs []string
	var oldTotalCost float64
	if old != nil {
		if len(old.prevSessionIDs) > 0 {
			oldPrevIDs = make([]string, len(old.prevSessionIDs))
			copy(oldPrevIDs, old.prevSessionIDs)
		}
		// Preserve the cumulative cost across process replacement so the
		// dashboard doesn't flash $0.00 between spawn and the first result
		// event. Prefer the live process's value (freshest) over the
		// store-restored s.totalCost; fall back to the latter when no
		// process is attached (restored-from-disk sessions).
		if p := old.loadProcess(); p != nil {
			oldTotalCost = p.TotalCost()
		}
		if oldTotalCost == 0 {
			oldTotalCost = old.totalCost
		}
	}
	r.mu.Unlock()

	var oldHistory []cli.EventEntry
	var prevIDs []string
	if old != nil {
		old.historyMu.RLock()
		if p := old.loadProcess(); p != nil && !p.Alive() {
			// Dead process: EventEntries() includes both injected history and live events
			// logged during the last run. Use this instead of persistedHistory, which only
			// holds the JSONL-loaded snapshot and misses events accumulated since that load.
			oldHistory = p.EventEntries()
		} else if len(old.persistedHistory) > 0 {
			oldHistory = make([]cli.EventEntry, len(old.persistedHistory))
			copy(oldHistory, old.persistedHistory)
		}
		old.historyMu.RUnlock()

		// Build session chain: inherit old chain and append old session ID,
		// but only when the old ID differs from resumeID (i.e. a truly new
		// CLI session is replacing the old one, not just resuming the same one).
		if oldID := old.getSessionID(); oldID != "" && oldID != resumeID {
			prevIDs = make([]string, len(oldPrevIDs), len(oldPrevIDs)+1)
			copy(prevIDs, oldPrevIDs)
			prevIDs = append(prevIDs, oldID)
		} else {
			prevIDs = oldPrevIDs
		}
		// Cap the chain to bound sessions.json size and JSONL load time on
		// long-lived chats; oldest entries are the cheapest to drop because
		// the retained tail carries the most recent conversational context.
		if len(prevIDs) > maxPrevSessionIDs {
			prevIDs = prevIDs[len(prevIDs)-maxPrevSessionIDs:]
		}
	}

	r.mu.Lock()
	// ── TOCTOU guard 2: Defends against concurrent spawnSession during history copy.
	// While we held historyMu (not r.mu), another goroutine may have completed
	// spawnSession for this key. Same check as guard 1, different unlock window.
	if existing, ok := r.sessions[key]; ok && existing.isAlive() {
		r.mu.Unlock()
		proc.Close()
		return existing, nil
	}

	s := &ManagedSession{
		key:              key,
		persistedHistory: oldHistory,
		prevSessionIDs:   prevIDs,
		totalCost:        oldTotalCost,
		exempt:           opts.Exempt,
		onSessionID: func(id string) {
			r.mu.Lock()
			r.trackSessionID(id)
			if id != "" {
				r.sessionIDToKey[id] = key
			}
			r.mu.Unlock()
		},
	}
	s.setWorkspace(workspace)
	s.SetBackend(backendID)
	s.SetCLIName(wrapper.CLIName)
	s.SetCLIVersion(wrapper.CLIVersion)
	s.storeProcess(proc)
	// Matches the reconnect path (ReconnectShims): notify the dashboard when
	// a turn completes out-of-band (e.g. result arrives via readLoop without
	// an active Send capturing it). SetOnTurnDone is mu-guarded inside Process,
	// so calling it after storeProcess is safe.
	proc.SetOnTurnDone(func() { r.notifyChange() })
	if len(oldHistory) > 0 {
		proc.InjectHistory(oldHistory)
	}
	s.setSessionID(resumeID)
	r.trackSessionID(resumeID)
	if resumeID != "" {
		r.sessionIDToKey[resumeID] = key
	}
	s.touchLastActive()
	r.attachHistorySource(s)
	r.sessions[key] = s
	r.indexAdd(key)
	if !opts.Exempt {
		r.activeCount.Add(1)
	}

	r.storeDirty = true
	r.storeGen.Add(1)
	slog.Info("session spawned", "key", key, "active", r.activeCount.Load(), "exempt", opts.Exempt)
	// OBS2: counter bumped inside the write-lock so it reflects the authoritative
	// "spawn succeeded" point (past both TOCTOU guards, past storeProcess). Exempt
	// sessions are excluded — they don't consume a normal session slot and
	// inflating session_create_total with planner/scratch churn muddies the signal.
	if !opts.Exempt {
		metrics.SessionCreateTotal.Add(1)
	}
	r.mu.Unlock()

	// Load conversation history from Claude's local JSONL when resuming.
	// This restores dashboard event display after service restarts.
	// Load the full chain (prev IDs + resume ID) to recover history
	// that accumulated across multiple CLI session IDs.
	if resumeID != "" && r.claudeDir != "" && len(oldHistory) == 0 {
		ids := make([]string, 0, len(prevIDs)+1)
		ids = append(ids, prevIDs...)
		ids = append(ids, resumeID)
		// Budgeted tail walk: collect the most recent maxPersistedHistory
		// entries and stop, which typically avoids opening most of a long
		// prev-id chain.
		//
		// Deliberately NOT using r.historyCtx: that context is cancelled
		// at Shutdown's first step, so a user message arriving during the
		// 30s drain window would resume with empty dashboard history. A
		// fresh 15s timeout gives slow storage room to breathe while
		// still bounding the request path.
		histCtx, histCancel := context.WithTimeout(context.Background(), 15*time.Second)
		allEntries := discovery.LoadHistoryChainTailCtx(
			histCtx, r.claudeDir, ids, workspace, maxPersistedHistory,
		)
		histCancel()
		if len(allEntries) > 0 {
			s.InjectHistory(allEntries)
			slog.Info("loaded session history on resume", "key", key, "entries", len(allEntries), "chain", len(ids))
		}
	}

	r.notifyChange()
	return s, nil
}

// countActive recounts alive processes (corrects drift from undetected exits).
// Exempt sessions are not counted toward max_procs capacity.
func (r *Router) countActive() {
	count := int64(0)
	for _, s := range r.sessions {
		if s.exempt {
			continue
		}
		if s.isAlive() {
			count++
		}
	}
	r.activeCount.Store(count)
}

// countExempt returns the number of alive exempt (planner) sessions.
// Caller must hold r.mu.
func (r *Router) countExempt() int {
	count := 0
	for _, s := range r.sessions {
		if s.exempt && s.isAlive() {
			count++
		}
	}
	return count
}

// evictOldest closes the oldest idle (non-Running) session to free a slot.
// Releases and re-acquires r.mu during Close() to avoid blocking other goroutines.
// Returns true if a session was evicted.
func (r *Router) evictOldest() bool {
	var oldest *ManagedSession
	for _, s := range r.sessions {
		if s.exempt {
			continue // planner sessions are never evicted
		}
		if !s.isAlive() || s.loadProcess().IsRunning() {
			continue
		}
		if oldest == nil || s.GetLastActive().Before(oldest.GetLastActive()) {
			oldest = s
		}
	}
	if oldest == nil {
		return false
	}
	slog.Info("evicting oldest session", "key", oldest.key, "idle", time.Since(oldest.GetLastActive()))
	// OBS2: bump before Unlock so it aligns with the "decision to evict" point;
	// the subsequent proc.Close() is async-capable and can fail, but the eviction
	// decision is already committed (deathReason set, storeDirty marked below).
	metrics.SessionEvictTotal.Add(1)
	storeStringAtomic(&oldest.deathReason, "evicted")
	// Keep oldest.process non-nil so concurrent holders don't get nil-panic.
	// After Close(), Alive() returns false; countActive() below recounts correctly.
	//
	// Eviction does not re-spawn for the same key (it just frees a slot for
	// the next unrelated GetOrCreate), so we deliberately skip
	// waitSocketGoneForKey here. If a future caller starts immediately
	// re-spawning the evicted key, add it — see the UCCLEP-2026-04-26
	// fix in Reset/ResetAndRecreate/Takeover for the pattern.
	proc := oldest.loadProcess()
	r.mu.Unlock()
	proc.Close()
	if r.shutdownCond != nil {
		r.shutdownCond.Broadcast()
	}
	r.mu.Lock()
	r.countActive() // recount instead of manual decrement to avoid double-count races
	// Mark store dirty + bump version so the eviction is persisted on the
	// next save cycle and propagated to the dashboard on the next Version()
	// poll. Without these two lines, a crash within the (up to) 60-second
	// save interval re-spawns the evicted session on restart, and dashboards
	// polling on version diff skip the refresh that would remove the dead
	// session from the sidebar. Every other mutation site pairs liveness
	// changes with this pattern. R59-GO-H2.
	r.storeDirty = true
	r.storeGen.Add(1)
	return true
}

// unregisterSessionLocked removes a session from all routing indexes. Caller
// must hold r.mu. If keepBackendOverride is true, backendOverrides[key] is
// preserved so a following spawnSession can consume it atomically (used by
// ResetAndRecreate / Takeover which reuse the same key). On terminal removal
// paths (Reset / Remove / Cleanup prune) pass false to prevent override leaks.
func (r *Router) unregisterSessionLocked(key string, s *ManagedSession, keepBackendOverride bool) {
	if s == nil {
		return
	}
	if id := s.getSessionID(); id != "" {
		delete(r.sessionIDToKey, id)
	}
	r.indexDel(key)
	delete(r.sessions, key)
	if !keepBackendOverride {
		delete(r.backendOverrides, key)
	}
}

// Reset discards the session for the given key (user sent /new).
func (r *Router) Reset(key string) {
	r.mu.Lock()

	s, ok := r.sessions[key]
	if !ok {
		r.mu.Unlock()
		return
	}

	proc := s.loadProcess()
	wasActive := !s.exempt && proc != nil && proc.Alive()
	r.unregisterSessionLocked(key, s, false)
	if wasActive {
		if r.activeCount.Add(-1) < 0 {
			r.activeCount.Store(0)
		}
	}
	r.storeDirty = true
	r.storeGen.Add(1)
	r.mu.Unlock()

	if proc != nil && proc.Alive() {
		proc.Close()
	}
	// Belt-and-suspenders: Close waits for proc.done which fires on shim
	// EOF, and in the normal path the shim's Run() defer chain unlinks the
	// socket before EOF propagates. But proc could be nil/!Alive (shim
	// still live after CLI crash, or a stale pointer we never wired a
	// readLoop to). Give the socket a short window to actually disappear
	// before downstream GetOrCreate attempts a same-key StartShim, which
	// would otherwise hit the dial-first guard ("refusing to clobber")
	// described in shim/server.go. Bounded at 2s so a truly stuck shim
	// falls through and the caller sees the real error instead of hanging.
	waitSocketGoneForKey(key, 2*time.Second)
	if r.shutdownCond != nil {
		r.shutdownCond.Broadcast()
	}

	slog.Info("session reset", "key", key)
	r.notifyChange()
}

// waitSocketGoneForKey bridges router-level session keys to the shim
// socket path derived from KeyHash, so callers don't need to plumb a
// shim.Manager reference through every Reset path. Returns quickly if
// the socket was never created.
func waitSocketGoneForKey(key string, maxWait time.Duration) {
	if key == "" {
		return
	}
	socketPath := shim.SocketPath(shim.KeyHash(key))
	shim.WaitSocketGone(socketPath, maxWait)
}

// ResetAndRecreate atomically resets a session and spawns a new one for the same key.
// This avoids the race window between Reset and GetOrCreate where a concurrent
// message could create a session with wrong opts.
func (r *Router) ResetAndRecreate(ctx context.Context, key string, opts AgentOpts) (*ManagedSession, error) {
	r.mu.Lock()

	// Delete old session if present
	hadOld := false
	if s, ok := r.sessions[key]; ok {
		hadOld = true
		proc := s.loadProcess()
		wasActive := !s.exempt && proc != nil && proc.Alive()
		// keepBackendOverride=true: the new opts may carry its own backend,
		// and spawnSession below consumes and clears the override atomically.
		r.unregisterSessionLocked(key, s, true)
		if wasActive {
			if r.activeCount.Add(-1) < 0 {
				r.activeCount.Store(0)
			}
		}
		r.storeDirty = true
		r.storeGen.Add(1)

		if proc != nil && proc.Alive() {
			r.mu.Unlock()
			proc.Close()
			// Same rationale as Router.Reset: make sure the shim
			// socket is gone before spawnSession's StartShim dials
			// it. Without this, ResetAndRecreate races the 30s
			// zombie window and fails with "refusing to clobber"
			// on the immediate re-bind.
			waitSocketGoneForKey(key, 2*time.Second)
			if r.shutdownCond != nil {
				r.shutdownCond.Broadcast()
			}
			r.mu.Lock()
		}
	}

	// Spawn new session while still holding mu (spawnSession handles unlock/relock)
	s, err := r.spawnSession(ctx, key, "", opts)
	if err != nil {
		// spawnSession already unlocked mu on error
		if hadOld {
			r.notifyChange()
		}
		return nil, err
	}
	// spawnSession already called notifyChange on success
	return s, nil
}

// RenameSession moves a session entry from oldKey to newKey, preserving the
// running process, sessionID, history, and totalCost. Used by the scratch
// promote flow to turn an ephemeral aside into a regular sidebar session
// without killing the CLI process underneath.
//
// Returns false when:
//   - oldKey == newKey
//   - oldKey does not exist
//   - newKey already exists (collision would otherwise drop an active session)
//   - newKey fails session-key validation
//
// The caller must ensure no Send is actively in flight for oldKey. In the
// scratch promote flow the drawer UI disables the save button while a turn
// is running, so the handler only invokes this when the session is idle.
//
// The onSessionID closure on the fresh ManagedSession captures newKey by
// value. A second RenameSession on the promoted key would leave that closure
// writing the pre-second-rename newKey into sessionIDToKey; today only the
// scratch → sidebar promote path invokes this, so that race is not reachable.
// If a future caller chains renames on the same session, rebuild onSessionID
// inside the destination struct or switch it to read s.key lazily.
func (r *Router) RenameSession(oldKey, newKey string) bool {
	if oldKey == newKey {
		return false
	}
	if err := ValidateSessionKey(newKey); err != nil {
		slog.Warn("RenameSession: invalid new key", "err", err)
		return false
	}
	r.mu.Lock()

	old, ok := r.sessions[oldKey]
	if !ok {
		r.mu.Unlock()
		return false
	}
	if _, collision := r.sessions[newKey]; collision {
		r.mu.Unlock()
		return false
	}

	// Session key is immutable on ManagedSession (parseKeyParts caches via
	// sync.Once; Snapshot depends on those cached parts). A fresh struct is
	// the only safe way to change the key.
	// R184-IDIOM-L2: clone prevSessionIDs so a subsequent spawnSession path
	// that appends to old.prevSessionIDs (in-place if cap permits) cannot
	// silently mutate fresh.prevSessionIDs. spawnSession already clones at
	// its construction site; Rename must do the same for symmetry.
	// persistedHistory is not re-written by the old session after Rename so
	// it is safe to share the backing array.
	fresh := &ManagedSession{
		key:              newKey,
		persistedHistory: old.persistedHistory,
		prevSessionIDs:   slices.Clone(old.prevSessionIDs),
		totalCost:        old.totalCost,
		exempt:           old.exempt,
		onSessionID: func(id string) {
			r.mu.Lock()
			r.trackSessionID(id)
			if id != "" {
				r.sessionIDToKey[id] = newKey
			}
			r.mu.Unlock()
		},
	}
	fresh.setWorkspace(old.Workspace())
	// Copy atomic fields (backend / CLI name+ver / user label / death reason /
	// lastActive / lastPrompt / lastActivity / sessionID). Each field is an
	// atomic.Pointer[string] so plain Load/Store round-trips are race-safe;
	// we hold r.mu which blocks concurrent writers of everything except the
	// Send hot path (lastPrompt / lastActivity), which are idempotent on copy.
	fresh.SetBackend(old.Backend())
	fresh.SetCLIName(old.CLIName())
	fresh.SetCLIVersion(old.CLIVersion())
	fresh.SetUserLabel(old.UserLabel())
	if dr := loadStringAtomic(&old.deathReason); dr != "" {
		storeStringAtomic(&fresh.deathReason, dr)
	}
	fresh.lastActive.Store(old.lastActive.Load())
	// Go through storeStringAtomic so each write allocates a fresh *string —
	// direct `.Store(lp)` would share the underlying pointer with `old` and
	// diverge from the rest of the codebase's "always helper" convention.
	// Currently safe because strings are immutable, but keeping the invariant
	// uniform avoids confusion if a future refactor ever makes the pointee
	// mutable.
	if lp := loadStringAtomic(&old.lastPrompt); lp != "" {
		storeStringAtomic(&fresh.lastPrompt, lp)
	}
	if la := loadStringAtomic(&old.lastActivity); la != "" {
		storeStringAtomic(&fresh.lastActivity, la)
	}
	fresh.setSessionID(old.getSessionID())

	// Move the process pointer so the running CLI keeps serving requests
	// under the new key. The old struct becomes an orphan with process=nil,
	// so any goroutine holding a stale reference to `old` that attempts Send
	// fails cleanly with "no active process".
	if proc := old.loadProcess(); proc != nil {
		fresh.storeProcess(proc)
	}
	old.storeProcess(nil)

	// Rebind the history source to the renamed session — the old Source
	// captured `old.snapshotChainIDs` which reads the now-orphaned struct.
	r.attachHistorySource(fresh)

	// Swap map entries and maintain every derived index.
	r.sessions[newKey] = fresh
	delete(r.sessions, oldKey)
	r.indexDel(oldKey)
	r.indexAdd(newKey)
	if id := fresh.getSessionID(); id != "" {
		r.sessionIDToKey[id] = newKey
	}
	if b, ok := r.backendOverrides[oldKey]; ok {
		r.backendOverrides[newKey] = b
		delete(r.backendOverrides, oldKey)
	}
	r.storeDirty = true
	r.storeGen.Add(1)
	r.mu.Unlock()

	slog.Info("session renamed", "old", oldKey, "new", newKey)
	r.notifyChange()
	return true
}

// Remove removes a session from the router and kills its process.
// Used by the dashboard to hide sessions from the list.
func (r *Router) Remove(key string) bool {
	r.mu.Lock()
	s, ok := r.sessions[key]
	if !ok {
		r.mu.Unlock()
		return false
	}

	// Kill process if alive
	proc := s.loadProcess()
	wasActive := !s.exempt && proc != nil && proc.Alive()
	r.unregisterSessionLocked(key, s, false)
	if wasActive {
		if r.activeCount.Add(-1) < 0 {
			r.activeCount.Store(0)
		}
	}
	r.storeDirty = true
	r.storeGen.Add(1)
	r.mu.Unlock()

	if proc != nil && proc.Alive() {
		// Remove is a pure delete, not a re-spawn, so we intentionally do
		// not call waitSocketGoneForKey. If a caller ever chains Remove
		// → GetOrCreate for the same key (e.g., a "restart session" UI
		// button), add the wait there — see Reset/ResetAndRecreate for
		// the UCCLEP-2026-04-26 pattern.
		proc.Close()
	}
	if r.shutdownCond != nil {
		r.shutdownCond.Broadcast()
	}

	slog.Info("session removed", "key", key)
	r.notifyChange()
	return true
}

// Cleanup closes sessions idle beyond TTL.
// First pass runs under RLock so PID syscalls / process.Alive checks don't
// block message processing (which needs write lock via GetOrCreate).
// Mutations (prune, activeCount recount) still require the write lock.
func (r *Router) Cleanup() {
	type expiredEntry struct {
		s    *ManagedSession
		key  string
		proc processIface
	}

	// ── Pass 1: snapshot candidate sessions under RLock ────────────
	// Single-pass build with a conservative capacity hint (half the map —
	// planner/exempt and suspended sessions are typically the majority on
	// idle deployments, so over-allocating to len(r.sessions) wastes cap on
	// every 5-minute tick). A prior two-loop version pre-counted candidates
	// to size the slice exactly, but loadProcess() is an atomic read whose
	// result can change between the two passes, and the doubled map walk
	// paid O(2n) for no correctness benefit. R59-GO-M1.
	r.mu.RLock()
	type cand struct {
		key        string
		s          *ManagedSession
		proc       processIface
		lastActive time.Time
	}
	candidates := make([]cand, 0, len(r.sessions)/2+1)
	for key, s := range r.sessions {
		if s.exempt {
			continue // planner sessions are never expired by TTL
		}
		proc := s.loadProcess()
		if proc == nil {
			continue
		}
		candidates = append(candidates, cand{key, s, proc, s.GetLastActive()})
	}
	ttl := r.ttl
	totalTimeout := r.totalTimeout
	r.mu.RUnlock()

	if totalTimeout <= 0 {
		totalTimeout = cli.DefaultTotalTimeout
	}
	stuckThreshold := 2 * totalTimeout

	// ── Pass 2: classify outside the lock (may perform PID syscalls) ─
	var expired []expiredEntry
	var stuckKill []expiredEntry
	now := time.Now()
	for _, c := range candidates {
		alive := c.proc.Alive()
		if !alive {
			continue
		}
		running := c.proc.IsRunning()

		// Stuck running: watchdog failed, reclaim slot.
		if running {
			if age := now.Sub(c.lastActive); age > stuckThreshold {
				slog.Warn("stuck running session detected, force killing",
					"key", c.key, "running_for", age, "threshold", stuckThreshold)
				storeStringAtomic(&c.s.deathReason, "stuck_running")
				stuckKill = append(stuckKill, expiredEntry{c.s, c.key, c.proc})
			}
			continue
		}

		// PID liveness: shim alive but CLI PID is gone.
		if pid := c.proc.PID(); pid > 0 && !osutil.PidAlive(pid) {
			slog.Warn("CLI process gone but session still alive, force killing",
				"key", c.key, "pid", pid)
			storeStringAtomic(&c.s.deathReason, "pid_gone")
			stuckKill = append(stuckKill, expiredEntry{c.s, c.key, c.proc})
			continue
		}

		// Normal idle TTL expiry.
		if now.Sub(c.lastActive) > ttl {
			slog.Info("session expired", "key", c.key, "idle", now.Sub(c.lastActive))
			storeStringAtomic(&c.s.deathReason, "idle_timeout")
			expired = append(expired, expiredEntry{c.s, c.key, c.proc})
		}
	}

	closedCount := 0
	for _, e := range stuckKill {
		e.proc.Kill()
		closedCount++
	}
	// TTL-expired sessions are closed but never re-spawned for the same
	// key by this function, so waitSocketGoneForKey is unnecessary here.
	// The next unrelated GetOrCreate will hash to a different socket.
	for _, e := range expired {
		e.proc.Close()
		closedCount++
	}
	if r.shutdownCond != nil {
		r.shutdownCond.Broadcast()
	}

	r.mu.Lock()
	// Prune orphaned sessions: nil process, no session ID, past prune TTL.
	// Maintain a running newActive counter so we avoid a separate countActive() O(n) pass.
	var pruned int
	var newActive int64
	for key, s := range r.sessions {
		if s.exempt {
			continue // planner sessions are never pruned
		}
		if r.shouldPrune(s, now) {
			// Terminal removal: free the backend override too (previous versions
			// leaked it; see MED-5 in 2026-04-26 architecture review).
			r.unregisterSessionLocked(key, s, false)
			pruned++
			continue
		}
		if s.isAlive() {
			newActive++
		}
	}
	r.activeCount.Store(newActive)

	// Snapshot sessions for periodic save (while still holding the lock).
	// Skip save if nothing changed since last Cleanup cycle.
	if closedCount > 0 || pruned > 0 {
		r.storeDirty = true
		r.storeGen.Add(1)
	}
	var sessionsCopy map[string]*ManagedSession
	var knownIDsCopy map[string]bool
	var wsOverridesCopy map[string]string
	storePath := r.storePath
	snapshotGen := r.storeGen.Load()
	snapshotWsGen := r.wsOverridesGen.Load()
	if r.storeDirty {
		sessionsCopy = make(map[string]*ManagedSession, len(r.sessions))
		for k, v := range r.sessions {
			sessionsCopy[k] = v
		}
	}
	if r.wsOverridesDirty {
		wsOverridesCopy = make(map[string]string, len(r.workspaceOverrides))
		for k, v := range r.workspaceOverrides {
			wsOverridesCopy[k] = v
		}
	}
	// knownIDs is append-only and relatively stable. Throttle its fsync to
	// bound disk I/O (see knownIDsSaveInterval constant). Commit
	// knownIDsSavedAt optimistically here — still under r.mu — so a
	// concurrent saveIfDirty tick on the neighboring interval boundary
	// sees the updated timestamp and skips the redundant work. (The
	// underlying tmp file is unique per WriteFileAtomic call via
	// os.CreateTemp, so this throttle is an I/O budget gate, not a
	// file-level race guard.)
	var snapshotKnownIDsGen uint64
	if r.knownIDsDirty && now.Sub(r.knownIDsSavedAt) >= knownIDsSaveInterval {
		knownIDsCopy = make(map[string]bool, len(r.knownIDs))
		for id := range r.knownIDs {
			knownIDsCopy[id] = true
		}
		snapshotKnownIDsGen = r.knownIDsGen
		r.knownIDsSavedAt = now
	}

	r.mu.Unlock()

	// Periodic save outside lock to reduce crash-recovery data loss.
	if sessionsCopy != nil {
		if err := saveStore(storePath, sessionsCopy); err != nil {
			slog.Warn("periodic session save failed", "err", err)
		} else {
			// Only clear dirty flag if no concurrent mutation occurred since snapshot.
			r.mu.Lock()
			if r.storeGen.Load() == snapshotGen {
				r.storeDirty = false
			}
			r.mu.Unlock()
		}
	}
	if wsOverridesCopy != nil {
		if err := saveWorkspaceOverrides(storePath, wsOverridesCopy); err != nil {
			slog.Warn("periodic workspace overrides save failed", "err", err)
		} else {
			// Only clear dirty flag if no concurrent SetWorkspace occurred since snapshot.
			r.mu.Lock()
			if r.wsOverridesGen.Load() == snapshotWsGen {
				r.wsOverridesDirty = false
			}
			r.mu.Unlock()
		}
	}
	if knownIDsCopy != nil {
		// knownIDsSavedAt was committed under r.mu above (pre-save) to
		// gate concurrent saveIfDirty. On success we only clear the dirty
		// flag; on failure we leave it set so the next tick retries,
		// accepting one extra interval of delay in exchange for no
		// torn-write race.
		if err := saveKnownIDs(storePath, knownIDsCopy); err != nil {
			slog.Warn("periodic known IDs save failed", "err", err)
		} else {
			// Generation counter matches the (sessions | ws-overrides) pattern:
			// if a concurrent trackSessionID fired between snapshot and re-lock,
			// the gen will differ and we leave the dirty flag set so the next
			// tick retries. len()-equality alone is insufficient because an
			// add + evict pair produces identical lengths with different content.
			r.mu.Lock()
			if r.knownIDsGen == snapshotKnownIDsGen {
				r.knownIDsDirty = false
			}
			r.mu.Unlock()
		}
	}

	if len(expired) > 0 || len(stuckKill) > 0 || pruned > 0 {
		r.notifyChange()
	}
}

// shouldPrune returns true if a non-exempt session should be removed from the map.
// Covers: nil-process stubs, dead processes past pruneTTL. Caller must hold r.mu.
func (r *Router) shouldPrune(s *ManagedSession, now time.Time) bool {
	if now.Sub(s.GetLastActive()) <= r.pruneTTL {
		return false
	}
	proc := s.loadProcess()
	if proc == nil {
		return true // nil-process stub (with or without session ID)
	}
	return !proc.Alive() // exited process past pruneTTL
}

// StartCleanupLoop runs Cleanup periodically and saves dirty session state
// on a shorter interval to reduce data loss on crash.
func (r *Router) StartCleanupLoop(ctx context.Context, interval time.Duration) {
	go func() {
		cleanupTicker := time.NewTicker(interval)
		defer cleanupTicker.Stop()
		// Save dirty state every 30s to reduce crash-recovery data loss
		// from ~TTL/2 (~15min) to ~30s.
		saveTicker := time.NewTicker(30 * time.Second)
		defer saveTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-cleanupTicker.C:
				r.Cleanup()
			case <-saveTicker.C:
				r.saveIfDirty()
			}
		}
	}()
}

// saveIfDirty saves the session store if any mutations have occurred since the last save.
// Also persists knownIDs on the same throttle as Cleanup so crashes between
// Cleanup ticks do not discard newly discovered session IDs.
func (r *Router) saveIfDirty() {
	r.mu.Lock()
	knownIDsDue := r.knownIDsDirty && time.Since(r.knownIDsSavedAt) >= knownIDsSaveInterval
	if !r.storeDirty && !r.wsOverridesDirty && !knownIDsDue {
		r.mu.Unlock()
		return
	}
	var sessionsCopy map[string]*ManagedSession
	if r.storeDirty {
		sessionsCopy = make(map[string]*ManagedSession, len(r.sessions))
		for k, v := range r.sessions {
			sessionsCopy[k] = v
		}
	}
	var wsOverridesCopy map[string]string
	if r.wsOverridesDirty {
		wsOverridesCopy = make(map[string]string, len(r.workspaceOverrides))
		for k, v := range r.workspaceOverrides {
			wsOverridesCopy[k] = v
		}
	}
	var knownIDsCopy map[string]bool
	var snapshotKnownIDsGen uint64
	if knownIDsDue {
		knownIDsCopy = make(map[string]bool, len(r.knownIDs))
		for id := range r.knownIDs {
			knownIDsCopy[id] = true
		}
		snapshotKnownIDsGen = r.knownIDsGen
		// Commit savedAt under r.mu so a concurrent Cleanup tick
		// re-checking the throttle skips — both paths share the same
		// .tmp target file and torn writes cannot be recovered.
		r.knownIDsSavedAt = time.Now()
	}
	storePath := r.storePath
	snapshotGen := r.storeGen.Load()
	snapshotWsGen := r.wsOverridesGen.Load()
	r.mu.Unlock()

	if sessionsCopy != nil {
		if err := saveStore(storePath, sessionsCopy); err != nil {
			slog.Warn("periodic session save failed", "err", err)
		} else {
			r.mu.Lock()
			if r.storeGen.Load() == snapshotGen {
				r.storeDirty = false
			}
			r.mu.Unlock()
		}
	}
	if wsOverridesCopy != nil {
		if err := saveWorkspaceOverrides(storePath, wsOverridesCopy); err != nil {
			slog.Warn("periodic workspace overrides save failed", "err", err)
		} else {
			// Only clear dirty flag if no concurrent SetWorkspace occurred since snapshot.
			r.mu.Lock()
			if r.wsOverridesGen.Load() == snapshotWsGen {
				r.wsOverridesDirty = false
			}
			r.mu.Unlock()
		}
	}
	if knownIDsCopy != nil {
		// savedAt committed pre-save; only toggle dirty on success.
		if err := saveKnownIDs(storePath, knownIDsCopy); err != nil {
			slog.Warn("periodic known IDs save failed", "err", err)
		} else {
			// Match the storeGen/wsOverridesGen pattern: only clear dirty if
			// no concurrent trackSessionID fired since the snapshot.
			r.mu.Lock()
			if r.knownIDsGen == snapshotKnownIDsGen {
				r.knownIDsDirty = false
			}
			r.mu.Unlock()
		}
	}
}

// StartShimReconcileLoop periodically checks for suspended sessions that have
// live shim processes and reconnects them. This covers edge cases where the
// connection to a shim drops during normal operation (e.g. temporary I/O error)
// but the shim and CLI process are still alive.
func (r *Router) StartShimReconcileLoop(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Thread ctx so SIGTERM during a per-shim 15s handshake
				// aborts promptly instead of waiting one full timeout per
				// suspended session.
				r.ReconnectShimsCtx(ctx)
			}
		}
	}()
}

// Shutdown gracefully closes all sessions, waiting for running ones to complete.
// Idempotent: subsequent calls return immediately after the first completes.
//
// CONTRACT: Shutdown assumes the naozhi process terminates shortly after it
// returns. Two watcher goroutines (the one below that wraps
// `r.historyWg.Wait()` + the shim reconcile ticker in Scheduler.Stop) are
// allowed to outlive Shutdown when their work is blocked on hung I/O —
// relying on OS teardown for cleanup. If future code ever makes Router
// reusable after Shutdown (tests that spin a router up and down, hot
// reloads, etc.), those watchers would accumulate one-per-cycle. The
// R44-REL-HIST-GOROUTINE / R44-REL-TRIGGER-GOROUTINE audit items pin this
// assumption; a `TestShutdown_SingleShotContract` source-level test
// enforces `shutdownOnce` stays in place so any attempt to make Shutdown
// reversible trips CI and forces a re-audit.
func (r *Router) Shutdown() {
	r.shutdownOnce.Do(r.shutdown)
}

func (r *Router) shutdown() {
	// Cancel the history ctx so in-flight LoadHistory*Ctx calls (both startup
	// preloaders and reconnect-time chain walkers) abort instead of blocking
	// behind slow filesystem reads. The bounded Wait below provides a hard
	// deadline on top of cancellation in case a syscall is stuck past the
	// ctx check point.
	if r.historyCancel != nil {
		r.historyCancel()
	}

	// Wait for startup history-loading goroutines to finish first,
	// but don't block forever if filesystem I/O is hung (e.g. NFS).
	// Reduced from 15s to 5s now that cancellation short-circuits the
	// loaders at the next chunk/line boundary; the remaining budget is
	// for goroutines mid-syscall.
	//
	// Goroutine leak on timeout is intentional and bounded by the
	// "Shutdown is single-shot, process terminates next" contract above.
	// The wrapper goroutine exits the moment historyWg reaches zero —
	// either naturally (loaders finish) or after the CLI process hosting
	// the hung syscall is reaped by the kernel on OS teardown. Do NOT
	// replace historyWg.Wait() with a ctx-aware pattern here: the only
	// reason we spawn a goroutine at all is that WaitGroup has no
	// ctx-aware Wait; the select below IS the bounded-wait primitive.
	historyDone := make(chan struct{})
	go func() {
		// Goroutine intentionally left running on timeout; cleaned up on process exit.
		// See Shutdown godoc for the single-shot lifecycle contract that
		// makes this acceptable. R44-REL-HIST-GOROUTINE.
		r.historyWg.Wait()
		close(historyDone)
	}()
	historyTimer := time.NewTimer(5 * time.Second)
	select {
	case <-historyDone:
		historyTimer.Stop()
	case <-historyTimer.C:
		slog.Warn("shutdown: history loading timed out after 5s, proceeding")
	}
	// Deadline timer: broadcast to unblock Wait() when timeout expires
	timer := time.AfterFunc(ShutdownTimeout, func() {
		if r.shutdownCond != nil {
			r.shutdownCond.Broadcast()
		}
	})
	defer timer.Stop()

	r.mu.Lock()

	// Wait for running sessions to complete (up to ShutdownTimeout)
	deadline := time.Now().Add(ShutdownTimeout)
	for {
		running := false
		for _, s := range r.sessions {
			if p := s.loadProcess(); p != nil && p.IsRunning() {
				running = true
				break
			}
		}
		if !running || time.Now().After(deadline) {
			break
		}
		if r.shutdownCond != nil {
			r.shutdownCond.Wait() // atomically releases and re-acquires r.mu
		} else {
			// Fallback for tests without shutdownCond
			r.mu.Unlock()
			time.Sleep(100 * time.Millisecond)
			r.mu.Lock()
		}
	}

	// Snapshot sessions for saving outside lock
	sessionsCopy := make(map[string]*ManagedSession, len(r.sessions))
	for k, v := range r.sessions {
		sessionsCopy[k] = v
	}
	storePath := r.storePath
	knownIDsCopy := make(map[string]bool, len(r.knownIDs))
	for id := range r.knownIDs {
		knownIDsCopy[id] = true
	}
	wsOverrides := make(map[string]string, len(r.workspaceOverrides))
	for k, v := range r.workspaceOverrides {
		wsOverrides[k] = v
	}

	// Collect processes to close, then release lock to close concurrently
	var procs []processIface
	for key, s := range r.sessions {
		if p := s.loadProcess(); p != nil && p.Alive() {
			slog.Info("shutting down session", "key", key)
			procs = append(procs, p)
		}
	}
	r.mu.Unlock()

	// Save session state outside lock (avoids JSON marshal + file I/O under mutex).
	// disk_full is surfaced as a distinct structured field so monitoring can
	// page on ENOSPC separately from transient write failures; shutdown loses
	// all un-persisted state silently otherwise. Each error chain is walked
	// once — the three save paths are independent, so sharing a single flag
	// would mis-attribute a disk-full on saveStore to saveKnownIDs.
	if err := saveStore(storePath, sessionsCopy); err != nil {
		slog.Error("save session store on shutdown", "err", err, "disk_full", osutil.IsDiskFull(err))
	}
	if err := saveKnownIDs(storePath, knownIDsCopy); err != nil {
		slog.Error("save known session IDs on shutdown", "err", err, "disk_full", osutil.IsDiskFull(err))
	}
	if err := saveWorkspaceOverrides(storePath, wsOverrides); err != nil {
		slog.Error("save workspace overrides on shutdown", "err", err, "disk_full", osutil.IsDiskFull(err))
	}

	// Detach shim processes (keep them alive for reconnect after restart)
	// instead of Close (which would kill the CLI).
	var wg sync.WaitGroup
	for _, proc := range procs {
		wg.Add(1)
		go func(p processIface) {
			defer wg.Done()
			// Shutdown happens last in the graceful-stop sequence, so a panic
			// inside Detach/Close (e.g. a nil shim conn from a late race)
			// would bring down the whole process and skip any remaining
			// cleanup in main. Swallow so the rest of the goroutines still
			// finish and naozhi exits cleanly.
			defer func() {
				if r := recover(); r != nil {
					slog.Error("session shutdown: detach panicked",
						"panic", r, "stack", string(debug.Stack()))
				}
			}()
			if dp, ok := p.(interface{ Detach() }); ok {
				dp.Detach()
			} else {
				p.Close()
			}
		}(proc)
	}
	wg.Wait()
}

// DefaultWorkspace returns the router's default working directory.
func (r *Router) DefaultWorkspace() string {
	return r.workspace
}

// stripResumeArgs removes --resume <value> from CLI args.
// Used by drift check: --resume is session-specific, not a config change.
//
// Fast path: return the original slice unchanged if --resume is absent.
// reconnectShims calls this once per discovered shim during startup; for
// deployments with many shims where no session was mid-turn the arg is
// absent and we avoid the O(N) slice alloc + copy. R64-PERF-9.
func stripResumeArgs(args []string) []string {
	hasResume := false
	for _, a := range args {
		if a == "--resume" {
			hasResume = true
			break
		}
	}
	if !hasResume {
		return args
	}
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--resume" {
			// Skip the bare flag. If a value follows, skip that too. A
			// trailing `--resume` with no value must also be dropped —
			// otherwise it survives into the drift-check compare and
			// spuriously shuts down the shim on args equality mismatch.
			// R65-GO-M-2.
			if i+1 < len(args) {
				i++
			}
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// Version returns a monotonic counter incremented on every session mutation.
// Used by the dashboard for efficient change detection without full JSON
// comparison. storeGen is atomic so this is lock-free — the dashboard polls
// Version() from the /api/sessions hot path next to a separate ListSessions
// RLock, and the previous implementation doubled that lock traffic.
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

// CLIPath returns the CLI binary path for health checks.
func (r *Router) CLIPath() string {
	if r.wrapper == nil {
		return ""
	}
	return r.wrapper.CLIPath
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

// ListSessions returns a snapshot of all sessions for the dashboard.
// Collects references under r.mu, then releases before snapshotting
// to avoid blocking the router while getSessionID() waits on sendMu.
func (r *Router) ListSessions() []SessionSnapshot {
	r.mu.RLock()
	refs := make([]*ManagedSession, 0, len(r.sessions))
	for _, s := range r.sessions {
		refs = append(refs, s)
	}
	r.mu.RUnlock()

	snapshots := make([]SessionSnapshot, len(refs))
	for i, s := range refs {
		snapshots[i] = s.Snapshot()
	}
	return snapshots
}

// GetSession returns the session for the given key, or nil.
func (r *Router) GetSession(key string) *ManagedSession {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sessions[key]
}

// SetUserLabel updates the operator-set display label for the given session.
// Passing an empty label clears any prior value. Callers are responsible for
// validating label length/charset via ValidateUserLabel; this method only
// performs the store + version bump + onChange broadcast so connected
// dashboards see the change immediately (not on the next /api/sessions poll).
//
// Returns false when the session key is unknown (no mutation performed).
//
// No-op fast path: when the requested label equals the current value, skip
// the dirty flag + version bump + WS broadcast. A dashboard that replays the
// same label (e.g. blur-without-edit on an editable title) otherwise forces a
// full saveIfDirty cycle (2-5 ms fsync on SSD) and a sessions_update fanout
// to every connected client for zero behavioural change. R176-PERF-P1.
func (r *Router) SetUserLabel(key, label string) bool {
	r.mu.Lock()
	s := r.sessions[key]
	if s == nil {
		r.mu.Unlock()
		return false
	}
	if s.UserLabel() == label {
		r.mu.Unlock()
		return true
	}
	s.SetUserLabel(label)
	r.storeDirty = true
	r.storeGen.Add(1)
	r.mu.Unlock()
	// Match every other mutator (Reset/Remove/ResetChat/spawnSession...): the
	// dashboard's onChange WebSocket broadcast needs a kick so the sidebar
	// refreshes instantly rather than waiting up to one poll interval. R64-GO-H1.
	r.notifyChange()
	return true
}

// InterruptSession sends SIGINT to the CLI process for the given session key.
// Returns true if the session was found and interrupted.
//
// WARNING: SIGINT terminates the whole CLI process on Claude `-p` mode (and
// any non-REPL CLI), which both kills the live shim conversation and burns a
// fresh shim slot on the next message. Prefer InterruptSessionSafe for
// operator-facing actions (dashboard "interrupt" button); this function is
// kept for callers that truly need process-level signalling (tests, forced
// teardown) or for the fallback branch inside InterruptSessionSafe itself.
func (r *Router) InterruptSession(key string) bool {
	r.mu.RLock()
	s := r.sessions[key]
	r.mu.RUnlock()
	if s == nil {
		return false
	}
	return s.Interrupt()
}

// InterruptSessionSafe is the preferred entry point for dashboard/HTTP/WS
// interrupt requests. It first attempts the in-band stream-json
// control_request path (InterruptViaControl), which aborts the active turn
// WITHOUT terminating the CLI subprocess, so the shim, socket, and session
// ID all survive for the next message. When the CLI protocol does not
// support control_request (ACP), it falls back to SIGINT via Interrupt();
// other non-Sent outcomes are returned unchanged.
//
// Returns the outcome so callers can surface accurate UI (e.g. "aborted"
// vs. "nothing was running").
//
// Design note — when to fall back to SIGINT:
//
//   - InterruptUnsupported (ACP protocol has no stdin-level interrupt): we
//     have to SIGINT; there is no other mechanism. SIGINT on ACP is also
//     not known to be destructive (ACP agents don't exit on signal), so
//     this fallback has a legitimate home.
//   - InterruptNoTurn (session alive but no active turn): do NOT fall back.
//     Raw SIGINT on an idle Claude `-p` subprocess terminates it, which
//     forces a brand-new shim on the next message. A button press on an
//     idle session should report "nothing was running" (→ `not_running` in
//     the HTTP layer), not silently close the session.
//   - InterruptError (transport write failed): do NOT fall back. The
//     failure almost certainly means the shim socket is broken; SIGINT
//     would travel the same broken transport and also fail. Surface the
//     error so F6's reconcile path has a chance to purge the zombie.
//
// For the Claude CLI `-p` mode — our primary use case — SIGINT terminates
// the CLI process entirely (not just the current turn). That cascades into
// shim sending cli_exited, naozhi's Alive() flipping to false, and the next
// user message starting a brand-new shim, leaking the previous socket path
// and sometimes losing resume context. control_request on CLI 2.1.119 has
// been verified to kill the in-flight tool invocation and emit a result
// event without killing the process.
func (r *Router) InterruptSessionSafe(key string) InterruptOutcome {
	outcome := r.InterruptSessionViaControl(key)
	switch outcome {
	case InterruptUnsupported:
		// Protocol has no stdin interrupt; SIGINT is the only option.
		if r.InterruptSession(key) {
			return InterruptSent
		}
		return InterruptNoSession
	case InterruptSent, InterruptNoSession, InterruptNoTurn, InterruptError:
		// Callers handle each outcome verbatim. The HTTP and WS handlers map
		// {InterruptNoTurn, InterruptError} to "not_running" so the dashboard
		// re-queries state.
		return outcome
	default:
		// A new outcome was added to the enum without updating this switch.
		// Log once and map to InterruptNoSession so the dashboard shows
		// "not_running" rather than silently passing through an outcome the
		// HTTP layer doesn't know how to render. R65-GO-L-3.
		slog.Warn("InterruptSessionSafe: unhandled interrupt outcome", "outcome", int(outcome), "key", key)
		return InterruptNoSession
	}
}

// InterruptSessionViaControl requests the CLI to abort the active turn via the
// stream-json control_request protocol (no SIGINT, no process kill). Unlike
// InterruptSession, the in-flight Send() observes the CLI's natural result
// event and returns normally, so ownership of the session stays with the
// current dispatch owner loop which can then process queued follow-up messages
// on the same live CLI.
//
// Returns an InterruptOutcome so callers can log accurately (a session that
// exists but has no active turn yet returns InterruptNoTurn, not
// InterruptNoSession — logging "aborted turn" in that case would be a lie).
func (r *Router) InterruptSessionViaControl(key string) InterruptOutcome {
	r.mu.RLock()
	s := r.sessions[key]
	r.mu.RUnlock()
	if s == nil {
		return InterruptNoSession
	}
	return s.InterruptViaControl()
}

// DiscoveryExcludeIDs returns session IDs to exclude from filesystem discovery.
// Only sessions with a running process are excluded to prevent duplicates.
// Suspended sessions (no process) are allowed through so their underlying
// session files appear in the history popover (deduplicated against the workspace).
func (r *Router) DiscoveryExcludeIDs() map[string]bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make(map[string]bool, len(r.sessions))
	for _, s := range r.sessions {
		if s.loadProcess() == nil {
			continue
		}
		if id := s.getSessionID(); id != "" {
			ids[id] = true
		}
		for _, id := range s.prevSessionIDs {
			ids[id] = true
		}
	}
	return ids
}

// maxKnownIDs caps the persistent known-IDs set to prevent unbounded growth.
// UUID session IDs are 36 bytes; at 10K entries this is ~360KB in memory.
const maxKnownIDs = 10000

// trackSessionID adds a session ID to the persistent known-IDs set.
// Caller must hold r.mu OR call before any concurrent access (e.g. NewRouter init).
//
// Eviction policy: FIFO by insertion order. Previous implementation relied on
// Go's random map iteration which could drop a still-active session ID, and
// the discovery scanner would then misclassify its live CLI process as an
// unknown external session. Maintaining an order slice alongside the map
// costs ~80KB at 10K entries — acceptable for the correctness win.
func (r *Router) trackSessionID(id string) {
	if id == "" {
		return
	}
	if r.knownIDs[id] {
		return
	}
	if len(r.knownIDs) >= maxKnownIDs {
		// Drop the oldest entry; r.knownIDsOrder invariant is that it holds
		// exactly the keys of r.knownIDs in insertion order. Shift in-place
		// rather than reslicing: `knownIDsOrder[1:]` keeps the backing array
		// pinned from the original data pointer, so after many evictions the
		// slice header drifts rightward and the leading, now-unused portion
		// of the array can't be reused — eventually forcing re-allocation.
		// The copy + clear tail approach keeps the header stable and lets the
		// allocator reuse the same buffer indefinitely.
		oldest := r.knownIDsOrder[0]
		delete(r.knownIDs, oldest)
		n := len(r.knownIDsOrder)
		copy(r.knownIDsOrder, r.knownIDsOrder[1:])
		r.knownIDsOrder[n-1] = ""
		r.knownIDsOrder = r.knownIDsOrder[:n-1]
	}
	r.knownIDs[id] = true
	r.knownIDsOrder = append(r.knownIDsOrder, id)
	r.knownIDsGen++
	r.knownIDsDirty = true
}

// RegisterForResume creates a suspended session entry so that the next
// GetOrCreate call for this key will resume the given session ID.
// If another session already targets the same sessionID, the existing key
// is returned (deduplication) and no new entry is created.
func (r *Router) RegisterForResume(key, sessionID, workspace, lastPrompt string) (effectiveKey string) {
	r.mu.Lock()
	if _, exists := r.sessions[key]; exists {
		r.mu.Unlock()
		return key // already exists with this exact key
	}
	// Deduplicate: if another session already targets this sessionID, reuse it.
	if existingKey, ok := r.sessionIDToKey[sessionID]; ok {
		if _, exists := r.sessions[existingKey]; exists {
			r.mu.Unlock()
			return existingKey
		}
		// Stale index entry; clean up and continue.
		delete(r.sessionIDToKey, sessionID)
	}
	s := &ManagedSession{
		key: key,
	}
	s.setWorkspace(workspace)
	s.SetCLIName(r.cliNameDefault())
	s.SetCLIVersion(r.cliVersionDefault())
	s.setSessionID(sessionID)
	if lastPrompt != "" {
		storeStringAtomic(&s.lastPrompt, lastPrompt)
	}
	r.trackSessionID(sessionID)
	if sessionID != "" {
		r.sessionIDToKey[sessionID] = key
	}
	s.lastActive.Store(time.Now().UnixNano())
	r.attachHistorySource(s)
	r.sessions[key] = s
	r.indexAdd(key)
	r.storeDirty = true
	r.storeGen.Add(1)
	r.mu.Unlock()

	r.notifyChange()
	return key
}

// RegisterCronStub creates a suspended exempt session for a cron job so the
// job appears in the dashboard workspace list before its first execution.
// Key format is "cron:<jobID>". If an entry already exists, workspace and
// lastPrompt are refreshed in place (to reflect edits via dashboard).
// The stub has no process and no session ID; the first GetOrCreate call
// (at cron execute time) will spawn a real CLI process and reuse this entry.
func (r *Router) RegisterCronStub(key, workspace, lastPrompt string) {
	r.mu.Lock()
	if existing, ok := r.sessions[key]; ok {
		changed := false
		// Refresh workspace/prompt on existing stub; don't touch live process.
		if existing.loadProcess() == nil {
			if workspace != "" && existing.Workspace() != workspace {
				existing.setWorkspace(workspace)
				changed = true
			}
			if lastPrompt != "" && loadStringAtomic(&existing.lastPrompt) != lastPrompt {
				storeStringAtomic(&existing.lastPrompt, lastPrompt)
				changed = true
			}
			// R176-PERF-P1: only mark dirty + bump version when something
			// actually changed. Cron scheduler calls RegisterCronStub on
			// every reload of cron.yaml, and most reloads are a no-op — the
			// stubs already reflect the file's contents. Without this gate
			// each reload forced a saveIfDirty fsync (2-5 ms on SSD) and a
			// sessions_update fanout with no observable effect.
			if changed {
				r.storeDirty = true
				r.storeGen.Add(1)
			}
		}
		r.mu.Unlock()
		// Preserve the original "always notify on refresh" behaviour so the
		// dashboard's sidebar edit flow (rename → save → reload) gets an
		// immediate WS kick rather than waiting up to one poll interval.
		// notifyChange is cheap; the expensive path (saveIfDirty) is what we
		// just guarded above.
		r.notifyChange()
		return
	}
	s := &ManagedSession{
		key:    key,
		exempt: true,
	}
	s.setWorkspace(workspace)
	s.SetCLIName(r.cliNameDefault())
	s.SetCLIVersion(r.cliVersionDefault())
	if lastPrompt != "" {
		storeStringAtomic(&s.lastPrompt, lastPrompt)
	}
	s.lastActive.Store(time.Now().UnixNano())
	r.attachHistorySource(s)
	r.sessions[key] = s
	r.indexAdd(key)
	r.storeDirty = true
	r.storeGen.Add(1)
	r.mu.Unlock()

	r.notifyChange()
}

// ManagedExcludeSets returns PIDs, session IDs, and CWDs of all managed sessions
// in a single lock acquisition. Used by discovery.Scan to avoid three separate mutex grabs.
func (r *Router) ManagedExcludeSets() (pids map[int]bool, sessionIDs map[string]bool, cwds map[string]bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	pids = make(map[int]bool)
	sessionIDs = make(map[string]bool)
	cwds = make(map[string]bool)
	for _, s := range r.sessions {
		if id := s.getSessionID(); id != "" {
			sessionIDs[id] = true
		}
		if p := s.loadProcess(); p != nil && p.Alive() {
			if pid := p.PID(); pid > 0 {
				pids[pid] = true
			}
			if ws := s.Workspace(); ws != "" {
				cwds[ws] = true
			}
		}
	}
	return
}

// Takeover creates a managed session to replace an external Claude CLI session.
// It uses --resume to preserve the conversation context, and loads JSONL history
// for dashboard display. The caller must ensure the original process has been
// terminated before calling.
func (r *Router) Takeover(ctx context.Context, key string, sessionID string, workspace string, opts AgentOpts) (*ManagedSession, error) {
	// R188-SEC-M2: same flag-injection guard as GetOrCreate. Takeover flows
	// from upstream RPC with caller-supplied AgentOpts.
	if err := validateModel(opts.Model); err != nil {
		return nil, err
	}
	r.mu.Lock()
	// If key already exists (e.g. re-takeover same CWD), close the old process
	if s, ok := r.sessions[key]; ok {
		if p := s.loadProcess(); p != nil && p.Alive() {
			oldSession := s
			proc := p
			r.mu.Unlock()
			proc.Close()
			// Takeover reuses the same key, so the next spawnSession below
			// will StartShim against the same socket path. Wait for the
			// shim to release it (same race as Reset / ResetAndRecreate,
			// see UCCLEP-2026-04-26 design).
			waitSocketGoneForKey(key, 2*time.Second)
			r.mu.Lock()
			// Only delete if no concurrent goroutine replaced this session.
			// keepBackendOverride=true: Takeover re-spawns on the same key
			// and spawnSession below consumes the override atomically.
			if cur, ok := r.sessions[key]; ok && cur == oldSession {
				r.unregisterSessionLocked(key, cur, true)
				r.storeDirty = true
				r.storeGen.Add(1)
			} else if cur != nil && cur.isAlive() {
				// Concurrent GetOrCreate created a new session during Close();
				// abort takeover rather than silently returning wrong session.
				r.mu.Unlock()
				return nil, fmt.Errorf("concurrent session created for key %s during takeover", key)
			}
			// Implicit else: concurrent goroutine replaced the session with an exited
			// one. Leave r.sessions[key] as-is — spawnSession below will overwrite
			// it and call indexAdd, keeping the index consistent. No indexDel here
			// because we are not removing from r.sessions.
		} else {
			// Dead session branch: same keepBackendOverride=true rationale.
			r.unregisterSessionLocked(key, s, true)
			r.storeDirty = true
			r.storeGen.Add(1)
		}
		r.countActive()
	}
	// Set workspace override for the chat key prefix. Must bump the dirty
	// flag so the override is persisted; otherwise a crash before another
	// flushing path fires would lose the takeover's chosen workspace.
	if chatKey := chatKeyFor(key); chatKey != key {
		if prev, ok := r.workspaceOverrides[chatKey]; !ok || prev != workspace {
			r.workspaceOverrides[chatKey] = workspace
			r.wsOverridesDirty = true
			r.wsOverridesGen.Add(1)
		}
	}
	s, err := r.spawnSession(ctx, key, sessionID, opts)
	if err != nil {
		return nil, err
	}
	return s, nil
}
