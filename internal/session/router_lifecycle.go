// Package session router lifecycle methods.
//
// This file holds session lifecycle: GetOrCreate / spawn / Reset / Rename /
// workspace overrides / history wiring. router.go retains the Router struct
// definition, NewRouter, and infrastructure helpers (panicSafeSpawn etc.).
// Lock contracts are documented per-method via "// LOCK:" annotations on
// `*Locked` suffix functions.
package session

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
	"github.com/naozhi/naozhi/internal/history"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/shim"
)

// publishSessionLocked is the single funnel for installing a freshly-built
// ManagedSession into the router's lookup tables (attachHistorySource →
// sessions map → indexAdd). Every spawn / discovery / rename / takeover path
// must go through it so no site can forget the history source, which would
// leave the dashboard "history" drawer silently blank. Callers that already
// attached the source pass alreadyAttached=true to avoid double-attach.
//
// Post-condition: s.loadHistorySource() is non-nil (Noop at worst).
//
// LOCK: caller must hold r.mu (write).
func (r *Router) publishSessionLocked(key string, s *ManagedSession, alreadyAttached bool) {
	if !alreadyAttached {
		r.attachHistorySource(s)
	}
	if s.loadHistorySource() == nil {
		// Defence-in-depth against a mistaken alreadyAttached=true: log the
		// diagnostic and install a Noop so downstream callers need not nil-check.
		slog.Error("publishSessionLocked: history source missing after attach — falling back to Noop",
			"key", key, "alreadyAttached", alreadyAttached)
		s.SetHistorySource(history.Noop{})
	}
	r.ss.sessions[key] = s
	r.indexAdd(key)
}

// attachHistorySource picks the right history.Source for a session based on
// its backend ID and installs it, so EventEntriesBeforeCtx's disk fallback is
// live before the first pagination request. Composition (RFC §3.4 / §3.5):
// the local tier is the naozhi event log (empty when eventLogDir is unset);
// the fallback tier is the backend wrapper's per-format reader
// (Wrapper.NewHistorySource, Noop for unknown backends); MergedSource
// UUID-dedupes and time-sorts both.
func (r *Router) attachHistorySource(s *ManagedSession) {
	if s == nil {
		return
	}
	backend := s.Backend()
	if backend == "" {
		backend = r.bkStore.defaultBackend
	}

	// Unknown backend ID falls back to the default wrapper so a misconfigured
	// Backend() still gets a usable source instead of Noop.
	wrapper := r.bkStore.wrappers[backend]
	if wrapper == nil {
		wrapper = r.bkStore.wrapper
	}

	deps := cli.HistoryWiring{
		ClaudeDir:        r.claudeDir,
		KiroSessionsDir:  r.kiroSessionsDir,
		CodexSessionsDir: r.codexSessionsDir,
		EventLogDir:      r.eventLogDir,
	}

	// Wrapper.NewHistorySource never returns nil; the guard pins that
	// contract at the boundary.
	var fallback history.Source = wrapper.NewHistorySource(s, deps)
	if fallback == nil {
		fallback = history.Noop{}
	}

	// mergeWithEventLog returns fallback unchanged when r.eventLogDir is
	// empty, otherwise layers the event-log local tier in front of it.
	s.SetHistorySource(mergeWithEventLog(r.eventLogDir, s.key, fallback))
}

// ResetChat resets all sessions belonging to a chat (all agents).
func (r *Router) ResetChat(chatKeyPrefix string) {
	r.resetChatAndMaybeSetWorkspace(chatKeyPrefix, "", false)
}

// ResetChatAndSetWorkspace atomically resets all sessions belonging to a chat
// and installs a new workspace override for it under a single r.mu critical
// section. Two separate locked calls would let a concurrent GetOrCreate see the
// key idle with the override deleted and spawn in the OLD workspace (#2342).
func (r *Router) ResetChatAndSetWorkspace(chatKeyPrefix, path string) {
	r.resetChatAndMaybeSetWorkspace(chatKeyPrefix, path, true)
}

// resetChatAndMaybeSetWorkspace is the shared locked core for ResetChat and
// ResetChatAndSetWorkspace. When setWorkspace is true it installs `path` as the
// chat's workspace override before releasing r.mu, so callers that reset+set
// observe no intermediate state.
func (r *Router) resetChatAndMaybeSetWorkspace(chatKeyPrefix, path string, setWorkspace bool) {
	r.mu.Lock()
	var toClose []processIface
	var closedActive int
	if r.ss.byChat != nil {
		// resetSessionLocked deletes from r.ss.sessions only; the whole
		// index entry is dropped below.
		for key := range r.ss.byChat[chatKeyPrefix] {
			r.resetSessionLocked(key, &toClose, &closedActive)
		}
		delete(r.ss.byChat, chatKeyPrefix)
	} else {
		// Fallback O(n) scan for test-created routers without index.
		prefix := chatKeyPrefix + ":"
		var toDelete []string
		for key := range r.ss.sessions {
			if len(key) > len(chatKeyPrefix) && key[:len(prefix)] == prefix {
				toDelete = append(toDelete, key)
			}
		}
		for _, key := range toDelete {
			r.resetSessionLocked(key, &toClose, &closedActive)
		}
	}
	if closedActive > 0 {
		newCount := r.ss.activeCount.Add(-int64(closedActive))
		if newCount < 0 {
			r.ss.activeCount.Store(0)
		}
		// Reconcile the per-backend labeled gauge by batched recount; O(n)
		// but only on the rare chat-prefix reset.
		r.reconcileSessionActiveByBackendLocked()
	}
	// Delete marks the store dirty so the removal survives a crash before
	// any other path flips the flag.
	r.wsStore.Delete(chatKeyPrefix)
	if setWorkspace && chatKeyPrefix != "" {
		// Same locked section as the reset so no concurrent GetOrCreate sees
		// the chat reset with the override gone (#2342). The override was just
		// deleted, so this is always a fresh insert.
		r.putWorkspaceOverrideLocked(chatKeyPrefix, path)
	}
	r.ss.dirty = true
	r.ss.gen.Add(1)
	r.mu.Unlock()

	for _, proc := range toClose {
		proc.Close()
	}
	// Two locked sections by design: proc.Close() can block, so it runs
	// unlocked. Broadcast must happen under r.mu and only AFTER Close() flips
	// IsRunning() to false — shutdownCond waiters re-evaluate that predicate,
	// so broadcasting earlier is a missed-wakeup window. Same
	// Unlock→Close→relock-Broadcast shape as evictOldest.
	if r.shutdownCond != nil {
		r.mu.Lock()
		r.shutdownCond.Broadcast()
		r.mu.Unlock()
	}

	r.notifyChange()
}

// resetSessionLocked tears down a single session for ResetChat: collects any
// live process into toClose (caller Close()s it outside r.mu), drops the
// session's record + sessionID and backend-override mappings, and bumps
// closedActive when the session counted toward maxProcs. Caller MUST hold
// r.mu and is responsible for cleaning up r.ss.byChat.
func (r *Router) resetSessionLocked(key string, toClose *[]processIface, closedActive *int) {
	s := r.ss.sessions[key]
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
		delete(r.ss.idToKey, id)
	}
	delete(r.ss.sessions, key)
	// Drop the keyhash → key fast-path entry; equality-guarded so a rename
	// collision can't remove the wrong entry (#1646).
	if r.ss.keyhash != nil {
		kh := persist.KeyHash(key)
		if r.ss.keyhash[kh] == key {
			delete(r.ss.keyhash, kh)
		}
	}
	// Drop any one-shot backend pick so an abandoned dashboard choice does
	// not leak into backendOverrides.
	delete(r.bkStore.backendOverrides, key)
}

