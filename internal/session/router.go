package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/osutil"
)

// ShutdownTimeout is the maximum time to wait for graceful shutdown
// of running sessions (Router) and HTTP connections (Server).
// Exported so both session and server packages use a single value.
const ShutdownTimeout = 30 * time.Second

// ErrMaxProcs is returned when all process slots are occupied.
var ErrMaxProcs = errors.New("max concurrent processes reached")

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
	wrapper        *cli.Wrapper
	maxProcs       int
	ttl            time.Duration
	pruneTTL       time.Duration
	model          string
	extraArgs      []string
	workspace      string // default cwd for CLI processes
	claudeDir      string // ~/.claude dir for loading session history

	// workspaceOverrides stores per-chat workspace overrides.
	// Key format: "platform:chatType:chatID"
	workspaceOverrides map[string]string

	// activeCount tracks currently alive processes
	activeCount int

	// pendingSpawns tracks Spawn() calls in progress (lock released during spawn)
	pendingSpawns int

	storePath  string
	storeDirty bool   // true when sessions changed since last save
	storeGen   uint64 // incremented on each mutation, used to detect concurrent writes

	// knownIDs tracks ALL session IDs ever used by naozhi, including
	// sessions that have been removed/reset/evicted. Used by the
	// discovered-session scanner to match CLI processes to naozhi keys,
	// and as a secondary filter for filesystem-based recent sessions.
	knownIDs      map[string]bool
	knownIDsDirty bool

	// sessionIDToKey is a reverse index from session ID to session key.
	// Used by RegisterForResume for O(1) deduplication instead of O(n) scan.
	// Maintained under r.mu by setSessionIDIndex/clearSessionIDIndex.
	sessionIDToKey map[string]string

	noOutputTimeout time.Duration
	totalTimeout    time.Duration

	onChange func() // called (outside lock) when session list changes

	// historyWg tracks startup history-loading goroutines so Shutdown waits for them.
	historyWg sync.WaitGroup
}

// chatKeyFor strips the last ":agentID" segment from a session key to get the chat key.
func chatKeyFor(key string) string {
	if idx := strings.LastIndexByte(key, ':'); idx >= 0 {
		return key[:idx]
	}
	return key
}

// cliNameDefault returns the CLI display name from the wrapper, or empty if no wrapper.
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
	Wrapper         *cli.Wrapper
	MaxProcs        int
	TTL             time.Duration
	PruneTTL        time.Duration
	Model           string
	ExtraArgs       []string
	Workspace       string
	StorePath       string
	NoOutputTimeout time.Duration
	TotalTimeout    time.Duration
	ClaudeDir       string
}

