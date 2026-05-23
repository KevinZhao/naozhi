// Package session — auto-workspace-chain implementation.
//
// See docs/rfc/auto-workspace-chain.md (Draft v3, APPROVED) for the full
// design. This file owns:
//
//   - SessionIDExcluder       — interface for "is this CLI sessionID already
//     owned by another subsystem?". Implementations
//     in cron / sysession / router itself.
//   - AutoChainPolicy         — interface for "is auto-chain on for this
//     workspace, and with what window/cap?".
//     Default impl reads global config; future
//     ProjectManager can implement same interface
//     for per-workspace overrides without changing
//     pickWorkspaceChain's signature (RFC §10 OQ).
//   - pickWorkspaceChain      — the pure decision function.
//   - combinedExcluder        — fan-out helper.
//   - selfExcluder            — backfill-internal "ID already consumed by
//     another decision in this batch" hook.
//   - filterByExcluder        — re-validation filter used by §4.4-A phase 3
//     and §4.4-B Phase 3 (New-B1 closure).
//   - recentFilterAsExcluder  — adapter so cron/sys can implement
//     discovery.RecentSessionsFilter once and
//     flow into both auto-chain and history-panel
//     filtering (Arch-MINOR-2).
//   - GlobalAutoChainPolicy   — the production policy struct, populated by
//     cmd/naozhi/main.go from cfg.Session.AutoChain.
package session

import (
	"sort"
	"time"

	"github.com/naozhi/naozhi/internal/discovery"
)

// SessionIDExcluder reports whether a CLI sessionID is already owned
// by some other naozhi subsystem (active session, cron run, sysession
// daemon, scratch). pickWorkspaceChain treats every "true" as a hard
// exclusion — those IDs are never auto-chained into a fresh session.
//
// Implementations MUST be safe for concurrent use AND side-effect-free
// on the read path. Any internal cache must be populated outside of
// IsExcluded (e.g. eagerly at registration time) so the hot path stays
// branch-light. Mirrors the contract for discovery.RecentSessionsFilter.
type SessionIDExcluder interface {
	IsExcluded(sessionID string) bool
}

// AutoChainPolicy is the per-decision policy interface (RFC v3 Arch-B4).
// Default impl is GlobalAutoChainPolicy reading RouterConfig.AutoChain.
// Phase 3 will introduce ProjectManagerAutoChainPolicy that returns
// per-workspace overrides; pickWorkspaceChain's signature does not
// change between the two.
type AutoChainPolicy interface {
	Enabled(workspace string) bool
	Window(workspace string) time.Duration
	Cap(workspace string) int
}

// GlobalAutoChainPolicy is the production AutoChainPolicy backed by
// the global config block. Zero-value means "disabled" — callers should
// always populate via cmd/naozhi wiring (see cfg.Session.AutoChain).
type GlobalAutoChainPolicy struct {
	EnabledFlag bool
	WindowDur   time.Duration
	CapValue    int
}

// Enabled returns the global enabled flag, ignoring workspace.
func (p GlobalAutoChainPolicy) Enabled(string) bool { return p.EnabledFlag }

// Window returns the global window duration, ignoring workspace.
func (p GlobalAutoChainPolicy) Window(string) time.Duration { return p.WindowDur }

// Cap returns the global cap, ignoring workspace.
func (p GlobalAutoChainPolicy) Cap(string) int { return p.CapValue }

// disabledAutoChainPolicy is the safe fallback used when RouterConfig
// leaves AutoChainPolicy nil. Always returns Enabled=false so the
// feature is opt-in even on a freshly-constructed Router.
type disabledAutoChainPolicy struct{}

func (disabledAutoChainPolicy) Enabled(string) bool         { return false }
func (disabledAutoChainPolicy) Window(string) time.Duration { return 0 }
func (disabledAutoChainPolicy) Cap(string) int              { return 0 }

// combinedExcluder fans IsExcluded out across multiple SessionIDExcluder
// instances. Used by pickWorkspaceChain (Phase 2) and by the spawn /
// backfill verify steps (Phase 3, RFC §4.4 New-B1).
//
// inner is intentionally a slice (not a set) — order is irrelevant for
// correctness, and slice iteration with bounded N (≤4 in practice:
// router + cron + sys + selfExcluder) beats map ops on short fan-outs.
type combinedExcluder struct {
	inner []SessionIDExcluder
}

// IsExcluded short-circuits on the first hit.
func (c combinedExcluder) IsExcluded(id string) bool {
	for _, e := range c.inner {
		if e != nil && e.IsExcluded(id) {
			return true
		}
	}
	return false
}

// selfExcluder lets the backfill loop accumulate "IDs already given to
// an earlier session in this same batch" without going through the
// router. Backed by a plain map because runAutoChainBackfillOnce is
// single-threaded by construction (RFC §4.4-B Arch-MINOR-3 contract).
type selfExcluder struct {
	set map[string]bool
}

// IsExcluded reports whether id was claimed by a prior decision in the
// same backfill pass.
func (s selfExcluder) IsExcluded(id string) bool {
	if s.set == nil {
		return false
	}
	return s.set[id]
}

