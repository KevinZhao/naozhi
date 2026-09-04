package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// agentHexRe whitelists the hex component of an agent-<hex>.jsonl filename
// (CLI emits 17 chars; 8-64 accepted) and refuses path separators, dots and
// metacharacters before the value reaches filepath.Join — defence in depth
// against cross-platform traversal corner cases.
var agentHexRe = regexp.MustCompile(`^[A-Za-z0-9]{8,64}$`)

// maxConcurrentResolves caps concurrent Resolve calls per SubagentLinker.
// Each Resolve may sleep up to retryLimit*retryInterval (3 s) waiting for the
// CLI to flush its jsonl, and a multi-agent turn emits 10+ task_started in a
// burst.
const maxConcurrentResolves = 8

// resolveWorkerCount and resolveQueueDepth size DispatchResolve's worker pool
// (#415). 4 workers stay under the resolveSem cap of 8; a depth of 16 soaks
// bursty task_started replays on shim reconnect. A full queue falls back to an
// inline Resolve with a warning so no task_started is dropped.
const (
	resolveWorkerCount = 4
	resolveQueueDepth  = 16
)

// resolveJob carries one Resolve invocation across the dispatch queue.
type resolveJob struct {
	ctx              context.Context
	taskID           string
	toolUseID        string
	name             string
	description      string
	agentToolUseTime int64
}

// staleAgentReuseSlack is the lookback grace applied when comparing an agent
// jsonl's first-row timestamp to the parent Agent tool_use: a row older than
// this is a stale same-name reuse from a prior turn. The CLI flushes within
// ~500 ms; 10 s absorbs timestamp skew without admitting the previous turn.
const staleAgentReuseSlack = 10 * time.Second

// maxMetaCacheEntries bounds the per-Resolve path→firstLineMeta cache; a
// directory with hundreds of stale agent files would otherwise grow it across
// retry attempts. The cache is cleared when exceeded.
const maxMetaCacheEntries = 256

// maxNamedLinkHistory caps LinkInfo records retained per agent name in byName,
// which is consulted only for same-name respawn detection (recent
// FirstPromptIDs suffice). Unbounded, a long session re-invoking the same
// agent type would leak one LinkInfo per task.
const maxNamedLinkHistory = 32

// appendNamedLink appends info to byName[name], keeping at most the newest
// maxNamedLinkHistory entries. Caller must hold l.mu as a write lock.
func (l *SubagentLinker) appendNamedLink(name string, info LinkInfo) {
	cur := l.byName[name]
	cur = append(cur, info)
	if len(cur) > maxNamedLinkHistory {
		// Re-allocate when the backing array ballooned so the oversized
		// store can be collected; otherwise copy down in place.
		if cap(cur) > maxNamedLinkHistory*2 {
			trimmed := make([]LinkInfo, maxNamedLinkHistory)
			copy(trimmed, cur[len(cur)-maxNamedLinkHistory:])
			cur = trimmed
		} else {
			n := copy(cur, cur[len(cur)-maxNamedLinkHistory:])
			cur = cur[:n]
		}
	}
	l.byName[name] = cur
}

