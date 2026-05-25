// Package session router lifecycle methods.
//
// Extracted from router.go on 2026-05-19 as part of the router-split
// refactor (docs/design/router-split-design.md). For history prior to
// commit 74085ac923b4d0153ae968e1b9a01e075afb7200, see:
//
//	git log --follow internal/session/router.go
//
// This file holds session lifecycle: GetOrCreate / spawn / Reset / Rename /
// workspace overrides / history wiring. router.go retains the Router struct
// definition, NewRouter, and infrastructure helpers (panicSafeSpawn etc.).
//
// All methods here read/write fields on Router (defined in router.go). Lock
// contracts are documented per-method via "// LOCK:" annotations on `*Locked`
// suffix functions.
package session

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
	"github.com/naozhi/naozhi/internal/history"
	"github.com/naozhi/naozhi/internal/history/merged"
	"github.com/naozhi/naozhi/internal/history/naozhilog"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/shim"
)

// attachHistorySource picks the right history.Source for a session based on
// its backend ID and installs it. Called immediately after every
// ManagedSession allocation in this file so EventEntriesBeforeCtx's disk
// fallback is live before the first pagination request can arrive.
//
// Composition (RFC §3.4 / §3.5):
//   - The local tier is naozhilog.Source (empty when eventLogDir is
//     unset). It reads naozhi-native per-session logs that carry full
//     EventEntry fidelity including Images, ImagePaths, and agent-team
//     linkage.
//   - The fallback tier comes from the backend's *cli.Wrapper via
//     Wrapper.NewHistorySource (Sprint 1a). The wrapper holds a
//     per-backend factory that knows which on-disk format to read —
//     claudejsonl and kirojsonl are both registered via blank imports
//     in router_core.go — so adding a new backend's history reader
//     does not require an edit here. Unknown / unregistered backends
//     degrade to NoopHistorySource. R228-CR-P3-3.
//   - MergedSource wraps both tiers and returns a UUID-deduped,
//     time-sorted result. Skipping the merge when the local tier is
//     disabled keeps the old single-source path live for deployments
//     that opt out.
func (r *Router) attachHistorySource(s *ManagedSession) {
	if s == nil {
		return
	}
	backend := s.Backend()
	if backend == "" {
		backend = r.defaultBackend
	}

	// Resolve the wrapper for this backend. wrappers may be nil (legacy
	// single-wrapper deployments) and an unknown backend ID falls back to
	// the router's default wrapper so a misconfigured Backend() still
	// gets a usable source instead of silently routing to Noop.
	wrapper := r.wrappers[backend]
	if wrapper == nil {
		wrapper = r.wrapper
	}

	deps := cli.HistoryWiring{
		ClaudeDir:       r.claudeDir,
		KiroSessionsDir: r.kiroSessionsDir,
		EventLogDir:     r.eventLogDir,
	}

	// Wrapper.NewHistorySource is nil-safe and never returns nil; the
	// extra guard below pins the contract at the boundary so future
	// refactors of the cli package can't silently regress callers here.
	var fallback history.Source = wrapper.NewHistorySource(s, deps)
	if fallback == nil {
		fallback = history.Noop{}
	}

	if r.eventLogDir == "" {
		// Event log persistence opted out — old single-source behaviour.
		s.SetHistorySource(fallback)
		return
	}

	s.SetHistorySource(&merged.Source{
		Local:    naozhilog.New(r.eventLogDir, s.key),
		Fallback: fallback,
	})
}

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