// AgentOpts provides per-agent overrides for session creation.
//
// ExtraArgs aliasing contract: callers receiving AgentOpts from KeyResolver
// get a freshly-cloned ExtraArgs (safe to append). Callers populating
// AgentOpts must own ExtraArgs exclusively — do NOT alias slices held by
// other goroutines.
type AgentOpts struct {
	Model     string
	ExtraArgs []string
	Workspace string // override workspace (empty = use default/chat override)
	Backend   string // backend ID ("claude" / "kiro" / …); empty = router default
	// AccessProfile names the access profile (auth/upstream env overlay +
	// default model) to spawn under. Empty = global default. Resume
	// continuity takes precedence over the caller's value: a dead session
	// must resume on the SAME auth chain it was created on (RFC
	// project-access-profile §7).
	AccessProfile string
	// Effort overrides the backend's thinking-effort tier for this session.
	// Empty = inherit. Only ACP-protocol backends act on it.
	Effort string
	// SystemPrompt is the text appended to the CLI's system prompt
	// (`--append-system-prompt` via cli.SpawnOptions.AppendSystemPrompt).
	// Layers (agents[<id>].system_prompt → planner prompt → scratch context)
	// are pre-joined by JoinSystemPrompts into this one string. Empty = no
	// flag. Never put the flag into ExtraArgs instead — it is denylisted
	// there and silently stripped (#2493). Only the Claude backend renders it.
	SystemPrompt string
	Exempt       bool // exempt from TTL, eviction, and activeCount (planner sessions)
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
	// Flag-injection guard: opts.Model originates from dashboard WS, upstream
	// RPC, or planner config and must be validated at the router boundary.
	if err := validateModel(opts.Model); err != nil {
		return nil, 0, err
	}
	// Backend flows into slog attrs and persisted state JSON; this gate only
	// rejects shape-invalid input (wrapperFor tolerates unknown backends).
	if err := validateBackend(opts.Backend); err != nil {
		return nil, 0, err
	}
	r.mu.Lock()

	// N concurrent GetOrCreate on the same fresh key would each call
	// spawnSession and only one would win the shim-socket dial guard. Park on
	// the per-spawn done-channel (closed by spawnSession's defer) while a
	// spawn is in flight, then relock and re-evaluate.
	for {
		if s, ok := r.ss.sessions[key]; ok {
			if s.isAlive() {
				s.touchLastActive()
				r.mu.Unlock()
				return s, SessionExisting, nil
			}
			// The resume branch must honour the SAME coalesce guard as the
			// not-found path: a second concurrent spawnSession would reuse the
			// in-flight channel and its defer would close an already-closed
			// channel → panic (#2221). The winner owns the close.
			if ch, inflight := r.pp.SpawnInFlight(key); inflight {
				r.mu.Unlock()
				select {
				case <-ctx.Done():
					return nil, 0, ctx.Err()
				case <-ch:
				}
				r.mu.Lock()
				continue
			}
			slog.Info("session process exited, resuming", "key", key, "session_id", s.getSessionID())
			s, err := r.spawnSession(ctx, key, s.getSessionID(), opts)
			if err != nil {
				return nil, 0, fmt.Errorf("session %s: %w", key, err)
			}
			return s, SessionResumed, nil
		}
		ch, inflight := r.pp.SpawnInFlight(key)
		if !inflight {
			break
		}
		// Someone else is spawning this key; wait unlocked, then re-evaluate
		// (pick up their session, or spawn our own on their failure).
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-ch:
		}
		r.mu.Lock()
	}

	// Debug, not Info: spawnSession logs "session spawned" at Info moments later.
	slog.Debug("creating new session", "key", key)
	// Consume the per-key shim-stuck flag (set by a Reset whose
	// socket-gone wait timed out, #1324) under r.mu BEFORE spawnSession,
	// which unlocks/relocks internally; apply the wrap on the error path.
	stuck := r.pp.ConsumeShimStuck(key)
	s, err := r.spawnSession(ctx, key, "", opts)
	if err != nil {
		if stuck {
			// errors.Is chain lets callers pin on ErrShimStuck.
			return nil, 0, fmt.Errorf("session %s: %w: %w", key, ErrShimStuck, err)
		}
		return nil, 0, fmt.Errorf("session %s: %w", key, err)
	}
	return s, SessionNew, nil
}

// spawnParams carries the pure-computation output of resolveSpawnParamsLocked:
// the merged backend, model, args, workspace, and (possibly downgraded)
// resumeID that spawnSession feeds into cli.SpawnOptions.
type spawnParams struct {
	BackendID string // effective backend ID after override/fallback resolution
	Wrapper   *cli.Wrapper
	Model     string
	Args      []string
	// Effort is the resolved thinking-effort tier ("" = pass no flag).
	Effort string
	// SystemPrompt is AgentOpts.SystemPrompt passed through unchanged; there
	// is no backend- or router-level tier for it.
	SystemPrompt string
	Workspace    string
	// ResumeID after workspace/jsonl guard. Empty means "spawn fresh".
	ResumeID string
	// AccessProfileID is the resolved access-profile name ("" = global
	// default), recorded on the session and persisted so a post-restart
	// resume relocks the same auth chain.
	AccessProfileID string
	// AccessProfileEnv is the RAW profile env map (still holding *_FILE
	// references); nil when AccessProfileID is "". spawnSession expands it
	// AFTER releasing r.mu — file reads must not happen under the lock.
	AccessProfileEnv map[string]string
	// Overlay is the per-request layer that went into Model/Effort/Args. The
	// shim persists it so the arg-drift comparison on the next restart can
	// re-merge it against current config (#2494). Always populated: a spawn
	// with no overrides yields the zero value ("known and empty").
	Overlay shim.SpawnOverlay
}