// SubagentLinker maps agent task_ids (and their originating Agent
// tool_use_ids) to the transcript jsonl Claude CLI writes under
// <projectDir>/<sessionID>/subagents/agent-<hex>.jsonl, so the dashboard can
// render each agent's internal event stream. Resolve is async: the CLI emits
// system.task_started immediately but flushes the first jsonl row 0-500 ms
// later, so Resolve retries within a bounded grace window (default 3 s) and
// the OnResolve callbacks then start the tailer and backfill
// EventEntry.InternalAgentID for persistence.
type SubagentLinker struct {
	mu              sync.RWMutex
	byTaskID        map[string]LinkInfo
	byToolUseID     map[string]LinkInfo
	byName          map[string][]LinkInfo
	projectDir      string
	parentSessionID string

	dirCache struct {
		at      time.Time
		entries []metaEntry
	}

	onResolveMu  sync.Mutex
	onResolveFns []func(taskID, toolUseID, internalAgentID string)

	// resolveSem is a counting semaphore bounding concurrent Resolve calls.
	resolveSem chan struct{}

	// resolveJobs feeds the worker pool started lazily by DispatchResolve;
	// nil until the first call.
	resolveJobs chan resolveJob

	// resolvePoolOnce guards pool creation. Workers live on poolCtx, never
	// the per-request ctx of the first dispatcher — a short-lived ctx would
	// otherwise cancel all workers and leave later jobs unconsumed (#1661).
	resolvePoolOnce sync.Once

	// poolCtx governs worker-pool lifetime (SetPoolContext with
	// Process.lifecycleContext()). Unset in bare test fixtures, in which case
	// DispatchResolve falls back to the first caller's ctx.
	poolCtx context.Context

	// inflightTasks holds taskIDs with a Resolve already running so callers
	// can skip spawning duplicates for replayed task_started events (#1354).
	// sync.Map: one write per unique task_id, a read per task_started.
	inflightTasks sync.Map // map[taskID]struct{}

	// Tunable via tests. Defaults: 250ms * 12 = 3s grace; 200ms dir cache.
	retryInterval time.Duration
	retryLimit    int
	cacheTTL      time.Duration

	// scanHook fires after every rawScan (test-only cache hit/miss counting).
	scanHook func()

	// readMetaHook fires on every readFirstLineMeta cache miss in the retry
	// loop (test-only); nil in production.
	readMetaHook func()
}

// LinkInfo is the resolved mapping for a single agent task. Zero value is the
// "unknown task" sentinel. An entry with Resolved=true + InternalAgentID=""
// is a tombstone (grace window expired, jsonl missing or pruned).
type LinkInfo struct {
	InternalAgentID string
	JSONLPath       string
	Name            string
	Resolved        bool
	FirstPromptID   string
	FromHistory     bool
}

// metaEntry is one on-disk candidate surfaced by scanMetaFiles.
type metaEntry struct {
	hex       string
	metaPath  string
	jsonlPath string
	agentType string
}

// NewSubagentLinker returns an empty, context-free linker. Call SetContext
// after the parent process emits its first system.init event.
func NewSubagentLinker() *SubagentLinker {
	return &SubagentLinker{
		byTaskID:      make(map[string]LinkInfo),
		byToolUseID:   make(map[string]LinkInfo),
		byName:        make(map[string][]LinkInfo),
		resolveSem:    make(chan struct{}, maxConcurrentResolves),
		retryInterval: 250 * time.Millisecond,
		retryLimit:    12,
		cacheTTL:      200 * time.Millisecond,
	}
}

// SetPoolContext binds the resolve worker-pool lifetime to a process-scoped
// context (Process.lifecycleContext()) so a short-lived per-request ctx can
// never cancel the workers (#1661). Call once before the first
// DispatchResolve; later calls are ignored. nil is treated as unset.
func (l *SubagentLinker) SetPoolContext(ctx context.Context) {
	l.mu.Lock()
	if l.poolCtx == nil {
		l.poolCtx = ctx
	}
	l.mu.Unlock()
}

// SetContext installs the on-disk lookup root. Must be called before Resolve
// can succeed. Project dir is derived from the process cwd (resolveProjectDir);
// sessionID comes from the first system.init event.
func (l *SubagentLinker) SetContext(projectDir, parentSessionID string) {
	l.mu.Lock()
	prev := l.projectDir != "" && l.parentSessionID != ""
	l.projectDir = projectDir
	l.parentSessionID = parentSessionID
	l.mu.Unlock()
	if !prev {
		slog.Info("agent_link: SetContext installed",
			"project_dir", projectDir, "session_id", parentSessionID)
	}
}

