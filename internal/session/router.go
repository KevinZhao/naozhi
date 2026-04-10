package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/discovery"
)

// ShutdownTimeout is the maximum time to wait for graceful shutdown
// of running sessions (Router) and HTTP connections (Server).
// Exported so both session and server packages use a single value.
const ShutdownTimeout = 30 * time.Second

// ErrMaxProcs is returned when all process slots are occupied.
var ErrMaxProcs = errors.New("max concurrent processes reached")

// Router manages session key -> ManagedSession mapping.
type Router struct {
	mu           sync.Mutex
	shutdownCond *sync.Cond // signaled when process state changes; conditioned on mu
	sessions     map[string]*ManagedSession
	// sessionsByChat is a secondary index: chat key → session keys.
	// Enables O(k) ResetChat instead of O(n) full scan (k = agents per chat, typically 1-3).
	// Nil in test-created routers; all helpers below are nil-safe.
	sessionsByChat map[string][]string
	wrapper        *cli.Wrapper
	maxProcs       int
	ttl            time.Duration
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
	r := &Router{
		sessions:           make(map[string]*ManagedSession),
		sessionsByChat:     make(map[string][]string),
		wrapper:            cfg.Wrapper,
		maxProcs:           cfg.MaxProcs,
		ttl:                cfg.TTL,
		model:              cfg.Model,
		extraArgs:          cfg.ExtraArgs,
		workspace:          cfg.Workspace,
		claudeDir:          cfg.ClaudeDir,
		workspaceOverrides: make(map[string]string),
		storePath:          cfg.StorePath,
		knownIDs:           make(map[string]bool),
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
			}
			s.setSessionID(entry.SessionID)
			if entry.LastActive != 0 {
				s.lastActive.Store(entry.LastActive)
			}
			r.sessions[key] = s
			r.indexAdd(key)
			r.trackSessionID(entry.SessionID)
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
				Key:       key,
				workspace: rs.Workspace,
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
		sem := make(chan struct{}, 10) // limit concurrent disk I/O
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

// SetOnChange registers a callback invoked when the session list changes.
func (r *Router) SetOnChange(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onChange = fn
}

// notifyChange calls the onChange callback if set. Must be called outside r.mu.
func (r *Router) notifyChange() {
	r.mu.Lock()
	fn := r.onChange
	r.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// NotifyIdle wakes the Shutdown wait loop so it can re-check running sessions.
// Call this after a message send completes (session transitions from running to ready).
func (r *Router) NotifyIdle() {
	if r.shutdownCond != nil {
		r.shutdownCond.L.Lock()
		r.shutdownCond.Broadcast()
		r.shutdownCond.L.Unlock()
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
	r.mu.Lock()
	defer r.mu.Unlock()
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
			if s.process != nil && s.process.Alive() {
				toClose = append(toClose, s.process)
				if !s.Exempt {
					closedActive++
				}
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
				if s.process != nil && s.process.Alive() {
					toClose = append(toClose, s.process)
					if !s.Exempt {
						closedActive++
					}
				}
			}
		}
		for _, key := range toDelete {
			delete(r.sessions, key)
		}
	}
	r.activeCount -= closedActive
	if r.activeCount < 0 {
		r.activeCount = 0
	}
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
		if s.process != nil && s.process.Alive() {
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
		const maxExempt = 20
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
		Model:           model,
		ResumeID:        resumeID,
		ExtraArgs:       args,
		WorkingDir:      workspace,
		NoOutputTimeout: r.noOutputTimeout,
		TotalTimeout:    r.totalTimeout,
	}

	// Release lock during Spawn (may block on ACP Init handshake)
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

	// TOCTOU guard: another goroutine may have spawned this key while we were unlocked
	if existing, ok := r.sessions[key]; ok && existing.process != nil && existing.process.Alive() {
		r.mu.Unlock()
		proc.Close() // discard the redundant process
		return existing, nil
	}

	// Get old session reference, then release r.mu to copy history under sendMu only
	old := r.sessions[key]
	r.mu.Unlock()

	var oldHistory []cli.EventEntry
	var prevIDs []string
	if old != nil {
		old.sendMu.Lock()
		if old.process != nil && !old.process.Alive() {
			// Dead process: EventEntries() includes both injected history and live events
			// logged during the last run. Use this instead of persistedHistory, which only
			// holds the JSONL-loaded snapshot and misses events accumulated since that load.
			oldHistory = old.process.EventEntries()
		} else if len(old.persistedHistory) > 0 {
			oldHistory = make([]cli.EventEntry, len(old.persistedHistory))
			copy(oldHistory, old.persistedHistory)
		}
		old.sendMu.Unlock()

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
	// Re-check TOCTOU after re-acquiring lock (another goroutine may have spawned)
	if existing, ok := r.sessions[key]; ok && existing.process != nil && existing.process.Alive() {
		r.mu.Unlock()
		proc.Close()
		return existing, nil
	}

	s := &ManagedSession{
		Key:              key,
		process:          proc,
		workspace:        workspace,
		cliName:          r.wrapper.CLIName,
		cliVersion:       r.wrapper.CLIVersion,
		persistedHistory: oldHistory,
		prevSessionIDs:   prevIDs,
		Exempt:           opts.Exempt,
		onSessionID: func(id string) {
			r.mu.Lock()
			r.trackSessionID(id)
			r.mu.Unlock()
		},
	}
	if len(oldHistory) > 0 {
		proc.InjectHistory(oldHistory)
	}
	s.setSessionID(resumeID)
	r.trackSessionID(resumeID)
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
		if s.process != nil && s.process.Alive() {
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
		if s.Exempt && s.process != nil && s.process.Alive() {
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
		if s.process == nil || !s.process.Alive() || s.process.IsRunning() {
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
	proc := oldest.process
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

	wasActive := !s.Exempt && s.process != nil && s.process.Alive()
	proc := s.process
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
		wasActive := !s.Exempt && s.process != nil && s.process.Alive()
		proc := s.process
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

	wasActive := !s.Exempt && s.process != nil && s.process.Alive()
	// Kill process if alive
	proc := s.process
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
// Releases r.mu during Close() to avoid blocking message processing.
func (r *Router) Cleanup() {
	r.mu.Lock()

	type expiredEntry struct {
		key  string
		proc processIface
	}
	var expired []expiredEntry

	now := time.Now()
	for key, s := range r.sessions {
		if s.Exempt {
			continue // planner sessions are never expired by TTL
		}
		if s.process != nil && s.process.Alive() && !s.process.IsRunning() && now.Sub(s.GetLastActive()) > r.ttl {
			slog.Info("session expired", "key", key, "idle", now.Sub(s.GetLastActive()))
			s.deathReason.Store("idle_timeout")
			expired = append(expired, expiredEntry{key, s.process})
		}
	}
	r.mu.Unlock()

	closedCount := 0
	for _, e := range expired {
		e.proc.Close()
		closedCount++
	}
	if r.shutdownCond != nil {
		r.shutdownCond.Broadcast()
	}

	r.mu.Lock()
	// Prune orphaned sessions: nil process, no session ID, past TTL.
	// Maintain a running newActive counter so we avoid a separate countActive() O(n) pass.
	var pruned int
	var newActive int
	for key, s := range r.sessions {
		if s.Exempt {
			continue // planner sessions are never pruned
		}
		if s.process == nil && s.getSessionID() == "" && now.Sub(s.GetLastActive()) > r.ttl {
			r.indexDel(key)
			delete(r.sessions, key)
			pruned++
			continue
		}
		// Prune resume stubs: nil process with session ID, past 7*TTL.
		// 7x multiplier gives suspended sessions ~3.5 hours at default 30m TTL,
		// matching a typical work session. Balances timely cleanup against
		// preserving sessions the user may still want to --resume.
		if s.process == nil && s.getSessionID() != "" && now.Sub(s.GetLastActive()) > 7*r.ttl {
			r.indexDel(key)
			delete(r.sessions, key)
			pruned++
			continue
		}
		// Prune exited sessions with no resumable session ID
		if s.process != nil && !s.process.Alive() && s.getSessionID() == "" && now.Sub(s.GetLastActive()) > r.ttl {
			r.indexDel(key)
			delete(r.sessions, key)
			pruned++
			continue
		}
		// Prune old exited sessions even with session ID (prevents unbounded growth).
		// Same 7x TTL window as suspended sessions above.
		if s.process != nil && !s.process.Alive() && now.Sub(s.GetLastActive()) > 7*r.ttl {
			r.indexDel(key)
			delete(r.sessions, key)
			pruned++
			continue
		}
		// Session survived all prune conditions; count it if its process is alive.
		if s.process != nil && s.process.Alive() {
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

	if len(expired) > 0 || pruned > 0 {
		r.notifyChange()
	}
}

// StartCleanupLoop runs Cleanup periodically.
func (r *Router) StartCleanupLoop(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.Cleanup()
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
			if s.process != nil && s.process.IsRunning() {
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
		if s.process != nil && s.process.Alive() {
			slog.Info("shutting down session", "key", key)
			procs = append(procs, s.process)
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

	var wg sync.WaitGroup
	for _, proc := range procs {
		wg.Add(1)
		go func(p processIface) {
			defer wg.Done()
			p.Close()
		}(proc)
	}
	wg.Wait()
}

// DefaultWorkspace returns the router's default working directory.
func (r *Router) DefaultWorkspace() string {
	return r.workspace
}

// Version returns a monotonic counter incremented on every session mutation.
// Used by the dashboard for efficient change detection without full JSON comparison.
func (r *Router) Version() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
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
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.activeCount, len(r.sessions)
}

// ListSessions returns a snapshot of all sessions for the dashboard.
// Collects references under r.mu, then releases before snapshotting
// to avoid blocking the router while getSessionID() waits on sendMu.
func (r *Router) ListSessions() []SessionSnapshot {
	r.mu.Lock()
	refs := make([]*ManagedSession, 0, len(r.sessions))
	for _, s := range r.sessions {
		refs = append(refs, s)
	}
	r.mu.Unlock()

	snapshots := make([]SessionSnapshot, len(refs))
	for i, s := range refs {
		snapshots[i] = s.Snapshot()
	}
	return snapshots
}

// GetSession returns the session for the given key, or nil.
func (r *Router) GetSession(key string) *ManagedSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[key]
}

// InterruptSession sends SIGINT to the CLI process for the given session key.
// Returns true if the session was found and interrupted.
func (r *Router) InterruptSession(key string) bool {
	r.mu.Lock()
	s := r.sessions[key]
	r.mu.Unlock()
	if s == nil {
		return false
	}
	return s.Interrupt()
}

// ActiveSessionIDs returns the set of session IDs currently managed by the router,
// including their full session chains. Pruned historical sessions are NOT included,
// allowing them to reappear as resumable recent sessions in the dashboard.
func (r *Router) ActiveSessionIDs() map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
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
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make(map[string]bool, len(r.sessions))
	for _, s := range r.sessions {
		if s.process == nil {
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

// trackSessionID adds a session ID to the persistent known-IDs set.
// Caller must hold r.mu OR call before any concurrent access (e.g. NewRouter init).
func (r *Router) trackSessionID(id string) {
	if id == "" {
		return
	}
	if r.knownIDs[id] {
		return
	}
	r.knownIDs[id] = true
	r.knownIDsDirty = true
}

// RegisterForResume creates a suspended session entry so that the next
// GetOrCreate call for this key will resume the given session ID.
// If another session already targets the same sessionID, the existing key
// is returned (deduplication) and no new entry is created.
func (r *Router) RegisterForResume(key, sessionID, workspace string) (effectiveKey string) {
	r.mu.Lock()
	if _, exists := r.sessions[key]; exists {
		r.mu.Unlock()
		return key // already exists with this exact key
	}
	// Deduplicate: if another session already targets this sessionID, reuse it.
	for k, s := range r.sessions {
		if s.getSessionID() == sessionID {
			r.mu.Unlock()
			return k
		}
	}
	s := &ManagedSession{
		Key:       key,
		workspace: workspace,
	}
	s.setSessionID(sessionID)
	r.trackSessionID(sessionID)
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
	r.mu.Lock()
	defer r.mu.Unlock()
	pids = make(map[int]bool)
	sessionIDs = make(map[string]bool)
	cwds = make(map[string]bool)
	for _, s := range r.sessions {
		if id := s.getSessionID(); id != "" {
			sessionIDs[id] = true
		}
		if s.process != nil && s.process.Alive() {
			if pid := s.process.PID(); pid > 0 {
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
		if s.process != nil && s.process.Alive() {
			oldSession := s
			proc := s.process
			r.mu.Unlock()
			proc.Close()
			r.mu.Lock()
			// Only delete if no concurrent goroutine replaced this session
			if cur, ok := r.sessions[key]; ok && cur == oldSession {
				r.indexDel(key)
				delete(r.sessions, key)
				r.storeDirty = true
				r.storeGen++
			} else if cur != nil && cur.process != nil && cur.process.Alive() {
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