// NewRouter creates a session router.
func NewRouter(cfg RouterConfig) *Router {
	if cfg.MaxProcs <= 0 {
		cfg.MaxProcs = 3
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 30 * time.Minute
	}
	if cfg.PruneTTL <= 0 {
		cfg.PruneTTL = 72 * time.Hour
	}
	r := &Router{
		sessions:           make(map[string]*ManagedSession),
		sessionsByChat:     make(map[string][]string),
		wrapper:            cfg.Wrapper,
		maxProcs:           cfg.MaxProcs,
		ttl:                cfg.TTL,
		pruneTTL:           cfg.PruneTTL,
		model:              cfg.Model,
		extraArgs:          cfg.ExtraArgs,
		workspace:          cfg.Workspace,
		claudeDir:          cfg.ClaudeDir,
		workspaceOverrides: make(map[string]string),
		storePath:          cfg.StorePath,
		knownIDs:           make(map[string]bool),
		sessionIDToKey:     make(map[string]string),
		noOutputTimeout:    cfg.NoOutputTimeout,
		totalTimeout:       cfg.TotalTimeout,
	}
	r.shutdownCond = sync.NewCond(&r.mu)

	// Load historical session IDs (all IDs ever used by naozhi)
	if loaded := loadKnownIDs(r.storePath); loaded != nil {
		r.knownIDs = loaded
	}

	// Restore sessions from store
	if restored := loadStore(r.storePath); restored != nil {
		for key, entry := range restored {
			s := &ManagedSession{
				Key:            key,
				workspace:      entry.Workspace,
				totalCost:      entry.TotalCost,
				prevSessionIDs: entry.PrevSessionIDs,
				Exempt:         strings.HasPrefix(key, "project:"),
				cliName:        r.cliNameDefault(),
				cliVersion:     r.cliVersionDefault(),
			}
			s.setSessionID(entry.SessionID)
			if entry.LastActive != 0 {
				s.lastActive.Store(entry.LastActive)
			}
			r.sessions[key] = s
			r.indexAdd(key)
			r.trackSessionID(entry.SessionID)
			if entry.SessionID != "" {
				r.sessionIDToKey[entry.SessionID] = key
			}
		}
	}

	// Discover recent sessions from filesystem and register as resumable.
	// This gives the dashboard immediate access to past conversations.
	// Only exclude session IDs that are currently managed (active in store + their chains),
	// so that pruned historical naozhi sessions can reappear as recent sessions.
	if r.claudeDir != "" {
		excludeIDs := make(map[string]bool, len(r.sessions)*3)
		for _, s := range r.sessions {
			if id := s.getSessionID(); id != "" {
				excludeIDs[id] = true
			}
			for _, id := range s.prevSessionIDs {
				excludeIDs[id] = true
			}
		}
		for _, rs := range discovery.RecentSessions(r.claudeDir, 10, 7*24*time.Hour, excludeIDs) {
			if rs.SessionID == "" || len(rs.SessionID) < 8 {
				continue
			}
			cwdKey := rs.SessionID[:8]
			if rs.Workspace != "" {
				cwdKey = strings.ReplaceAll(strings.TrimPrefix(rs.Workspace, "/"), "/", "-")
			}
			key := "local:history:" + sanitizeKeyComponent(cwdKey) + ":general"
			if _, exists := r.sessions[key]; exists {
				slog.Debug("skipping discovered session: key already registered",
					"key", key, "session_id", rs.SessionID)
				continue
			}
			s := &ManagedSession{
				Key:        key,
				workspace:  rs.Workspace,
				cliName:    r.cliNameDefault(),
				cliVersion: r.cliVersionDefault(),
			}
			s.setSessionID(rs.SessionID)
			if rs.LastActive != 0 {
				s.lastActive.Store(rs.LastActive * 1_000_000) // ms → ns
			} else {
				s.lastActive.Store(time.Now().UnixNano())
			}
			r.sessions[key] = s
			r.indexAdd(key)
		}
		r.storeDirty = true
		r.storeGen++
	}

	// Async-load JSONL history for all suspended sessions so the dashboard
	// shows conversation history without waiting for the next message.
	// Loads the full session chain (prev → current) to restore history
	// that accumulated across multiple CLI session IDs.
	if r.claudeDir != "" {
		sem := make(chan struct{}, historyLoadConcurrency) // limit concurrent disk I/O
		for _, s := range r.sessions {
			s := s
			if s.getSessionID() == "" {
				continue
			}
			r.historyWg.Add(1)
			go func() {
				defer r.historyWg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				// Build ordered list of all session IDs: prev chain + current
				ids := make([]string, 0, len(s.prevSessionIDs)+1)
				ids = append(ids, s.prevSessionIDs...)
				ids = append(ids, s.getSessionID())

				var allEntries []cli.EventEntry
				for _, id := range ids {
					entries, err := discovery.LoadHistory(r.claudeDir, id, s.workspace)
					if err != nil || len(entries) == 0 {
						continue
					}
					allEntries = append(allEntries, entries...)
				}
				if len(allEntries) == 0 {
					return
				}
				s.InjectHistory(allEntries)
				slog.Info("loaded session history on startup", "key", s.Key, "entries", len(allEntries), "chain", len(ids))
				r.notifyChange()
			}()
		}
	}

	return r
}