// OnResolve appends a callback fired after every Resolve (success or
// tombstone, the latter with internalAgentID="" so tailers are not started).
// Callbacks run in append order, outside l.mu, serialised by onResolveMu.
func (l *SubagentLinker) OnResolve(fn func(taskID, toolUseID, internalAgentID string)) {
	if fn == nil {
		return
	}
	l.onResolveMu.Lock()
	l.onResolveFns = append(l.onResolveFns, fn)
	l.onResolveMu.Unlock()
}

// Query returns the cached mapping for taskID without scanning disk:
// ok=false for unknown task_ids, ok=true with empty InternalAgentID for
// tombstones, so HTTP handlers can distinguish 202 from 404.
func (l *SubagentLinker) Query(taskID string) (LinkInfo, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	info, ok := l.byTaskID[taskID]
	return info, ok
}

// QueryOrResolveFast returns a cached mapping when available; otherwise runs
// the direct-path stat once (no retry loop, no scan) so an agent row whose
// task_started never reached the Linker (pre-restart) still resolves from
// disk in <1 ms. Returns (LinkInfo{}, false) when the Linker has no context
// yet or the stat missed.
func (l *SubagentLinker) QueryOrResolveFast(taskID string) (LinkInfo, bool) {
	l.mu.RLock()
	if info, ok := l.byTaskID[taskID]; ok {
		l.mu.RUnlock()
		return info, ok
	}
	projectDir := l.projectDir
	sessionID := l.parentSessionID
	l.mu.RUnlock()
	if projectDir == "" || sessionID == "" {
		return LinkInfo{}, false
	}
	subagentDir := filepath.Join(projectDir, sessionID, "subagents")
	info, ok := l.resolveByTaskIDFast(taskID, "", subagentDir, sessionID)
	return info, ok
}

// TryMarkResolveInflight atomically claims the in-flight slot for taskID.
// ok=true on first claim; later callers get ok=false and SHOULD skip spawning
// a Resolve. Resolve's defer clears the claim, so a duplicate arriving after
// completion may re-claim. Empty taskID returns ok=false (#1354).
func (l *SubagentLinker) TryMarkResolveInflight(taskID string) bool {
	if taskID == "" {
		return false
	}
	_, loaded := l.inflightTasks.LoadOrStore(taskID, struct{}{})
	return !loaded
}

// DispatchResolve enqueues a Resolve onto the long-lived worker pool (#415).
// The first call starts resolveWorkerCount workers on poolCtx — never this
// caller's ctx (#1661); each job still carries its own ctx for the Resolve
// itself. A full queue falls back to an inline goroutine with a warning so
// no task_started is dropped. Empty taskID is a no-op.
func (l *SubagentLinker) DispatchResolve(ctx context.Context, taskID, toolUseID, name, description string, agentToolUseMS int64) {
	if taskID == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	l.resolvePoolOnce.Do(func() {
		// Fall back to the caller's ctx only when SetPoolContext was never
		// called (bare test fixtures).
		l.mu.RLock()
		lifetime := l.poolCtx
		l.mu.RUnlock()
		if lifetime == nil {
			lifetime = ctx
		}
		l.resolveJobs = make(chan resolveJob, resolveQueueDepth)
		for i := 0; i < resolveWorkerCount; i++ {
			go l.resolveWorker(lifetime)
		}
	})
	job := resolveJob{
		ctx:              ctx,
		taskID:           taskID,
		toolUseID:        toolUseID,
		name:             name,
		description:      description,
		agentToolUseTime: agentToolUseMS,
	}
	select {
	case l.resolveJobs <- job:
		return
	default:
		// Queue full: inline goroutine so readLoop never blocks on the 3 s
		// retry budget. Warn because saturation means the CLI is emitting
		// task_started faster than the pool drains.
		slog.Warn("agent_link: resolve queue full, falling back to inline goroutine",
			"task_id", taskID, "queue_depth", resolveQueueDepth)
		go l.Resolve(ctx, taskID, toolUseID, name, description, agentToolUseMS)
	}
}

