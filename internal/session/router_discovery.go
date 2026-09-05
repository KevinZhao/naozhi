// Package session router label / interrupt / discovery / takeover.
//
// This file holds operator-facing controls (SetUserLabel, the Interrupt
// family) and discovery integration (DiscoveryExcludeIDs, RegisterForResume,
// RegisterCronStub*, ManagedExcludeSets, Takeover).
package session

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/metrics"
)

// SetUserLabel is the human-driven label setter (dashboard rename, IM /label,
// upstream RPC). It records origin="user", which locks out sysession daemon
// overwrites until ClearUserLabelOrigin resets it (RFC v2.1 §7.3). An empty
// label clears the prior value. Returns false when the key is unknown.
func (r *Router) SetUserLabel(key, label string) bool {
	return r.SetUserLabelWithOrigin(key, label, "user")
}

// SetUserLabelWithOrigin is the lower-level label writer that also records who
// set the label. origin must be "user" or "auto"; anything else is treated as
// "user" so a widened namespace cannot silently mark a label daemon-overwritable.
// When origin=="auto" and the current LabelOrigin is "user", the write is
// rejected: AutoTitler reads a Snapshot, then spends 5–25s in an LLM call, and
// a human rename in that window flips origin to "user", so origin MUST be
// re-read under r.mu before the daemon overwrites (RFC v2.1 §11.1). Returns
// false when the key is unknown or the write is rejected.
func (r *Router) SetUserLabelWithOrigin(key, label, origin string) bool {
	if origin != "user" && origin != "auto" {
		origin = "user"
	}
	r.mu.Lock()
	s := r.ss.sessions[key]
	if s == nil {
		r.mu.Unlock()
		return false
	}
	// Re-read origin under the lock. Empty origin is equivalent to "user"
	// (stores without an origin field), so daemons must leave those alone too.
	currentOrigin := s.LabelOrigin()
	if origin == "auto" && (currentOrigin == "user" || currentOrigin == "") && s.UserLabel() != "" {
		r.mu.Unlock()
		return false
	}
	// No-op fast path: same label and same origin → don't dirty the store.
	if s.UserLabel() == label && currentOrigin == origin {
		r.mu.Unlock()
		return true
	}
	s.SetUserLabel(label)
	s.setLabelOrigin(origin)
	r.ss.dirty = true
	r.ss.gen.Add(1)
	r.mu.Unlock()
	// Kick the dashboard's onChange WS broadcast like every other mutator.
	r.notifyChange()
	return true
}

// ClearUserLabelOrigin clears both LabelOrigin and UserLabel so a sysession
// daemon (e.g. AutoTitler) can take back over. The label is cleared AS WELL so
// the "empty origin = user-set" rule in SetUserLabelWithOrigin stays
// unambiguous: non-empty label + empty origin is user-set; empty label + empty
// origin is the daemon's signal to retake control (RFC v2.1 §9.3).
// Returns false when the session key is unknown.
func (r *Router) ClearUserLabelOrigin(key string) bool {
	r.mu.Lock()
	s := r.ss.sessions[key]
	if s == nil {
		r.mu.Unlock()
		return false
	}
	if s.LabelOrigin() == "" && s.UserLabel() == "" {
		r.mu.Unlock()
		return true // already cleared, no-op
	}
	s.SetUserLabel("")
	s.setLabelOrigin("")
	r.ss.dirty = true
	r.ss.gen.Add(1)
	r.mu.Unlock()
	r.notifyChange()
	return true
}

// VisitSessions iterates over all live sessions, invoking fn for each; fn
// returning false stops early. The visit runs under RLock so the map cannot
// mutate mid-iteration, and each snapshot is computed inline without leaking
// the *ManagedSession (RFC v2.1 §8). fn must not call back into Router methods
// that take r.mu. It uses snapshotReadOnly, NOT Snapshot, so the view computed
// under RLock is side-effect free (no SetModel mirror write) (#1577).
func (r *Router) VisitSessions(fn func(SessionSnapshot) bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.ss.sessions {
		if !fn(s.snapshotReadOnly()) {
			return
		}
	}
}

// EventEntriesForKey returns the full event-log entries for the given session
// key, or nil when the key is unknown (AutoTitler reviews every user turn).
// r.mu is released before the read so the inner historyMu acquisition does
// not nest under r.mu.
func (r *Router) EventEntriesForKey(key string) []clievent.EventEntry {
	r.mu.RLock()
	s := r.ss.sessions[key]
	r.mu.RUnlock()
	if s == nil {
		return nil
	}
	return s.EventEntries()
}

