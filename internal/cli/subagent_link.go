package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// agentHexRe whitelists the hex component of an agent-<hex>.jsonl filename
// before we accept it from the subagents/ directory. Claude CLI today emits
// 17-char lowercase hex; we accept 8-64 for forward compatibility but refuse
// anything containing path separators, dots, or shell metacharacters.
// R201-SEC-M2 defense-in-depth: filepath.Join(dir, "agent-"+hex+".jsonl")
// with a malformed hex would normally be cleaned by filepath.Clean, but
// having the sanity gate here removes any doubt about cross-platform
// traversal corner cases (Windows short names, symlink prefix collapse).
var agentHexRe = regexp.MustCompile(`^[A-Za-z0-9]{8,64}$`)

// SubagentLinker maps agent task_ids (and their originating Agent tool_use_ids)
// to the on-disk transcript jsonl Claude CLI writes under
// <projectDir>/<sessionID>/subagents/agent-<hex>.jsonl. The dashboard uses
// this mapping to render each agent's internal event stream when the user
// clicks an agent row in the banner (RFC v4 agent-team-ui §3.3).
//
// Resolve is async — the CLI emits system.task_started immediately after the
// parent-stream Agent tool_use, but flushes the first .meta.json / jsonl row
// 0-500 ms later. Callers spawn Resolve in a goroutine with a bounded grace
// window (default 3 s). The on-resolve callback then starts the silent
// TranscriptReader tailer and backfills EventEntry.InternalAgentID so
// persistHistory captures the linkage (see §3.3.7 SeedFromHistory).
// maxConcurrentResolves caps the number of Resolve goroutines that may
// execute concurrently per SubagentLinker. Each Resolve sleeps up to
// retryLimit*retryInterval (default 3 s) waiting for the CLI to flush its
// jsonl; a single multi-agent turn can emit 10+ task_started events in
// rapid succession, each spawning a goroutine. Without a cap, long-running
// agents accumulate hundreds of parked goroutines per session.
const maxConcurrentResolves = 8

// staleAgentJSONLWindow guards against name-collision aliasing in Step 5
// of Resolve: if a candidate jsonl's first-row timestamp predates the
// parent's Agent tool_use by more than this window, the file belongs to a
// previous turn that happened to reuse the same agent name (Claude CLI
// keeps subagents/agent-<hex>.jsonl files across turns). The 10s value
// covers worst-case CLI flush + clock-skew between the wrapper's
// monotonic timestamp and the CLI's wall-clock first-line timestamp,
// while staying well below typical multi-second between-turn idle gaps.
// R10 design note in the original Resolve scan; lifted to a named const
// so readers see the rationale without grepping the inline comment.
const staleAgentJSONLWindow = 10 * time.Second

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

	// resolveSem bounds concurrent Resolve goroutines. Buffered channel
	// acts as a counting semaphore: acquire by sending, release by receiving.
	resolveSem chan struct{}

	// Tunable via tests. Defaults: 250ms * 12 = 3s grace; 200ms dir cache.
	retryInterval time.Duration
	retryLimit    int
	cacheTTL      time.Duration

	// Optional hook fired after every rawScan of the subagents directory.
	// Wired by TestLinker_Resolve_DirCacheTTL to count cache hits/misses
	// without adding a telemetry atomic to production code.
	scanHook func()
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
// tombstone). Multiple subscribers compose: the cli package registers one
// that writes InternalAgentID back to the EventLog; the server package
// registers another to kick off agent_tailer. Callbacks run in append
// order, outside l.mu but serialised by onResolveMu.
//
// Tombstone resolutions fire with internalAgentID="" so the server tailer
// layer can skip starting a silent tailer rather than waiting forever.
func (l *SubagentLinker) OnResolve(fn func(taskID, toolUseID, internalAgentID string)) {
	if fn == nil {
		return
	}
	l.onResolveMu.Lock()
	l.onResolveFns = append(l.onResolveFns, fn)
	l.onResolveMu.Unlock()
}

// Query returns the cached mapping for taskID without scanning disk. Used by
// the HTTP /api/sessions/agent_events handler; returns ok=false for unknown
// task_ids and ok=true+empty InternalAgentID for tombstones so the handler
// can distinguish "still resolving" (202) from "no record" (404).
func (l *SubagentLinker) Query(taskID string) (LinkInfo, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	info, ok := l.byTaskID[taskID]
	return info, ok
}