// resolveWorker is the long-lived consumer for the dispatch queue; exits when
// ctx is canceled. No panic-recover on purpose: a panic should surface loudly.
func (l *SubagentLinker) resolveWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-l.resolveJobs:
			if !ok {
				return
			}
			l.Resolve(job.ctx, job.taskID, job.toolUseID, job.name, job.description, job.agentToolUseTime)
		}
	}
}

// clearResolveInflight releases the TryMarkResolveInflight claim; Resolve
// defers it so a later task_started for the same taskID can re-claim.
func (l *SubagentLinker) clearResolveInflight(taskID string) {
	if taskID == "" {
		return
	}
	l.inflightTasks.Delete(taskID)
}

// ConfigureForTest overrides the grace/poll/cache timings so cross-package
// tests reach terminal verdicts in milliseconds. Not for production callers.
func (l *SubagentLinker) ConfigureForTest(retryIntervalNS int64, retryLimit int, cacheTTLNS int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.retryInterval = time.Duration(retryIntervalNS)
	l.retryLimit = retryLimit
	l.cacheTTL = time.Duration(cacheTTLNS)
}

// ProjectSessionDir returns <projectDir>/<parentSessionID>, or "" before
// SetContext. Anchors the /api/sessions/tool_result path-traversal defence.
func (l *SubagentLinker) ProjectSessionDir() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.projectDir == "" || l.parentSessionID == "" {
		return ""
	}
	return filepath.Join(l.projectDir, l.parentSessionID)
}