// recentFilterAsExcluder adapts a discovery.RecentSessionsFilter to the
// SessionIDExcluder interface (RFC §4.3 Arch-MINOR-2). cron / sysession
// only need to implement RecentSessionsFilter once; the adapter lets
// auto-chain consume the same filter without separate boilerplate.
//
// SkipWorkspace is intentionally ignored — auto-chain is already scoped
// to a single workspace by the time pickWorkspaceChain runs, so a
// per-ID exclusion via SkipSessionID is the only relevant primitive.
type recentFilterAsExcluder struct {
	f discovery.RecentSessionsFilter
}

// IsExcluded forwards to SkipSessionID. nil filter → never excluded.
func (a recentFilterAsExcluder) IsExcluded(id string) bool {
	if a.f == nil {
		return false
	}
	return a.f.SkipSessionID(id)
}

// AsExcluder is the public factory used by cmd/naozhi wiring:
//
//	router.AddSessionIDExcluder(session.AsExcluder(scheduler))
//
// scheduler must satisfy discovery.RecentSessionsFilter. Returning
// SessionIDExcluder (not the concrete adapter) keeps the seam stable
// across future changes.
func AsExcluder(f discovery.RecentSessionsFilter) SessionIDExcluder {
	return recentFilterAsExcluder{f: f}
}

// filterByExcluder returns the subset of ids not currently excluded.
// Used by the spawn-path phase-3 re-check and the backfill phase-3
// re-check to close the New-B1 race: between the lock-free pick and
// the lock-held apply, cron / sys / another spawn may have claimed
// an ID; we re-validate under lock with the freshly-snapshotted
// excluder set.
//
// Result preserves input order. Non-allocating fast path when no ID
// drops out — the dominant case in steady state.
func filterByExcluder(ids []string, excluder SessionIDExcluder) []string {
	if len(ids) == 0 {
		return nil
	}
	for i, id := range ids {
		if excluder.IsExcluded(id) {
			out := make([]string, 0, len(ids)-1)
			out = append(out, ids[:i]...)
			for _, rest := range ids[i+1:] {
				if !excluder.IsExcluded(rest) {
					out = append(out, rest)
				}
			}
			return out
		}
	}
	return ids
}

// defaultAutoChainWindow is the fallback look-back window applied when a
// policy returns Window<=0. Seven days matches RFC §4.2 — long enough to
// cover an interrupted multi-day investigation across a weekend yet short
// enough to keep the JSONL scan bounded on busy workspaces. A misconfigured
// policy (zero / negative duration) silently inherits this floor instead of
// degenerating to "no chain ever".
const defaultAutoChainWindow = 7 * 24 * time.Hour

// pickWorkspaceChain returns sessionIDs to auto-prefix to a freshly
// created session's prev_session_ids. Pure: no router mutation.
//
// Inputs are seam-injected so tests can fake them without touching the
// production filesystem:
//   - listJSONL: discovery.ListWorkspaceJSONL in production; a fixture
//     fn in tests.
//   - excluder: combined cron + sys + r.sessions excluder + (in backfill)
//     a selfExcluder for already-consumed IDs.
//   - policy: GlobalAutoChainPolicy (or future per-workspace impl).
//   - now: clock injection for window-cutoff tests.
//
// Sentinel checks at the top close Go-B5 (cap=0 / window=0 zero-value
// defaults) — even a misconfigured policy degrades to safe behaviour
// rather than producing zero results or unbounded chains.
func pickWorkspaceChain(
	workspace string,
	listJSONL func(workspace string) []discovery.WorkspaceJSONL,
	excluder SessionIDExcluder,
	policy AutoChainPolicy,
	now time.Time,
) []string {
	if workspace == "" || policy == nil || !policy.Enabled(workspace) {
		return nil
	}
	window := policy.Window(workspace)
	if window <= 0 {
		window = defaultAutoChainWindow
	}
	chainCap := policy.Cap(workspace)
	if chainCap <= 0 || chainCap > maxPrevSessionIDs {
		chainCap = maxPrevSessionIDs
	}
	if listJSONL == nil {
		return nil
	}
	files := listJSONL(workspace)
	if len(files) == 0 {
		return nil
	}

	cutoff := now.Add(-window).UnixMilli()

	type cand struct {
		id    string
		mtime int64
	}
	cands := make([]cand, 0, len(files))
	for _, f := range files {
		if f.Mtime < cutoff {
			continue
		}
		if excluder != nil && excluder.IsExcluded(f.SessionID) {
			continue
		}
		cands = append(cands, cand{id: f.SessionID, mtime: f.Mtime})
	}
	if len(cands) == 0 {
		return nil
	}
	sort.SliceStable(cands, func(i, j int) bool {
		return cands[i].mtime < cands[j].mtime
	})
	// Reserve one slot for the about-to-spawn session itself; chain holds
	// strictly previous IDs, so the cap-1 ceiling matches §4.2 step 4.
	keep := chainCap - 1
	if keep <= 0 {
		// chainCap=1 means "no prev allowed"; degenerate but legal.
		return nil
	}
	if len(cands) > keep {
		// Take the tail (mtime-newest keep items). RFC §4.2 step 4
		// v3.1 revision: dashboard pagination walks newest→oldest, so
		// the chain's earliest entries (very old sessions in the same
		// workspace) are the cheapest to drop when we exceed cap.
		// Slice still ascends by mtime — prev_session_ids is
		// oldest→newest by contract.
		cands = cands[len(cands)-keep:]
	}
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.id
	}
	return out
}
