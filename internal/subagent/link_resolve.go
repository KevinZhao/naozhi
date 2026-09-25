package subagent

// Resolve pipeline: turning a Task tool_use into a subagent's session id.
// Dispatch is bounded by a worker pool and each attempt re-scans the CLI's
// subagents dir, because the CLI writes the meta file some time AFTER the tool
// call appears. Split out of link.go (#2714 G-follow).

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// SetPoolContext binds the resolve worker-pool lifetime to a process-scoped
// context (Process.lifecycleContext()) so a short-lived per-request ctx can
// never cancel the workers (#1661). Call once before the first
// DispatchResolve; later calls are ignored. nil is treated as unset.
func (l *Linker) SetPoolContext(ctx context.Context) {
	l.mu.Lock()
	if l.poolCtx == nil {
		l.poolCtx = ctx
	}
	l.mu.Unlock()
}

// QueryOrResolveFast returns a cached mapping when available; otherwise runs
// the direct-path stat once (no retry loop, no scan) so an agent row whose
// task_started never reached the Linker (pre-restart) still resolves from
// disk in <1 ms. Returns (LinkInfo{}, false) when the Linker has no context
// yet or the stat missed.
func (l *Linker) QueryOrResolveFast(taskID string) (LinkInfo, bool) {
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
func (l *Linker) TryMarkResolveInflight(taskID string) bool {
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
func (l *Linker) DispatchResolve(ctx context.Context, taskID, toolUseID, name, description string, agentToolUseMS int64) {
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
func (l *Linker) resolveWorker(ctx context.Context) {
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
func (l *Linker) clearResolveInflight(taskID string) {
	if taskID == "" {
		return
	}
	l.inflightTasks.Delete(taskID)
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
func (l *Linker) Resolve(ctx context.Context, taskID, toolUseID, name, description string, agentToolUseMS int64) (LinkInfo, bool) {
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

	// Acquire a slot before the retry loop (nil in bare test fixtures). A free
	// slot is taken outright; only a full pool waits, and that wait is bounded
	// by the retry budget so a busy pool drops rather than extends the grace
	// window. The two steps are separate because select picks at random among
	// ready cases: one select over "slot free" and "budget spent" could drop a
	// call with the pool idle.
	if l.resolveSem != nil {
		select {
		case l.resolveSem <- struct{}{}:
		default:
			semTimeout := time.Duration(l.retryLimit+1) * l.retryInterval
			semCtx, cancelSem := context.WithTimeout(ctx, semTimeout)
			select {
			case l.resolveSem <- struct{}{}:
				cancelSem()
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
		defer func() { <-l.resolveSem }()
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
func (l *Linker) resolveByTaskIDFast(taskID, toolUseID, subagentDir, sessionID string) (LinkInfo, bool) {
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