// sleepOrCancel waits for d and returns true, or false if ctx is canceled
// first, so shutdown waits at most one retryInterval instead of the full
// retry budget (#644).
func sleepOrCancel(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Resolve maps taskID to its on-disk transcript and returns the LinkInfo plus
// whether a terminal verdict (Resolved=true) was reached. Idempotent: cached
// results (including tombstones for permanently missing task_ids) are O(1).
// The direct agent-<task_id>.jsonl path is tried first (covers replayed
// entries with empty name); the agentType scan with retry is the fallback for
// older CLIs. ctx should be Process-scoped: cancellation is observed at every
// retry sleep and the semaphore acquire, returning (LinkInfo{}, false) with no
// cache write (#644).
func (l *SubagentLinker) Resolve(ctx context.Context, taskID, toolUseID, name, description string, agentToolUseMS int64) (LinkInfo, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Clear the TryMarkResolveInflight claim on every exit path (#1354).
	defer l.clearResolveInflight(taskID)
	// Step 1: already resolved? (cheap fast path, no semaphore needed)
	l.mu.RLock()
	if info, ok := l.byTaskID[taskID]; ok {
		l.mu.RUnlock()
		return info, info.Resolved
	}
	projectDir := l.projectDir
	sessionID := l.parentSessionID
	l.mu.RUnlock()
	if projectDir == "" || sessionID == "" {
		slog.Info("agent_link: Resolve bailing — missing context",
			"task_id", taskID, "projectDir_set", projectDir != "",
			"sessionID_set", sessionID != "")
		return LinkInfo{}, false
	}

	subagentDir := filepath.Join(projectDir, sessionID, "subagents")

	// Fast path: one stat on agent-<task_id>.jsonl beats scanning the
	// directory, and works when the replayed entry has an empty name.
	if info, ok := l.resolveByTaskIDFast(taskID, toolUseID, subagentDir, sessionID); ok {
		return info, true
	}

	// Acquire a slot before the retry loop (nil in bare test fixtures). The
	// wait is bounded by the retry budget so a busy pool drops rather than
	// extends the grace window.
	if l.resolveSem != nil {
		semTimeout := time.Duration(l.retryLimit+1) * l.retryInterval
		semCtx, cancelSem := context.WithTimeout(ctx, semTimeout)
		select {
		case l.resolveSem <- struct{}{}:
			cancelSem()
			defer func() { <-l.resolveSem }()
		case <-semCtx.Done():
			cancelSem()
			if ctx.Err() != nil {
				slog.Debug("agent_link: resolve canceled while waiting for semaphore", "task_id", taskID, "err", ctx.Err())
			} else {
				slog.Debug("agent_link: resolve semaphore full, dropping", "task_id", taskID)
			}
			return LinkInfo{}, false
		}
	}

	var picked metaEntry
	var pickedFirst firstLineMeta

	// Step 5 scratch type; slices are reused across retry attempts.
	type scored struct {
		entry   metaEntry
		first   firstLineMeta
		modTime time.Time
		size    int64
	}
	var candidates []metaEntry
	var filtered []scored

	// First-line meta is immutable once written, so cache it per path across
	// retry attempts, keyed on ModTime+Size so a rewritten candidate is
	// re-read (#1883).
	type metaCacheEntry struct {
		modTime time.Time
		size    int64
		meta    firstLineMeta
		err     error
	}
	metaCache := map[string]metaCacheEntry{}

	// Steps 2-4: scan, filter by agentType, retry while empty.
	for attempt := 0; attempt <= l.retryLimit; attempt++ {
		if len(metaCache) > maxMetaCacheEntries {
			clear(metaCache)
		}
		entries := l.scanMetaFiles(subagentDir)
		candidates = candidates[:0]
		if cap(candidates) < len(entries) {
			candidates = make([]metaEntry, 0, len(entries))
		}
		for _, e := range entries {
			if e.agentType == name {
				candidates = append(candidates, e)
			}
		}
		if len(candidates) == 0 {
			if attempt == l.retryLimit {
				break
			}
			if !sleepOrCancel(ctx, l.retryInterval) {
				slog.Debug("agent_link: resolve canceled mid-retry (no candidates)", "task_id", taskID, "attempt", attempt, "err", ctx.Err())
				return LinkInfo{}, false
			}
			continue
		}

		// Step 5: per-candidate stat + first-line sessionId & timestamp cross-check.
		filtered = filtered[:0]
		if cap(filtered) < len(candidates) {
			filtered = make([]scored, 0, len(candidates))
		}
		for _, cand := range candidates {
			st, err := os.Stat(cand.jsonlPath)
			if err != nil || st.Size() == 0 {
				continue
			}
			ce, cached := metaCache[cand.jsonlPath]
			if !cached || !ce.modTime.Equal(st.ModTime()) || ce.size != st.Size() {
				if l.readMetaHook != nil {
					l.readMetaHook()
				}
				meta, perr := readFirstLineMeta(cand.jsonlPath)
				ce = metaCacheEntry{modTime: st.ModTime(), size: st.Size(), meta: meta, err: perr}
				metaCache[cand.jsonlPath] = ce
			}
			if ce.err != nil {
				continue
			}
			first := ce.meta
			if first.SessionID != "" && first.SessionID != sessionID {
				continue
			}
			// A first row older than the parent tool_use by more than the slack
			// is a same-name reuse from a prior turn.
			if !first.Timestamp.IsZero() && agentToolUseMS > 0 {
				agentTS := time.UnixMilli(agentToolUseMS)
				if first.Timestamp.Before(agentTS.Add(-staleAgentReuseSlack)) {
					continue
				}
			}
			filtered = append(filtered, scored{cand, first, st.ModTime(), st.Size()})
		}
		if len(filtered) == 0 {
			if attempt == l.retryLimit {
				break
			}
			if !sleepOrCancel(ctx, l.retryInterval) {
				slog.Debug("agent_link: resolve canceled mid-retry (no filtered)", "task_id", taskID, "attempt", attempt, "err", ctx.Err())
				return LinkInfo{}, false
			}
			continue
		}

		// Step 6: pick by (mtime desc, size desc).
		best := filtered[0]
		for _, s := range filtered[1:] {
			if s.modTime.After(best.modTime) || (s.modTime.Equal(best.modTime) && s.size > best.size) {
				best = s
			}
		}
		picked = best.entry
		pickedFirst = best.first
		break
	}

	// Step 7: finalise cache entry.
	l.mu.Lock()
	defer l.mu.Unlock()

	// Re-check under write lock — a concurrent Resolve may have resolved first.
	if info, ok := l.byTaskID[taskID]; ok {
		return info, info.Resolved
	}

	if picked.hex == "" {
		// Tombstone path.
		info := LinkInfo{Resolved: true, Name: name}
		l.byTaskID[taskID] = info
		if toolUseID != "" {
			l.byToolUseID[toolUseID] = info
		}
		l.fireCallbacksDropLock(taskID, toolUseID, "")
		return info, true
	}

	info := LinkInfo{
		InternalAgentID: "agent-" + picked.hex,
		JSONLPath:       picked.jsonlPath,
		Name:            name,
		Resolved:        true,
		FirstPromptID:   pickedFirst.PromptID,
	}

	// Step 7b: same-name respawn — log it; existing task_id mappings stay
	// untouched and only this task_id gets the new LinkInfo.
	if existing := l.byName[name]; len(existing) > 0 && pickedFirst.PromptID != "" {
		for _, prev := range existing {
			if prev.FirstPromptID != "" && prev.FirstPromptID != pickedFirst.PromptID {
				slog.Warn("agent_link: duplicate name spawn detected",
					"name", name,
					"old_prompt_id", prev.FirstPromptID,
					"new_prompt_id", pickedFirst.PromptID,
					"task_id", taskID,
				)
				break
			}
		}
	}

	l.byTaskID[taskID] = info
	if toolUseID != "" {
		l.byToolUseID[toolUseID] = info
	}
	l.appendNamedLink(name, info)
	l.fireCallbacksDropLock(taskID, toolUseID, info.InternalAgentID)
	return info, true
}

// resolveByTaskIDFast resolves via the agent-<task_id>.jsonl filename
// convention with a single stat, robust to an empty `name`. ok=true only on a
// positive stat with a matching first-line sessionId; ok=false falls through
// to the agentType scan for CLIs whose filename scheme differs.
func (l *SubagentLinker) resolveByTaskIDFast(taskID, toolUseID, subagentDir, sessionID string) (LinkInfo, bool) {
	if !agentHexRe.MatchString(taskID) {
		slog.Debug("agent_link: fast-path skip, bad hex", "task_id", taskID)
		return LinkInfo{}, false
	}
	jsonlPath := filepath.Join(subagentDir, "agent-"+taskID+".jsonl")
	st, err := os.Stat(jsonlPath)
	if err != nil || st.Size() == 0 {
		slog.Debug("agent_link: fast-path stat miss",
			"task_id", taskID, "path", jsonlPath, "err", err)
		return LinkInfo{}, false
	}
	first, err := readFirstLineMeta(jsonlPath)
	if err != nil {
		return LinkInfo{}, false
	}
	// sessionId cross-check: the projectDir encoding is lossy, so two cwds
	// can share a directory and must not leak jsonl across sessions.
	if first.SessionID != "" && first.SessionID != sessionID {
		return LinkInfo{}, false
	}

	// Display name from the sibling meta.json; optional.
	name := ""
	if data, err := os.ReadFile(filepath.Join(subagentDir, "agent-"+taskID+".meta.json")); err == nil {
		var m struct {
			AgentType string `json:"agentType"`
		}
		if json.Unmarshal(data, &m) == nil {
			name = m.AgentType
		}
	}

	info := LinkInfo{
		InternalAgentID: "agent-" + taskID,
		JSONLPath:       jsonlPath,
		Name:            name,
		Resolved:        true,
		FirstPromptID:   first.PromptID,
	}

	l.mu.Lock()
	if cached, ok := l.byTaskID[taskID]; ok {
		l.mu.Unlock()
		return cached, cached.Resolved
	}
	l.byTaskID[taskID] = info
	if toolUseID != "" {
		l.byToolUseID[toolUseID] = info
	}
	if name != "" {
		l.appendNamedLink(name, info)
	}
	l.fireCallbacksDropLock(taskID, toolUseID, info.InternalAgentID)
	l.mu.Unlock()
	slog.Info("agent_link: resolved by task_id fast path",
		"task_id", taskID, "agent_type", name, "jsonl_size", st.Size())
	return info, true
}

// SeedFromHistory pre-populates the cache from persisted EventEntry records
// (Process.InjectHistory after AppendBatch) so reconnect/respawn keeps the
// task_id → jsonl mapping. Entries missing InternalAgentID or JSONLPath are
// skipped. Paths outside ~/.claude/projects are refused: a mutated
// sessions/*.jsonl must not redirect agent_events streaming to an arbitrary
// readable file.
func (l *SubagentLinker) SeedFromHistory(entries []clievent.EventEntry) {
	if len(entries) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	claudeRoot := claudeProjectsRoot()
	for _, e := range entries {
		if e.TaskID == "" || e.InternalAgentID == "" || e.JSONLPath == "" {
			continue
		}
		// claudeRoot (not l.projectDir) so entries persisted under a previous
		// cwd in the same session are still accepted.
		clean := filepath.Clean(e.JSONLPath)
		if !strings.HasPrefix(clean, claudeRoot+string(filepath.Separator)) {
			slog.Warn("agent_link: SeedFromHistory rejected jsonl path outside claude projects root",
				"task_id", e.TaskID, "path", e.JSONLPath)
			continue
		}
		// A live Resolve outranks historical data.
		if _, ok := l.byTaskID[e.TaskID]; ok {
			continue
		}
		info := LinkInfo{
			InternalAgentID: e.InternalAgentID,
			JSONLPath:       clean,
			Name:            e.Subagent,
			FirstPromptID:   e.FirstPromptID,
			Resolved:        true,
			FromHistory:     true,
		}
		l.byTaskID[e.TaskID] = info
		if e.ToolUseID != "" {
			if _, ok := l.byToolUseID[e.ToolUseID]; !ok {
				l.byToolUseID[e.ToolUseID] = info
			}
		}
		if e.Subagent != "" {
			l.appendNamedLink(e.Subagent, info)
		}
	}
}

// claudeProjectsRoot returns ~/.claude/projects; shared by resolveProjectDir
// and SeedFromHistory's prefix check so the two cannot drift.
func claudeProjectsRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".claude", "projects")
}

