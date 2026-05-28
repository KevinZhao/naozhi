// Package session router shim reconcile + reconnect loop.
//
// Extracted from router.go on 2026-05-19 as part of the router-split
// refactor (docs/design/router-split-design.md). For history prior to
// commit 4c81da5006a9e9caaa57102d6a6a92ef11370555, see:
//
//	git log --follow internal/session/router.go
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
	"github.com/naozhi/naozhi/internal/discovery"
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
// Deduplication is by `*shim.Manager` pointer identity: two wrappers
// (e.g. claude + kiro) configured to share a single ShimManager instance
// — typical when both backends point at the same state dir — appear once
// in the result. Wrappers that hold structurally-equivalent but separately-
// constructed managers (different *shim.Manager addresses, even if every
// field matches) appear twice; this is intentional, since each manager
// owns its own UNIX socket / cgroup pool and Discover()/handshake calls
// must hit each one.
// R230-CQ-17.
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
	for _, w := range r.wrappers {
		add(w)
	}
	add(r.wrapper)
	return out
}

// ReconnectShims discovers surviving shim processes and reconnects sessions.
// Called after NewRouter to restore sessions that were active before naozhi restart.
// Uses the router's historyCtx so SIGTERM during startup aborts the per-shim
// handshakes (15s each) instead of blocking shutdown until every reconnect
// times out. R216-GO-4.
func (r *Router) ReconnectShims() {
	r.reconnectShims(r.historyCtx)
}

// ReconnectShimsCtx is the context-aware variant used by the reconcile loop so
// SIGTERM during a 15 s handshake aborts promptly instead of waiting per session.
func (r *Router) ReconnectShimsCtx(ctx context.Context) {
	r.reconnectShims(ctx)
}

// shimState classifies how reconnectShims should dispatch a discovered shim.
// The zero value (shimStateSkip) is the safe no-op, so adding a new bool
// flag that defaults false will not silently reroute an existing case.
// R70-ARCH-H4.
type shimState int

const (
	shimStateSkip      shimState = iota // spawn in flight or session already has a live process
	shimStateOrphan                     // session missing; shim must be killed
	shimStateNoWrapper                  // no CLI wrapper registered for the shim's backend
	shimStateDrift                      // stored CLI args differ from current config
	shimStateReconnect                  // ready for Reattach
)

// classifyShimState is a pure boolean decision tree over the five inputs
// reconnectShims observes per discovered shim. Extracted so the branch
// matrix can be table-tested without standing up processes or wrappers.
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