// QueryOrResolveFast returns a cached mapping when available; otherwise runs
// the fast-path stat once (no retry loop, no scan fallback) and returns the
// result. HTTP endpoints use this instead of Query so users who click an
// agent row whose live task_started never reached the Linker (e.g. because
// the event happened pre-restart and shim replay didn't include it) get a
// direct answer from disk in a single stat call — typically < 1 ms.
//
// Returns (zeroLinkInfo, false) when:
//   - projectDir or parentSessionID is not yet set (Linker not ready)
//   - the direct-path stat missed (no such agent-<task_id>.jsonl file)
//
// The caller distinguishes these by Linker state: the endpoint already
// null-checks Linker. "Not yet ready" surfaces as 202 pending; the endpoint
// keeps that contract with this helper so the client retry loop still
// converges on the "give up" toast after MAX_SWITCH_RETRIES.
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

// ConfigureForTest overrides the default grace/poll/cache timings so tests
// reach terminal verdicts in milliseconds rather than 3+ seconds. Not meant
// for production callers — the private fields it mutates are the same ones
// subagent_link_test.go manipulates directly via the test-only in-package
// access; this entrypoint exists so cross-package tests (server/httptest)
// can dial them in too.
func (l *SubagentLinker) ConfigureForTest(retryIntervalNS int64, retryLimit int, cacheTTLNS int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.retryInterval = time.Duration(retryIntervalNS)
	l.retryLimit = retryLimit
	l.cacheTTL = time.Duration(cacheTTLNS)
}

// ProjectSessionDir returns <projectDir>/<parentSessionID>. Empty if context
// not yet installed. Used by the /api/sessions/tool_result endpoint to anchor
// path-traversal defence (§4 Security).
func (l *SubagentLinker) ProjectSessionDir() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.projectDir == "" || l.parentSessionID == "" {
		return ""
	}
	return filepath.Join(l.projectDir, l.parentSessionID)
}