// ReconnectShims discovers surviving shim processes and reconnects sessions.
// Called after NewRouter to restore sessions that were active before naozhi restart.
func (r *Router) ReconnectShims() {
	if r.wrapper == nil || r.wrapper.ShimManager == nil {
		return
	}

	states, err := r.wrapper.ShimManager.Discover()
	if err != nil {
		slog.Warn("shim discovery failed", "err", err)
		return
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
		r.mu.Unlock()

		if !ok {
			slog.Info("orphan shim found, shutting down", "key", state.Key)
			// Connect briefly to send shutdown
			if handle, err := r.wrapper.ShimManager.Reconnect(context.Background(), state.Key, 0); err == nil {
				handle.Shutdown()
			} else {
				syscall.Kill(state.ShimPID, syscall.SIGUSR2) //nolint:errcheck
			}
			continue
		}

		// Skip if session already has a live process
		if hasLiveProcess {
			continue
		}

		// CLI args drift check: if config changed (model, args), shut down old shim
		// and let the next message create a new session with updated config.
		// Strip --resume <id> from stored args since it's session-specific, not config.
		storedBase := stripResumeArgs(state.CLIArgs)
		currentArgs := r.wrapper.Protocol.BuildArgs(cli.SpawnOptions{
			Model:     r.model,
			ExtraArgs: r.extraArgs,
		})
		if len(storedBase) > 0 && !slices.Equal(storedBase, currentArgs) {
			slog.Info("shim config drifted, shutting down old shim",
				"key", state.Key,
				"old_args_len", len(storedBase),
				"new_args_len", len(currentArgs))
			if handle, err := r.wrapper.ShimManager.Reconnect(context.Background(), state.Key, 0); err == nil {
				handle.Shutdown()
			}
			continue
		}

		// Reconnect
		lastSeq := int64(0) // full replay on restart
		proc, replays, err := r.wrapper.SpawnReconnect(
			context.Background(), state.Key, lastSeq, r.wrapper.Protocol,
			r.noOutputTimeout, r.totalTimeout,
		)
		if err != nil {
			slog.Warn("shim reconnect failed", "key", state.Key, "err", err)
			continue
		}

		// Inject replay events into eventLog (not via eventCh — avoids deadlock).
		// Use EventEntryFromEvent for proper Summary extraction across all event types.
		for _, replay := range replays {
			if replay.Type == "replay" {
				ev, _, err := r.wrapper.Protocol.ReadEvent(replay.Line)
				if err != nil || ev.Type == "" {
					continue
				}
				if entry, ok := cli.EventEntryFromEvent(ev); ok {
					proc.InjectHistory([]cli.EventEntry{entry})
				}
			}
		}

		// Inject persisted JSONL history from PREVIOUS sessions only.
		// The current session's events are already covered by the replay above.
		// Injecting both would cause duplicate messages in the dashboard.
		if len(sess.prevSessionIDs) > 0 && r.claudeDir != "" {
			var prevEntries []cli.EventEntry
			for _, id := range sess.prevSessionIDs {
				entries, err := discovery.LoadHistory(r.claudeDir, id, sess.workspace)
				if err != nil || len(entries) == 0 {
					continue
				}
				prevEntries = append(prevEntries, entries...)
			}
			if len(prevEntries) > 0 {
				proc.InjectHistory(prevEntries)
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
		// All three operations are non-blocking (atomic store + func pointer):
		// safe to hold r.mu throughout.
		proc.SetOnTurnDone(func() { r.notifyChange() })
		sess.ReattachProcess(proc, state.SessionID)
		if !sess.Exempt {
			r.activeCount++
		}
		r.storeGen++
		r.mu.Unlock()

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

// SetOnChange registers a callback invoked when the session list changes.
func (r *Router) SetOnChange(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onChange = fn
}

// notifyChange calls the onChange callback if set. Must be called outside r.mu.
func (r *Router) notifyChange() {
	r.mu.RLock()
	fn := r.onChange
	r.mu.RUnlock()
	if fn != nil {
		fn()
	}
}

// NotifyIdle wakes the Shutdown wait loop so it can re-check running sessions.
// Call this after a message send completes (session transitions from running to ready).
// Broadcast does not require the associated lock to be held.
func (r *Router) NotifyIdle() {
	if r.shutdownCond != nil {
		r.shutdownCond.Broadcast()
	}
}

// ChatKey builds a chat-level key (without agent suffix) for workspace overrides.
func ChatKey(platform, chatType, chatID string) string {
	return platform + ":" + chatType + ":" + chatID
}

// SetWorkspace sets the working directory override for a chat.
func (r *Router) SetWorkspace(chatKey, path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.workspaceOverrides[chatKey] = path
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
				if !s.Exempt {
					closedActive++
				}
			}
			if id := s.getSessionID(); id != "" {
				delete(r.sessionIDToKey, id)
			}
			delete(r.sessions, key)
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
					if !s.Exempt {
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
		}
	}
	r.activeCount -= closedActive
	if r.activeCount < 0 {
		r.activeCount = 0
	}
	delete(r.workspaceOverrides, chatKeyPrefix)
	r.storeDirty = true
	r.storeGen++
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
			return nil, 0, err
		}
		return s, SessionResumed, nil
	}

	slog.Info("creating new session", "key", key)
	s, err := r.spawnSession(ctx, key, "", opts)
	if err != nil {
		return nil, 0, err
	}
	return s, SessionNew, nil
}