// ResetChat resets all sessions belonging to a chat (all agents).
func (r *Router) ResetChat(chatKeyPrefix string) {
	r.mu.Lock()
	var toClose []processIface
	var closedActive int
	if r.sessionsByChat != nil {
		// O(k) path via index (k = agents per chat, typically 1-3).
		// resetSessionLocked deletes from r.sessions only; we drop the
		// whole index entry below. Iteration order over a map set is not
		// guaranteed but each resetSessionLocked is independent. R226-CR-15.
		for key := range r.sessionsByChat[chatKeyPrefix] {
			r.resetSessionLocked(key, &toClose, &closedActive)
		}
		delete(r.sessionsByChat, chatKeyPrefix)
	} else {
		// Fallback O(n) scan for test-created routers without index.
		// Pre-compute the prefix once so the loop body doesn't re-allocate
		// `chatKeyPrefix + ":"` on every iteration.
		prefix := chatKeyPrefix + ":"
		var toDelete []string
		for key := range r.sessions {
			if len(key) > len(chatKeyPrefix) && key[:len(prefix)] == prefix {
				toDelete = append(toDelete, key)
			}
		}
		for _, key := range toDelete {
			r.resetSessionLocked(key, &toClose, &closedActive)
		}
	}
	if closedActive > 0 {
		newCount := r.activeCount.Add(-int64(closedActive))
		if newCount < 0 {
			r.activeCount.Store(0)
		}
		// Multi-Backend RFC §10 (Sprint 6a): reconcile the per-backend
		// labeled gauge against the residual sessions. Per-key Dec
		// instrumentation in the loop above would require carrying each
		// session's backend through toClose; the batched recount is
		// O(n) over r.sessions but n is bounded (~100s) and only runs
		// on chat prefix reset which is rare (user /reset action).
		r.reconcileSessionActiveByBackendLocked()
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
	// R191-CONC-H1-e: Broadcast under r.mu (see evictOldest comment).
	//
	// R242-ARCH-25: this is two distinct locked sections by design — the
	// first (above) commits the routing-table mutations, then we drop the
	// lock so proc.Close() (which can block on shim socket teardown,
	// goroutine joins, etc.) does not pin every other Router caller. Only
	// AFTER Close() flips IsRunning() to false is it safe to wake
	// shutdownCond waiters: they re-evaluate the predicate
	// `loadProcess().IsRunning() == false`, so broadcasting before Close
	// returns is a missed-wakeup window (waiter sees true, sleeps again,
	// never re-woken). The same Unlock→Close→relock-Broadcast pattern is
	// used by evictOldest (line ~1075) and resetSessionLocked-style
	// callers — keep the shape consistent so reviewers can pattern-match.
	if r.shutdownCond != nil {
		r.mu.Lock()
		r.shutdownCond.Broadcast()
		r.mu.Unlock()
	}

	r.notifyChange()
}

// resetSessionLocked tears down a single session for ResetChat: closes any
// live process (caller will Close() outside r.mu via toClose), drops the
// session's record + sessionID and backend-override mappings, and bumps
// closedActive when the session counted toward maxProcs. Caller MUST hold
// r.mu and is responsible for cleaning up r.sessionsByChat (the indexed and
// fallback paths drop their own bookkeeping in distinct ways). R226-CR-15.
func (r *Router) resetSessionLocked(key string, toClose *[]processIface, closedActive *int) {
	s := r.sessions[key]
	if s == nil {
		return
	}
	if p := s.loadProcess(); p != nil && p.Alive() {
		*toClose = append(*toClose, p)
		if !s.exempt {
			*closedActive++
		}
	}
	if id := s.getSessionID(); id != "" {
		delete(r.sessionIDToKey, id)
	}
	delete(r.sessions, key)
	// Drop any per-session backend pick queued via SetSessionBackend. Without
	// this, an abandoned dashboard "choose backend" pick for a key that is
	// then reset leaks an entry into backendOverrides that is only cleared by
	// a later spawnSession for the same key, which may never happen.
	delete(r.backendOverrides, key)
}

// AgentOpts provides per-agent overrides for session creation.
//
// ExtraArgs aliasing contract:  callers receiving AgentOpts from
// KeyResolver get a freshly-cloned ExtraArgs (safe to append).  Callers
// populating AgentOpts to feed the router should treat ExtraArgs as
// owned exclusively by them — do NOT keep aliases to slices held by
// other goroutines (R215-ARCH-P2-8 / R37-CONCUR1).
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
	// Same boundary guard for Backend: flows into slog attrs and persisted
	// state JSON. wrapperFor already tolerates unknown backends by falling
	// back to the default wrapper; this gate only rejects shape-invalid
	// input (control chars, whitespace, overlength).
	if err := validateBackend(opts.Backend); err != nil {
		return nil, 0, err
	}
	r.mu.Lock()

	// Passthrough exposes a concurrency pattern that the old Send path never
	// did: N goroutines call GetOrCreate on the same key simultaneously for a
	// fresh (not-yet-existing) session. Without coordination each goroutine
	// calls spawnSession → wrapper.Spawn → shim.StartShim and only one wins
	// the shim-socket dial guard; the rest fail with "refusing to clobber".
	// Drop and retry while spawnSession for this key is in flight so the late
	// callers just pick up the session the winner creates.
	//
	// R243-ARCH-4: wait via the per-spawn done-channel that spawnSession
	// closes from its defer instead of a 20ms tick poll. Late callers wake
	// the instant the winner finishes (success or failure) rather than after
	// the next tick — typical shim spawn is 100-300ms, so removing the poll
	// drops the late caller's wakeup latency from ~10-20ms (half a tick) to
	// near-zero. Also frees one *time.Timer alloc per waiter.
	for {
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
		ch, inflight := r.spawningKeys[key]
		if !inflight {
			break
		}
		// Someone else is spawning this key right now. Release the router
		// mutex and wait for them to finish; spawnSession's defer closes
		// `ch` after deleting the map entry, so the next loop iteration
		// either picks up the freshly installed r.sessions[key]
		// (SessionExisting / SessionResumed) or — on spawn failure — falls
		// through to spawn its own.
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-ch:
		}
		r.mu.Lock()
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

// spawnParams carries the pure-computation output of resolveSpawnParamsLocked:
// the merged backend, model, args, workspace, and (possibly downgraded)
// resumeID that spawnSession feeds into cli.SpawnOptions. Extracting this
// struct keeps spawnSession's branching narrow and lets the merge rules be
// table-tested in isolation (R70-ARCH-H2).
type spawnParams struct {
	BackendID string // effective backend ID after override/fallback resolution (may differ from opts.Backend)
	Wrapper   *cli.Wrapper
	Model     string
	Args      []string
	Workspace string
	// ResumeID after workspace/jsonl guard. Empty means "spawn fresh".
	ResumeID string
}

// resolveSpawnParamsLocked computes the merged spawn parameters for a new
// session. The caller MUST hold r.mu (write lock) because this reads
// r.backendOverrides, r.workspaceOverrides, r.sessions and mutates
// r.backendOverrides (consuming the one-shot dashboard pick).
//
// Pure-ish: no I/O except resolveResumeID's jsonl stat. No log output, no
// process spawn — a test can exercise the merge rules without standing up
// wrappers or filesystems beyond what resolveResumeID already needs.
//
// LOCK: caller must hold r.mu for writing.
func (r *Router) resolveSpawnParamsLocked(key, resumeID string, opts AgentOpts) spawnParams {
	// Backend pick precedence (highest to lowest):
	//  1. AgentOpts.Backend                — explicit per-request choice
	//  2. one-shot r.backendOverrides[key] — dashboard "pick backend"
	//  3. existing r.sessions[key].Backend — resume continuity
	//  4. r.defaultBackend (via wrapperFor) — router fallback
	//
	// The override is consumed so a later Reset→spawn for the same key does
	// not silently carry the old pick. The session-backend fallback closes
	// a kiro→cc downgrade bug: a kiro session whose CLI process exited
	// (TTL idle, ACP transport drop) but whose ManagedSession is still in
	// r.sessions would, on the next message, call back through GetOrCreate
	// → spawnSession with an empty opts.Backend and a one-shot override
	// already consumed by the first spawn. Without the existing-session
	// fallback the second spawn picks r.defaultBackend (typically claude),
	// resolveResumeID then ENOENTs the kiro session_id under
	// ~/.claude/projects/, downgrades resume to "fresh", and the dashboard
	// silently flips the backend chip from kiro→cc — losing both the
	// conversation and the operator's original pick.
	reqBackend := opts.Backend
	if len(r.backendOverrides) > 0 {
		if reqBackend == "" {
			reqBackend = r.backendOverrides[key]
		}
		delete(r.backendOverrides, key)
	}
	if reqBackend == "" {
		if old := r.sessions[key]; old != nil {
			if b := old.Backend(); b != "" {
				reqBackend = b
			}
		}
	}
	wrapper, backendID := r.wrapperFor(reqBackend)

	// Model merge: router default ← backend override ← per-request opts.
	model := r.model
	if bm, ok := r.backendModels[backendID]; ok && bm != "" {
		model = bm
	}
	if opts.Model != "" {
		model = opts.Model
	}

	// Args: backend-scoped replacement wins over router-wide extraArgs, then
	// per-request ExtraArgs is appended. REPLACE (not append) semantics for the
	// backend level matches RouterConfig.BackendExtraArgs godoc (R53-ARCH-002).
	baseArgs := r.extraArgs
	if ba, ok := r.backendExtraArgs[backendID]; ok && len(ba) > 0 {
		baseArgs = ba
	}
	args := make([]string, len(baseArgs))
	copy(args, baseArgs)
	args = append(args, opts.ExtraArgs...)

	// Workspace: opts override > per-chat override > old session workspace > default.
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
	if !workspaceOverridden && resumeID != "" {
		if old := r.sessions[key]; old != nil {
			if ws := old.Workspace(); ws != "" {
				workspace = ws
			}
		}
	}

	// ResumeID guard: drop when the jsonl Claude CLI would read is missing so
	// the spawn falls through to a fresh session instead of exit-1'ing on
	// "No conversation found". See resolveResumeID for rationale.
	resumeID = resolveResumeID(r.claudeDir, workspace, key, resumeID)

	return spawnParams{
		BackendID: backendID,
		Wrapper:   wrapper,
		Model:     model,
		Args:      args,
		Workspace: workspace,
		ResumeID:  resumeID,
	}
}

// collectPreviousHistory gathers JSONL-backed history entries and the
// session ID chain for a respawn. Returns (entries, chain). Pure
// computation — no mutation of r.sessions; caller must hold r.mu
// if it needs serialisation w.r.t. sibling spawn attempts.
//
// Extracted from spawnSession (R70-ARCH-H2 paired with
// resolveSpawnParamsLocked). The dead-process branch prefers
// EventEntries() over persistedHistory because EventEntries includes
// live events accumulated since the JSONL snapshot was last loaded;
// the live-but-suspended branch (no process, or alive waiting) falls
// back to the persisted snapshot.
func collectPreviousHistory(oldSess *ManagedSession, oldPrevIDs []string, resumeID string) ([]cli.EventEntry, []string) {
	if oldSess == nil {
		return nil, nil
	}

	// R215-GO-P1-1: split the historyMu critical section so that p.EventEntries()
	// is invoked WITHOUT holding session.historyMu. EventEntries acquires
	// cli.Process.eventLog.mu internally; if any future caller decides to call
	// back into a session method while holding eventLog.mu (e.g. a sink that
	// asks the owner session for its persistedHistory) the previous order
	// (historyMu → eventLog.mu) would deadlock against the reverse path.
	//
	// Two-phase pattern:
	//  1. Under historyMu.RLock: snapshot whatever we need from session-owned
	//     state — here, the live process pointer and a copy of persistedHistory.
	//     Critical-section boundary is tight and only touches session fields.
	//  2. After releasing historyMu: invoke EventEntries() on the snapshotted
	//     process, which is safe because *cli.Process is immutable once
	//     loadProcess returns (the pointer can only be replaced by storeProcess
	//     under sendMu, which we don't acquire — but the old Process keeps its
	//     own eventLog alive until GC, so reading entries from it is sound).
	var entries []cli.EventEntry
	oldSess.historyMu.RLock()
	p := oldSess.loadProcess()
	var persistedSnapshot []cli.EventEntry
	if (p == nil || p.Alive()) && len(oldSess.persistedHistory) > 0 {
		persistedSnapshot = make([]cli.EventEntry, len(oldSess.persistedHistory))
		copy(persistedSnapshot, oldSess.persistedHistory)
	}
	oldSess.historyMu.RUnlock()

	if p != nil && !p.Alive() {
		// Dead process: EventEntries() includes both injected history and live events
		// logged during the last run. Use this instead of persistedHistory, which only
		// holds the JSONL-loaded snapshot and misses events accumulated since that load.
		entries = p.EventEntries()
	} else {
		entries = persistedSnapshot
	}

	// Build session chain: inherit old chain and append old session ID,
	// but only when the old ID differs from resumeID (i.e. a truly new
	// CLI session is replacing the old one, not just resuming the same one).
	var prevIDs []string
	if oldID := oldSess.getSessionID(); oldID != "" && oldID != resumeID {
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
	return entries, prevIDs
}

// markSpawnDoneLocked closes the per-spawn done channel and removes the
// spawningKeys map entry for key. Caller MUST hold r.mu. Single point of
// truth for the close-before-delete sequence so no future caller can swap
// the order accidentally — both ops are commutative under r.mu (waiters
// observe close via the channel reference they already hold, not via
// map lookup) but the convention matters for grep-ability and review.
// R248-ARCH-10.
func (r *Router) markSpawnDoneLocked(key string, ch chan struct{}) {
	close(ch)
	delete(r.spawningKeys, key)
}

// spawnSession creates a new process, optionally resuming an existing session.
// LOCK: enter with r.mu held. This function releases and re-acquires r.mu
// internally (around Spawn() and history collection) to avoid blocking other
// goroutines during slow protocol init (e.g. ACP handshake). Callers MUST NOT
// hold any other lock when invoking; the defer reacquires r.mu only.
func (r *Router) spawnSession(ctx context.Context, key string, resumeID string, opts AgentOpts) (*ManagedSession, error) {
	// Mark this key as spawning so ReconnectShims does not mistake the freshly
	// started shim's state file for an orphan. Every return path below leaves
	// r.mu unlocked, so the defer reacquires it to delete the marker. Lazy
	// init tolerates test-only Routers constructed with &Router{...}.
	//
	// R243-ARCH-4: the map value is a per-spawn done-channel rather than a
	// presence-only struct{}. close(ch) wakes any GetOrCreate caller parked
	// on the same key in O(1) regardless of waiter count, replacing the
	// previous 20ms tick poll. close-before-delete is for readability, not
	// correctness — both run under r.mu, and any waiter observes the close
	// via the channel reference it already holds (not via map lookup), so
	// the two ops are commutative. Kept in this order purely as a uniform
	// convention. R248-GO-3.
	if r.spawningKeys == nil {
		r.spawningKeys = make(map[string]chan struct{})
	}
	doneCh := make(chan struct{})
	r.spawningKeys[key] = doneCh
	defer func() {
		r.mu.Lock()
		r.markSpawnDoneLocked(key, doneCh)
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
		// Guard against unbounded exempt session growth (e.g., many projects).
		//
		// R242-ARCH-2 hard isolation: the per-namespace sub-quota check
		// runs FIRST so a noisy cron chat (DefaultMaxJobsPerChat × N
		// chats) can no longer push planner / sys stubs out of the
		// shared pool. Only after the sub-quota passes do we apply the
		// global maxExemptSessions ceiling as a relief valve for a
		// future namespace added without explicit sub-quota wiring.
		kind := exemptKind(key)
		if kind != "" {
			if perKind := r.countExemptByKind(kind); perKind >= exemptCapFor(kind) {
				r.mu.Unlock()
				return nil, fmt.Errorf("%w: %s namespace (%d)", ErrMaxExemptSessions, kind, exemptCapFor(kind))
			}
		}
		if r.countExempt() >= maxExemptSessions {
			r.mu.Unlock()
			return nil, fmt.Errorf("%w (%d)", ErrMaxExemptSessions, maxExemptSessions)
		}
	}

	// Merge backend / model / args / workspace / resumeID into a single
	// struct so the branching below stays linear. Under r.mu; consumes the
	// one-shot backendOverrides entry for `key`. R70-ARCH-H2.
	sp := r.resolveSpawnParamsLocked(key, resumeID, opts)
	wrapper := sp.Wrapper
	backendID := sp.BackendID
	workspace := sp.Workspace
	resumeID = sp.ResumeID

	spawnOpts := cli.SpawnOptions{
		Key:             key,
		Model:           sp.Model,
		ResumeID:        resumeID,
		ExtraArgs:       sp.Args,
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
	var oldCreatedAt int64
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
			oldTotalCost = loadTotalCost(&old.totalCost)
		}
		// Carry the original creation timestamp across spawn so resume /
		// reset-and-recreate / takeover paths keep the session in its
		// established sidebar position. installFreshSessionLocked stamps now
		// when this is zero (genuinely-new key).
		oldCreatedAt = old.createdAt.Load()
	}
	r.mu.Unlock()

	oldHistory, prevIDs := collectPreviousHistory(old, oldPrevIDs, resumeID)

	// ── Auto-workspace-chain attach (RFC docs/rfc/auto-workspace-chain.md
	// §4.4-A). Three-phase lock pattern, guards against TOCTOU vs cron /
	// sysession registering new internal sessionIDs in the gap between
	// candidate selection and apply (New-B1):
	//
	//   Phase 1 — r.mu held: snapshot router-side excluder + extras ptr.
	//   Phase 2 — lock free: pickWorkspaceChain runs (ReadDir behind cache).
	//   Phase 3 — r.mu held: re-validate against the current excluder set.
	//
	// Skipped when this is an internal session (cron / sys / scratch),
	// when policy disables auto-chain for the workspace, or when the
	// session is inheriting prev / oldHistory from a prior incarnation
	// (resume / chain rotation paths already established the chain).
	autoChainAttached := r.maybeAttachAutoChainOnSpawn(key, workspace, prevIDs, oldHistory)
	if len(autoChainAttached) > 0 {
		prevIDs = autoChainAttached
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

	s := r.installFreshSessionLocked(
		key, proc, workspace, backendID, wrapper, resumeID,
		oldHistory, prevIDs, oldTotalCost, oldCreatedAt, opts.Exempt,
	)
	r.mu.Unlock()

	// Stamp origin labels for the auto-chained segment AFTER the session
	// is published (s.prevSessionIDs is now populated by
	// installFreshSessionLocked). Doing this outside r.mu avoids holding
	// r.mu through historyMu — the lock order is r.mu → historyMu, and
	// SetPrevSessionOrigins takes only historyMu.
	if len(autoChainAttached) > 0 {
		s.SetPrevSessionOrigins(autoChainAttached, "auto-spawn")
		slog.Info("auto-chain attached on spawn",
			"key", key,
			"workspace", workspace,
			"chain_ids", autoChainAttached,
			"chain_len", len(autoChainAttached))
		metrics.AutoChainSpawnAttachTotal.Add(1)
	}

	r.loadResumeHistoryOnSpawn(ctx, s, key, resumeID, workspace, prevIDs, oldHistory)

	// RFC §3.2.2 ordering contract: SetPersistSink ONLY after every
	// InjectHistory call above has completed. Any remaining bulk
	// history injection for this session happens later via the
	// NewRouter startup goroutine, which uses s.InjectHistory; the
	// AppendBatch that flows through must carry replayPhase=true
	// because sinkReady was still false when those entries were
	// appended. Persister drops them (see RFC §3.2.3 runtime guard).
	r.installPersistSink(proc, key)

	r.notifyChange()
	return s, nil
}

// installFreshSessionLocked attaches a freshly-spawned process to the
// router indices + event log. Extracted from spawnSession (CQ2 Round 213);
// pure state-mutation block with no I/O. Ordering matches the original
// inlined block verbatim; callers must still invoke installPersistSink AFTER
// this returns (RFC §3.2.2).
//
// LOCK: caller must hold r.mu for writing.
func (r *Router) installFreshSessionLocked(
	key string,
	proc *cli.Process,
	workspace string,
	backendID string,
	wrapper *cli.Wrapper,
	resumeID string,
	oldHistory []cli.EventEntry,
	prevIDs []string,
	oldTotalCost float64,
	oldCreatedAt int64,
	exempt bool,
) *ManagedSession {
	s := &ManagedSession{
		key:              key,
		persistedHistory: oldHistory,
		prevSessionIDs:   prevIDs,
		exempt:           exempt,
		onSessionID: func(id string) {
			r.mu.Lock()
			r.trackSessionID(id)
			if id != "" {
				r.sessionIDToKey[id] = key
			}
			r.mu.Unlock()
		},
	}
	storeTotalCost(&s.totalCost, oldTotalCost)
	// Sidebar order anchor: inherit oldCreatedAt when this spawn replaces a
	// prior incarnation (resume / ResetAndRecreate / takeover); fall back to
	// now for genuinely-new keys via initCreatedAtIfUnset.
	if oldCreatedAt != 0 {
		s.createdAt.Store(oldCreatedAt)
	} else {
		s.initCreatedAtIfUnset()
	}
	s.setWorkspace(workspace)
	s.SetBackend(backendID)
	s.SetCLIName(wrapper.CLIName)
	s.SetCLIVersion(wrapper.CLIVersion)
	// attachProcessAndSnapshotPersisted: serialises storeProcess + seededLen
	// reset under historyMu so a concurrent InjectHistory observes the
	// (process, seededLen=len(persistedHistory)) pair and forwards only
	// genuinely-new tail. The returned snapshot is the same content as
	// oldHistory but goes through the same lock-protected path that the
	// reconnect branch uses, keeping the semantics symmetric.
	snapshot := s.attachProcessAndSnapshotPersisted(proc)
	// Matches the reconnect path (ReconnectShims): notify the dashboard when
	// a turn completes out-of-band (e.g. result arrives via readLoop without
	// an active Send capturing it). SetOnTurnDone is mu-guarded inside Process,
	// so calling it after storeProcess is safe.
	proc.SetOnTurnDone(func() { r.notifyChange() })
	if len(snapshot) > 0 {
		proc.InjectHistory(snapshot)
	}
	// Effective session ID: prefer the resumeID the caller asked us to
	// resume, but if there isn't one, fall back to whatever the protocol
	// captured during Init / fresh handshake (ACP `session/new` returns a
	// UUID synchronously; claude leaves it empty until the first turn's
	// system/init event lands and process_send.go.SetSessionID kicks in).
	//
	// Without this fallback, a freshly-spawned kiro session has empty
	// ManagedSession.sessionID until the user sends their first message,
	// and the periodic saveStore loop (store.go:135 `if sid != ""`) drops
	// the entry on the floor — losing the session across naozhi restarts.
	// claude is unaffected because no resume + no first turn → empty sid
	// matches its legacy behaviour.
	effectiveSID := resumeID
	if effectiveSID == "" {
		effectiveSID = proc.GetSessionID()
	}
	s.setSessionID(effectiveSID)
	if effectiveSID != "" {
		r.trackSessionID(effectiveSID)
		r.sessionIDToKey[effectiveSID] = key
	}
	s.touchLastActive()
	r.attachHistorySource(s)
	r.sessions[key] = s
	r.indexAdd(key)
	if !exempt {
		r.activeCount.Add(1)
	}

	r.storeDirty = true
	r.storeGen.Add(1)
	slog.Info("session spawned", "key", key, "active", r.activeCount.Load(), "exempt", exempt)
	// OBS2: counter bumped inside the write-lock so it reflects the authoritative
	// "spawn succeeded" point (past both TOCTOU guards, past storeProcess). Exempt
	// sessions are excluded — they don't consume a normal session slot and
	// inflating session_create_total with planner/scratch churn muddies the signal.
	if !exempt {
		metrics.SessionCreateTotal.Add(1)
		// Multi-Backend RFC §10 (Sprint 6a): track per-backend gauge of
		// active sessions. Mirrors r.activeCount.Add(1) above but split
		// by backend so dashboards can answer "how many kiro vs claude
		// sessions are live?". Decrement happens at all the same sites
		// that decrement activeCount (resetLocked / Remove / evict /
		// recompute paths).
		metrics.RecordSessionActive(s.Backend(), 1)
	}
	return s
}

// installPersistSink wires the event-log persister into the given
// Process's EventLog. No-op when the persister is disabled. Called
// exclusively from spawnSession / ReattachProcess AFTER any
// InjectHistory calls have completed, per the ordering contract in
// RFC §3.2.2.
//
// Called with a nil proc in some test harnesses; we guard because
// Process is behind an interface (processIface) and the hook is
// only meaningful for real CLI-backed processes. Fake processes
// used in router_test.go don't expose SetPersistSink; they're
// caught by the type assertion below and silently skipped.
func (r *Router) installPersistSink(proc processIface, key string) {
	if r.eventLogPersister == nil {
		return
	}
	realProc, ok := proc.(*cli.Process)
	if !ok {
		return
	}
	log := realProc.EventLog()
	if log == nil {
		return
	}
	sink := newEventLogSink(
		r.eventLogPersister.SinkFor(key),
		r.attachmentTracker,
		persist.KeyHash(key),
	)
	log.SetPersistSink(sink)
}

// loadResumeHistoryOnSpawn walks the previous CLI session-ID chain and, when
// a resume is in progress with no in-memory history yet, synchronously loads
// the JSONL chain from r.claudeDir and injects it into s. No-op when the
// resume conditions are not met.
//
// Synchronous — runs on the spawnSession caller goroutine. The historyWg
// Add/Done dance still tracks the call so Shutdown.Wait can drain in-flight
// loads before tearing down dependent state (R229-GO-4: 15s budget per
// spawn could stretch past the 30s drain window when several concurrent
// spawnSession calls each open large JSONL chains). The IIFE surrounding
// the body scopes the deferred historyWg.Done / context cancels so they
// fire on every return path without leaking past the load attempt.
//
// Cancellation contract:
//   - Parent on r.historyCtx so Shutdown's historyCancel() wakes the reader
//     immediately rather than waiting for the 15s per-spawn timeout
//     (R229-GO-4 follow-up).
//   - Caller ctx (typically GetOrCreate's request ctx) is fanned in via
//     context.AfterFunc so a cancelled GetOrCreate also releases the reader
//     (R225-GO-8 invariant).
//   - Skipped entirely once historyCtx is already cancelled (Shutdown started
//     before this spawn reached the load step).
//
// Lock contract: must NOT be called with r.mu held — InjectHistory acquires
// session.historyMu independently, and the inner reader can take seconds.
//
// Extracted from spawnSession so the per-fix churn stays inside this dedicated
// helper instead of forcing readers to scroll past it in the long spawn path.
func (r *Router) loadResumeHistoryOnSpawn(
	ctx context.Context,
	s *ManagedSession,
	key, resumeID, workspace string,
	prevIDs []string,
	oldHistory []cli.EventEntry,
) {
	if resumeID == "" || r.claudeDir == "" || len(oldHistory) > 0 {
		return
	}

	// R232-GO-2 / R230-GO-1 / R233-GO-1: hold the WaitGroup ticket across
	// the historyCtx.Err() check so Shutdown's historyWg.Wait() cannot race
	// past a late Add(1). The skip branch immediately Done()s; the load
	// branch keeps the ticket until the IIFE returns.
	r.historyWg.Add(1)
	if r.historyCtx != nil && r.historyCtx.Err() != nil {
		r.historyWg.Done()
		return
	}

	ids := make([]string, 0, len(prevIDs)+1)
	ids = append(ids, prevIDs...)
	ids = append(ids, resumeID)

	func() {
		defer r.historyWg.Done()
		parent := r.historyCtx
		if parent == nil {
			parent = context.Background()
		}
		histCtx, histCancel := context.WithTimeout(parent, 15*time.Second)
		defer histCancel()
		if ctx != nil {
			stop := context.AfterFunc(ctx, histCancel)
			defer stop()
		}
		allEntries := discovery.LoadHistoryChainTailCtx(
			histCtx, r.claudeDir, ids, workspace, maxPersistedHistory,
		)
		if len(allEntries) > 0 {
			s.InjectHistory(allEntries)
			slog.Info("loaded session history on resume", "key", key, "entries", len(allEntries), "chain", len(ids))
		}
	}()
}

// countActive recounts alive processes (corrects drift from undetected exits).
// Exempt sessions are not counted toward max_procs capacity. Caller must
// hold r.mu.
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
	// Multi-Backend RFC §10 (Sprint 6a): keep the per-backend labeled
	// gauge in sync with the freshly recounted r.sessions snapshot.
	// Done in the same critical section as activeCount.Store so the
	// two bookkeeping totals never diverge from each other.
	r.reconcileSessionActiveByBackendLocked()
}

// reconcileSessionActiveByBackendLocked rebuilds the metrics.SessionActive
// pair (legacy unlabeled mirror + per-backend labeled gauge) from r.sessions.
//
// Used by bulk teardown paths (countActive / cleanupSessionsByChatPrefix /
// Cleanup prune) where per-key Inc/Dec bookkeeping in the loop would
// require threading each session's backend through the batched-close
// machinery. Single-session sites (Reset / Remove / evictOldest) call
// metrics.RecordSessionActive(backend, -1) directly for lower overhead.
//
// Backends that previously had sessions but no longer do are explicitly
// driven to zero — without ForEachKey the bucket would stay stuck at
// its last non-zero value.
//
// LOCK: caller must hold r.mu for writing.
func (r *Router) reconcileSessionActiveByBackendLocked() {
	var total int64
	perBackend := make(map[string]int64, 4)
	for _, s := range r.sessions {
		if s.exempt {
			continue
		}
		if s.isAlive() {
			total++
			perBackend[s.Backend()]++
		}
	}
	// Reconcile the unlabeled mirror (naozhi_session_active) by setting
	// it to the freshly counted total. expvar.Int has no Set, so use
	// Add(want - current) which is atomic enough for a gauge.
	currentTotal := metrics.SessionActive.Value()
	if delta := total - currentTotal; delta != 0 {
		metrics.SessionActive.Add(delta)
	}
	// Reconcile the labeled gauge per backend.
	allBackends := map[string]struct{}{}
	for k := range perBackend {
		allBackends[k] = struct{}{}
	}
	metrics.SessionActiveByBackend.ForEachKey(func(k string) {
		allBackends[k] = struct{}{}
	})
	// Drive each backend's gauge to its authoritative count via a single
	// atomic Add per key. The previous "loop of N Inc/Dec" exposed
	// partial intermediate values to /debug/vars scrapers between
	// iterations: we hold r.mu but not any lock on the expvar map, so a
	// reader sweeping concurrently could observe e.g. N/2 sessions for
	// 'kiro' while the loop was halfway through. expvar.Map.Add is
	// atomic per key, so the reconcile transition is now a single jump.
	for backend := range allBackends {
		current := metrics.SessionActiveByBackend.Get(backend)
		want := perBackend[backend]
		metrics.SessionActiveByBackend.Add(want-current, backend)
	}
}

// countExempt returns the total number of alive exempt sessions across
// all namespaces. Caller must hold r.mu.
//
// R242-ARCH-2: kept for the global-cap relief-valve check in spawn.
// Per-namespace gating goes through countExemptByKind so cron / planner
// / sys quotas are isolated.
func (r *Router) countExempt() int {
	count := 0
	for _, s := range r.sessions {
		if s.exempt && s.isAlive() {
			count++
		}
	}
	return count
}

// countExemptByKind returns the alive exempt-session count for a single
// namespace ("cron" / "project" / "sys"). Caller must hold r.mu. R242-
// ARCH-2: the per-kind walk is O(N) like countExempt but typed against
// the prefix family so a noisy cron chat (DefaultMaxJobsPerChat × N
// chats) can no longer push planner / sys stubs out of the global pool.
//
// "" kind returns 0 (defensive; an exempt session whose key matches no
// known prefix is a misconfiguration that countExempt also misses; a
// future audit hook should panic on encountering one but the current
// "log+continue" stance avoids a startup crash on operator misconfig).
func (r *Router) countExemptByKind(kind string) int {
	if kind == "" {
		return 0
	}
	count := 0
	for k, s := range r.sessions {
		if !s.exempt || !s.isAlive() {
			continue
		}
		if exemptKind(k) == kind {
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
		if oldest == nil || s.LastActive().Before(oldest.LastActive()) {
			oldest = s
		}
	}
	if oldest == nil {
		return false
	}
	slog.Info("evicting oldest session", "key", oldest.key, "idle", time.Since(oldest.LastActive()))
	// OBS2: bump before Unlock so it aligns with the "decision to evict" point;
	// the subsequent proc.Close() is async-capable and can fail, but the eviction
	// decision is already committed (deathReason set, storeDirty marked below).
	metrics.SessionEvictTotal.Add(1)
	// Multi-Backend RFC §10 (Sprint 6a): evictOldest below relies on
	// r.countActive() to recompute the legacy total post-Close, but the
	// labeled gauge needs an explicit decrement keyed on the evictee's
	// backend. Done now (under the lock, before Unlock for Close) so the
	// metric reflects the eviction decision instead of the post-Close
	// recount which only sees the residual sessions.
	metrics.RecordSessionActive(oldest.Backend(), -1)
	storeAtomicString(&oldest.deathReason, "evicted")
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
	r.mu.Lock()
	// R191-CONC-H1: Broadcast under r.mu to avoid missed-wakeup window with
	// Shutdown's cond.Wait. sync.Cond.Broadcast docs say holding L is optional
	// only when the wakeup predicate is purely state-atomic; here the predicate
	// reads r.sessions[*].loadProcess().IsRunning() which the Close() above
	// just flipped. R183-REL-H1 established the "hold r.mu across Broadcast"
	// pattern for NotifyIdle; extending here to evict/reset/remove/cleanup
	// (R191-CONC-H1-a/b/c/d).
	if r.shutdownCond != nil {
		r.shutdownCond.Broadcast()
	}
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

// unregisterSessionLocked removes a session from all routing indexes.
// If keepBackendOverride is true, backendOverrides[key] is preserved so a
// following spawnSession can consume it atomically (used by
// ResetAndRecreate / Takeover which reuse the same key). On terminal removal
// paths (Reset / Remove / Cleanup prune) pass false to prevent override leaks.
//
// LOCK: caller must hold r.mu for writing.
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

// resetLocked performs the in-lock teardown shared by Reset and
// ResetAndDiscardOverride. Caller must run the finishResetUnlocked
// sequence after releasing the lock.
//
// Returns the live process (for Close after lock release), the session
// UUID captured before teardown (for the retired-session notification —
// r.sessions[key] is unregistered here, so callers cannot recover the
// UUID after the lock drops), and the success flag.
//
// LOCK: caller must hold r.mu for writing.
func (r *Router) resetLocked(key string) (processIface, string, bool) {
	s, ok := r.sessions[key]
	if !ok {
		return nil, "", false
	}
	proc := s.loadProcess()
	wasActive := !s.exempt && proc != nil && proc.Alive()
	backend := s.Backend()
	sessionID := s.SessionID()
	r.unregisterSessionLocked(key, s, false)
	if wasActive {
		if r.activeCount.Add(-1) < 0 {
			r.activeCount.Store(0)
		}
		// Multi-Backend RFC §10 (Sprint 6a): mirror the activeCount
		// decrement into the labeled gauge so per-backend dashboards
		// stay in sync with the legacy unlabeled total.
		metrics.RecordSessionActive(backend, -1)
	}
	r.storeDirty = true
	r.storeGen.Add(1)
	return proc, sessionID, true
}

// Reset discards the session for the given key (user sent /new).
func (r *Router) Reset(key string) {
	r.mu.Lock()
	proc, sessionID, ok := r.resetLocked(key)
	r.mu.Unlock()
	if !ok {
		return
	}
	r.finishResetUnlocked(key, sessionID, proc)
}

// ResetAndDiscardOverride atomically resets the session AND deletes the
// per-chat workspace override, closing the race where a concurrent
// SetWorkspace would otherwise survive a bare Reset+delete pair and leak
// into the next session (Round-207 SM1).
func (r *Router) ResetAndDiscardOverride(key string) {
	r.mu.Lock()
	proc, sessionID, hadSession := r.resetLocked(key)
	if _, existed := r.workspaceOverrides[key]; existed {
		delete(r.workspaceOverrides, key)
		r.wsOverridesDirty = true
		r.wsOverridesGen.Add(1)
	}
	r.mu.Unlock()
	if !hadSession {
		return
	}
	r.finishResetUnlocked(key, sessionID, proc)
}

// finishResetUnlocked runs the post-unlock teardown shared by Reset and
// ResetAndDiscardOverride. Must be called without r.mu held. sessionID
// is the UUID captured by resetLocked before unregister cleared
// r.sessions[key]; pass through as-is to notifyKeyRetired so the
// dashboard history-sort hook can stamp retired_at.
func (r *Router) finishResetUnlocked(key, sessionID string, proc processIface) {
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
	// R191-CONC-H1-b: Broadcast under r.mu (see evictOldest comment).
	if r.shutdownCond != nil {
		r.mu.Lock()
		r.shutdownCond.Broadcast()
		r.mu.Unlock()
	}

	slog.Info("session reset", "key", key)
	r.notifyKeyRetired(key, sessionID)
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
//
// NOTE (R62-GO-3): ResetAndRecreate releases r.mu between session
// teardown and respawn so proc.Close() can run without holding the
// router mutex. A concurrent GetOrCreate arriving in that window
// can win the race and spawn a fresh session with its own opts,
// which may not match what the caller of ResetAndRecreate expected.
//
// Mitigation: callers whose behavior depends on opts.Backend being
// honored MUST treat ResetAndRecreate's returned session as a
// best-effort — it guarantees "a fresh session exists" but not
// "a fresh session with MY opts". The TOCTOU guard in spawnSession
// returns existing sessions rather than stacking dup spawns, so the
// invariant "exactly one live session per key" holds. Round 209's
// SM1 (ResetAndDiscardOverride) is the atomic alternative when
// opts fidelity matters.
func (r *Router) ResetAndRecreate(ctx context.Context, key string, opts AgentOpts) (*ManagedSession, error) {
	r.mu.Lock()

	// Delete old session if present
	hadOld := false
	if s, ok := r.sessions[key]; ok {
		hadOld = true
		proc := s.loadProcess()
		wasActive := !s.exempt && proc != nil && proc.Alive()
		oldBackend := s.Backend()
		// keepBackendOverride=true: the new opts may carry its own backend,
		// and spawnSession below consumes and clears the override atomically.
		r.unregisterSessionLocked(key, s, true)
		if wasActive {
			if r.activeCount.Add(-1) < 0 {
				r.activeCount.Store(0)
			}
			// Multi-Backend RFC §10 (Sprint 6a): per-backend gauge mirror.
			// The follow-up spawnSession will Inc the gauge for the new
			// backend (which may differ from oldBackend if opts.Backend
			// changed) — net change is 0 if same backend, +1/-1 otherwise.
			metrics.RecordSessionActive(oldBackend, -1)
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
			r.mu.Lock()
			// R191-CONC-H1-f: Broadcast under r.mu (see evictOldest comment).
			if r.shutdownCond != nil {
				r.shutdownCond.Broadcast()
			}
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
		slog.Warn("rename session: invalid new key", "err", err)
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
	// persistedHistory: clone the backing array too. NewRouter launches an
	// async history-load goroutine that holds the `s` pointer; if the load
	// completes after Rename swapped keys, s.InjectHistory appends to
	// old.persistedHistory. When len<cap in that backing array, the append
	// writes into bytes that fresh.persistedHistory also points to.
	freshHistory := slices.Clone(old.persistedHistory)
	fresh := &ManagedSession{
		key:              newKey,
		persistedHistory: freshHistory,
		prevSessionIDs:   slices.Clone(old.prevSessionIDs),
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
	storeTotalCost(&fresh.totalCost, loadTotalCost(&old.totalCost))
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
	if dr := loadAtomicString(&old.deathReason); dr != "" {
		storeAtomicString(&fresh.deathReason, dr)
	}
	fresh.lastActive.Store(old.lastActive.Load())
	// Carry the original creation timestamp so the renamed row keeps its
	// established sidebar position. Zero-fallback to now would shove the
	// row to the bottom — surprising for the scratch-promote flow where
	// the user is preserving an existing conversation.
	if old.createdAt.Load() != 0 {
		fresh.createdAt.Store(old.createdAt.Load())
	} else {
		fresh.initCreatedAtIfUnset()
	}
	// Go through storeAtomicString so each write allocates a fresh *string —
	// direct `.Store(lp)` would share the underlying pointer with `old` and
	// diverge from the rest of the codebase's "always helper" convention.
	// Currently safe because strings are immutable, but keeping the invariant
	// uniform avoids confusion if a future refactor ever makes the pointee
	// mutable.
	if lp := loadAtomicString(&old.lastPrompt); lp != "" {
		storeAtomicString(&fresh.lastPrompt, lp)
	}
	if la := loadAtomicString(&old.lastActivity); la != "" {
		storeAtomicString(&fresh.lastActivity, la)
	}
	fresh.setSessionID(old.getSessionID())

	// Move the process pointer so the running CLI keeps serving requests
	// under the new key. The old struct becomes an orphan with process=nil,
	// so any goroutine holding a stale reference to `old` that attempts Send
	// fails cleanly with "no active process".
	//
	// The proc's EventLog already carries the entries that match
	// fresh.persistedHistory (they were forwarded earlier under `old`), so
	// fresh.persistedSeededLen must mirror len(fresh.persistedHistory) — a
	// fresh.InjectHistory after takeover should forward only newly-arrived
	// tail. adoptProcessAlreadySeeded handles that under historyMu.
	if proc := old.loadProcess(); proc != nil {
		fresh.adoptProcessAlreadySeeded(proc)
	}
	old.storeProcess(nil)

	// Rebind the history source to the renamed session — the old Source
	// captured `old.SnapshotChainIDs` which reads the now-orphaned struct.
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

// stripResumeArgs removes --resume <id> pairs from a CLI arg slice.
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