// Resolve executes the 7-step algorithm in RFC §3.3.1.
// Returns the LinkInfo and whether Resolve has reached a terminal verdict
// (Resolved=true). Idempotent: once cached, subsequent calls are O(1).
//
// Tombstones are cached so repeated Resolve attempts on a permanently-missing
// task_id do not re-scan every poll.
//
// Claude CLI 2.1.132 emits subagents/agent-<task_id>.jsonl (the hex component
// is literally the task_id). We try that direct path first — it skips the
// scan entirely for the common case, and also covers historical entries
// replayed via InjectHistory where `name` is empty (so the original scan +
// agentType match would always miss). The legacy scan stays as a fallback
// for older CLI versions or sidechain agents whose filename convention
// differs.
func (l *SubagentLinker) Resolve(taskID, toolUseID, name, description string, agentToolUseMS int64) (LinkInfo, bool) {
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

	// Fast path: Claude 2.1.132 puts the task_id directly into the filename.
	// Try that before the name-based scan — cheap stat beats scanning a
	// directory of dozens/hundreds of candidate meta files, and handles the
	// shim-reconnect case where historical EventEntry has empty name.
	if info, ok := l.resolveByTaskIDFast(taskID, toolUseID, subagentDir, sessionID); ok {
		return info, true
	}

	// Acquire a slot before the retry loop. Each retry sleeps up to
	// retryInterval, so a multi-agent turn that emits 10+ task_started
	// events would park 10+ goroutines for up to 3 s each without this cap.
	// resolveSem is nil in linkers constructed by tests that don't call
	// NewSubagentLinker (legacy test helpers), so guard with a nil check.
	//
	// Timeout matches the total retry budget (retryLimit * retryInterval)
	// so we drop rather than extend the grace window when all slots are busy.
	if l.resolveSem != nil {
		t := time.NewTimer(time.Duration(l.retryLimit+1) * l.retryInterval)
		select {
		case l.resolveSem <- struct{}{}:
			// Stop returns false when the timer already fired; drain the
			// channel so a tied select that picked the semaphore arm does
			// not leak a runtime timer with a non-empty C.
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
			defer func() { <-l.resolveSem }()
		case <-t.C:
			slog.Debug("agent_link: resolve semaphore full, dropping", "task_id", taskID)
			return LinkInfo{}, false
		}
	}

	var picked metaEntry
	var pickedFirst firstLineMeta

	// Steps 2-4: scan, filter by agentType, retry while empty.
	for attempt := 0; attempt <= l.retryLimit; attempt++ {
		entries := l.scanMetaFiles(subagentDir)
		// Allocate fresh — entries is the cached dirCache slice (see
		// scanMetaFiles); the prior `entries[:0:0]` trick reused its
		// header but always re-allocated on first append, so it offered
		// no advantage and obscured the relationship between the cache
		// slice and the per-attempt working slice.
		candidates := make([]metaEntry, 0, len(entries))
		for _, e := range entries {
			if e.agentType == name {
				candidates = append(candidates, e)
			}
		}
		if len(candidates) == 0 {
			if attempt == l.retryLimit {
				break
			}
			// R220-GO-1 / R219-GO-1 anchor: time.Sleep here is intentional —
			// piping ctx through Resolve is a Breaking change (signature
			// includes a new parameter, every test helper must follow) and
			// is queued in those lanes as a single atomic refactor with the
			// resolveSem acquire path. Bounded by retryLimit*retryInterval
			// (~3s default), and on shutdown the parent process is already
			// torn down before this goroutine matters — Resolve's worst-case
			// orphan window is the same 3s budget it already promises.
			time.Sleep(l.retryInterval)
			continue
		}

		// Step 5: per-candidate stat + first-line sessionId & timestamp cross-check.
		type scored struct {
			entry   metaEntry
			first   firstLineMeta
			modTime time.Time
			size    int64
		}
		filtered := make([]scored, 0, len(candidates))
		for _, cand := range candidates {
			st, err := os.Stat(cand.jsonlPath)
			if err != nil || st.Size() == 0 {
				continue
			}
			first, err := readFirstLineMeta(cand.jsonlPath)
			if err != nil {
				continue
			}
			if first.SessionID != "" && first.SessionID != sessionID {
				continue
			}
			// R10: if the jsonl's first row timestamp predates the parent's
			// Agent tool_use by more than staleAgentJSONLWindow, treat as
			// stale (same-name reuse from a previous turn).
			if !first.Timestamp.IsZero() && agentToolUseMS > 0 {
				agentTS := time.UnixMilli(agentToolUseMS)
				if first.Timestamp.Before(agentTS.Add(-staleAgentJSONLWindow)) {
					continue
				}
			}
			filtered = append(filtered, scored{cand, first, st.ModTime(), st.Size()})
		}
		if len(filtered) == 0 {
			if attempt == l.retryLimit {
				break
			}
			// Same R220-GO-1 / R219-GO-1 anchor as the candidates==0 sleep
			// above — Resolve does not yet take ctx, so cancellation arms
			// are deferred to the breaking refactor in those lanes.
			time.Sleep(l.retryInterval)
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

	// Step 7b: same-name respawn defence. If we've already resolved a
	// LinkInfo for `name` with a different promptId, we're looking at a
	// CLI same-name reuse. Log it and refuse to touch existing task_id
	// mappings — only this new task_id gets the new LinkInfo.
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
	l.byName[name] = append(l.byName[name], info)
	l.fireCallbacksDropLock(taskID, toolUseID, info.InternalAgentID)
	return info, true
}

// resolveByTaskIDFast covers the Claude 2.1.132 convention where the
// subagents/agent-<hex>.jsonl filename's hex component IS the task_id.
// On a cache miss + empty candidate set (the original name-based scan would
// retry for 3 s grace and then tombstone), we stat the direct path first —
// cheap, stable, and robust to missing/empty `name` (which is exactly the
// shim-reconnect + history-replay case).
//
// Returns ok=true only on a positive stat with a non-empty first-line session
// match. ok=false falls through to the original scan (and its retry loop) so
// older CLIs / sidechain agents whose filename scheme differs still work.
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
	// sessionId cross-check defends against the lossy projectDir encoding
	// collision (§3.3.2) — two distinct cwds mapping to the same encoded
	// directory must not let agent jsonl from one session leak into another.
	if first.SessionID != "" && first.SessionID != sessionID {
		return LinkInfo{}, false
	}

	// Optionally pull the agent name from the sibling meta.json for
	// display purposes. Not required for Resolve to succeed; leave empty
	// on any error since the dashboard already has the name from the
	// parent-stream `agent` EventEntry when present.
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
	// Re-check under write lock (another goroutine may have raced in).
	if cached, ok := l.byTaskID[taskID]; ok {
		l.mu.Unlock()
		return cached, cached.Resolved
	}
	l.byTaskID[taskID] = info
	if toolUseID != "" {
		l.byToolUseID[toolUseID] = info
	}
	if name != "" {
		l.byName[name] = append(l.byName[name], info)
	}
	l.fireCallbacksDropLock(taskID, toolUseID, info.InternalAgentID)
	l.mu.Unlock()
	slog.Info("agent_link: resolved by task_id fast path",
		"task_id", taskID, "agent_type", name, "jsonl_size", st.Size())
	return info, true
}

// SeedFromHistory pre-populates the cache from persisted EventEntry records.
// Called by Process.InjectHistory after AppendBatch so that shim reconnect
// / CLI-dead respawn do not lose the task_id → jsonl mapping established in
// a previous session lifetime (A3 defence, §3.3.7).
//
// Tolerant of gaps: EventEntry with empty InternalAgentID or JSONLPath is
// skipped silently — the Linker will re-Resolve on live events as usual.
//
// Defense in depth (R201-SEC-M1): the persisted JSONL file is writable by
// anything running as the naozhi uid, but we still refuse to trust a path
// that does not live under the expected ~/.claude/projects/ tree. Without
// this check, an attacker who could mutate sessions/*.jsonl (e.g. via a
// separate file-disclosure bug or filesystem-level compromise) could
// redirect agent_events streaming to an arbitrary readable file.
func (l *SubagentLinker) SeedFromHistory(entries []EventEntry) {
	if len(entries) == 0 {
		return
	}
	// Lock against concurrent Resolve + snapshot the project root for the
	// prefix check. projectDir comes from resolveProjectDir(cwd), so it
	// already ends at ~/.claude/projects/<encoded>.
	l.mu.Lock()
	defer l.mu.Unlock()
	claudeRoot := claudeProjectsRoot()
	for _, e := range entries {
		if e.TaskID == "" || e.InternalAgentID == "" || e.JSONLPath == "" {
			continue
		}
		// Refuse jsonl paths that escape the claude projects root. Accept
		// either l.projectDir (strictest) OR claudeRoot (for entries
		// persisted under a previous cwd whose projectDir no longer matches
		// — shim reconnect after `cd` within the same session). Rejecting
		// an otherwise-valid historical entry just costs us Resolve having
		// to re-scan, which is the exact path Linker was designed to handle.
		clean := filepath.Clean(e.JSONLPath)
		if !strings.HasPrefix(clean, claudeRoot+string(filepath.Separator)) {
			slog.Warn("agent_link: SeedFromHistory rejected jsonl path outside claude projects root",
				"task_id", e.TaskID, "path", e.JSONLPath)
			continue
		}
		// Don't overwrite an already-cached task_id — a live Resolve
		// outranks historical data.
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
			l.byName[e.Subagent] = append(l.byName[e.Subagent], info)
		}
	}
}