// resolveSpawnParamsLocked computes the merged spawn parameters for a new
// session. It is the SINGLE source of truth for workspace + backend + model +
// args + resumeID resolution; any spawn-adjacent path (Takeover, Reattach…)
// MUST route through it rather than re-implement the precedence (#735).
// workspace_resolver_contract_test.go asserts exactly one
// `workspace = opts.Workspace` site survives in this file. No I/O beyond
// bounded stat/ReadDir probes; consumes the one-shot dashboard backend pick.
//
// LOCK: caller must hold r.mu for writing.
func (r *Router) resolveSpawnParamsLocked(key, resumeID string, opts AgentOpts) spawnParams {
	// Backend precedence: opts.Backend > one-shot backendOverrides[key]
	// (consumed here) > existing session's Backend (resume continuity) >
	// defaultBackend. Without the existing-session tier a dead kiro session
	// would respawn on the default backend, fail the resume probe, and
	// silently downgrade to a fresh claude session.
	reqBackend := opts.Backend
	if len(r.bkStore.backendOverrides) > 0 {
		if reqBackend == "" {
			reqBackend = r.bkStore.backendOverrides[key]
		}
		delete(r.bkStore.backendOverrides, key)
	}
	if reqBackend == "" {
		if old := r.ss.sessions[key]; old != nil {
			if b := old.Backend(); b != "" {
				reqBackend = b
			}
		}
	}
	wrapper, backendID := r.wrapperFor(reqBackend)

	// Access-profile precedence (RFC project-access-profile §2/§7): existing
	// session's recorded profile (RESUME LOCK — a dead session must resume on
	// the SAME auth chain; re-resolving would cross accounts) > one-shot
	// dashboard override (consumed here) > opts.AccessProfile > "". An unknown
	// ID resolves to "" with a warning: the SAFE default, never a wrong account.
	accessProfileID := opts.AccessProfile
	if len(r.bkStore.accessProfileOverrides) > 0 {
		if ov, ok := r.bkStore.accessProfileOverrides[key]; ok {
			accessProfileID = ov
			delete(r.bkStore.accessProfileOverrides, key)
		}
	}
	if old := r.ss.sessions[key]; old != nil {
		if ap := old.AccessProfile(); ap != "" {
			accessProfileID = ap
		}
	}
	// defaultAccessProfile is the lowest tier: applies ONLY when every source
	// above left the ID empty, so picks and resume-locked profiles always win.
	if accessProfileID == "" && r.defaultAccessProfile != "" {
		accessProfileID = r.defaultAccessProfile
	}
	var accessProfileEnv map[string]string
	if accessProfileID != "" {
		if ap, ok := r.accessProfiles[accessProfileID]; ok {
			accessProfileEnv = ap.Env
		} else {
			slog.Warn("access profile not found; falling back to global default",
				"key", key, "access_profile", accessProfileID)
			accessProfileID = ""
		}
	}

	// Per-request overlay the shim persists for the drift re-merge (#2494).
	// AccessProfile is the RESOLVED id so the drift side resolves default_model
	// the same way. ExtraArgs is cloned: the overlay outlives r.mu (JSON-encoded
	// after the unlock), so it must not alias the caller's slice.
	overlay := shim.SpawnOverlay{
		Model:         opts.Model,
		Effort:        opts.Effort,
		ExtraArgs:     slices.Clone(opts.ExtraArgs),
		AccessProfile: accessProfileID,
		// Argv-bearing and per-session, so it must ride in the overlay (#2493).
		AppendSystemPrompt: opts.SystemPrompt,
	}

	// mergeArgvLayers is shared verbatim with driftCompareArgs so the two argv
	// cannot diverge. Chain: backend defaults < access-profile default_model <
	// opts < session tuning (operator's dashboard pick for THIS session,
	// tuningspec-validated at write and at store load). Effort deliberately has
	// NO access-profile tier (docs/rfc/kiro-effort-control.md §4.2).
	// A key with no session yet may carry a pre-spawn pick
	// (bkStore.tuningOverrides); spawnSession consumes it onto the fresh entry.
	var tuningModel, tuningEffort string
	if old := r.ss.sessions[key]; old != nil {
		tuningModel, tuningEffort = old.TuningModel(), old.TuningEffort()
	} else if pt, ok := r.bkStore.tuningOverrides[key]; ok {
		tuningModel, tuningEffort = pt.Model, pt.Effort
	}
	merged := mergeArgvLayers(
		r.backendDefaultsFor(backendID),
		profileDefaultModelFor(r.accessProfiles, accessProfileID),
		overlay, tuningModel, tuningEffort)
	model, effort, args := merged.Model, merged.Effort, merged.Args

	// Workspace: opts override > per-chat override > old session workspace >
	// default. The chat tier goes through resolveWorkspaceLocked — the single
	// chat-level resolution point — so it cannot drift from GetWorkspace (#883).
	workspaceOverridden := false
	var workspace string
	if opts.Workspace != "" {
		workspace = opts.Workspace
		workspaceOverridden = true
	} else if chatKey := chatKeyFor(key); chatKey != key {
		workspace = r.resolveWorkspaceLocked(chatKey)
		// Only an explicit per-chat override pins out the resume tier; a bare
		// default must still let the resume-session workspace win below.
		if _, ok := r.wsStore.Lookup(chatKey); ok {
			workspaceOverridden = true
		}
	} else {
		workspace = r.defaultCWD
	}
	if !workspaceOverridden && resumeID != "" {
		if old := r.ss.sessions[key]; old != nil {
			if ws := old.Workspace(); ws != "" {
				workspace = ws
			}
		}
	}

	// ResumeID guard: drop when the backend's on-disk resume target is missing
	// so the spawn falls through to a fresh session instead of failing on
	// "No conversation found". The probe is backend-aware (see resolveResumeID).
	resumeID = resolveResumeID(backendID, r.claudeDir, r.kiroSessionsDir, workspace, key, resumeID)

	// Canonicalize on-disk case for fresh spawns: on case-insensitive APFS a
	// differently-cased spelling forks two project identities for one tree.
	// Ordering contract: runs AFTER the resume guard, because claude's --resume
	// looks up the jsonl under the slug derived from the exact spelling the
	// previous incarnation stored; re-casing would orphan that transcript.
	if resumeID == "" {
		workspace = osutil.CanonicalCase(workspace)
	}

	return spawnParams{
		BackendID:        backendID,
		Wrapper:          wrapper,
		Model:            model,
		Args:             args,
		Effort:           effort,
		SystemPrompt:     merged.SystemPrompt,
		Workspace:        workspace,
		ResumeID:         resumeID,
		AccessProfileID:  accessProfileID,
		AccessProfileEnv: accessProfileEnv,
		Overlay:          overlay,
	}
}

// sessionOverrides is the operator-owned per-session state that must outlive
// the process it was set on: dashboard tuning override and user label with its
// origin (label+origin travel as one unit). Captured by
// snapshotOldSessionLocked and re-applied by installFreshSessionLocked.
type sessionOverrides struct {
	tuningModel  string
	tuningEffort string
	userLabel    string
	labelOrigin  string
}

// consumePendingTuningLocked moves a pre-spawn tuning pick
// (bkStore.tuningOverrides, recorded by SetSessionTuning for a key with no
// session) onto the overrides installFreshSessionLocked will stamp on the
// new entry, and drops the one-shot record. Returns ov unchanged when there
// is none. Caller holds r.mu.
func (r *Router) consumePendingTuningLocked(key string, ov sessionOverrides) sessionOverrides {
	pt, ok := r.bkStore.tuningOverrides[key]
	if !ok {
		return ov
	}
	delete(r.bkStore.tuningOverrides, key)
	out := ov
	out.tuningModel = pt.Model
	out.tuningEffort = pt.Effort
	return out
}