// shutdownShimViaReconnect briefly reconnects to an existing shim and
// asks it to Shutdown gracefully, with a timeout guard so a hung
// socket cannot stall the caller. When sigusr2Fallback is true, a
// failed Reconnect triggers SIGUSR2 on the shim PID (shim's
// reload-and-die signal); with sigusr2Fallback false, failure is
// silent (drift path: wrapper is guaranteed non-nil by classify, and
// we let the 30s discovery tick revisit on next failure).
//
// IIFE-with-defer style in-place equivalent; the helper owns context
// cancel so callers cannot forget it (R32-REL1 invariant).
//
// Returns no error: the original branches were fire-and-forget and
// preserving that keeps the extraction behaviour-identical.
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
		sess, ok := r.sessions[state.Key]
		var hasLiveProcess bool
		var sessPrevIDs []string
		if ok && sess.isAlive() {
			hasLiveProcess = true
		}
		// Snapshot prevSessionIDs while still holding r.mu; the field is
		// guarded by r.mu and the async history-load goroutine (see
		// NewRouter) plus concurrent spawnSession both write to it. Reading
		// after Unlock would data-race with those writers.
		if ok {
			sessPrevIDs = slices.Clone(sess.prevSessionIDs)
		}
		_, spawning := r.spawningKeys[state.Key]
		r.mu.Unlock()

		// Resolve the wrapper recorded at shim startup so reconnect uses
		// the matching Protocol and binary. An empty Backend in the state
		// file predates multi-backend support and falls back to the
		// router default.
		recWrapper, recBackendID := r.wrapperFor(state.Backend)

		// Compute args drift up-front (only meaningful when we have a wrapper);
		// classifyShimState picks the branch. Strip --resume <id> from stored
		// args since it's session-specific, not config.
		var argsDrift bool
		var storedBase, currentArgs []string
		if recWrapper != nil {
			storedBase = stripResumeArgs(state.CLIArgs)
			// backendDefaultsFor centralises the model/extraArgs lookup that
			// otherwise duplicated the resolveSpawnParamsLocked logic
			// (R222-ARCH-14, #739).
			driftModel, driftArgs := r.backendDefaultsFor(recBackendID)
			currentArgs = recWrapper.Protocol.BuildArgs(cli.SpawnOptions{
				Model:     driftModel,
				ExtraArgs: driftArgs,
			})
			argsDrift = len(storedBase) > 0 && !slices.Equal(storedBase, currentArgs)
		}

		switch classifyShimState(spawning, ok, hasLiveProcess, recWrapper == nil, argsDrift) {
		case shimStateSkip:
			// spawnSession in flight, or session already has a live process.
			// Next tick will re-evaluate if anything changed.
			continue
		case shimStateOrphan:
			slog.Info("orphan shim found, shutting down", "key", state.Key)
			// Connect briefly to send shutdown. Bound the reconnect so a
			// hung shim socket cannot stall NewRouter startup — we fall
			// through to SIGUSR2 if the timeout fires.
			shutdownShimViaReconnect(parentCtx, recWrapper, state, shimReconnectTimeout, true)
			continue
		case shimStateNoWrapper:
			slog.Warn("shim reconnect skipped: no wrapper for backend",
				"key", state.Key, "backend", state.Backend)
			continue
		case shimStateDrift:
			slog.Info("shim config drifted, shutting down old shim",
				"key", state.Key,
				"old_args_len", len(storedBase),
				"new_args_len", len(currentArgs))
			// Drift path: classify guarantees recWrapper is non-nil, so no
			// SIGUSR2 fallback needed — if Reconnect fails, the 30s tick
			// will revisit.
			shutdownShimViaReconnect(parentCtx, recWrapper, state, shimReconnectTimeout, false)
			// After killing the old shim the session becomes suspended until the
			// next user message spawns a fresh process. NewRouter's async JSONL
			// load loop skips this key because shimManagedKeys() already claimed
			// it, so without an explicit backfill here the dashboard panel stays
			// blank until the user sends something. Load JSONL directly into
			// persistedHistory (InjectHistory is proc-nil safe) so the sidebar
			// shows the last conversation while the session waits for revival.
			if r.claudeDir != "" && state.SessionID != "" {
				ids := make([]string, 0, len(sessPrevIDs)+1)
				ids = append(ids, sessPrevIDs...)
				ids = append(ids, state.SessionID)
				// Wrap in an IIFE so a panic inside InjectHistory /
				// extractLastPromptFromProcess still releases the context's
				// timer. Mirrors the pattern used in spawnSession's history
				// load. R218-GO-10.
				func() {
					histCtx, histCancel := context.WithTimeout(parentCtx, shimReconnectTimeout)
					defer histCancel()
					histEntries := discovery.LoadHistoryChainTailCtx(
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
		// shimStateReconnect falls through here; the reconnect path is too
		// long to nest inside the switch, so we exit on every other case and
		// let the reconnect body run at the loop's natural indent level.

		// Reconnect. Timeout-bounded so a stuck shim handshake cannot stall
		// NewRouter indefinitely; on timeout we log and keep iterating.
		lastSeq := int64(0) // full replay on restart
		spawnCtx, spawnCancel := context.WithTimeout(parentCtx, shimReconnectTimeout)
		proc, replays, err := recWrapper.SpawnReconnect(
			spawnCtx, state.Key, lastSeq, recWrapper.Protocol,
			r.noOutputTimeout, r.totalTimeout,
		)
		spawnCancel()
		if err != nil {
			// ENOENT on the socket path = zombie shim (live PID, missing
			// filesystem entry). Discover's F4 check will prune it on the
			// next 30s tick, but that means 30s of WARN spam AND every
			// dashboard retry in between also fails. Eagerly clean up so
			// the next user message spawns a fresh shim instead of hitting
			// the same dead path. isENOENTErr unwraps any wrapper layers
			// (fmt.Errorf → net.OpError → os.SyscallError) and avoids
			// matching against the strerror text — that string is locale-
			// dependent and silently mismatches under LANG=zh_CN.UTF-8.
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

		// Install the turn-done callback before any history/JSONL work
		// completes so result events arriving during the JSONL-load window
		// (the readLoop is already running inside SpawnReconnect) do not
		// fire the nil-callback path and leave the dashboard stuck on a
		// "running" spinner until the next unrelated broadcast.
		proc.SetOnTurnDone(func() { r.notifyChange() })

		// Wrapper.SpawnReconnect has no cwd (shim owns it), so its
		// proc.InitLinker("") left the SubagentLinker with empty
		// projectDir and Resolve bails on every team agent task_id.
		// Replay the workspace from the persisted session record so the
		// Linker can locate ~/.claude/projects/<encoded-cwd>/<session>/
		// subagents/ for any in-flight teammate tasks.
		if ws := sess.Workspace(); ws != "" {
			proc.SetCwdForLinker(ws)
		}

		// Shim replays (DrainReplay output) are intentionally NOT injected
		// into EventLog — they lack per-event timestamps and would corrupt
		// chronology. But they DO carry the `system.task_started` markers
		// for any in-process teammate / sidechain agent the shim saw before
		// naozhi restart. Without plumbing those markers to the Linker, the
		// dashboard drill-in serves 202 forever because Linker.Query has
		// never seen the task_id. Walk the replay once, extract each
		// task_started, and kick an async Resolve — Resolve is idempotent
		// + cached, so this costs at most one stat per unique task_id.
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
				// Replay frames map 1:1 to a single semantic event in
				// practice (the multi-event ACP turn-end response is not
				// captured as a replay), but iterating the slice keeps the
				// linker resilient if a future protocol fans out from one
				// frame.
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
					// R260528-GO-1: SubagentLinker.Resolve dropped its
					// dead description parameter; the local desc binding
					// no longer needs to be threaded through. Comment
					// retained because the wallclock=0 reasoning still
					// applies.
					// R224-GO-3: pass 0 instead of time.Now().UnixMilli().
					// subagent_link.Resolve uses agentToolUseMS to filter
					// out subagent jsonl files whose first row predates
					// agentTS-10s ("same-name reuse" staleness guard).
					// Replay frames in this branch have no preserved
					// per-event timestamp (cli.Event.recvAt is unexported
					// and only stamped at live readLoop time), so any
					// time.Now()-derived value here is fiction relative
					// to the historical task — and a fiction that's
					// always *newer* than the real task, which means
					// every candidate jsonl passes the guard and the
					// filter is effectively disabled with a misleading
					// non-zero argument. Resolve treats
					// agentToolUseMS<=0 as "skip the time filter"
					// (subagent_link.go:328), which is the honest
					// fail-open for the replay path: we're consciously
					// declining to enforce staleness because we lack
					// the data to do so. Same-name reuse on replay is
					// extremely rare (would require a parent agent
					// finishing an old task, the exact sub-task name
					// being reused after >10s, and the shim restart
					// surfacing both in one DrainReplay), and Resolve's
					// other guards (sessionID match, toolUseID dedup,
					// per-jsonl modtime ordering) still apply.
					wallclock := int64(0)
					go linker.Resolve(parentCtx, taskID, toolUseID, name, wallclock)
				}
			}
		}

		// Restore dashboard history from JSONL only.
		//
		// Replay events are intentionally NOT injected into persistedHistory:
		// they originate from the shim stdout ring buffer, which has no native
		// per-event timestamp, so EventEntriesFromEvent stamps them all with
		// time.Now() at reconnect moment — this breaks chronological ordering
		// against user entries loaded from JSONL (which carry real ts).
		//
		// Replay is still useful for runtime state (isMidTurn detection inside
		// SpawnReconnect, and any live bytes readLoop picks up post-reconnect).
		// For long-term history, JSONL is authoritative — it records both
		// user input (stdin) and assistant output with accurate timestamps.
		//
		// Tradeoff: if naozhi restarts within seconds of the last turn, the
		// current session's JSONL may not yet be flushed to disk; assistant
		// entries for that turn are transiently absent from the dashboard
		// until the next live event repopulates them. Self-healing.
		//
		// R52-CONCUR-004 sub-issue: histEntries is loaded from JSONL fresh
		// rather than merged with sess.persistedHistory. If the in-memory
		// persistedHistory contains entries that never made it to JSONL
		// (e.g. an interim user prompt or assistant chunk that crashed
		// before disk flush), the new proc.eventLog will not see those
		// entries and EventEntriesSince's "proc != nil → proc.EventEntries"
		// fast path may under-return briefly. The fallback path
		// (EventEntriesBeforeCtx, dashboard scrollback) merges
		// sess.persistedHistory with the on-disk tier so the user-visible
		// history converges; the gap is only in the live "since X" stream
		// during the reconnect window. A proper merge here requires first
		// landing R51-CONCUR-002 (sendMu protection on
		// ReattachProcessNoCallback) so we can guarantee no in-flight Send
		// is racing the merge. Tracked under R51-CONCUR-002 as the master
		// sendMu refactor.
		//
		// R231-CQ-1: only load+inject when persistedHistory is empty.
		// If tier1/tier2 (NewRouter startup goroutine via s.InjectHistory)
		// already populated it, the upcoming ReattachProcessNoCallback at
		// the bottom of this branch will snapshot persistedHistory into
		// the fresh proc — re-injecting the same JSONL entries here would
		// double-fill proc.EventLog (direct proc.InjectHistory append +
		// snapshot copy via attachProcessAndSnapshotPersisted's seed).
		// Route through sess.InjectHistory so persistedHistory stays the
		// single source of truth and seededLen accounting protects against
		// duplicate forwarding.
		if r.claudeDir != "" && !sess.hasInjectedHistory() {
			ids := make([]string, 0, len(sessPrevIDs)+1)
			ids = append(ids, sessPrevIDs...)
			if state.SessionID != "" {
				ids = append(ids, state.SessionID)
			}
			// Use parentCtx (reconcile loop / startup ctx) rather than
			// r.historyCtx: historyCtx is cancelled as Shutdown's FIRST
			// action, so a reconcile tick that fires during the 30s drain
			// window would see ctx.Canceled and load zero entries, leaving
			// the reconnected session's dashboard panel empty.
			// Bounded budget (maxPersistedHistory) and the inner
			// shimReconnectTimeout still protect against hung storage.
			histCtx, histCancel := context.WithTimeout(parentCtx, shimReconnectTimeout)
			histEntries := discovery.LoadHistoryChainTailCtx(
				histCtx, r.claudeDir, ids, sess.Workspace(), maxPersistedHistory,
			)
			histCancel()
			if len(histEntries) > 0 {
				// sess.InjectHistory appends to persistedHistory; proc is
				// not yet attached so loadProcess() inside InjectHistory
				// observes nil and skips forwarding. The snapshot path in
				// ReattachProcessNoCallback below seeds proc from
				// persistedHistory exactly once.
				sess.InjectHistory(histEntries)
			}
		}

		// TOCTOU guard: re-check under lock that the session hasn't been replaced
		// by a concurrent spawnSession while we were replaying history (lock was
		// released). Then atomically attach the process under the same lock hold
		// to eliminate the race window where a concurrent GetOrCreate could see
		// isAlive()==false between check and ReattachProcess.
		r.mu.Lock()
		currentSess := r.sessions[state.Key]
		if currentSess != sess || (currentSess != nil && currentSess.isAlive()) {
			r.mu.Unlock()
			proc.Close()
			slog.Info("shim reconnect aborted: session replaced concurrently", "key", state.Key)
			continue
		}
		// ReattachProcess calls onSessionID which tries to r.mu.Lock(),
		// but we already hold the lock here. Do the tracking directly
		// to avoid deadlock (sync.RWMutex is not reentrant).
		// (onTurnDone was already bound before the JSONL-load window
		// to avoid missing early result events.)
		sess.ReattachProcessNoCallback(proc, state.SessionID)
		// Record the backend + wrapper-provided CLI identity so the
		// dashboard snapshot reflects the actual backend post-reconnect,
		// even for sessions restored from a pre-multi-backend store.
		// Writes go through atomic.Pointer[string] so the lock-free Snapshot()
		// in ListSessions remains race-free.
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
			r.trackSessionID(state.SessionID)
			r.sessionIDToKey[state.SessionID] = state.Key
		}
		if !sess.exempt {
			r.activeCount.Add(1)
		}
		// Mark store dirty so the next Cleanup/saveIfDirty cycle persists
		// the reconnected session's backend/CLI identity and active flag.
		// Without this, a naozhi crash within the (up to) 60-second gap
		// before the next save would lose the shim-reconnect state even
		// though the shim itself kept the CLI process alive. Every other
		// storeGen.Add site pairs with storeDirty = true for this reason.
		r.storeDirty = true
		r.storeGen.Add(1)
		r.mu.Unlock()

		// Event-log persist sink goes last so the InjectHistory +
		// shim replay above land with sinkReady=false (replayPhase=true
		// on the persister side) and are dropped rather than written
		// back to disk. See RFC §3.2.2.
		r.installPersistSink(proc, state.Key)

		// Extract lastPrompt/lastActivity from replay + JSONL entries so the
		// sidebar shows a meaningful label instead of "(no prompt)".
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
		// SM2 (#394): defensive activeCount reconciliation. spawnSession
		// runs concurrent to ReconnectShims and both Add(1) under r.mu;
		// historically a same-key race between the two could drift the
		// counter by ±1, surfacing later as a spurious ErrMaxProcs until
		// the next Cleanup self-heals via countActive(). Calling
		// countActive() here on the post-reconcile aggregate keeps the
		// O(N) walk off the per-spawn fast path while still converging
		// the counter to truth at every reconcile tick (>=1 reconnected).
		// pendingSpawns isn't reset because in-flight Spawns still need
		// their reservation — countActive only restores the visible
		// activeCount; pendingSpawns is decremented by the slot's RAII
		// release in Spawn's defer.
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
		// Mirror StartCleanupLoop: a panic inside ReconnectShimsCtx would
		// otherwise silently kill the loop goroutine and shim recovery would
		// stop for the lifetime of the process. Auto-restart with a short
		// cool-down so a panicking iteration cannot hot-loop.
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
				// Thread ctx so SIGTERM during a per-shim 15s handshake
				// aborts promptly instead of waiting one full timeout per
				// suspended session.
				r.ReconnectShimsCtx(ctx)
			}
		}
	}()
}
