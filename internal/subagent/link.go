// Package subagent links a Claude subagent (Task tool) invocation to the JSONL
// transcript the CLI writes for it, and reads that transcript into EventEntry
// pages. #2649 G1-f.
//
// Moved out of internal/cli, which it never needed: 1,440 non-test lines whose
// imports are stdlib plus claudefs (the transcript path layout), clievent (the
// entry shape) and textutil. internal/dashboard and internal/server pulled in the
// subprocess spawner to name a Linker or a LinkInfo.
//
// Two renames dropped the stutter that a package boundary made visible:
// SubagentLinker → Linker and NewSubagentLinker → NewLinker.
package subagent

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// agentHexRe whitelists the hex component of an agent-<hex>.jsonl filename
// (CLI emits 17 chars; 8-64 accepted) and refuses path separators, dots and
// metacharacters before the value reaches filepath.Join — defence in depth
// against cross-platform traversal corner cases.
var agentHexRe = regexp.MustCompile(`^[A-Za-z0-9]{8,64}$`)

// maxConcurrentResolves caps concurrent Resolve calls per Linker.
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
func (l *Linker) appendNamedLink(name string, info LinkInfo) {
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

// Linker maps agent task_ids (and their originating Agent
// tool_use_ids) to the transcript jsonl Claude CLI writes under
// <projectDir>/<sessionID>/subagents/agent-<hex>.jsonl, so the dashboard can
// render each agent's internal event stream. Resolve is async: the CLI emits
// system.task_started immediately but flushes the first jsonl row 0-500 ms
// later, so Resolve retries within a bounded grace window (default 3 s) and
// the OnResolve callbacks then start the tailer and backfill
// EventEntry.InternalAgentID for persistence.
type Linker struct {
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

// NewLinker returns an empty, context-free linker. Call SetContext
// after the parent process emits its first system.init event.
func NewLinker() *Linker {
	return &Linker{
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
// can succeed. Project dir is derived from the process cwd (ProjectDir);
// sessionID comes from the first system.init event.
// ParentSessionID returns the session ID Resolve matches transcripts against,
// or "" before SetContext has supplied one. Read under the Linker's own lock:
// the caller (cli.Process.SetCwdForLinker, deciding whether to seed it from the
// shim handshake) used to reach into l.parentSessionID directly, which stopped
// being possible when this package moved out of internal/cli (#2649 G1-f).
func (l *Linker) ParentSessionID() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.parentSessionID
}

func (l *Linker) SetContext(projectDir, parentSessionID string) {
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
func (l *Linker) OnResolve(fn func(taskID, toolUseID, internalAgentID string)) {
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
func (l *Linker) Query(taskID string) (LinkInfo, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	info, ok := l.byTaskID[taskID]
	return info, ok
}

// ConfigureForTest overrides the grace/poll/cache timings so cross-package
// tests reach terminal verdicts in milliseconds. Not for production callers.
func (l *Linker) ConfigureForTest(retryIntervalNS int64, retryLimit int, cacheTTLNS int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.retryInterval = time.Duration(retryIntervalNS)
	l.retryLimit = retryLimit
	l.cacheTTL = time.Duration(cacheTTLNS)
}

// ProjectSessionDir returns <projectDir>/<parentSessionID>, or "" before
// SetContext. Anchors the /api/sessions/tool_result path-traversal defence.
func (l *Linker) ProjectSessionDir() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.projectDir == "" || l.parentSessionID == "" {
		return ""
	}
	return filepath.Join(l.projectDir, l.parentSessionID)
}

// fireCallbacksDropLock runs every registered callback with l.mu RELEASED
// (callbacks may re-enter Query/Resolve) and onResolveMu serialising delivery.
// Caller MUST hold l.mu as a write lock; the function drops it around dispatch
// and re-acquires it before returning so the caller's deferred Unlock is
// balanced — hence "DropLock", not "Locked".
func (l *Linker) fireCallbacksDropLock(taskID, toolUseID, internalAgentID string) {
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

// firstLineMeta holds the fields Resolve step 5 needs from the agent jsonl.
type firstLineMeta struct {
	SessionID string    `json:"sessionId"`
	PromptID  string    `json:"promptId"`
	Timestamp time.Time `json:"-"`
}

// errFirstLineTooLong signals that the agent jsonl's first line exceeds the
// 32KB ReadSlice buffer, so the truncated prefix is never fed to Unmarshal.
var errFirstLineTooLong = errors.New("agent jsonl first line exceeds 32KB buffer")