// snapshotOldSessionLocked captures the per-session fields that spawnSession
// needs to read AFTER it releases r.mu. Pure read; nil-safe.
//
// LOCK: caller MUST hold r.mu — these fields are written under r.mu by
// sibling paths (RegisterCronStub, evictOldest, spawnSession itself), so
// reading them after r.mu is released races those writers.
func snapshotOldSessionLocked(old *ManagedSession) ([]string, float64, float64, int64, sessionOverrides) {
	if old == nil {
		return nil, 0, 0, 0, sessionOverrides{}
	}
	var oldPrevIDs []string
	if len(old.prevSessionIDs) > 0 {
		oldPrevIDs = make([]string, len(old.prevSessionIDs))
		copy(oldPrevIDs, old.prevSessionIDs)
	}
	// Preserve cumulative cost across process replacement so the dashboard
	// doesn't flash $0.00. Prefer the live process's value; fall back to the
	// store-restored total when no process is attached.
	var oldTotalCost float64
	if p := old.loadProcess(); p != nil {
		oldTotalCost = p.TotalCost()
	}
	if oldTotalCost == 0 {
		oldTotalCost = loadTotalCost(&old.totalCost)
	}
	// Carry the original creation timestamp so the session keeps its sidebar
	// position; installFreshSessionLocked stamps now when zero.
	oldCreatedAt := old.createdAt.Load()
	// costSpent MUST carry across the replacement (same logical session).
	// lastCumulativeCost is deliberately NOT carried: the new CLI re-counts
	// from ~0, so the baseline must restart at 0 (see TurnCostDelta). Known
	// bounded loss: a turn still in flight on the OLD process lands its delta
	// on the orphaned struct; cost is advisory, not billing-authoritative (#2284).
	oldCostSpent := loadTotalCost(&old.costSpent)
	// Overrides are snapshotted HERE, under the same r.mu hold, from the same
	// object as history/cost/createdAt. installFreshSessionLocked must not
	// re-read r.ss.sessions[key]: the entry may be swapped or removed during
	// the unlocked history copy, pairing one session's history with another's tuning.
	ov := sessionOverrides{
		tuningModel:  old.TuningModel(),
		tuningEffort: old.TuningEffort(),
		userLabel:    old.UserLabel(),
		labelOrigin:  old.LabelOrigin(),
	}
	return oldPrevIDs, oldTotalCost, oldCostSpent, oldCreatedAt, ov
}

// collectPreviousHistory gathers JSONL-backed history entries and the
// session ID chain for a respawn. Returns (entries, chain, userTurns);
// userTurns is computed here so the spawn path seeds persistedUserTurns
// without an independent O(n) rescan (#2089). Pure computation; caller must
// hold r.mu if it needs serialisation w.r.t. sibling spawn attempts. The
// dead-process branch prefers EventEntries() over persistedHistory because it
// includes live events accumulated since the JSONL snapshot was loaded.
func collectPreviousHistory(oldSess *ManagedSession, oldPrevIDs []string, resumeID string) ([]clievent.EventEntry, []string, int64) {
	if oldSess == nil {
		return nil, nil, 0
	}

	// p.EventEntries() must be invoked WITHOUT holding historyMu: it takes
	// eventLog.mu internally, and a historyMu → eventLog.mu order would
	// deadlock against any sink calling back into the session. So: snapshot
	// the process pointer + persistedHistory under RLock, release, then read
	// entries (the old Process keeps its eventLog alive until GC).
	var entries []clievent.EventEntry
	// userTurns == -1 signals "unknown"; the dead-process branch counts once.
	userTurns := int64(-1)
	oldSess.historyMu.RLock()
	p := oldSess.loadProcess()
	var persistedSnapshot []clievent.EventEntry
	if (p == nil || p.Alive()) && len(oldSess.persistedHistory) > 0 {
		persistedSnapshot = make([]clievent.EventEntry, len(oldSess.persistedHistory))
		copy(persistedSnapshot, oldSess.persistedHistory)
		userTurns = oldSess.persistedUserTurns.Load()
	}
	oldSess.historyMu.RUnlock()

	if p != nil && !p.Alive() {
		entries = p.EventEntries()
	} else {
		entries = persistedSnapshot
	}

	// Append the old session ID to the chain only when it differs from
	// resumeID (a new CLI session replaces the old one, not a same-ID resume).
	var prevIDs []string
	if oldID := oldSess.getSessionID(); oldID != "" && oldID != resumeID {
		prevIDs = make([]string, len(oldPrevIDs), len(oldPrevIDs)+1)
		copy(prevIDs, oldPrevIDs)
		prevIDs = append(prevIDs, oldID)
	} else {
		prevIDs = oldPrevIDs
	}
	// Cap the chain to bound sessions.json size and JSONL load time; the
	// retained tail carries the most recent context.
	if len(prevIDs) > maxPrevSessionIDs {
		prevIDs = prevIDs[len(prevIDs)-maxPrevSessionIDs:]
	}
	if userTurns < 0 {
		userTurns = countUserTurns(entries)
	}
	return entries, prevIDs, userTurns
}

// countUserTurns returns the number of Type=="user" entries in entries; the
// single definition of "what counts as a user turn".
func countUserTurns(entries []clievent.EventEntry) int64 {
	var n int64
	for i := range entries {
		if entries[i].Type == "user" {
			n++
		}
	}
	return n
}