// spawnSession creates a new process, optionally resuming an existing session.
// Caller must hold r.mu. Releases r.mu during Spawn() to avoid blocking other
// goroutines during potentially slow protocol init (e.g., ACP handshake).
func (r *Router) spawnSession(ctx context.Context, key string, resumeID string, opts AgentOpts) (*ManagedSession, error) {
	// Exempt sessions (planners) bypass maxProcs capacity check but have their own limit
	if !opts.Exempt {
		// Recount to correct drift from undetected process exits (OOM, SIGKILL)
		r.countActive()
		if r.activeCount+r.pendingSpawns >= r.maxProcs {
			if !r.evictOldest() {
				r.mu.Unlock()
				return nil, fmt.Errorf("%w (%d), all busy", ErrMaxProcs, r.maxProcs)
			}
			if r.activeCount+r.pendingSpawns >= r.maxProcs {
				r.mu.Unlock()
				return nil, fmt.Errorf("%w (%d), all busy", ErrMaxProcs, r.maxProcs)
			}
		}
	} else {
		// Guard against unbounded exempt session growth (e.g., many projects)
		const maxExempt = maxExemptSessions
		exemptCount := r.countExempt()
		if exemptCount >= maxExempt {
			r.mu.Unlock()
			return nil, fmt.Errorf("max exempt sessions reached (%d)", maxExempt)
		}
	}

	// Merge agent opts with router defaults
	model := r.model
	if opts.Model != "" {
		model = opts.Model
	}
	args := make([]string, len(r.extraArgs))
	copy(args, r.extraArgs)
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
	// When resuming after restart, workspaceOverrides is empty (not persisted across restarts).
	// Fall back to the old session's stored workspace so --resume finds the session in the
	// correct project directory (Claude stores sessions under ~/.claude/projects/<sha256(cwd)>/).
	if !workspaceOverridden && resumeID != "" {
		if old := r.sessions[key]; old != nil && old.workspace != "" {
			workspace = old.workspace
		}
	}

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
	if r.wrapper == nil {
		r.mu.Lock()
		r.pendingSpawns--
		r.mu.Unlock()
		return nil, fmt.Errorf("spawn process: no CLI wrapper configured")
	}
	proc, err := r.wrapper.Spawn(ctx, spawnOpts)
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
			prevIDs = make([]string, len(old.prevSessionIDs), len(old.prevSessionIDs)+1)
			copy(prevIDs, old.prevSessionIDs)
			prevIDs = append(prevIDs, oldID)
		} else if len(old.prevSessionIDs) > 0 {
			prevIDs = make([]string, len(old.prevSessionIDs))
			copy(prevIDs, old.prevSessionIDs)
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
		Key:              key,
		workspace:        workspace,
		cliName:          r.wrapper.CLIName,
		cliVersion:       r.wrapper.CLIVersion,
		persistedHistory: oldHistory,
		prevSessionIDs:   prevIDs,
		Exempt:           opts.Exempt,
		onSessionID: func(id string) {
			r.mu.Lock()
			r.trackSessionID(id)
			if id != "" {
				r.sessionIDToKey[id] = key
			}
			r.mu.Unlock()
		},
	}
	s.storeProcess(proc)
	if len(oldHistory) > 0 {
		proc.InjectHistory(oldHistory)
	}
	s.setSessionID(resumeID)
	r.trackSessionID(resumeID)
	if resumeID != "" {
		r.sessionIDToKey[resumeID] = key
	}
	s.touchLastActive()
	r.sessions[key] = s
	r.indexAdd(key)
	if !opts.Exempt {
		r.activeCount++
	}

	r.storeDirty = true
	r.storeGen++
	slog.Info("session spawned", "key", key, "active", r.activeCount, "exempt", opts.Exempt)
	r.mu.Unlock()

	// Load conversation history from Claude's local JSONL when resuming.
	// This restores dashboard event display after service restarts.
	// Load the full chain (prev IDs + resume ID) to recover history
	// that accumulated across multiple CLI session IDs.
	if resumeID != "" && r.claudeDir != "" && len(oldHistory) == 0 {
		ids := make([]string, 0, len(prevIDs)+1)
		ids = append(ids, prevIDs...)
		ids = append(ids, resumeID)
		var allEntries []cli.EventEntry
		for _, id := range ids {
			if entries, err := discovery.LoadHistory(r.claudeDir, id, workspace); err == nil && len(entries) > 0 {
				allEntries = append(allEntries, entries...)
			}
		}
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
	count := 0
	for _, s := range r.sessions {
		if s.Exempt {
			continue
		}
		if s.isAlive() {
			count++
		}
	}
	r.activeCount = count
}

