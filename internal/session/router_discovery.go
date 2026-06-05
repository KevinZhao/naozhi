// Package session router label / interrupt / discovery / takeover.
//
// Extracted from router.go on 2026-05-19 as part of the router-split
// refactor (docs/design/router-split-design.md). For history prior to
// commit 880f15f8482b51ebf4db7066583ab1b4ff18f1ba, see:
//
//	git log --follow internal/session/router.go
//
// This file holds operator-facing controls (SetUserLabel, the Interrupt
// family) and discovery integration (DiscoveryExcludeIDs, trackSessionID,
// RegisterForResume, RegisterCronStub*, ManagedExcludeSets, Takeover).
package session

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/metrics"
)

// SetUserLabel is the human-driven label setter (dashboard rename, IM
// /label commands, upstream RPC). It always records origin="user", which
// permanently locks out sysession daemon overwrites until ClearUserLabelOrigin
// resets it (RFC v2.1 §7.3).
//
// Passing an empty label clears the prior value. Callers are responsible for
// validating length/charset via ValidateUserLabel.
//
// Returns false when the session key is unknown (no mutation performed).
//
// No-op fast path: when the requested label equals the current value AND
// origin is already "user", skip the dirty flag + version bump + WS broadcast.
// R176-PERF-P1.
func (r *Router) SetUserLabel(key, label string) bool {
	return r.SetUserLabelWithOrigin(key, label, "user")
}

