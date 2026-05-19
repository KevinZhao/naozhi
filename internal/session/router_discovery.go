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

	"github.com/naozhi/naozhi/internal/metrics"
)

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
		slog.Warn("InterruptSessionSafe: unhandled interrupt outcome", "outcome", outcome, "key", key)
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
//
// 等价于 RegisterCronStubWithChain(key, workspace, lastPrompt, nil)，
// 保留给不关心 history chain 的调用方（测试、旧集成）。
func (r *Router) RegisterCronStub(key, workspace, lastPrompt string) {
	r.RegisterCronStubWithChain(key, workspace, lastPrompt, nil)
}

// RegisterCronStubWithChain 在 RegisterCronStub 的基础上注入一个
// session-ID 链：stub 没有自己的 sessionID（exempt=true，无进程），但
// historySource 查 JSONL 时要用到 chain。对于 cron 任务，chain 就是
// 上一次成功执行留下的 session_id（cron.Job.LastSessionID）。没有它，
// fresh_context=true 场景每次 Reset 都会让 stub 的 chain 为空，dashboard
// 点击定时任务只能看到一个空白的事件面板。
//
// chainIDs 空 / nil 时行为与 RegisterCronStub 相同。existing 分支下如果
// 新 chain 与旧 chain 不同，会同步刷新 prevSessionIDs 并重挂
// historySource，保证 cron 每次执行完 recordResult 后侧边栏立刻能查到
// 最新一次的 JSONL（而不是等下次重启）。
func (r *Router) RegisterCronStubWithChain(key, workspace, lastPrompt string, chainIDs []string) {
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
			// prevSessionIDs 的所有历史写路径（spawnSession:1786 / RenameSession:2142
			// / 本函数 new 分支:3259）都在 r.mu 下做，读路径（961 / 1722 / 3083
			// 以及下一行）也全部在 r.mu 下。managed.go:SnapshotChainIDs 虽然用
			// historyMu.RLock，但因为写者不拿 historyMu，historyMu 对该字段
			// 而言并不构成真正的同步——真正的 invariant 是"r.mu 写/r.mu 读"。
			// 因此 chain 刷新直接在 r.mu 临界区内做，与其它写路径一致，不引入
			// 混合锁协议；attachHistorySource 只读 r 的不可变字段 + 写 s 的
			// atomic.Pointer，同样安全可以在 r.mu 下调。
			if len(chainIDs) > 0 && !slices.Equal(existing.prevSessionIDs, chainIDs) {
				existing.prevSessionIDs = slices.Clone(chainIDs)
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
	if err := validateBackend(opts.Backend); err != nil {
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