// countExempt returns the number of alive exempt (planner) sessions.
// Caller must hold r.mu.
func (r *Router) countExempt() int {
	count := 0
	for _, s := range r.sessions {
		if s.Exempt && s.isAlive() {
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
		if s.Exempt {
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
	slog.Info("evicting oldest session", "key", oldest.Key, "idle", time.Since(oldest.GetLastActive()))
	oldest.deathReason.Store("evicted")
	// Keep oldest.process non-nil so concurrent holders don't get nil-panic.
	// After Close(), Alive() returns false; countActive() below recounts correctly.
	proc := oldest.loadProcess()
	r.mu.Unlock()
	proc.Close()
	if r.shutdownCond != nil {
		r.shutdownCond.Broadcast()
	}
	r.mu.Lock()
	r.countActive() // recount instead of manual decrement to avoid double-count races
	return true
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
	wasActive := !s.Exempt && proc != nil && proc.Alive()
	if id := s.getSessionID(); id != "" {
		delete(r.sessionIDToKey, id)
	}
	r.indexDel(key)
	delete(r.sessions, key)
	if wasActive {
		r.activeCount--
		if r.activeCount < 0 {
			r.activeCount = 0
		}
	}
	r.storeDirty = true
	r.storeGen++
	r.mu.Unlock()

	if proc != nil && proc.Alive() {
		proc.Close()
	}
	if r.shutdownCond != nil {
		r.shutdownCond.Broadcast()
	}

	slog.Info("session reset", "key", key)
	r.notifyChange()
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
		wasActive := !s.Exempt && proc != nil && proc.Alive()
		if id := s.getSessionID(); id != "" {
			delete(r.sessionIDToKey, id)
		}
		r.indexDel(key)
		delete(r.sessions, key)
		if wasActive {
			r.activeCount--
			if r.activeCount < 0 {
				r.activeCount = 0
			}
		}
		r.storeDirty = true
		r.storeGen++

		if proc != nil && proc.Alive() {
			r.mu.Unlock()
			proc.Close()
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
	wasActive := !s.Exempt && proc != nil && proc.Alive()
	if id := s.getSessionID(); id != "" {
		delete(r.sessionIDToKey, id)
	}
	r.indexDel(key)
	delete(r.sessions, key)
	if wasActive {
		r.activeCount--
		if r.activeCount < 0 {
			r.activeCount = 0
		}
	}
	r.storeDirty = true
	r.storeGen++
	r.mu.Unlock()

	if proc != nil && proc.Alive() {
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
// Releases r.mu during Close()/Kill() to avoid blocking message processing.
func (r *Router) Cleanup() {
	r.mu.Lock()

	type expiredEntry struct {
		key  string
		proc processIface
	}
	var expired []expiredEntry
	var stuckKill []expiredEntry // stuck running or PID-dead — force kill

	now := time.Now()
	for key, s := range r.sessions {
		if s.Exempt {
			continue // planner sessions are never expired by TTL
		}
		proc := s.loadProcess()
		if proc == nil {
			continue
		}
		alive := proc.Alive()

		// ── Stuck running detection ─────────────────────────────────

		// If a session has been in StateRunning longer than 2× the
		// process total-timeout, the Send() watchdog failed to fire.
		// Force-kill to reclaim the slot.
		if alive && proc.IsRunning() {
			totalTimeout := r.totalTimeout
			if totalTimeout <= 0 {
				totalTimeout = cli.DefaultTotalTimeout
			}
			stuckThreshold := 2 * totalTimeout
			if age := now.Sub(s.GetLastActive()); age > stuckThreshold {
				slog.Warn("stuck running session detected, force killing",
					"key", key, "running_for", age, "threshold", stuckThreshold)
				s.deathReason.Store("stuck_running")
				stuckKill = append(stuckKill, expiredEntry{key, proc})
				continue
			}
		}

		// ── PID liveness check ──────────────────────────────────────
		// The shim connection may appear alive (readLoop not yet EOF)
		// while the underlying CLI process has already exited.
		// Verify via kill(pid, 0) and force-kill the stale shim.
		if alive && !proc.IsRunning() {
			if pid := proc.PID(); pid > 0 && !osutil.PidAlive(pid) {
				slog.Warn("CLI process gone but session still alive, force killing",
					"key", key, "pid", pid)
				s.deathReason.Store("pid_gone")
				stuckKill = append(stuckKill, expiredEntry{key, proc})
				continue
			}
		}

		// ── Normal idle TTL expiry ──────────────────────────────────
		if alive && !proc.IsRunning() && now.Sub(s.GetLastActive()) > r.ttl {
			slog.Info("session expired", "key", key, "idle", now.Sub(s.GetLastActive()))
			s.deathReason.Store("idle_timeout")
			expired = append(expired, expiredEntry{key, proc})
		}
	}
	r.mu.Unlock()

	closedCount := 0
	for _, e := range stuckKill {
		e.proc.Kill()
		closedCount++
	}
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
	var newActive int
	for key, s := range r.sessions {
		if s.Exempt {
			continue // planner sessions are never pruned
		}
		sid := s.getSessionID()
		if s.loadProcess() == nil && sid == "" && now.Sub(s.GetLastActive()) > r.pruneTTL {
			r.indexDel(key)
			delete(r.sessions, key)
			pruned++
			continue
		}
		// Prune resume stubs: nil process with session ID, past prune TTL.
		if s.loadProcess() == nil && sid != "" && now.Sub(s.GetLastActive()) > r.pruneTTL {
			delete(r.sessionIDToKey, sid)
			r.indexDel(key)
			delete(r.sessions, key)
			pruned++
			continue
		}
		// Prune exited sessions with no resumable session ID
		if p := s.loadProcess(); p != nil && !p.Alive() && sid == "" && now.Sub(s.GetLastActive()) > r.pruneTTL {
			r.indexDel(key)
			delete(r.sessions, key)
			pruned++
			continue
		}
		// Prune old exited sessions even with session ID (prevents unbounded growth).
		if p := s.loadProcess(); p != nil && !p.Alive() && now.Sub(s.GetLastActive()) > r.pruneTTL {
			if sid != "" {
				delete(r.sessionIDToKey, sid)
			}
			r.indexDel(key)
			delete(r.sessions, key)
			pruned++
			continue
		}
		// Session survived all prune conditions; count it if its process is alive.
		if s.isAlive() {
			newActive++
		}
	}
	r.activeCount = newActive

	// Snapshot sessions for periodic save (while still holding the lock).
	// Skip save if nothing changed since last Cleanup cycle.
	if closedCount > 0 || pruned > 0 {
		r.storeDirty = true
		r.storeGen++
	}
	var sessionsCopy map[string]*ManagedSession
	var knownIDsCopy map[string]bool
	storePath := r.storePath
	snapshotGen := r.storeGen
	if r.storeDirty {
		sessionsCopy = make(map[string]*ManagedSession, len(r.sessions))
		for k, v := range r.sessions {
			sessionsCopy[k] = v
		}
	}
	if r.knownIDsDirty {
		knownIDsCopy = make(map[string]bool, len(r.knownIDs))
		for id := range r.knownIDs {
			knownIDsCopy[id] = true
		}
	}

	r.mu.Unlock()

	// Periodic save outside lock to reduce crash-recovery data loss.
	if sessionsCopy != nil {
		if err := saveStore(storePath, sessionsCopy); err != nil {
			slog.Warn("periodic session save failed", "err", err)
		} else {
			// Only clear dirty flag if no concurrent mutation occurred since snapshot.
			r.mu.Lock()
			if r.storeGen == snapshotGen {
				r.storeDirty = false
			}
			r.mu.Unlock()
		}
	}
	if knownIDsCopy != nil {
		if err := saveKnownIDs(storePath, knownIDsCopy); err != nil {
			slog.Warn("periodic known IDs save failed", "err", err)
		} else {
			// Only clear dirty flag if no concurrent trackSessionID added new IDs.
			// knownIDs is append-only, so length comparison is sufficient.
			r.mu.Lock()
			if len(r.knownIDs) == len(knownIDsCopy) {
				r.knownIDsDirty = false
			}
			r.mu.Unlock()
		}
	}

	if len(expired) > 0 || len(stuckKill) > 0 || pruned > 0 {
		r.notifyChange()
	}
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
func (r *Router) saveIfDirty() {
	r.mu.Lock()
	if !r.storeDirty {
		r.mu.Unlock()
		return
	}
	sessionsCopy := make(map[string]*ManagedSession, len(r.sessions))
	for k, v := range r.sessions {
		sessionsCopy[k] = v
	}
	storePath := r.storePath
	snapshotGen := r.storeGen
	r.mu.Unlock()

	if err := saveStore(storePath, sessionsCopy); err != nil {
		slog.Warn("periodic session save failed", "err", err)
		return
	}
	r.mu.Lock()
	if r.storeGen == snapshotGen {
		r.storeDirty = false
	}
	r.mu.Unlock()
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
				r.ReconnectShims()
			}
		}
	}()
}

// Shutdown gracefully closes all sessions, waiting for running ones to complete.
func (r *Router) Shutdown() {
	// Wait for startup history-loading goroutines to finish first,
	// but don't block forever if filesystem I/O is hung (e.g. NFS).
	historyDone := make(chan struct{})
	go func() {
		// Goroutine intentionally left running on timeout; cleaned up on process exit.
		r.historyWg.Wait()
		close(historyDone)
	}()
	select {
	case <-historyDone:
	case <-time.After(15 * time.Second):
		slog.Warn("shutdown: history loading timed out after 15s, proceeding")
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

	// Collect processes to close, then release lock to close concurrently
	var procs []processIface
	for key, s := range r.sessions {
		if p := s.loadProcess(); p != nil && p.Alive() {
			slog.Info("shutting down session", "key", key)
			procs = append(procs, p)
		}
	}
	r.mu.Unlock()

	// Save session state outside lock (avoids JSON marshal + file I/O under mutex)
	if err := saveStore(storePath, sessionsCopy); err != nil {
		slog.Error("save session store on shutdown", "err", err)
	}
	if err := saveKnownIDs(storePath, knownIDsCopy); err != nil {
		slog.Error("save known session IDs on shutdown", "err", err)
	}

	// Detach shim processes (keep them alive for reconnect after restart)
	// instead of Close (which would kill the CLI).
	var wg sync.WaitGroup
	for _, proc := range procs {
		wg.Add(1)
		go func(p processIface) {
			defer wg.Done()
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
func stripResumeArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--resume" && i+1 < len(args) {
			i++ // skip --resume and its value
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// Version returns a monotonic counter incremented on every session mutation.
// Used by the dashboard for efficient change detection without full JSON comparison.
func (r *Router) Version() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.storeGen
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
func (r *Router) Stats() (active, total int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.activeCount, len(r.sessions)
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

// InterruptSession sends SIGINT to the CLI process for the given session key.
// Returns true if the session was found and interrupted.
func (r *Router) InterruptSession(key string) bool {
	r.mu.RLock()
	s := r.sessions[key]
	r.mu.RUnlock()
	if s == nil {
		return false
	}
	return s.Interrupt()
}

// ActiveSessionIDs returns the set of session IDs currently managed by the router,
// including their full session chains. Pruned historical sessions are NOT included,
// allowing them to reappear as resumable recent sessions in the dashboard.
func (r *Router) ActiveSessionIDs() map[string]bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make(map[string]bool, len(r.sessions)*3)
	for _, s := range r.sessions {
		if id := s.getSessionID(); id != "" {
			ids[id] = true
		}
		for _, id := range s.prevSessionIDs {
			ids[id] = true
		}
	}
	return ids
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
// When the set exceeds maxKnownIDs, older entries are evicted (random eviction
// is acceptable since this set is only used for heuristic dedup in discovery).
func (r *Router) trackSessionID(id string) {
	if id == "" {
		return
	}
	if r.knownIDs[id] {
		return
	}
	// Evict random entries when at capacity (go map iteration is random).
	if len(r.knownIDs) >= maxKnownIDs {
		toEvict := len(r.knownIDs) - maxKnownIDs + 1
		for k := range r.knownIDs {
			delete(r.knownIDs, k)
			toEvict--
			if toEvict <= 0 {
				break
			}
		}
	}
	r.knownIDs[id] = true
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
		Key:        key,
		workspace:  workspace,
		cliName:    r.cliNameDefault(),
		cliVersion: r.cliVersionDefault(),
	}
	s.setSessionID(sessionID)
	if lastPrompt != "" {
		s.lastPrompt.Store(lastPrompt)
	}
	r.trackSessionID(sessionID)
	if sessionID != "" {
		r.sessionIDToKey[sessionID] = key
	}
	s.lastActive.Store(time.Now().UnixNano())
	r.sessions[key] = s
	r.indexAdd(key)
	r.storeDirty = true
	r.storeGen++
	r.mu.Unlock()

	r.notifyChange()
	return key
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
			if s.workspace != "" {
				cwds[s.workspace] = true
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
	r.mu.Lock()
	// If key already exists (e.g. re-takeover same CWD), close the old process
	if s, ok := r.sessions[key]; ok {
		if p := s.loadProcess(); p != nil && p.Alive() {
			oldSession := s
			proc := p
			r.mu.Unlock()
			proc.Close()
			r.mu.Lock()
			// Only delete if no concurrent goroutine replaced this session
			if cur, ok := r.sessions[key]; ok && cur == oldSession {
				if id := cur.getSessionID(); id != "" {
					delete(r.sessionIDToKey, id)
				}
				r.indexDel(key)
				delete(r.sessions, key)
				r.storeDirty = true
				r.storeGen++
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
			if id := s.getSessionID(); id != "" {
				delete(r.sessionIDToKey, id)
			}
			r.indexDel(key)
			delete(r.sessions, key)
			r.storeDirty = true
			r.storeGen++
		}
		r.countActive()
	}
	// Set workspace override for the chat key prefix
	if chatKey := chatKeyFor(key); chatKey != key {
		r.workspaceOverrides[chatKey] = workspace
	}
	s, err := r.spawnSession(ctx, key, sessionID, opts)
	if err != nil {
		return nil, err
	}
	return s, nil
}