// spawnSession creates a new process, optionally resuming an existing session.
// LOCK: enter with r.mu held. This function releases and re-acquires r.mu
// internally (around Spawn() and history collection) to avoid blocking other
// goroutines during slow protocol init (e.g. ACP handshake). Callers MUST NOT
// hold any other lock when invoking; the defer reacquires r.mu only.
func (r *Router) spawnSession(ctx context.Context, key string, resumeID string, opts AgentOpts) (*ManagedSession, error) {
	// Shutdown gate (#1822): r.stopped is set under r.mu immediately before
	// Shutdown's snapshot and read here with r.mu held on entry, so gate and
	// snapshot are mutually exclusive (no TOCTOU) and a late spawn cannot
	// install a shim+CLI the snapshot missed. Sits BEFORE the done-channel
	// defer so nothing is left dangling; unlock-on-error like every path below.
	if r.stopped.Load() {
		r.mu.Unlock()
		return nil, ErrRouterStopped
	}

	// Mark this key as spawning so ReconnectShims does not treat the fresh
	// shim's state file as an orphan. Every return path below leaves r.mu
	// unlocked; the defer relocks and close(doneCh) wakes parked GetOrCreates.
	// BeginSpawn reuses a guard pre-installed by ResetAndRecreate so the marker
	// stays continuous and no concurrent GetOrCreate spawns with other opts (#775).
	doneCh := r.pp.BeginSpawn(key)
	defer func() {
		r.mu.Lock()
		r.pp.EndSpawn(key, doneCh)
		r.mu.Unlock()
	}()

	// Exempt sessions (planners) bypass maxProcs but have their own limit.
	if !opts.Exempt {
		// Only recount (O(n)) when we appear to be at capacity, to detect
		// drift from undetected process exits before refusing. All three
		// checks run under r.mu; int64 locals avoid 32-bit wrap.
		maxProcs64 := int64(r.maxProcs)
		pending64 := int64(r.pp.PendingSpawns())
		if r.ss.activeCount.Load()+pending64 >= maxProcs64 {
			r.countActive()
		}
		if r.ss.activeCount.Load()+pending64 >= maxProcs64 {
			if !r.evictOldest() {
				r.mu.Unlock()
				return nil, fmt.Errorf("%w (%d), all busy", ErrMaxProcs, r.maxProcs)
			}
			// evictOldest() drops r.mu around proc.Close(), so pendingSpawns
			// may have changed; re-read it or a stale value over-spawns past
			// maxProcs / falsely refuses (#2082).
			pending64 = int64(r.pp.PendingSpawns())
			if r.ss.activeCount.Load()+pending64 >= maxProcs64 {
				r.mu.Unlock()
				return nil, fmt.Errorf("%w (%d), all busy", ErrMaxProcs, r.maxProcs)
			}
		}
	} else {
		// Per-namespace sub-quota runs FIRST so a noisy cron chat cannot push
		// planner / sys stubs out of the shared pool; the global
		// maxExemptSessions ceiling is a relief valve for namespaces without
		// sub-quota wiring. One combined walk yields both counts.
		kind := exemptKind(key)
		perKind, totalExempt := r.countExemptCombined(kind)
		if kind != "" {
			if perKind >= exemptCapFor(kind) {
				r.mu.Unlock()
				return nil, fmt.Errorf("%w: %s namespace (%d)", ErrMaxExemptSessions, kind, exemptCapFor(kind))
			}
		}
		if totalExempt >= maxExemptSessions {
			r.mu.Unlock()
			return nil, fmt.Errorf("%w (%d)", ErrMaxExemptSessions, maxExemptSessions)
		}
	}

	// Under r.mu; consumes the one-shot backendOverrides entry for `key`.
	sp := r.resolveSpawnParamsLocked(key, resumeID, opts)
	wrapper := sp.Wrapper
	backendID := sp.BackendID
	workspace := sp.Workspace
	resumeID = sp.ResumeID
	accessProfileID := sp.AccessProfileID
	accessProfileEnv := sp.AccessProfileEnv

	// argv-bearing fields come from the shared constructor so this path and
	// the arg-drift comparison cannot diverge. DebugFile uses the
	// side-effecting cliDebugFileFor (log pre-created 0600) where drift uses
	// the read-only cliDebugPathFor.
	spawnOpts := r.argvSpawnOptions(sp.Model, sp.Effort, r.cliDebugFileFor(key), sp.SystemPrompt, sp.Args)
	// ResumeID is session state, not config: the drift side strips it
	// (stripResumeArgs) so a resumed session is not read as drift.
	spawnOpts.ResumeID = resumeID
	// Always non-nil: an empty overlay must be recorded as "known and empty"
	// or the drift check treats this shim as legacy (#2494).
	spawnOverlay := sp.Overlay
	spawnOpts.SpawnOverlay = &spawnOverlay
	// Process wiring BuildArgs never reads.
	spawnOpts.Key = key
	spawnOpts.WorkingDir = workspace
	spawnOpts.NoOutputTimeout = r.noOutputTimeout
	spawnOpts.TotalTimeout = r.totalTimeout

	// ── Lock release 1: Spawn may block (ACP Init handshake, process startup),
	// so r.mu is released around it. pendingSpawns keeps a concurrent Cleanup
	// from pruning the slot we are about to fill. The happy path decrements via
	// releaseLocked() once r.mu is re-taken; the deferred release() is an
	// idempotent safety net for panics / early returns.
	slot := r.acquirePendingSpawnSlotLocked()
	defer slot.release()
	r.mu.Unlock()
	if wrapper == nil {
		return nil, fmt.Errorf("spawn process (backend %q): %w", backendID, ErrNoCLIWrapper)
	}
	// Expand the access-profile env overlay OUTSIDE r.mu (reads *_FILE secrets
	// from disk). FAIL-LOUD on a missing secret — silently spawning on the
	// global default would run this session on the wrong account.
	if len(accessProfileEnv) > 0 {
		overlay, err := resolveEnvOverlay(accessProfileEnv)
		if err != nil {
			return nil, fmt.Errorf("access profile %q: %w", accessProfileID, err)
		}
		spawnOpts.EnvOverlay = overlay
	}
	// Panic-safe: if Spawn panics, pendingSpawns must still be decremented or
	// the router permanently refuses new sessions with ErrMaxProcs.
	// wrapper.Runner() is the placement seam (agentcore-cloud-sandbox RFC §4.2).
	proc, err := panicSafeSpawn(ctx, wrapper.Runner(), spawnOpts, key, backendID)
	r.mu.Lock()
	slot.releaseLocked()
	if err != nil {
		r.mu.Unlock()
		return nil, fmt.Errorf("spawn process: %w", err)
	}

	// ── TOCTOU guard 1: while unlocked for Spawn(), a concurrent spawnSession
	// may have installed a live session for this key; if so discard ours.
	if existing, ok := r.ss.sessions[key]; ok && existing.isAlive() {
		r.mu.Unlock()
		proc.Close()
		return existing, nil
	}

	// ── Lock release 2: copy old session history under historyMu only.
	// Holding both r.mu and historyMu would violate lock ordering (historyMu
	// is acquired independently by event injection). The old reference is
	// safe to read because sessions are never mutated after creation, only replaced.
	old := r.ss.sessions[key]
	// Capture the SID being replaced: if the respawn rotates to a different
	// effectiveSID, idToKey[oldSID] would dangle and later mis-route a resume;
	// installFreshSessionLocked clears it (#2093).
	var oldSID string
	if old != nil {
		oldSID = old.getSessionID()
	}
	oldPrevIDs, oldTotalCost, oldCostSpent, oldCreatedAt, oldOverrides := snapshotOldSessionLocked(old)
	if old == nil {
		oldOverrides = r.consumePendingTuningLocked(key, oldOverrides)
	}
	r.mu.Unlock()

	oldHistory, prevIDs, oldUserTurns := collectPreviousHistory(old, oldPrevIDs, resumeID)

	// prevIDs holds ONLY the real same-key sessionID rotation chain; no
	// workspace-based chain guessing (docs/rfc/project-stable-session-key.md §9.1).

	r.mu.Lock()
	// ── TOCTOU guard 2: same check as guard 1 for the history-copy unlock window.
	if existing, ok := r.ss.sessions[key]; ok && existing.isAlive() {
		r.mu.Unlock()
		proc.Close()
		return existing, nil
	}

	s := r.installFreshSessionLocked(
		key, proc, workspace, backendID, accessProfileID, wrapper, resumeID,
		oldHistory, prevIDs, oldTotalCost, oldCostSpent, oldCreatedAt, opts.Exempt, oldSID,
		oldUserTurns, oldOverrides,
	)
	r.mu.Unlock()

	r.bindNewSessionHistory(ctx, s, proc, key, resumeID, workspace, prevIDs, oldHistory)

	r.notifyChange()
	return s, nil
}

// bindNewSessionHistory loads the resume-history chain into a freshly-spawned
// session and THEN installs the event-log persist sink, in that exact order:
// SetPersistSink must run only AFTER every InjectHistory call, otherwise the
// bulk replay entries are written back to disk instead of being dropped as
// replayPhase (RFC §3.2.2 / §3.2.3, #733).
//
// LOCK: must NOT be called with r.mu held — history is injected under
// historyMu, which is never held together with r.mu (router_core.go).
func (r *Router) bindNewSessionHistory(
	ctx context.Context,
	s *ManagedSession,
	proc *cli.Process,
	key string,
	resumeID string,
	workspace string,
	prevIDs []string,
	oldHistory []clievent.EventEntry,
) {
	r.loadResumeHistoryOnSpawn(ctx, s, key, resumeID, workspace, prevIDs, oldHistory)
	r.installPersistSink(proc, key)
}