// SetUserLabelWithOrigin is the lower-level label writer that also records
// who set the label. origin must be "user" or "auto"; any other value is
// treated as "user" (defensive — only the human-facing API and AutoTitler
// reach this path, and accidentally widening the namespace must not silently
// mark something as daemon-overwritable).
//
// Crucially, when origin=="auto" and the current LabelOrigin is "user",
// the write is rejected (returns false). This closes the daemon-vs-user
// race window from RFC v2.1 §11.1: AutoTitler reads a Snapshot showing
// origin="auto", invokes a 5–25s LLM call, and during that window a human
// rename via SetUserLabel can flip origin to "user" — we MUST re-read
// origin atomically under r.mu before letting the daemon overwrite, or
// the user's manual edit gets silently lost.
//
// Returns false when the session key is unknown OR when the daemon-vs-user
// race-protection rejects the write.
func (r *Router) SetUserLabelWithOrigin(key, label, origin string) bool {
	if origin != "user" && origin != "auto" {
		origin = "user"
	}
	r.mu.Lock()
	s := r.sessions[key]
	if s == nil {
		r.mu.Unlock()
		return false
	}
	// Race-window close: re-read origin under the lock. Empty origin is
	// equivalent to "user" (legacy / pre-v2.1 stores), so daemons must
	// also leave those alone.
	currentOrigin := s.LabelOrigin()
	if origin == "auto" && (currentOrigin == "user" || currentOrigin == "") && s.UserLabel() != "" {
		// User had set a label (or legacy entry counts as user); daemon stops.
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
	r.storeDirty = true
	r.storeGen.Add(1)
	r.mu.Unlock()
	// Match every other mutator (Reset/Remove/ResetChat/spawnSession...): the
	// dashboard's onChange WebSocket broadcast needs a kick so the sidebar
	// refreshes instantly rather than waiting up to one poll interval. R64-GO-H1.
	r.notifyChange()
	return true
}

// ClearUserLabelOrigin clears both the LabelOrigin and the UserLabel so a
// sysession daemon (e.g. AutoTitler) can take back over. We clear the label
// AS WELL so the "empty origin = legacy/user-set" backward-compat rule in
// SetUserLabelWithOrigin remains unambiguous: legacy stores have non-empty
// label + empty origin, and that's still treated as user-set; explicit
// Clear has empty label + empty origin, and that's the daemon's signal to
// retake control.
//
// Dashboard "restore auto naming" action calls this via
// POST /api/system/labels/clear-origin (RFC v2.1 §9.3).
//
// Returns false when the session key is unknown.
func (r *Router) ClearUserLabelOrigin(key string) bool {
	r.mu.Lock()
	s := r.sessions[key]
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
	r.storeDirty = true
	r.storeGen.Add(1)
	r.mu.Unlock()
	r.notifyChange()
	return true
}

// VisitSessions iterates over all live sessions in the router, invoking fn
// for each one. fn returning false stops iteration early. The visit is
// lock-protected (RLock) so the session map cannot mutate mid-iteration,
// but each Snapshot is computed inline without leaking the *ManagedSession
// to the caller.
//
// This is the daemon-friendly read path: AutoTitler's tick filters >100
// sessions and only acts on at most one per tick, so a streaming visit
// avoids the GC overhead of the slice-returning Snapshot() variant. RFC
// v2.1 §8 / §M8.
//
// Note: fn must not call back into Router methods that take r.mu (it runs
// under RLock). Idiomatic usage is to copy the SessionSnapshot fields the
// daemon needs and resume work after VisitSessions returns.
//
// R20260602-PERF-3 (#1577): this uses snapshotReadOnly, NOT Snapshot, so
// the per-session view computed under RLock is strictly side-effect free
// (no SetModel mirror write on the read path). The dashboard poll path
// keeps the mirroring Snapshot() so live model still reaches sessions.json.
func (r *Router) VisitSessions(fn func(SessionSnapshot) bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.sessions {
		if !fn(s.snapshotReadOnly()) {
			return
		}
	}
}

// EventEntriesForKey returns the full event-log entries for the given session
// key, or nil when the key is unknown. AutoTitler uses this so the rename
// prompt can review every user turn in the conversation rather than just the
// LastPrompt cached on SessionSnapshot.
//
// Live-process branch goes through ManagedSession.EventEntries(), which itself
// prefers the live process's ring buffer and falls back to persistedHistory
// when the session is dead/suspended. r.mu is released before the read so the
// inner historyMu acquisition does not nest under r.mu.
func (r *Router) EventEntriesForKey(key string) []cli.EventEntry {
	r.mu.RLock()
	s := r.sessions[key]
	r.mu.RUnlock()
	if s == nil {
		return nil
	}
	return s.EventEntries()
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
		slog.Warn("interrupt session safe: unhandled interrupt outcome", "outcome", outcome, "key", key)
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
	outcome := s.InterruptViaControl()
	// R172-ARCH-D10: counter per outcome class. NoSession is deliberately
	// NOT counted here — that path returns early above, and a
	// key-does-not-exist lookup isn't a signal about interrupt behaviour.
	// Sent is counted so operators have a denominator for "what fraction of
	// interrupts actually reached the CLI?".
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
		key:    key,
		exempt: isExemptKey(key),
	}
	s.setWorkspace(workspace)
	s.SetCLIName(r.CLIName())
	s.SetCLIVersion(r.CLIVersion())
	s.setSessionID(sessionID)
	if lastPrompt != "" {
		storeAtomicString(&s.lastPrompt, lastPrompt)
	}
	r.trackSessionID(sessionID)
	if sessionID != "" {
		r.sessionIDToKey[sessionID] = key
	}
	s.lastActive.Store(time.Now().UnixNano())
	s.initCreatedAtIfUnset()
	// R215-ARCH-P2-2: single publish funnel.
	r.publishSessionLocked(key, s, false)
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
//
// Misuse (key not IsCronKey) panics: callsites that pass a wrong-prefix
// key would otherwise silently leave dangling no-op stubs that fail later
// in obscure ways (RFC v2.1 §8.1).
func (r *Router) RegisterCronStub(key, workspace, lastPrompt string) {
	if !IsCronKey(key) {
		panic(fmt.Sprintf("session: RegisterCronStub called with non-cron key %q", key))
	}
	r.registerStub(key, workspace, lastPrompt, nil)
}

// RegisterCronStubWithChain 在 RegisterCronStub 的基础上注入一个
// session-ID 链：stub 没有自己的 sessionID（exempt=true，无进程），但
// historySource 查 JSONL 时要用到 chain。对于 cron 任务，chain 就是
// 上一次成功执行留下的 session_id（cron.Job.LastSessionID）。没有它，
// fresh_context=true 场景每次 Reset 都会让 stub 的 chain 为空，dashboard
// 点击定时任务只能看到一个空白的事件面板。
//
// chainIDs 空 / nil 时行为与 RegisterCronStub 相同。
func (r *Router) RegisterCronStubWithChain(key, workspace, lastPrompt string, chainIDs []string) {
	if !IsCronKey(key) {
		panic(fmt.Sprintf("session: RegisterCronStubWithChain called with non-cron key %q", key))
	}
	r.registerStub(key, workspace, lastPrompt, chainIDs)
}

// RegisterSystemStub creates a suspended exempt session for a sysession
// daemon. Key format is "sys:<daemon-name>" (validated via IsSysKey;
// callsite misuse panics, mirroring RegisterCronStub).
//
// Phase 1 daemons typically do NOT register a stub — Runner-style daemons
// (AutoTitler) only spawn transient claude -p subprocesses (RFC v2.1 §6).
// This entry point is reserved for future daemons that need a long-lived
// ManagedSession (e.g. a stateful aggregator that survives across ticks).
//
// existing 分支下如果 workspace/lastPrompt 没变就 no-op（避免每 tick 强刷
// 触发不必要的 saveIfDirty + WS fanout）。
func (r *Router) RegisterSystemStub(key, workspace, lastPrompt string) {
	if !IsSysKey(key) {
		panic(fmt.Sprintf("session: RegisterSystemStub called with non-sys key %q", key))
	}
	r.registerStub(key, workspace, lastPrompt, nil)
}

// registerStub is the shared exempt-stub registration path used by
// RegisterCronStub / RegisterCronStubWithChain / RegisterSystemStub.
// Callers must validate the key namespace before invoking this; the
// helper itself does no namespace check (its single shared implementation
// is intentionally namespace-agnostic so cron and sys can co-evolve their
// public wrappers without a fork in this body — RFC v2.1 §8.1).
//
// existing 分支下如果新 chain 与旧 chain 不同，会同步刷新 prevSessionIDs
// 并重挂 historySource，保证 cron 每次执行完 recordResult 后侧边栏立刻
// 能查到最新一次的 JSONL（而不是等下次重启）。
//
// prevSessionIDs 的所有历史写路径（spawnSession / RenameSession / 本函数
// new 分支）都在 r.mu 下做，读路径同样在 r.mu 下。managed.go:SnapshotChainIDs
// 虽然用 historyMu.RLock，但因为写者不拿 historyMu，historyMu 对该字段
// 而言并不构成真正的同步——真正的 invariant 是"r.mu 写/r.mu 读"。因此
// chain 刷新直接在 r.mu 临界区内做，与其它写路径一致，不引入混合锁协议；
// attachHistorySource 只读 r 的不可变字段 + 写 s 的 atomic.Pointer，同样
// 安全可以在 r.mu 下调。
func (r *Router) registerStub(key, workspace, lastPrompt string, chainIDs []string) {
	r.mu.Lock()
	if existing, ok := r.sessions[key]; ok {
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
				// R230C-GO-1 hardening (#1777): route the chain swap through
				// the historyMu-guarded setter rather than a bare write under
				// r.mu alone. existing is already published, so SnapshotChainIDs
				// (invoked from cli history-source factories that do NOT hold
				// r.mu, only historyMu.RLock) can race this refresh; writing
				// under historyMu makes that RLock a real happens-before instead
				// of the "defensive rope" the SnapshotChainIDs doc describes.
				existing.ReplacePrevSessionIDs(chainIDs)
				// workspace 变了 historySource 里也要刷（cwd 变化会导致
				// projDirName 命中不同的 claude 项目目录）；一并重装最省心。
				r.attachHistorySource(existing)
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
	// R242-ARCH-2 (#720): the per-namespace exempt sub-quota gate lives in
	// spawnSession, but stub registration creates an exempt entry WITHOUT
	// spawning a process, so it never crosses that gate. A misbehaving cron
	// scheduler (or a config with many jobs / projects) could therefore grow
	// r.sessions with exempt stubs past the bucket's intended ceiling and
	// silently starve the other namespaces' alive-spawn budget. We do not
	// hard-reject here (dropping a cron/sys stub would lose its history-chain
	// and break dashboard panels), but we surface the over-quota condition so
	// the exhaustion the issue flags is observable instead of invisible.
	if kind := exemptKind(key); kind != "" {
		if existing := r.countExemptByKind(kind); existing >= exemptCapFor(kind) {
			slog.Warn("exempt stub registration exceeds namespace sub-quota",
				"key", key, "namespace", kind,
				"existing", existing, "cap", exemptCapFor(kind))
		}
	}
	s := &ManagedSession{
		key:    key,
		exempt: true,
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
	// R215-ARCH-P2-2: single publish funnel.
	r.publishSessionLocked(key, s, false)
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
	if err := validateBackend(opts.Backend); err != nil {
		return nil, err
	}
	r.mu.Lock()
	// If key already exists (e.g. re-takeover same CWD), close the old process
	if s, ok := r.sessions[key]; ok {
		// Decrement deltas mirror the resetLocked pattern: only sessions
		// that were non-exempt AND alive contributed to activeCount, so
		// only those need a -1. R220-PERF-1 fast path: replaces an O(n)
		// countActive() recount that fired even when at most one session
		// transitioned, which scaled poorly past ~500 sessions on Takeover
		// hot paths.
		if p := s.loadProcess(); p != nil && p.Alive() {
			oldSession := s
			proc := p
			oldBackend := s.Backend()
			oldExempt := s.exempt
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
				if !oldExempt {
					if r.activeCount.Add(-1) < 0 {
						r.activeCount.Store(0)
					}
					metrics.RecordSessionActive(oldBackend, -1)
				}
			} else if cur != nil && cur.isAlive() {
				// Concurrent GetOrCreate created a new session during Close();
				// abort takeover rather than silently returning wrong session.
				r.mu.Unlock()
				return nil, fmt.Errorf("concurrent session created for key %s during takeover", key)
			}
			// Implicit else: concurrent goroutine replaced the session with an exited
			// one. Leave r.sessions[key] as-is — spawnSession below will overwrite
			// it and call indexAdd, keeping the index consistent. No indexDel here
			// because we are not removing from r.sessions. The activeCount delta
			// is also skipped because spawnSession will Store(+1) for the new
			// process if applicable.
		} else {
			// Dead session branch: same keepBackendOverride=true rationale.
			// Dead sessions weren't in activeCount, so no decrement is needed.
			r.unregisterSessionLocked(key, s, true)
			r.storeDirty = true
			r.storeGen.Add(1)
		}
	}
	// Set workspace override for the chat key prefix. Must bump the dirty
	// flag so the override is persisted; otherwise a crash before another
	// flushing path fires would lose the takeover's chosen workspace.
	if chatKey := chatKeyFor(key); chatKey != key {
		if prev, ok := r.wsStore.overrides[chatKey]; !ok || prev != workspace {
			r.wsStore.overrides[chatKey] = workspace
			r.wsStore.dirty = true
			r.wsStore.gen.Add(1)
		}
	}
	s, err := r.spawnSession(ctx, key, sessionID, opts)
	if err != nil {
		return nil, err
	}
	return s, nil
}