// claudeProjectsRoot returns ~/.claude/projects exactly as resolveProjectDir
// derives it. Kept as a separate helper so SeedFromHistory's prefix check
// stays in lockstep with Resolve's path construction; changing one without
// the other would either let a bogus path through or reject valid entries.
func claudeProjectsRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".claude", "projects")
}

// fireCallbacksDropLock runs every registered callback OUTSIDE the main mu
// lock but INSIDE onResolveMu to guarantee serial delivery across all
// subscribers. Caller MUST hold l.mu as a write lock; this function
// releases l.mu around the callback dispatch and re-acquires it before
// returning, so the caller's `defer l.mu.Unlock()` still fires correctly.
//
// The name carries the lock contract: previously named ...Locked (the
// usual Go convention is "caller holds the lock and the function does not
// touch it"), but this function intentionally drops and re-acquires l.mu —
// the rename to ...DropLock prevents future maintainers from treating it
// as a normal "Locked" helper and double-unlocking. (R227-GO-3)
//
// R222-GO-3 archive anchor: the prior concern about a callback re-entering
// linker.Query under nested lock release is non-issue because (a) Query
// takes RLock against l.mu which is fully released between the Unlock above
// and the deferred re-Lock below, and (b) onResolveMu copy-then-drop
// (lines 575–578) serialises subscriber dispatch independently of l.mu.
// A callback that calls back into Resolve still goes through Resolve's own
// mu acquisition, so the dispatch order is well-defined.
func (l *SubagentLinker) fireCallbacksDropLock(taskID, toolUseID, internalAgentID string) {
	l.onResolveMu.Lock()
	fns := make([]func(string, string, string), len(l.onResolveFns))
	copy(fns, l.onResolveFns)
	l.onResolveMu.Unlock()
	if len(fns) == 0 {
		return
	}
	l.mu.Unlock()
	// Re-acquire l.mu via defer so a panicking callback still leaves
	// l.mu Locked when the stack unwinds — otherwise the caller's own
	// `defer l.mu.Unlock()` would unlock an already-unlocked mutex and
	// fault, and the second panic from sync would also corrupt the
	// readLoop's panic-recover bookkeeping. (R227-GO-1)
	defer l.mu.Lock()
	for _, fn := range fns {
		fn(taskID, toolUseID, internalAgentID)
	}
}