// fireCallbacksDropLock runs every registered callback with l.mu RELEASED
// (callbacks may re-enter Query/Resolve) and onResolveMu serialising delivery.
// Caller MUST hold l.mu as a write lock; the function drops it around dispatch
// and re-acquires it before returning so the caller's deferred Unlock is
// balanced — hence "DropLock", not "Locked".
func (l *SubagentLinker) fireCallbacksDropLock(taskID, toolUseID, internalAgentID string) {
	l.onResolveMu.Lock()
	if len(l.onResolveFns) == 0 {
		l.onResolveMu.Unlock()
		return
	}
	fns := make([]func(string, string, string), len(l.onResolveFns))
	copy(fns, l.onResolveFns)
	l.onResolveMu.Unlock()
	l.mu.Unlock()
	// Re-acquire via defer so a panicking callback still leaves l.mu locked
	// for the caller's deferred Unlock.
	defer l.mu.Lock()
	for _, fn := range fns {
		fn(taskID, toolUseID, internalAgentID)
	}
}

// scanMetaFiles reads subagentDir and parses each .meta.json into
// (hex, agentType) pairs, TTL-cached (default 200ms) so concurrent Resolves
// in one turn share a scan. Cache hits return the cached slice by reference
// (callers must not mutate) under RLock so they run concurrently.
func (l *SubagentLinker) scanMetaFiles(dir string) []metaEntry {
	now := time.Now()
	l.mu.RLock()
	if !l.dirCache.at.IsZero() && now.Sub(l.dirCache.at) < l.cacheTTL {
		entries := l.dirCache.entries
		l.mu.RUnlock()
		return entries
	}
	l.mu.RUnlock()

	// Scan WITHOUT l.mu (ReadDir + ReadFile per meta is blocking IO that
	// would stall every concurrent fast-path RLock), then publish under the
	// write lock. scannedAt is captured BEFORE the scan so a later-started
	// scan wins the "freshest snapshot" comparison (#1595).
	scannedAt := time.Now()
	if l.scanHook != nil {
		l.scanHook()
	}
	entries := rawScanSubagentsDir(dir)

	l.mu.Lock()
	defer l.mu.Unlock()
	// Prefer a fresher cache published while we scanned unlocked.
	if !l.dirCache.at.IsZero() && l.dirCache.at.After(scannedAt) {
		return l.dirCache.entries
	}
	l.dirCache.at = scannedAt
	l.dirCache.entries = entries
	return entries
}