// EventEntriesForKeyAppend is the buffer-reusing variant of EventEntriesForKey:
// it appends the keyed session's event log onto dst and returns the grown slice,
// or dst unchanged when the key is unknown (#1885). Ownership matches
// ManagedSession.EventEntriesAppend: the caller must not retain dst across
// calls; the returned slice shares dst's backing array.
func (r *Router) EventEntriesForKeyAppend(dst []clievent.EventEntry, key string) []clievent.EventEntry {
	r.mu.RLock()
	s := r.ss.sessions[key]
	r.mu.RUnlock()
	if s == nil {
		return dst
	}
	return s.EventEntriesAppend(dst)
}

// InterruptSession sends SIGINT to the CLI process for the given session key.
// Returns true if the session was found and interrupted.
// WARNING: SIGINT terminates the whole CLI process on Claude `-p` mode, killing
// the live shim conversation. Prefer InterruptSessionSafe for operator-facing
// actions; this is for process-level signalling and the fallback branch.
func (r *Router) InterruptSession(key string) bool {
	r.mu.RLock()
	s := r.ss.sessions[key]
	r.mu.RUnlock()
	if s == nil {
		return false
	}
	return s.Interrupt()
}

// InterruptSessionSafe is the preferred entry point for dashboard/HTTP/WS
// interrupt requests. It first tries the in-band stream-json control_request
// path, which aborts the active turn WITHOUT terminating the CLI subprocess,
// so shim, socket and session ID survive. It falls back to SIGINT only for
// InterruptUnsupported (ACP has no stdin-level interrupt and does not exit on
// signal). InterruptNoTurn does NOT fall back: SIGINT on an idle Claude `-p`
// subprocess terminates it, so an idle press must report "nothing was running".
// InterruptError does NOT fall back either: the shim socket is almost certainly
// broken and SIGINT would travel the same transport; surfacing the error lets
// the reconcile path purge the zombie.
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
		// HTTP/WS handlers map {InterruptNoTurn, InterruptError} to "not_running".
		return outcome
	default:
		// Unhandled enum value: map to InterruptNoSession so the dashboard shows
		// "not_running" instead of an unrenderable outcome.
		slog.Warn("interrupt session safe: unhandled interrupt outcome", "outcome", outcome, "key", key)
		return InterruptNoSession
	}
}

// InterruptSessionViaControl requests the CLI to abort the active turn via the
// stream-json control_request protocol (no SIGINT, no process kill). The
// in-flight Send() observes the CLI's natural result event and returns
// normally, so the dispatch owner loop keeps the session and can process
// queued follow-ups on the same live CLI. Alive-but-idle returns
// InterruptNoTurn, not InterruptNoSession.
func (r *Router) InterruptSessionViaControl(key string) InterruptOutcome {
	r.mu.RLock()
	s := r.ss.sessions[key]
	r.mu.RUnlock()
	if s == nil {
		return InterruptNoSession
	}
	outcome := s.InterruptViaControl()
	// NoSession is deliberately NOT counted (an unknown key says nothing about
	// interrupt behaviour); Sent gives operators the denominator.
	switch outcome {
	case InterruptSent:
		metrics.InterruptSentTotal.Add(1)
	case InterruptNoTurn:
		metrics.InterruptNoTurnTotal.Add(1)
	case InterruptUnsupported:
		metrics.InterruptUnsupportedTotal.Add(1)
	case InterruptError:
		metrics.InterruptErrorTotal.Add(1)
	}
	return outcome
}