// installFreshSessionLocked attaches a freshly-spawned process to the router
// indices + event log. Pure state mutation, no I/O. Callers must invoke
// installPersistSink AFTER this returns (RFC §3.2.2).
//
// LOCK: caller must hold r.mu for writing.
func (r *Router) installFreshSessionLocked(
	key string,
	proc *cli.Process,
	workspace string,
	backendID string,
	accessProfileID string,
	wrapper *cli.Wrapper,
	resumeID string,
	oldHistory []clievent.EventEntry,
	prevIDs []string,
	oldTotalCost float64,
	oldCostSpent float64,
	oldCreatedAt int64,
	exempt bool,
	oldSID string,
	oldUserTurns int64,
	overrides sessionOverrides,
) *ManagedSession {
	s := &ManagedSession{
		key:              key,
		persistedHistory: oldHistory,
		prevSessionIDs:   prevIDs,
		exempt:           exempt,
		runStore:         r.sessionRuns,
		onSessionID: func(id string) {
			r.mu.Lock()
			r.kid.Track(id)
			if id != "" {
				r.ss.idToKey[id] = key
			}
			r.mu.Unlock()
		},
	}
	// Seed persistedUserTurns so the proc==nil snapshot branch and AutoTitler
	// min-turn gate see the correct count immediately; s is unpublished so
	// there are no concurrent readers. oldUserTurns was computed by
	// collectPreviousHistory (#2089).
	if len(oldHistory) > 0 {
		s.persistedUserTurns.Store(oldUserTurns)
	}
	storeTotalCost(&s.totalCost, oldTotalCost)
	// lastCumulativeCost stays 0 on purpose: the new CLI incarnation re-counts
	// from scratch, so the first post-spawn reading is itself the delta.
	storeTotalCost(&s.costSpent, oldCostSpent)
	// Sidebar order anchor: inherit oldCreatedAt when replacing a prior incarnation.
	if oldCreatedAt != 0 {
		s.createdAt.Store(oldCreatedAt)
	} else {
		s.initCreatedAtIfUnset()
	}
	s.setWorkspace(workspace)
	s.SetBackend(backendID)
	// Recorded so a later resume relocks the same auth chain (§7).
	s.SetAccessProfile(accessProfileID)
	s.SetCLIName(wrapper.CLIName)
	s.SetCLIVersion(wrapper.CLIVersion)
	// Operator-owned state must outlive the process: this spawn's argv was
	// built from the OLD entry's tuning, and without carrying it the next TTL
	// recycle drops back to config default and a restart reads the shim as
	// arg-drift. Values come from the snapshotOldSessionLocked capture, never
	// from a re-read of r.ss.sessions[key].
	s.SetTuningModel(overrides.tuningModel)
	s.SetTuningEffort(overrides.tuningEffort)
	s.SetUserLabel(overrides.userLabel)
	s.setLabelOrigin(overrides.labelOrigin) // label+origin travel as one unit
	// Serialises storeProcess + seededLen reset under historyMu so a concurrent
	// InjectHistory observes the (process, seededLen) pair and forwards only
	// genuinely-new tail; same lock-protected path as the reconnect branch.
	snapshot := s.attachProcessAndSnapshotPersisted(proc)
	// Notify the dashboard on out-of-band turn completion (as ReconnectShims
	// does). SetOnTurnDone is mu-guarded inside Process, so post-storeProcess is safe.
	proc.SetOnTurnDone(func() { r.notifyChange() })
	if len(snapshot) > 0 {
		proc.InjectHistory(snapshot)
	}
	// Prefer resumeID, else whatever the protocol captured during Init (ACP
	// returns a UUID synchronously; claude stays empty until the first turn).
	// Without the fallback a fresh kiro session has an empty sessionID and
	// saveStore drops it, losing the session across restarts.
	effectiveSID := resumeID
	if effectiveSID == "" {
		effectiveSID = proc.SessionID()
	}
	s.setSessionID(effectiveSID)
	// When a respawn rotates the SID, drop the old idToKey entry iff it still
	// points at this key, so a resume of the retired SID cannot be mis-routed;
	// the "still maps to key" guard avoids clobbering another live session's
	// entry (#2093).
	if oldSID != "" && oldSID != effectiveSID {
		if mapped, ok := r.ss.idToKey[oldSID]; ok && mapped == key {
			delete(r.ss.idToKey, oldSID)
		}
	}
	if effectiveSID != "" {
		r.kid.Track(effectiveSID)
		r.ss.idToKey[effectiveSID] = key
	}
	s.touchLastActive()
	r.publishSessionLocked(key, s, false)
	if !exempt {
		r.ss.activeCount.Add(1)
	}

	r.ss.dirty = true
	r.ss.gen.Add(1)
	logSessionLifecycle("spawned", key, "active", r.ss.activeCount.Load(), "exempt", exempt)
	// Counters bumped inside the write-lock at the authoritative "spawn
	// succeeded" point. Exempt sessions are excluded: they don't consume a
	// slot and planner/scratch churn would muddy the signal.
	if !exempt {
		metrics.SessionCreateTotal.Add(1)
		// Per-backend mirror of activeCount; decremented at every site that
		// decrements activeCount.
		metrics.RecordSessionActive(s.Backend(), 1)
	}
	return s
}

// installPersistSink wires the event-log persister into the given Process's
// EventLog. No-op when the persister is disabled or proc is not a real
// *cli.Process (test fakes). Must be called AFTER any InjectHistory calls
// have completed (RFC §3.2.2).
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
	persisterSink := r.eventLogPersister.SinkFor(key)
	keyhash := persist.KeyHash(key)
	sink := newEventLogSink(persisterSink, r.attachmentTracker, keyhash)
	// Single-entry sink lets EventLog.Append skip a 1-slot slice literal;
	// both sinks feed the same persisterSink (#410).
	sinkOne := newEventLogSinkOne(persisterSink, r.attachmentTracker, keyhash)
	log.SetPersistSinkPair(sink, sinkOne)
}

// loadResumeHistoryOnSpawn synchronously loads the JSONL chain for a resume
// with no in-memory history yet and injects it into s. No-op otherwise.
//
// historyWg tracks the call so Shutdown can drain in-flight loads. The load
// ctx is parented on r.historyCtx (Shutdown's historyCancel wakes the reader
// immediately) with the caller ctx fanned in via context.AfterFunc, and is
// skipped entirely once historyCtx is already cancelled.
//
// LOCK: must NOT be called with r.mu held — InjectHistory acquires
// session.historyMu independently, and the reader can take seconds.
func (r *Router) loadResumeHistoryOnSpawn(
	ctx context.Context,
	s *ManagedSession,
	key, resumeID, workspace string,
	prevIDs []string,
	oldHistory []clievent.EventEntry,
) {
	if resumeID == "" || r.claudeDir == "" || len(oldHistory) > 0 {
		return
	}

	// Decide skip-or-load BEFORE Add(1), and make check+Add one critical
	// section vs Shutdown's historyCancel() under historyWgMu: an Add(1) on a
	// WaitGroup already drained to 0 while Shutdown's Wait() runs is a
	// WaitGroup misuse that can panic (#1655, #2186). Mirrors runHistoryTask.
	r.historyWgMu.Lock()
	if r.historyCtx != nil && r.historyCtx.Err() != nil {
		r.historyWgMu.Unlock()
		return
	}
	r.historyWg.Add(1)
	r.historyWgMu.Unlock()

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
		allEntries := r.historyLoader.LoadHistoryChainTail(
			histCtx, r.claudeDir, ids, workspace, maxPersistedHistory,
		)
		if len(allEntries) > 0 {
			s.InjectHistory(allEntries)
			slog.Info("loaded session history on resume", "key", key, "entries", len(allEntries), "chain", len(ids))
		}
	}()
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
		delete(r.ss.idToKey, id)
	}
	r.indexDel(key)
	delete(r.ss.sessions, key)
	if !keepBackendOverride {
		delete(r.bkStore.backendOverrides, key)
		// One-shot dashboard pick with the same lifecycle as backendOverrides;
		// terminal removal must clear it so an abandoned pick does not leak.
		delete(r.bkStore.accessProfileOverrides, key)
		delete(r.bkStore.tuningOverrides, key)
		// The shim-stuck flag is only consumed by GetOrCreate, so terminal
		// removals must clear it or the entry lives for the process lifetime.
		r.pp.ClearShimStuck(key)
	}
}