// scanMetaFiles reads subagentDir and parses each .meta.json to surface
// (hex, agentType) pairs. TTL-cached (default 200ms) so a burst of 3+
// concurrent Resolve calls during the same turn share one disk scan.
//
// Cache miss → rawScan populates dirCache. Cache hit → returns the cached
// slice by reference; callers must not mutate it.
//
// Hot-path RLock fast-path (R215-CR-P2-4): under a turn with N parallel
// Agent tool_use events, every Resolve calls scanMetaFiles. Without the
// fast-path each one serialises on l.mu (write lock) even when the cache
// is fresh — pure cache hits should run concurrently. We try RLock
// first; if the cache is fresh, return immediately. On miss we upgrade
// to the write lock and double-check (another goroutine may have
// populated the cache between our RUnlock and Lock).
func (l *SubagentLinker) scanMetaFiles(dir string) []metaEntry {
	l.mu.RLock()
	if !l.dirCache.at.IsZero() && time.Since(l.dirCache.at) < l.cacheTTL {
		entries := l.dirCache.entries
		l.mu.RUnlock()
		return entries
	}
	l.mu.RUnlock()

	l.mu.Lock()
	defer l.mu.Unlock()
	// Double-check: another goroutine may have populated the cache
	// between our RUnlock and Lock. The TTL still applies so a stale
	// entry from before the gap is correctly rejected.
	if !l.dirCache.at.IsZero() && time.Since(l.dirCache.at) < l.cacheTTL {
		return l.dirCache.entries
	}
	if l.scanHook != nil {
		l.scanHook()
	}
	entries := rawScanSubagentsDir(dir)
	l.dirCache.at = time.Now()
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
		// agentType is a short identifier; legitimate meta files are well under
		// 8 KiB. Cap the read so a corrupt/oversized file (or a name collision
		// with a stray multi-MB JSON) cannot inflate scan latency.
		const maxMetaBytes = 8 * 1024
		if st, err := os.Stat(metaPath); err != nil || st.Size() > maxMetaBytes {
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
// 32KB ReadSlice buffer. Without this sentinel, Unmarshal on the truncated
// prefix would return a generic JSON syntax error and silently degrade the
// fast path. R222-GO-7.
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
		// ErrBufferFull means we got a partial line; do not feed truncated
		// bytes to Unmarshal — surface the condition explicitly so the
		// caller's "skip this candidate" branch is reached without logging
		// a misleading JSON syntax error. R222-GO-7.
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

// resolveProjectDir mirrors Claude CLI's encoded-cwd convention used for
// ~/.claude/projects/<encoded>. Walks runes and replaces every non-[A-Za-z0-9]
// rune with a single '-'. Does NOT collapse consecutive dashes — "/tmp/a--b"
// and "/tmp/a..b" encode differently. Empty input → empty output (callers
// treat this as "no project dir", i.e. Resolve bails).
//
// The encoding is lossy: "/tmp/a.b", "/tmp/a_b", "/tmp/a-b" all collide to
// "-tmp-a-b". Resolve defends against this via the first-line sessionId
// cross-check (§3.3.1 step 5).
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
