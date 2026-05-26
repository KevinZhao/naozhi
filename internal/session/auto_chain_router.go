// Router-side wiring for the auto-workspace-chain feature.
//
// Methods on *Router that the auto-chain spawn / backfill paths need:
//   - AddSessionIDExcluder              — wires cron / sysession excluders
//   - snapshotRouterExcludedLocked      — produces a SessionIDExcluder backed
//     by a r.sessions map snapshot taken
//     under r.mu (read-only afterward).
//   - runAutoChainBackfillOnce          — startup-only; back-fills empty
//     prev_session_ids on every existing
//     session that qualifies.
//
// Lock contract recap (RFC §4.5):
//   - r.mu protects r.sessions and r.excluders' atomic-pointer slot.
//   - historyMu (per-session) protects prevSessionIDs / prevSessionOrigins.
//   - The order is r.mu → historyMu when both are needed (backfill apply).
//   - Reads of prevSessionIDs go through historyMu RLock.
package session

import (
	"context"
	"log/slog"
	"slices"
	"sort"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/metrics"
)

// maybeAttachAutoChainOnSpawn implements the spawn-path auto-chain
// decision (RFC §4.4-A). Returns the chain to assign to prev_session_ids
// (oldest → newest). Returns nil when auto-chain is skipped (internal
// key, disabled policy, inherited prev / history, or no eligible
// candidates).
//
// Caller must NOT hold r.mu. The function takes r.mu internally for
// phase 1 (excluder snapshot) and phase 3 (re-validation), and runs
// the disk-touching pickWorkspaceChain call between the two windows.
//
// New-B1 closure: phase 3 re-builds the verifyExcluder from the
// up-to-date r.sessions snapshot AND the latest atomic-loaded
// extras pointer. A cron job that registered a new sessionID
// between phase 2 and phase 3 will be reflected via its registered
// SessionIDExcluder; the candidate is dropped before apply.
func (r *Router) maybeAttachAutoChainOnSpawn(
	key, workspace string,
	prevIDs []string,
	oldHistory []cli.EventEntry,
) []string {
	if r.autoChainPolicy == nil {
		return nil
	}
	if IsCronKey(key) || IsSysKey(key) || IsScratchKey(key) {
		return nil
	}
	if workspace == "" || !r.autoChainPolicy.Enabled(workspace) {
		return nil
	}
	// Inherited state (resume / chain rotation) means the chain has
	// already been decided by an earlier path; do not auto-attach on
	// top of it. Empty prev + empty history is the signal for "fresh
	// session, eligible for auto-attach".
	if len(prevIDs) > 0 || len(oldHistory) > 0 {
		return nil
	}

	// R242-ARCH-16 (#760): block until every SessionIDExcluder owner
	// (cron Scheduler, sysession Manager, future subsystems) has
	// registered. When the router was constructed with
	// PendingExcluders=false the gate is already closed and this
	// returns immediately. On timeout we skip the auto-chain attach
	// rather than risk leaking an internal sessionID into the chain.
	if !r.waitExcludersReady(context.Background()) {
		slog.Warn("auto-chain spawn: excluders-ready gate timed out; skipping attach",
			"key", key, "workspace", workspace)
		return nil
	}

	// Phase 1: snapshot excluders under r.mu.
	phase1 := r.snapshotCombinedExcluderLocked()

	// Phase 2: disk-touching candidate selection (no lock).
	auto := pickWorkspaceChain(workspace, r.autoChainListJSONL, phase1, r.autoChainPolicy, time.Now())
	if len(auto) == 0 {
		return nil
	}

	r.firePhase3SpawnHook()

	// Phase 3: re-validate under r.mu. Cron / sys may have registered
	// new sessionIDs since phase 1; rebuild the excluder set with the
	// latest snapshot before committing.
	phase3 := r.snapshotCombinedExcluderLocked()
	verified := filterByExcluder(auto, phase3)
	if drops := len(auto) - len(verified); drops > 0 {
		metrics.AutoChainTOCTOUCollisionTotal.Add(int64(drops))
		slog.Warn("auto-chain TOCTOU drop on spawn",
			"key", key,
			"workspace", workspace,
			"dropped", drops,
			"kept", len(verified))
	}
	if len(verified) == 0 {
		return nil
	}
	return verified
}

