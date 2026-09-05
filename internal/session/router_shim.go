// Package session router shim reconcile + reconnect loop.
//
// This file holds shim management: discovering surviving shim processes,
// classifying their state, reconnecting (or shutting down) them on startup,
// and the periodic reconcile loop.
package session

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/shim"
)

// shimManagedKeys returns the set of session keys that have a surviving shim
// process. Called by NewRouter to skip async JSONL loading for sessions that
// will be fully restored by ReconnectShims (replay + JSONL user entries).
func (r *Router) shimManagedKeys() map[string]bool {
	managers := r.shimManagers()
	if len(managers) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	for _, mgr := range managers {
		states, err := mgr.Discover()
		if err != nil {
			continue
		}
		for _, s := range states {
			seen[s.Key] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	return seen
}

// shimManagers returns the distinct ShimManager instances across wrappers.
// Dedup is by *shim.Manager pointer identity: wrappers sharing one manager
// appear once; separately-constructed managers appear separately, since each
// owns its own UNIX socket / cgroup pool and Discover()/handshake must hit each.
func (r *Router) shimManagers() []*shim.Manager {
	var out []*shim.Manager
	seen := make(map[*shim.Manager]bool)
	add := func(w *cli.Wrapper) {
		if w == nil || w.ShimManager == nil || seen[w.ShimManager] {
			return
		}
		seen[w.ShimManager] = true
		out = append(out, w.ShimManager)
	}
	for _, w := range r.bkStore.wrappers {
		add(w)
	}
	add(r.bkStore.wrapper)
	return out
}

// ReconnectShims discovers surviving shim processes and reconnects sessions.
// Called after NewRouter. Uses the router's historyCtx so SIGTERM during
// startup aborts the per-shim handshakes instead of blocking shutdown.
func (r *Router) ReconnectShims() {
	r.reconnectShims(r.historyCtx)
}

// ReconnectShimsCtx is the context-aware variant used by the reconcile loop so
// SIGTERM during a handshake aborts promptly instead of waiting per session.
func (r *Router) ReconnectShimsCtx(ctx context.Context) {
	r.reconnectShims(ctx)
}

// shimState classifies how reconnectShims should dispatch a discovered shim.
// The zero value (shimStateSkip) is the safe no-op, so a new bool flag that
// defaults false cannot silently reroute an existing case.
type shimState int

const (
	shimStateSkip      shimState = iota // spawn in flight or session already has a live process
	shimStateOrphan                     // session missing; shim must be killed
	shimStateNoWrapper                  // no CLI wrapper registered for the shim's backend
	shimStateDrift                      // stored CLI args differ from current config
	shimStateReconnect                  // ready for Reattach
)

// classifyShimState is a pure decision tree over the inputs reconnectShims
// observes per discovered shim, kept separate so it can be table-tested.
//
// Order matters: spawning > orphan > hasLiveProc > wrapperNil > argsDrift.
// A spawn in flight always wins because the new shim's state file may race
// ahead of ManagedSession registration — skipping avoids a false-orphan
// shutdown of the fresh shim.
func classifyShimState(spawning, sessFound, hasLiveProc, wrapperNil, argsDrift bool) shimState {
	if spawning {
		return shimStateSkip
	}
	if !sessFound {
		return shimStateOrphan
	}
	if hasLiveProc {
		return shimStateSkip
	}
	if wrapperNil {
		return shimStateNoWrapper
	}
	if argsDrift {
		return shimStateDrift
	}
	return shimStateReconnect
}

// shutdownShimViaReconnect briefly reconnects to an existing shim and asks it
// to Shutdown gracefully, timeout-guarded so a hung socket cannot stall the
// caller. With sigusr2Fallback, a failed Reconnect sends SIGUSR2 (the shim's
// reload-and-die signal) to the shim PID; otherwise failure is silent and the
// next discovery tick revisits. The helper owns context cancel so callers
// cannot forget it. Fire-and-forget: returns no error.
func shutdownShimViaReconnect(
	parentCtx context.Context,
	wrapper *cli.Wrapper,
	state shim.State,
	timeout time.Duration,
	sigusr2Fallback bool,
) {
	rctx, rcancel := context.WithTimeout(parentCtx, timeout)
	defer rcancel()

	var (
		handle  *shim.ShimHandle
		connErr error
	)
	if wrapper != nil && wrapper.ShimManager != nil {
		handle, connErr = wrapper.ShimManager.Reconnect(rctx, state.Key, 0)
	} else {
		connErr = fmt.Errorf("no shim manager for backend %q", state.Backend)
	}
	if connErr == nil {
		handle.Shutdown()
		return
	}
	if sigusr2Fallback {
		_ = osutil.SendShimReload(state.ShimPID)
	}
}

// driftCompareArgs reconstructs the argv a fresh spawn of this session would
// use, for comparison against the shim-recorded argv (tuning_drift_parity_test.go).
// Backend defaults, the session's tuning overrides (--model/--effort — omitting
// them would flag every tuned session as drift on restart) and the overlay the
// shim persisted at spawn (agents[].model/.effort/.extra_args + access profile,
// #2494) are re-merged through the same mergeArgvLayers the spawn used. sess
// may be nil (adopt path). A nil overlay (pre-#2494 state) degrades to a
// backend-defaults-only comparison; the caller logs that once per shim.
// Lock: called WITHOUT r.mu; the only guarded read is under RLock inside
// accessProfileDefaultModel.
func (r *Router) driftCompareArgs(recWrapper *cli.Wrapper, backendID, key string, sess *ManagedSession, overlay *shim.SpawnOverlay) []string {
	var ov shim.SpawnOverlay
	if overlay != nil {
		ov = *overlay
	}
	var tuningModel, tuningEffort string
	if sess != nil {
		tuningModel, tuningEffort = sess.TuningModel(), sess.TuningEffort()
	}
	merged := mergeArgvLayers(
		r.backendDefaultsFor(backendID),
		r.accessProfileDefaultModel(ov.AccessProfile),
		ov, tuningModel, tuningEffort)
	// Every argv-bearing field is mirrored by construction via argvSpawnOptions.
	// cliDebugPathFor (not cliDebugFileFor) keeps the comparison read-only.
	// systemPrompt is "" by design: AgentOpts.SystemPrompt is per-session and
	// not reconstructible here, so stripResumeArgs removes the stored
	// --append-system-prompt pair instead (#2493).
	opts := r.argvSpawnOptions(merged.Model, merged.Effort, r.cliDebugPathFor(key), merged.SystemPrompt, merged.Args)
	// Re-deriving argv re-hits the same gates every 30s reconcile tick; the
	// emitter's per-scope dedup keeps that as one Warn then Debug repeats.
	cli.EmitSpawnDiags(key, cli.SpawnDiagsFor(opts, cli.ProtocolCaps(recWrapper.Protocol)))
	return recWrapper.Protocol.BuildArgs(opts)
}

// shimArgsDrift is the arg-drift predicate for classifyShimState: does the
// argv the surviving shim recorded (minus the session-specific --resume pair)
// still equal what a fresh spawn would use today? Returns both argv so the
// caller can log the first divergence. Never drifts on an empty stored argv.
// A nil state.SpawnOverlay (shim spawned before the overlay was persisted,
// #2494) cannot see agents[].model/.effort and may restart that session ONCE;
// logged here so the operator can attribute the restart.
func (r *Router) shimArgsDrift(recWrapper *cli.Wrapper, backendID string, state shim.State, sess *ManagedSession) (drift bool, storedBase, currentArgs []string) {
	storedBase = stripResumeArgs(state.CLIArgs)
	if len(storedBase) == 0 {
		return false, storedBase, nil
	}
	if state.SpawnOverlay == nil {
		slog.Info("shim state predates spawn-overlay persistence; drift compare falls back to backend defaults",
			"key", state.Key, "pid", state.ShimPID)
	}
	currentArgs = r.driftCompareArgs(recWrapper, backendID, state.Key, sess, state.SpawnOverlay)
	return !slices.Equal(storedBase, currentArgs), storedBase, currentArgs
}

// firstArgvDivergence returns the first differing token of two argv slices as
// an (old, new) pair; "(absent)" marks a slice that ended first, both empty
// means equal. Tokens come from config.yaml (model ids, effort tiers, flags),
// so logging them verbatim is safe.
func firstArgvDivergence(oldArgs, newArgs []string) (string, string) {
	const absent = "(absent)"
	n := max(len(oldArgs), len(newArgs))
	for i := range n {
		o, w := absent, absent
		if i < len(oldArgs) {
			o = oldArgs[i]
		}
		if i < len(newArgs) {
			w = newArgs[i]
		}
		if o != w {
			return o, w
		}
	}
	return "", ""
}

// adoptableShimKey reports whether a live shim whose key is absent from
// sessions.json may be rebuilt + reconnected rather than killed (#1875). It
// rejects sys:/scratch: keys (never persisted, so a live shim there is an
// anomaly) and keys failing validation, mirroring validateKeyForShim: a
// malformed key on a shim state file is evidence of tampering, not a session.
func adoptableShimKey(key string) bool {
	if IsSysKey(key) || IsScratchKey(key) {
		return false
	}
	return ValidateSessionKey(key) == nil
}

// adoptLiveShimLocked rebuilds a ManagedSession for a live shim missing from
// sessions.json and publishes it, so classifyShimState sees sessFound=true and
// reconnects instead of orphan-killing (#1875). The shim state file is the
// source of truth (key, workspace, backend, session_id); it is adapted to a
// storeEntry and built via restoreSessionFromEntry so the adopted session is
// wired identically to persisted ones. Cost/label/model re-populate from the
// first post-reconnect system/init + result events.
//
// LOCK: caller MUST hold r.mu (publishSessionLocked + idToKey writes).
func (r *Router) adoptLiveShimLocked(state shim.State, backendID string, _ *cli.Wrapper) *ManagedSession {
	entry := &storeEntry{
		Key:       state.Key,
		SessionID: state.SessionID,
		Workspace: state.Workspace,
		Backend:   backendID,
	}
	r.restoreSessionFromEntry(state.Key, entry)
	return r.ss.sessions[state.Key]
}

func (r *Router) reconnectShims(parentCtx context.Context) {
	managers := r.shimManagers()
	if len(managers) == 0 {
		return
	}

	// Aggregate states across all managers and dedupe on key, as each shim
	// is uniquely identified by the session key regardless of backend.
	seenKey := make(map[string]bool)
	var states []shim.State
	for _, mgr := range managers {
		ss, err := mgr.Discover()
		if err != nil {
			slog.Warn("shim discovery failed", "err", err)
			continue
		}
		for _, s := range ss {
			if seenKey[s.Key] {
				continue
			}
			seenKey[s.Key] = true
			states = append(states, s)
		}
	}
	slog.Info("shim discovery complete", "found", len(states))

	reconnected := 0
	for _, state := range states {
		r.mu.Lock()
		sess, ok := r.ss.sessions[state.Key]
		var hasLiveProcess bool
		var sessPrevIDs []string
		if ok && sess.isAlive() {
			hasLiveProcess = true
		}
		// Snapshot prevSessionIDs while still holding r.mu: the field is
		// guarded by r.mu and written by the async history-load goroutine and
		// concurrent spawnSession, so reading after Unlock would data-race.
		if ok {
			sessPrevIDs = slices.Clone(sess.prevSessionIDs)
		}
		_, spawning := r.pp.SpawnInFlight(state.Key)
		r.mu.Unlock()

		// Resolve the wrapper recorded at shim startup so reconnect uses the
		// matching Protocol and binary; an empty Backend falls back to the
		// router default.
		recWrapper, recBackendID := r.wrapperFor(state.Backend)

		// Args drift is only meaningful when we have a wrapper;
		// classifyShimState picks the branch.
		var argsDrift bool
		var storedBase, currentArgs []string
		if recWrapper != nil {
			argsDrift, storedBase, currentArgs = r.shimArgsDrift(recWrapper, recBackendID, state, sess)
		}
		// Surface the drift per-field on the session (#2543): live sessions
		// hit shimStateSkip below and would otherwise discard it. Lock-free
		// store; also clears a stale marker once config and argv re-agree.
		if sess != nil && recWrapper != nil {
			if argsDrift {
				d := overlayDriftFields(storedBase, currentArgs)
				sess.overlayDrift.Store(&d)
			} else {
				sess.overlayDrift.Store(nil)
			}
		}

		// Adopt-before-classify (#1875): a live shim absent from sessions.json is
		// not necessarily an orphan — the store is written lazily (saveIfDirty
		// every 30s, entries with empty session_id dropped), so a session spawned
		// just before a crash has a live shim and no record. Adopt only when a
		// wrapper exists and the key is adoptable; otherwise fall through.
		if !ok && !spawning && recWrapper != nil && adoptableShimKey(state.Key) {
			r.mu.Lock()
			// Re-check under lock: a concurrent spawnSession may have installed
			// the session (or its spawning marker) between the snapshot above
			// and now.
			if existing, exists := r.ss.sessions[state.Key]; exists {
				// A concurrent spawnSession won; re-snapshot instead of adopting a duplicate.
				sess = existing
				ok = true
				sessPrevIDs = slices.Clone(sess.prevSessionIDs)
				if sess.isAlive() {
					hasLiveProcess = true
				}
			} else if _, racingSpawn := r.pp.SpawnInFlight(state.Key); racingSpawn {
				// spawnSession started inside the adopt window but hasn't
				// published yet: promote spawning so classifyShimState routes
				// to skip rather than adopting a competing copy.
				spawning = true
			} else {
				// Genuinely absent + no spawn in flight: rebuild from the shim
				// state file so classifyShimState sees sessFound and reconnects.
				adopted := r.adoptLiveShimLocked(state, recBackendID, recWrapper)
				sess = adopted
				ok = true
				sessPrevIDs = slices.Clone(adopted.prevSessionIDs)
				slog.Info("adopted live shim missing from store",
					"key", state.Key,
					"session_id", state.SessionID,
					"pid", state.ShimPID)
			}
			r.mu.Unlock()
		}

		switch classifyShimState(spawning, ok, hasLiveProcess, recWrapper == nil, argsDrift) {
		case shimStateSkip:
			// Next tick re-evaluates if anything changed.
			continue
		case shimStateOrphan:
			slog.Info("orphan shim found, shutting down", "key", state.Key)
			// Bounded reconnect so a hung shim socket cannot stall startup;
			// falls through to SIGUSR2 if the timeout fires.
			shutdownShimViaReconnect(parentCtx, recWrapper, state, shimReconnectTimeout, true)
			continue
		case shimStateNoWrapper:
			slog.Warn("shim reconnect skipped: no wrapper for backend",
				"key", state.Key, "backend", state.Backend)
			continue
		case shimStateDrift:
			// Naming the first divergence lets an operator tell an EXPECTED
			// restart (config edit) from a spurious one (a pre-#2494 shim whose
			// state carries no overlay — see the legacy_state attr).
			oldTok, newTok := firstArgvDivergence(storedBase, currentArgs)
			slog.Info("shim config drifted, shutting down old shim",
				"key", state.Key,
				"old_args_len", len(storedBase),
				"new_args_len", len(currentArgs),
				"first_diff_old", oldTok,
				"first_diff_new", newTok,
				"legacy_state", state.SpawnOverlay == nil)
			// classify guarantees recWrapper is non-nil, so no SIGUSR2
			// fallback; if Reconnect fails, the next tick revisits.
			shutdownShimViaReconnect(parentCtx, recWrapper, state, shimReconnectTimeout, false)
			// The session is now suspended until the next user message. NewRouter's
			// async JSONL load skipped this key (shimManagedKeys claimed it), so
			// backfill persistedHistory here (InjectHistory is proc-nil safe) or
			// the dashboard panel stays blank until the user sends something.
			if r.claudeDir != "" && state.SessionID != "" {
				ids := make([]string, 0, len(sessPrevIDs)+1)
				ids = append(ids, sessPrevIDs...)
				ids = append(ids, state.SessionID)
				// IIFE so a panic inside InjectHistory / extractLastPromptFromProcess
				// still releases the context's timer.
				func() {
					histCtx, histCancel := context.WithTimeout(parentCtx, shimReconnectTimeout)
					defer histCancel()
					histEntries := r.historyLoader.LoadHistoryChainTail(
						histCtx, r.claudeDir, ids, sess.Workspace(), maxPersistedHistory,
					)
					if len(histEntries) > 0 {
						sess.InjectHistory(histEntries)
						sess.extractLastPromptFromProcess()
						slog.Info("drifted shim: backfilled JSONL history",
							"key", state.Key, "entries", len(histEntries))
					}
				}()
			}
			continue
		}
		// shimStateReconnect falls through: the reconnect body is too long to
		// nest inside the switch.

		// Timeout-bounded so a stuck shim handshake cannot stall NewRouter
		// indefinitely; on timeout we log and keep iterating.
		lastSeq := int64(0) // full replay on restart
		spawnCtx, spawnCancel := context.WithTimeout(parentCtx, shimReconnectTimeout)
		proc, replays, err := recWrapper.SpawnReconnect(
			spawnCtx, state.Key, lastSeq, recWrapper.Protocol,
			r.noOutputTimeout, r.totalTimeout,
		)
		spawnCancel()
		if err != nil {
			// ENOENT on the socket path = zombie shim (live PID, missing socket).
			// Discover prunes it on the next tick, but eager cleanup spares 30s
			// of WARN spam and failing dashboard retries. isENOENTErr unwraps
			// wrapper layers rather than matching strerror text, which is
			// locale-dependent (mismatches under LANG=zh_CN.UTF-8).
			if isENOENTErr(err) {
				slog.Warn("shim reconnect: socket missing, cleaning up zombie",
					"key", state.Key, "pid", state.ShimPID, "err", err)
				if mgr := r.managerFor(recBackendID); mgr != nil {
					mgr.ForceCleanupZombie(state)
				}
				continue
			}
			slog.Warn("shim reconnect failed", "key", state.Key, "err", err)
			continue
		}

		// Bind before any JSONL work: readLoop is already running inside
		// SpawnReconnect, and a result event hitting the nil-callback path
		// leaves the dashboard stuck on a "running" spinner.
		proc.SetOnTurnDone(func() { r.notifyChange() })

		// SpawnReconnect has no SpawnOptions, so the spawn-pinned effort tier is
		// recovered from the argv the shim recorded. Fill-if-unset: a metadata
		// report already consumed from the replay stays authoritative.
		proc.SeedEffortFromArgs(state.CLIArgs)

		// SpawnReconnect has no cwd (shim owns it), so the SubagentLinker has an
		// empty projectDir and Resolve bails on every team agent task_id. Replay
		// the workspace so it can locate
		// ~/.claude/projects/<encoded-cwd>/<session>/subagents/.
		if ws := sess.Workspace(); ws != "" {
			proc.SetCwdForLinker(ws)
		}

		// Replays are NOT injected into EventLog (no per-event timestamps), but
		// they carry `system.task_started` markers for teammate/sidechain agents
		// the shim saw before restart. Without feeding those to the Linker the
		// dashboard drill-in serves 202 forever. Walk the replay once and kick an
		// async Resolve per unique task_id (Resolve is idempotent + cached).
		if linker := proc.Linker(); linker != nil && len(replays) > 0 {
			seen := make(map[string]struct{})
			for _, replay := range replays {
				if replay.Type != "replay" {
					continue
				}
				events, _, err := recWrapper.Protocol.ReadEvent(replay.Line)
				if err != nil || len(events) == 0 {
					continue
				}
				// Replay frames map 1:1 to a semantic event in practice; iterating
				// keeps the linker resilient if a protocol fans out from one frame.
				for _, ev := range events {
					if ev.Type != "system" || ev.SubType != "task_started" {
						continue
					}
					if ev.TaskID == "" || ev.ToolUseID == "" {
						continue
					}
					// Skip local_bash — no internal transcript on disk.
					if ev.TaskType == "local_bash" {
						continue
					}
					if _, dup := seen[ev.TaskID]; dup {
						continue
					}
					seen[ev.TaskID] = struct{}{}
					name := strings.TrimSpace(ev.Description)
					if i := strings.IndexByte(name, ':'); i > 0 {
						name = strings.TrimSpace(name[:i])
					}
					taskID, toolUseID := ev.TaskID, ev.ToolUseID
					desc := ev.Description
					// wallclock 0 = skip Resolve's staleness filter: replay frames
					// carry no per-event timestamp, and any time.Now()-derived value
					// would be newer than the real task and let every candidate
					// pass anyway. Fail open honestly; Resolve's other guards
					// (sessionID match, toolUseID dedup, modtime order) still apply.
					wallclock := int64(0)
					go linker.Resolve(parentCtx, taskID, toolUseID, name, desc, wallclock)
				}
			}
		}

		// Restore dashboard history from JSONL only. Replay events are NOT
		// injected into persistedHistory: they lack native timestamps and would
		// break ordering against JSONL user entries. Only load when
		// persistedHistory is empty — ReattachProcessNoCallback below snapshots
		// it into the fresh proc, so re-injecting would double-fill proc.EventLog.
		if r.claudeDir != "" && !sess.hasInjectedHistory() {
			ids := make([]string, 0, len(sessPrevIDs)+1)
			ids = append(ids, sessPrevIDs...)
			if state.SessionID != "" {
				ids = append(ids, state.SessionID)
			}
			// parentCtx, not r.historyCtx: historyCtx is cancelled as Shutdown's
			// FIRST action, so a reconcile tick during the drain window would
			// load zero entries and leave the panel empty. maxPersistedHistory +
			// shimReconnectTimeout still bound hung storage.
			histCtx, histCancel := context.WithTimeout(parentCtx, shimReconnectTimeout)
			histEntries := r.historyLoader.LoadHistoryChainTail(
				histCtx, r.claudeDir, ids, sess.Workspace(), maxPersistedHistory,
			)
			histCancel()
			if len(histEntries) > 0 {
				// proc is not yet attached, so InjectHistory only appends to
				// persistedHistory; ReattachProcessNoCallback below seeds proc
				// from it exactly once.
				sess.InjectHistory(histEntries)
			}
		}

		// TOCTOU guard: re-check under lock that the session hasn't been replaced
		// by a concurrent spawnSession while we were replaying history (lock was
		// released). Then atomically attach the process under the same lock hold
		// to eliminate the race window where a concurrent GetOrCreate could see
		// isAlive()==false between check and ReattachProcess.
		r.mu.Lock()
		currentSess := r.ss.sessions[state.Key]
		if currentSess != sess || (currentSess != nil && currentSess.isAlive()) {
			r.mu.Unlock()
			proc.Close()
			slog.Info("shim reconnect aborted: session replaced concurrently", "key", state.Key)
			continue
		}
		// ReattachProcess would call onSessionID which takes r.mu (held here;
		// not reentrant), so track directly; onTurnDone was bound earlier. The
		// TryLock-guarded variant keeps a Send() still unwinding on the dead
		// process (holding sendMu) from racing the storeProcess swap; non-blocking,
		// so the sendMu→r.mu ordering holds. If busy, the next tick retries (#750).
		if !sess.tryReattachProcessNoCallback(proc, state.SessionID) {
			r.mu.Unlock()
			proc.Close()
			slog.Info("shim reconnect deferred: send in flight on session",
				"key", state.Key)
			continue
		}
		// Record backend + CLI identity so the dashboard snapshot reflects the
		// actual backend post-reconnect. Writes go through atomic.Pointer[string]
		// so the lock-free Snapshot() in ListSessions remains race-free.
		if recBackendID != "" {
			sess.SetBackend(recBackendID)
		}
		if recWrapper.CLIName != "" {
			sess.SetCLIName(recWrapper.CLIName)
		}
		if recWrapper.CLIVersion != "" {
			sess.SetCLIVersion(recWrapper.CLIVersion)
		}
		if state.SessionID != "" {
			r.kid.Track(state.SessionID)
			r.ss.idToKey[state.SessionID] = state.Key
		}
		if !sess.exempt {
			r.ss.activeCount.Add(1)
		}
		// Mark store dirty so the next saveIfDirty persists the reconnected
		// backend/CLI identity and active flag; every storeGen.Add site pairs
		// with dirty = true.
		r.ss.dirty = true
		r.ss.gen.Add(1)
		r.mu.Unlock()

		// Persist sink goes last so the InjectHistory + shim replay above land
		// with sinkReady=false and are dropped rather than written back to disk
		// (RFC §3.2.2).
		r.installPersistSink(proc, state.Key)

		// Sidebar label instead of "(no prompt)".
		sess.extractLastPromptFromProcess()

		reconnected++
		slog.Info("session reconnected via shim",
			"key", state.Key,
			"session_id", state.SessionID,
			"replayed", len(replays))
	}

	if reconnected > 0 {
		r.notifyChange()
		slog.Info("shim reconnect complete", "count", reconnected)
		// Defensive activeCount reconciliation (#394): spawnSession runs
		// concurrently and both Add(1) under r.mu; a same-key race can drift
		// the counter ±1 (spurious ErrMaxProcs). countActive() here converges
		// it each reconcile tick without an O(N) walk on the spawn fast path.
		// pendingSpawns is left alone: in-flight Spawns release it in their defer.
		r.mu.Lock()
		r.countActive()
		r.mu.Unlock()
	}
}

// StartShimReconcileLoop periodically checks for suspended sessions that have
// live shim processes and reconnects them. This covers edge cases where the
// connection to a shim drops during normal operation (e.g. temporary I/O error)
// but the shim and CLI process are still alive.
func (r *Router) StartShimReconcileLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		slog.Warn("start shim reconcile loop: non-positive interval, reconcile disabled",
			"interval", interval)
		return
	}
	go func() {
		// A panic inside ReconnectShimsCtx would otherwise silently kill the loop
		// and shim recovery would stop for the process lifetime. Auto-restart
		// with a short cool-down so a panicking iteration cannot hot-loop.
		defer func() {
			if rec := recover(); rec != nil {
				metrics.PanicRecoveredTotal.Add(1)
				slog.Error("router shim-reconcile loop panic recovered",
					"panic", rec, "stack", string(debug.Stack()))
				if ctx.Err() == nil {
					time.AfterFunc(5*time.Second, func() {
						if ctx.Err() != nil {
							return
						}
						r.StartShimReconcileLoop(ctx, interval)
					})
				}
			}
		}()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Thread ctx so SIGTERM during a handshake aborts promptly.
				r.ReconnectShimsCtx(ctx)
			}
		}
	}()
}