// resetLocked performs the in-lock teardown shared by Reset and
// ResetAndDiscardOverride. Caller must run the finishResetUnlocked
// sequence after releasing the lock.
//
// Returns the live process (for Close after lock release), the session
// UUID captured before teardown (for the retired-session notification —
// r.ss.sessions[key] is unregistered here, so callers cannot recover the
// UUID after the lock drops), and the success flag.
//
// LOCK: caller must hold r.mu for writing.
func (r *Router) resetLocked(key string) (processIface, string, bool) {
	s, ok := r.ss.sessions[key]
	if !ok {
		return nil, "", false
	}
	proc := s.loadProcess()
	wasActive := !s.exempt && proc != nil && proc.Alive()
	backend := s.Backend()
	sessionID := s.SessionID()
	r.unregisterSessionLocked(key, s, false)
	if wasActive {
		if r.ss.activeCount.Add(-1) < 0 {
			r.ss.activeCount.Store(0)
		}
		metrics.RecordSessionActive(backend, -1)
	}
	r.ss.dirty = true
	r.ss.gen.Add(1)
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
// per-chat workspace override, so a concurrent SetWorkspace cannot survive a
// bare Reset+delete pair and leak into the next session.
func (r *Router) ResetAndDiscardOverride(key string) {
	r.mu.Lock()
	proc, sessionID, hadSession := r.resetLocked(key)
	r.wsStore.Delete(key)
	r.mu.Unlock()
	if !hadSession {
		return
	}
	r.finishResetUnlocked(key, sessionID, proc)
}

// finishResetUnlocked runs the post-unlock teardown shared by Reset and
// ResetAndDiscardOverride. Must be called without r.mu held. sessionID
// is the UUID captured by resetLocked before unregister cleared
// r.ss.sessions[key]; pass through as-is to notifyKeyRetired so the
// dashboard history-sort hook can stamp retired_at.
func (r *Router) finishResetUnlocked(key, sessionID string, proc processIface) {
	if proc != nil && proc.Alive() {
		proc.Close()
	}
	// proc may be nil/!Alive with the shim socket still bound (CLI crash,
	// stale pointer); give it a short window to disappear so a same-key
	// StartShim does not hit the dial-first "refusing to clobber" guard.
	// Bounded so a truly stuck shim surfaces the real error instead of hanging.
	gone := waitSocketGoneForKey(key, 2*time.Second)
	// Broadcast must happen under r.mu (see evictOldest).
	r.mu.Lock()
	if !gone {
		// Flag the key so the next GetOrCreate wraps any spawn error with
		// ErrShimStuck (#1324); cleared by that GetOrCreate.
		r.pp.MarkShimStuck(key)
		slog.Warn("shim socket still bound after Reset wait — flagging key for ErrShimStuck wrap on next GetOrCreate",
			"key", key)
	}
	if r.shutdownCond != nil {
		r.shutdownCond.Broadcast()
	}
	r.mu.Unlock()

	logSessionLifecycle("reset", key)
	r.notifyKeyRetired(key, sessionID)
	r.notifyChange()
}

// waitSocketGoneForKey waits up to maxWait for the shim socket derived from
// key to disappear; returns false on timeout. Socket naming lives behind
// cli.WaitSocketGoneForKey so this package does not reach into internal/shim
// (#711). Reset callers use the false branch to mark the key shim-stuck (#1324).
func waitSocketGoneForKey(key string, maxWait time.Duration) bool {
	return cli.WaitSocketGoneForKey(key, maxWait)
}

// ResetAndRecreate atomically resets a session and spawns a new one for the
// same key, so no concurrent message can create a session with other opts. A
// guard channel is installed via r.pp.BeginSpawn(key) BEFORE r.mu is released
// for proc.Close(); spawnSession reuses it, so the in-flight marker is
// continuous from the first unlock through spawnSession's defer (#775).
func (r *Router) ResetAndRecreate(ctx context.Context, key string, opts AgentOpts) (*ManagedSession, error) {
	r.mu.Lock()

	// Delete old session if present
	hadOld := false
	if s, ok := r.ss.sessions[key]; ok {
		hadOld = true
		proc := s.loadProcess()
		wasActive := !s.exempt && proc != nil && proc.Alive()
		oldBackend := s.Backend()
		// keepBackendOverride=true: the new opts may carry its own backend,
		// and spawnSession below consumes and clears the override atomically.
		r.unregisterSessionLocked(key, s, true)
		if wasActive {
			if r.ss.activeCount.Add(-1) < 0 {
				r.ss.activeCount.Store(0)
			}
			// spawnSession below Incs the gauge for the (possibly different)
			// new backend.
			metrics.RecordSessionActive(oldBackend, -1)
		}
		r.ss.dirty = true
		r.ss.gen.Add(1)

		if proc != nil && proc.Alive() {
			// Install the guardCh BEFORE releasing r.mu so a concurrent
			// GetOrCreate parks instead of spawning with different opts;
			// spawnSession reuses it and its defer closes+removes it (#775).
			r.pp.BeginSpawn(key)
			r.mu.Unlock()
			proc.Close()
			// As in Reset: the shim socket must be gone before spawnSession's
			// StartShim dials it, or the re-bind fails with "refusing to clobber".
			gone := waitSocketGoneForKey(key, 2*time.Second)
			r.mu.Lock()
			if !gone {
				// Flag for the ErrShimStuck wrap on the spawn failure path
				// below (#1324); consumed inline here, not by GetOrCreate.
				r.pp.MarkShimStuck(key)
				slog.Warn("shim socket still bound after ResetAndRecreate wait — flagging key for ErrShimStuck wrap on spawn failure",
					"key", key)
			}
			// Broadcast must happen under r.mu (see evictOldest).
			if r.shutdownCond != nil {
				r.shutdownCond.Broadcast()
			}
		}
	}

	// Still holding r.mu (spawnSession handles unlock/relock); consume the
	// shim-stuck flag set above.
	stuck := r.pp.ConsumeShimStuck(key)
	s, err := r.spawnSession(ctx, key, "", opts)
	if err != nil {
		// spawnSession already unlocked mu on error
		if hadOld {
			r.notifyChange()
		}
		if stuck {
			return nil, fmt.Errorf("%w: %w", ErrShimStuck, err)
		}
		return nil, err
	}
	// spawnSession's TOCTOU guard can return a concurrently-spawned session
	// with err==nil; the stuck flag must not be silently swallowed (#1702).
	warnShimStuckReuse(stuck, key)
	// spawnSession already called notifyChange on success
	return s, nil
}

// warnShimStuckReuse logs the shim-stuck diagnostic when ResetAndRecreate set
// shim-stuck but spawnSession returned a usable session without error
// (TOCTOU guard reused a concurrently-spawned session), so the signal is not
// lost on the success path (#1702).
func warnShimStuckReuse(stuck bool, key string) {
	if !stuck {
		return
	}
	slog.Warn("shim socket was still bound after ResetAndRecreate wait, but spawnSession reused an existing session (TOCTOU race); ErrShimStuck not wrapped, surfacing stuck diagnostic via log",
		"key", key)
}

// RenameSession moves a session entry from oldKey to newKey, preserving the
// running process, sessionID, history, and totalCost (scratch promote flow).
// Returns false when oldKey == newKey, oldKey does not exist, newKey already
// exists (a collision would drop an active session), or newKey fails
// session-key validation.
//
// The caller must ensure no Send is in flight for oldKey. The onSessionID
// closure captures newKey by value, so chaining a second rename on the same
// session would write a stale key into idToKey; rebuild the closure if a
// future caller needs that.
func (r *Router) RenameSession(oldKey, newKey string) bool {
	if oldKey == newKey {
		return false
	}
	if err := ValidateSessionKey(newKey); err != nil {
		slog.Warn("rename session: invalid new key", "err", err)
		return false
	}
	r.mu.Lock()

	old, ok := r.ss.sessions[oldKey]
	if !ok {
		r.mu.Unlock()
		return false
	}
	if _, collision := r.ss.sessions[newKey]; collision {
		r.mu.Unlock()
		return false
	}

	// Session key is immutable on ManagedSession (parseKeyParts caches via
	// sync.Once), so a fresh struct is the only safe way to change it. Clone
	// prevSessionIDs/persistedHistory: an in-place append on old (later spawn
	// or the async history-load goroutine) would write into a backing array
	// fresh shares. History + user-turn count snapshot together under historyMu.
	old.historyMu.RLock()
	freshHistory := slices.Clone(old.persistedHistory)
	oldUserTurns := old.persistedUserTurns.Load()
	old.historyMu.RUnlock()
	fresh := &ManagedSession{
		key:              newKey,
		persistedHistory: freshHistory,
		prevSessionIDs:   slices.Clone(old.prevSessionIDs),
		exempt:           old.exempt,
		runStore:         r.sessionRuns,
		onSessionID: func(id string) {
			r.mu.Lock()
			r.kid.Track(id)
			if id != "" {
				r.ss.idToKey[id] = newKey
			}
			r.mu.Unlock()
		},
	}
	// Seed persistedUserTurns so snapshot().MessageCount is correct before
	// any new turns arrive.
	if len(freshHistory) > 0 {
		fresh.persistedUserTurns.Store(oldUserTurns)
	}
	storeTotalCost(&fresh.totalCost, loadTotalCost(&old.totalCost))
	// Rename keeps the SAME live process, so the delta baseline carries over
	// too — resetting it would double-count the next turn.
	storeTotalCost(&fresh.costSpent, loadTotalCost(&old.costSpent))
	storeTotalCost(&fresh.lastCumulativeCost, loadTotalCost(&old.lastCumulativeCost))
	fresh.setWorkspace(old.Workspace())
	// Atomic fields: plain Load/Store round-trips are race-safe; r.mu blocks
	// all concurrent writers except the Send hot path (lastPrompt /
	// lastActivity), which are idempotent on copy.
	fresh.SetBackend(old.Backend())
	fresh.SetCLIName(old.CLIName())
	fresh.SetCLIVersion(old.CLIVersion())
	fresh.SetUserLabel(old.UserLabel())
	fresh.setLabelOrigin(old.LabelOrigin()) // label+origin travel as one unit
	// Tuning overrides follow the conversation: dropping them would make the
	// next respawn/drift check flip the session to default.
	fresh.SetTuningModel(old.TuningModel())
	fresh.SetTuningEffort(old.TuningEffort())
	if dr := loadAtomicString(&old.deathReason); dr != "" {
		storeAtomicString(&fresh.deathReason, dr)
	}
	fresh.lastActive.Store(old.lastActive.Load())
	// Carry the creation timestamp so the renamed row keeps its sidebar position.
	if oldCreatedAt := old.createdAt.Load(); oldCreatedAt != 0 {
		fresh.createdAt.Store(oldCreatedAt)
	} else {
		fresh.initCreatedAtIfUnset()
	}
	// storeAtomicString allocates a fresh *string per write rather than
	// sharing old's pointer, matching the codebase-wide convention.
	if lp := loadAtomicString(&old.lastPrompt); lp != "" {
		storeAtomicString(&fresh.lastPrompt, lp)
	}
	if la := loadAtomicString(&old.lastActivity); la != "" {
		storeAtomicString(&fresh.lastActivity, la)
	}
	fresh.setSessionID(old.getSessionID())

	// Move the process pointer; old becomes an orphan with process=nil so a
	// stale Send fails cleanly. The proc's EventLog already holds the entries
	// matching fresh.persistedHistory, so persistedSeededLen must mirror its
	// length (adoptProcessAlreadySeeded does that under historyMu) and a later
	// InjectHistory forwards only newly-arrived tail.
	if proc := old.loadProcess(); proc != nil {
		fresh.adoptProcessAlreadySeeded(proc)
	}
	old.storeProcess(nil)

	// Rebind the history source (the old Source reads the orphaned struct);
	// oldKey's map entry and index slot are removed next so the rename is
	// atomic under r.mu.
	r.publishSessionLocked(newKey, fresh, false)
	delete(r.ss.sessions, oldKey)
	r.indexDel(oldKey)
	if id := fresh.getSessionID(); id != "" {
		r.ss.idToKey[id] = newKey
	}
	if b, ok := r.bkStore.backendOverrides[oldKey]; ok {
		r.bkStore.backendOverrides[newKey] = b
		delete(r.bkStore.backendOverrides, oldKey)
	}
	if ap, ok := r.bkStore.accessProfileOverrides[oldKey]; ok {
		r.bkStore.accessProfileOverrides[newKey] = ap
		delete(r.bkStore.accessProfileOverrides, oldKey)
	}
	if pt, ok := r.bkStore.tuningOverrides[oldKey]; ok {
		r.bkStore.tuningOverrides[newKey] = pt
		delete(r.bkStore.tuningOverrides, oldKey)
	}
	r.ss.dirty = true
	r.ss.gen.Add(1)
	r.mu.Unlock()

	slog.Info("session renamed", "old", oldKey, "new", newKey)
	r.notifyChange()
	return true
}

// stripResumeArgs removes --resume <id> pairs from a CLI arg slice for the
// drift check: --resume is session-specific, not a config change.
// `--append-system-prompt` is NOT stripped — it travels in the overlay and a
// changed prompt correctly reads as drift (#2493). Returns the original slice
// unchanged when --resume is absent.
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
			// Skip the flag and its value; a trailing bare `--resume` must
			// also go or it spuriously reads as drift.
			if i+1 < len(args) {
				i++
			}
			continue
		}
		out = append(out, args[i])
	}
	return out
}