func rawScanSubagentsDir(dir string) []metaEntry {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]metaEntry, 0, len(ents))
	for _, ent := range ents {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if !strings.HasPrefix(name, "agent-") || !strings.HasSuffix(name, ".meta.json") {
			continue
		}
		hex := strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".meta.json")
		if !agentHexRe.MatchString(hex) {
			continue
		}
		metaPath := filepath.Join(dir, name)
		// Real meta files are well under 8 KiB; cap so a stray multi-MB file
		// cannot inflate scan latency.
		const maxMetaBytes = 8 * 1024
		if info, err := ent.Info(); err != nil || info.Size() > maxMetaBytes {
			continue
		}
		data, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}
		var m struct {
			AgentType string `json:"agentType"`
		}
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		if m.AgentType == "" {
			continue
		}
		out = append(out, metaEntry{
			hex:       hex,
			metaPath:  metaPath,
			jsonlPath: filepath.Join(dir, "agent-"+hex+".jsonl"),
			agentType: m.AgentType,
		})
	}
	return out
}

// firstLineMeta holds the fields Resolve step 5 needs from the agent jsonl.
type firstLineMeta struct {
	SessionID string    `json:"sessionId"`
	PromptID  string    `json:"promptId"`
	Timestamp time.Time `json:"-"`
}