// DiscoveryExcludeIDs returns session IDs to exclude from filesystem discovery.
// Only sessions with a running process are excluded to prevent duplicates;
// suspended sessions are allowed through so their session files appear in the
// history popover (deduplicated against the workspace).
func (r *Router) DiscoveryExcludeIDs() map[string]bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make(map[string]bool, len(r.ss.sessions))
	for _, s := range r.ss.sessions {
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

// RegisterForResume creates a suspended session entry so that the next
// GetOrCreate call for this key will resume the given session ID.
// If another session already targets the same sessionID, the existing key
// is returned (deduplication) and no new entry is created.
func (r *Router) RegisterForResume(key, sessionID, workspace, lastPrompt string) (effectiveKey string) {
	r.mu.Lock()
	if _, exists := r.ss.sessions[key]; exists {
		r.mu.Unlock()
		return key // already exists with this exact key
	}
	// Deduplicate: if another session already targets this sessionID, reuse it.
	if existingKey, ok := r.ss.idToKey[sessionID]; ok {
		if existing, exists := r.ss.sessions[existingKey]; exists {
			// idToKey is not cleaned for rotated IDs, so idToKey[sessionID]=K can
			// dangle while sessions[K] holds an UNRELATED session (key reused).
			// Only dedup when the found session genuinely owns this sessionID;
			// a blind reuse would cross-session bleed (#2093).
			if slices.Contains(existing.SnapshotChainIDs(), sessionID) {
				r.mu.Unlock()
				return existingKey
			}
		}
		// Stale or leaked index entry; clean up and continue.
		delete(r.ss.idToKey, sessionID)
	}
	s := &ManagedSession{
		key:      key,
		exempt:   isExemptKey(key),
		runStore: r.sessionRuns,
		costAcct: r.costAcct,
	}
	s.setWorkspace(workspace)
	s.SetCLIName(r.CLIName())
	s.SetCLIVersion(r.CLIVersion())
	s.setSessionID(sessionID)
	if lastPrompt != "" {
		storeAtomicString(&s.lastPrompt, lastPrompt)
	}
	r.kid.Track(sessionID)
	if sessionID != "" {
		r.ss.idToKey[sessionID] = key
	}
	s.lastActive.Store(time.Now().UnixNano())
	s.initCreatedAtIfUnset()
	r.publishSessionLocked(key, s, false)
	r.ss.dirty = true
	r.ss.gen.Add(1)
	r.mu.Unlock()

	r.notifyChange()
	return key
}

// RegisterCronStub creates a suspended exempt session for a cron job so the
// job appears in the dashboard before its first execution. Key format is
// "cron:<jobID>"; an existing entry has workspace/lastPrompt refreshed in
// place. The stub has no process or session ID; the first GetOrCreate reuses
// it. A non-cron key panics rather than leaving a dangling no-op stub (RFC v2.1 §8.1).
func (r *Router) RegisterCronStub(key, workspace, lastPrompt string) {
	if !IsCronKey(key) {
		panic(fmt.Sprintf("session: RegisterCronStub called with non-cron key %q", key))
	}
	r.registerStub(key, workspace, lastPrompt, nil)
}

// RegisterCronStubWithChain 在 RegisterCronStub 的基础上注入 session-ID 链：
// stub 没有自己的 sessionID（exempt=true，无进程），但 historySource 查 JSONL
// 要用 chain（cron 即上一次成功执行的 cron.Job.LastSessionID），否则
// fresh_context=true 每次 Reset 后 dashboard 只能看到空白事件面板。
// chainIDs 空 / nil 时行为与 RegisterCronStub 相同。
func (r *Router) RegisterCronStubWithChain(key, workspace, lastPrompt string, chainIDs []string) {
	if !IsCronKey(key) {
		panic(fmt.Sprintf("session: RegisterCronStubWithChain called with non-cron key %q", key))
	}
	r.registerStub(key, workspace, lastPrompt, chainIDs)
}

// RegisterSystemStub creates a suspended exempt session for a sysession daemon
// that needs a long-lived ManagedSession (RFC v2.1 §6). Key format is
// "sys:<daemon-name>"; misuse panics, mirroring RegisterCronStub.
// existing 分支下如果 workspace/lastPrompt 没变就 no-op（避免每 tick 强刷
// 触发不必要的 saveIfDirty + WS fanout）。
func (r *Router) RegisterSystemStub(key, workspace, lastPrompt string) {
	if !IsSysKey(key) {
		panic(fmt.Sprintf("session: RegisterSystemStub called with non-sys key %q", key))
	}
	r.registerStub(key, workspace, lastPrompt, nil)
}

// registerStub is the shared, namespace-agnostic exempt-stub registration path
// for RegisterCronStub* / RegisterSystemStub; callers validate the key
// namespace (RFC v2.1 §8.1). existing 分支下新 chain 与旧 chain 不同时同步刷新
// prevSessionIDs 并重挂 historySource，保证 cron recordResult 后侧边栏立刻
// 能查到最新 JSONL。
//
// prevSessionIDs 的所有写路径都在 r.mu 下做，读路径同样在 r.mu 下；
// SnapshotChainIDs 的 historyMu.RLock 对该字段不构成真正同步，invariant 是
// "r.mu 写/r.mu 读"，chain 刷新因此在 r.mu 临界区内做。attachHistorySource
// 只读 r 的不可变字段 + 写 s 的 atomic.Pointer，在 r.mu 下调用同样安全。
func (r *Router) registerStub(key, workspace, lastPrompt string, chainIDs []string) {
	r.mu.Lock()
	if existing, ok := r.ss.sessions[key]; ok {
		changed := false
		// Refresh workspace/prompt on existing stub; don't touch live process.
		if existing.loadProcess() == nil {
			if workspace != "" && existing.Workspace() != workspace {
				existing.setWorkspace(workspace)
				changed = true
			}
			if lastPrompt != "" && loadAtomicString(&existing.lastPrompt) != lastPrompt {
				storeAtomicString(&existing.lastPrompt, lastPrompt)
				changed = true
			}
			if len(chainIDs) > 0 && !slices.Equal(existing.prevSessionIDs, chainIDs) {
				// existing is already published, so SnapshotChainIDs (holding only
				// historyMu.RLock) can race this refresh; the historyMu-guarded
				// setter makes that RLock a real happens-before (#1777).
				existing.ReplacePrevSessionIDs(chainIDs)
				// workspace 变了 historySource 里也要刷（cwd 变化会导致
				// projDirName 命中不同的 claude 项目目录）。
				r.attachHistorySource(existing)
				changed = true
			}
			// Only mark dirty when something changed: the cron scheduler
			// re-registers stubs on every cron.yaml reload, and an unconditional
			// dirty would force a saveIfDirty fsync + WS fanout for nothing.
			if changed {
				r.ss.dirty = true
				r.ss.gen.Add(1)
			}
		}
		r.mu.Unlock()
		// Always notify on refresh so the sidebar edit flow gets an immediate
		// WS kick; notifyChange is cheap, saveIfDirty is what the gate guards.
		r.notifyChange()
		return
	}
	// The per-namespace exempt sub-quota gate lives in spawnSession; stub
	// registration creates an exempt entry WITHOUT spawning and never crosses
	// it, so it could starve other namespaces' alive-spawn budget. Not
	// hard-rejected (dropping a stub loses its history chain); surfaced (#720).
	if kind := exemptKind(key); kind != "" {
		if existing := r.countExemptByKind(kind); existing >= exemptCapFor(kind) {
			slog.Warn("exempt stub registration exceeds namespace sub-quota",
				"key", key, "namespace", kind,
				"existing", existing, "cap", exemptCapFor(kind))
		}
	}
	s := &ManagedSession{
		key:      key,
		exempt:   true,
		runStore: r.sessionRuns,
		costAcct: r.costAcct,
	}
	if len(chainIDs) > 0 {
		s.prevSessionIDs = slices.Clone(chainIDs)
	}
	s.setWorkspace(workspace)
	s.SetCLIName(r.CLIName())
	s.SetCLIVersion(r.CLIVersion())
	if lastPrompt != "" {
		storeAtomicString(&s.lastPrompt, lastPrompt)
	}
	s.lastActive.Store(time.Now().UnixNano())
	s.initCreatedAtIfUnset()
	r.publishSessionLocked(key, s, false)
	r.ss.dirty = true
	r.ss.gen.Add(1)
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
	for _, s := range r.ss.sessions {
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
	// Same flag-injection guard as GetOrCreate: AgentOpts is caller-supplied.
	if err := validateModel(opts.Model); err != nil {
		return nil, err
	}
	if err := validateBackend(opts.Backend); err != nil {
		return nil, err
	}
	r.mu.Lock()
	// If key already exists (e.g. re-takeover same CWD), close the old process
	if s, ok := r.ss.sessions[key]; ok {
		// Mirror resetLocked: only non-exempt AND alive sessions contributed to
		// activeCount, so only those get a -1 (no O(n) countActive recount).
		if p := s.loadProcess(); p != nil && p.Alive() {
			oldSession := s
			proc := p
			oldBackend := s.Backend()
			oldExempt := s.exempt
			r.mu.Unlock()
			proc.Close()
			// spawnSession below will StartShim against the same socket path;
			// wait for the shim to release it (same race as Reset).
			waitSocketGoneForKey(key, 2*time.Second)
			r.mu.Lock()
			// Only delete if no concurrent goroutine replaced this session.
			// keepBackendOverride=true: Takeover re-spawns on the same key
			// and spawnSession below consumes the override atomically.
			if cur, ok := r.ss.sessions[key]; ok && cur == oldSession {
				r.unregisterSessionLocked(key, cur, true)
				r.ss.dirty = true
				r.ss.gen.Add(1)
				if !oldExempt {
					if r.ss.activeCount.Add(-1) < 0 {
						r.ss.activeCount.Store(0)
					}
					metrics.RecordSessionActive(oldBackend, -1)
				}
			} else if cur != nil && cur.isAlive() {
				// Concurrent GetOrCreate created a new session during Close();
				// abort takeover rather than silently returning wrong session.
				r.mu.Unlock()
				return nil, fmt.Errorf("concurrent session created for key %s during takeover", key)
			}
			// Implicit else: a concurrent goroutine replaced the session with an
			// exited one. Leave it — spawnSession below overwrites it, calls
			// indexAdd and Stores +1 if applicable, so no indexDel/delta here.
		} else {
			// Dead session branch: same keepBackendOverride=true rationale.
			// Dead sessions weren't in activeCount, so no decrement is needed.
			r.unregisterSessionLocked(key, s, true)
			r.ss.dirty = true
			r.ss.gen.Add(1)
		}
	}
	// Workspace override for the chat key prefix. Adopt marks the store dirty
	// (only when changed) so the override survives a crash before another flush.
	if chatKey := chatKeyFor(key); chatKey != key {
		r.wsStore.Adopt(chatKey, workspace)
	}
	s, err := r.spawnSession(ctx, key, sessionID, opts)
	if err != nil {
		return nil, err
	}
	return s, nil
}