// AddSessionIDExcluder appends an external SessionIDExcluder to the
// router's excluder list. Called once at startup wiring (cmd/naozhi/main.go)
// for cron Scheduler and sysession Manager. Atomic copy-on-write so reads
// from the auto-chain decision path stay lock-free under r.mu.
//
// Idempotent for nil: a nil excluder is silently ignored so callers do
// not have to guard.
func (r *Router) AddSessionIDExcluder(e SessionIDExcluder) {
	if e == nil {
		return
	}
	for {
		cur := r.excluders.Load()
		var next []SessionIDExcluder
		if cur != nil {
			next = make([]SessionIDExcluder, 0, len(*cur)+1)
			next = append(next, *cur...)
		} else {
			next = make([]SessionIDExcluder, 0, 1)
		}
		next = append(next, e)
		if r.excluders.CompareAndSwap(cur, &next) {
			return
		}
	}
}

// extraExcluders returns the external excluder slice as a plain []SessionIDExcluder
// (deref of the atomic pointer). Empty when no excluder has been registered.
// Called inside r.mu critical sections in the auto-chain paths to keep
// lock semantics simple — the atomic Load is cheap and the resulting
// slice header is read-only after publication.
func (r *Router) extraExcluders() []SessionIDExcluder {
	cur := r.excluders.Load()
	if cur == nil {
		return nil
	}
	return *cur
}

// MarkExcludersReady signals that every owner of internal CLI session
// IDs has registered its SessionIDExcluder via AddSessionIDExcluder.
// Auto-chain readers block on this gate when the router was
// constructed with RouterConfig.PendingExcluders=true.
//
// Idempotent: subsequent calls are no-ops (sync.Once-guarded close).
// Safe to call from any goroutine. R242-ARCH-16 (#760).
func (r *Router) MarkExcludersReady() {
	r.excludersReadyOnce.Do(func() { close(r.excludersReadyCh) })
}

// excludersReadyWait is the default timeout for waitExcludersReady. The
// auto-chain paths that consult it are non-critical (no user-visible
// blocking), and a 5s ceiling keeps NewRouter / spawn paths from
// hanging if cmd-wireup never calls MarkExcludersReady (e.g. a partial
// startup that errored before wireup completed).
const excludersReadyWait = 5 * time.Second

// waitExcludersReady blocks until either the gate is closed (i.e.
// MarkExcludersReady was called), the supplied context is cancelled,
// or excludersReadyWait elapses. Returns true when the gate is closed
// in time; false on timeout or ctx cancel — callers fall through to
// the legacy "no exclusions" behaviour and emit a metrics counter.
//
// The function never blocks when RouterConfig.PendingExcluders was
// left unset (default), because NewRouter closes excludersReadyCh
// immediately in that case.
func (r *Router) waitExcludersReady(ctx context.Context) bool {
	if r.excludersReadyCh == nil {
		// Defensive: a zero-value Router constructed bypassing
		// NewRouter (older test helpers) would have a nil channel;
		// treat as ready to preserve legacy behaviour.
		return true
	}
	select {
	case <-r.excludersReadyCh:
		return true
	default:
	}
	t := time.NewTimer(excludersReadyWait)
	defer t.Stop()
	select {
	case <-r.excludersReadyCh:
		return true
	case <-ctx.Done():
		metrics.AutoChainExcludersWaitTimeout.Add(1)
		return false
	case <-t.C:
		metrics.AutoChainExcludersWaitTimeout.Add(1)
		return false
	}
}

// snapshotRouterExcludedLocked returns a SessionIDExcluder backed by
// every CLI session ID currently owned by an r.sessions entry (the
// session's getSessionID() and its prev_session_ids chain). Caller MUST
// hold r.mu (read or write).
//
// The returned excluder owns the snapshot map and is safe to use after
// r.mu is released — cron / sysession excluders consult their own
// state independently.
func (r *Router) snapshotRouterExcludedLocked() SessionIDExcluder {
	used := make(map[string]bool, len(r.sessions)*2)
	for _, s := range r.sessions {
		if id := s.getSessionID(); id != "" {
			used[id] = true
		}
		// prevSessionIDs is read under historyMu but we hold r.mu —
		// any writer of prevSessionIDs that holds historyMu does NOT
		// hold r.mu, so a torn read is theoretically possible. In
		// practice every prev write site (spawn lifecycle, this
		// backfill) takes r.mu first, then historyMu, then writes;
		// the historyMu RLock here is a defensive narrow window
		// that pairs with that order. We can't ignore historyMu
		// because daemon paths (sysession SetUserLabel) hold only
		// historyMu and might mutate adjacent fields.
		s.historyMu.RLock()
		for _, id := range s.prevSessionIDs {
			used[id] = true
		}
		s.historyMu.RUnlock()
	}
	return mapExcluder{set: used}
}