// errFirstLineTooLong signals that the agent jsonl's first line exceeds the
// 32KB ReadSlice buffer, so the truncated prefix is never fed to Unmarshal.
var errFirstLineTooLong = errors.New("agent jsonl first line exceeds 32KB buffer")

func readFirstLineMeta(path string) (firstLineMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return firstLineMeta{}, err
	}
	defer f.Close()
	reader := bufio.NewReaderSize(f, 32*1024)
	line, err := reader.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return firstLineMeta{}, errFirstLineTooLong
		}
		if len(line) == 0 {
			return firstLineMeta{}, err
		}
	}
	var raw struct {
		SessionID string `json:"sessionId"`
		PromptID  string `json:"promptId"`
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return firstLineMeta{}, err
	}
	out := firstLineMeta{SessionID: raw.SessionID, PromptID: raw.PromptID}
	if raw.Timestamp != "" {
		if ts, err := time.Parse(time.RFC3339Nano, raw.Timestamp); err == nil {
			out.Timestamp = ts
		}
	}
	return out, nil
}

// resolveProjectDir mirrors Claude CLI's encoded-cwd convention for
// ~/.claude/projects/<encoded>: every non-[A-Za-z0-9] rune becomes '-'
// (consecutive dashes are NOT collapsed). Empty input → "" (Resolve bails).
// The encoding is lossy ("/tmp/a.b" and "/tmp/a_b" collide), which the
// first-line sessionId cross-check in Resolve defends against.
func resolveProjectDir(cwd string) string {
	if cwd == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(cwd))
	for _, r := range cwd {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return filepath.Join(claudeProjectsRoot(), b.String())
}