// snapshotCombinedExcluderLocked is the canonical "router snapshot +
// extras" combined excluder used by the spawn-path auto-chain decision.
// Acquires r.mu internally for the snapshot + atomic-load and returns
// the bundle ready to feed into pickWorkspaceChain / filterByExcluder.
//
// R242-ARCH-18: previously inlined twice in maybeAttachAutoChainOnSpawn
// (phase 1 + phase 3) with subtly different slice-cap layouts. Extracting
// keeps phase 2 and phase 3 byte-for-byte equivalent so a future excluder
// type added to the bundle lands in both phases by construction rather
// than two coordinated edits.
//
// Two-helper split: this function is the locking shell; the actual
// bundle construction lives in combinedExcluderHeld so callers that
// already hold r.mu (e.g. runAutoChainBackfillOnce Phase 3, which
// holds the lock through the apply loop) can build the same bundle
// without re-entering r.mu. Keeping a single source of truth for
// "what goes in the bundle" closes R242-ARCH-18's secondary inlining
// at the backfill site.
func (r *Router) snapshotCombinedExcluderLocked() combinedExcluder {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.combinedExcluderHeld()
}

// combinedExcluderHeld assembles the router-snapshot + extras combined
// excluder. CALLER MUST HOLD r.mu (read or write). Used by
// snapshotCombinedExcluderLocked (which adds the lock shell) and by
// runAutoChainBackfillOnce Phase 3 (which already holds r.mu through
// the apply loop). Centralising the slice layout here means a future
// excluder source added to the bundle (extra-extras, future namespace
// gate, etc.) lands in every consumer by construction rather than
// requiring coordinated edits across two call sites — R242-ARCH-18
// closure for the backfill path.
func (r *Router) combinedExcluderHeld() combinedExcluder {
	routerExcluder := r.snapshotRouterExcludedLocked()
	extras := r.extraExcluders()
	inner := make([]SessionIDExcluder, 0, 1+len(extras))
	inner = append(inner, routerExcluder)
	inner = append(inner, extras...)
	return combinedExcluder{inner: inner}
}

// mapExcluder is a SessionIDExcluder backed by a plain map. Used by
// snapshotRouterExcludedLocked to expose its lock-released snapshot.
type mapExcluder struct {
	set map[string]bool
}

// IsExcluded reports whether id is in the snapshot.
func (m mapExcluder) IsExcluded(id string) bool {
	if m.set == nil {
		return false
	}
	return m.set[id]
}

// runAutoChainBackfillOnce is the NewRouter startup hook that fills
// prev_session_ids on every session that qualifies (RFC §4.4-B).
//
// CRITICAL ordering: must run synchronously BEFORE any goroutine in
// the "Async-load history" Tier 1 / Tier 2 block in NewRouter is
// launched. Otherwise Tier 2 goroutines would observe an outdated
// chain (still empty for old sessions) and load only the current
// sessionID's JSONL, breaking dashboard pagination for legacy
// sessions. Pinned by TestNewRouter_AutoChainPrecedesTier2Loaders.
//
// Phase layout:
//
//	Phase 1 (single r.mu lock): collect candidates + the router
//	excluder snapshot. No I/O.
//
//	Phase 2 (lock free): per candidate, call pickWorkspaceChain
//	(does ReadDir via discovery.ListWorkspaceJSONL behind a cache).
//	selfExcluder accumulates IDs claimed earlier in the batch.
//
//	Phase 3 (single r.mu lock + per-session historyMu): re-validate
//	each decision against the freshly-snapshotted excluder set
//	(closes the New-B1 race where cron starts a new session in the
//	gap between Phase 2 and Phase 3) and apply.
//
// Single-threaded by construction: only NewRouter calls this, so
// the consumed map need not be locked. Any future change that runs
// the function from a parallel context must add synchronisation
// (RFC §4.4-B Phase 2 note).
func (r *Router) runAutoChainBackfillOnce() {
	if r.autoChainPolicy == nil {
		return
	}
	// R242-ARCH-16 (#760): block until every SessionIDExcluder owner
	// has registered. The default-PendingExcluders=false embedder
	// (tests, single-owner topologies) sees no change because the
	// gate is closed at NewRouter.
	if !r.waitExcludersReady(r.historyCtx) {
		slog.Warn("auto-chain backfill: excluders-ready gate timed out; skipping run")
		return
	}
	startedAt := time.Now()
	updated := 0
	skipped := 0
	defer func() {
		// Single summary line per startup (RFC §7). Useful for ops to
		// confirm the backfill ran at all, see how many sessions were
		// touched, and capture the cost — without grepping per-session
		// "auto-chain backfill" lines.
		slog.Info("auto-chain backfill complete",
			"updated", updated,
			"skipped", skipped,
			"duration_ms", time.Since(startedAt).Milliseconds())
	}()

	// ── Phase 1 ──────────────────────────────────────────────────
	r.mu.Lock()
	candidates := make([]*ManagedSession, 0, len(r.sessions))
	for _, s := range r.sessions {
		if IsCronKey(s.key) || IsSysKey(s.key) || IsScratchKey(s.key) {
			continue
		}
		s.historyMu.RLock()
		hasPrev := len(s.prevSessionIDs) != 0
		s.historyMu.RUnlock()
		if hasPrev {
			continue
		}
		ws := s.Workspace()
		if ws == "" {
			metrics.AutoChainBackfillSkippedNoWorkspace.Add(1)
			skipped++
			continue
		}
		if !r.autoChainPolicy.Enabled(ws) {
			skipped++
			continue
		}
		candidates = append(candidates, s)
	}
	routerExcluder := r.snapshotRouterExcludedLocked()
	extras := r.extraExcluders()
	r.mu.Unlock()

	if len(candidates) == 0 {
		return
	}

	// Deterministic ordering: oldest-active session takes its chain
	// first so the early creation of one workspace's "first" session
	// reliably prefixes the chain rather than randomly.
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].lastActive.Load() < candidates[j].lastActive.Load()
	})

	// ── Phase 2 ──────────────────────────────────────────────────
	consumed := map[string]bool{}
	consumedSelf := selfExcluder{set: consumed}

	type decision struct {
		s   *ManagedSession
		ids []string
	}
	decisions := make([]decision, 0, len(candidates))

	for _, s := range candidates {
		// excluderBase aggregates router + cron/sys + selfExcluder so
		// each call sees both prior decisions and external state.
		inner := make([]SessionIDExcluder, 0, 2+len(extras))
		inner = append(inner, routerExcluder, consumedSelf)
		inner = append(inner, extras...)
		excluder := combinedExcluder{inner: inner}
		ids := pickWorkspaceChain(s.Workspace(), r.autoChainListJSONL, excluder, r.autoChainPolicy, time.Now())
		if len(ids) == 0 {
			metrics.AutoChainBackfillSkippedNoCandidates.Add(1)
			skipped++
			continue
		}
		for _, id := range ids {
			consumed[id] = true
		}
		decisions = append(decisions, decision{s: s, ids: ids})
	}

	r.firePhase3BackfillHook()

	if len(decisions) == 0 {
		return
	}

	// ── Phase 3 ──────────────────────────────────────────────────
	r.mu.Lock()
	defer r.mu.Unlock()
	// R242-ARCH-18: re-use combinedExcluderHeld so this Phase 3 verifier
	// stays byte-for-byte equivalent to the Phase 2 build inside
	// snapshotCombinedExcluderLocked. Previously inlined `routerExcluder
	// + extras` here drifted independently from the spawn-path bundle
	// (different slice-cap layout); both paths now share one source of
	// truth so a future excluder source added to the bundle lands here
	// by construction.
	verifier := r.combinedExcluderHeld()

	dirty := false
	for _, d := range decisions {
		verified := filterByExcluder(d.ids, verifier)
		if drops := len(d.ids) - len(verified); drops > 0 {
			metrics.AutoChainTOCTOUCollisionTotal.Add(int64(drops))
		}
		if len(verified) == 0 {
			metrics.AutoChainBackfillSkippedTOCTOUDrop.Add(1)
			skipped++
			continue
		}

		d.s.historyMu.Lock()
		// Re-check prev under historyMu — a parallel non-router writer
		// could have touched it. (No production path does today, but
		// the guard keeps invariants explicit for future code.)
		if len(d.s.prevSessionIDs) != 0 {
			d.s.historyMu.Unlock()
			metrics.AutoChainBackfillSkippedAlreadyFilled.Add(1)
			skipped++
			continue
		}
		d.s.prevSessionIDs = slices.Clone(verified)
		d.s.historyMu.Unlock()

		d.s.SetPrevSessionOrigins(verified, "auto-backfill")
		slog.Info("auto-chain backfill",
			"key", d.s.key,
			"workspace", d.s.Workspace(),
			"chain_ids", verified,
			"chain_len", len(verified))
		metrics.AutoChainBackfillAttachTotal.Add(1)
		updated++
		dirty = true
	}

	if dirty {
		r.storeDirty = true
		r.storeGen.Add(1)
	}
}
