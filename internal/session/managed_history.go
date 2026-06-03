package session

import (
	"log/slog"
	"slices"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/metrics"
)

// SnapshotChainIDs returns the session-ID chain (oldest → newest). The
// current session ID is appended only when non-empty — a just-spawned
// session that hasn't captured its first ID yet yields the prev chain
// alone, which matches how router.go builds the chain for JSONL loads
// today.
//
// Lock contract (R230C-GO-1): the authoritative invariant for
// prevSessionIDs is "writers hold r.mu; readers either hold r.mu or
// accept a stale-but-not-torn snapshot". All writers (registerStub /
// spawnSession.installFreshSessionLocked / RenameSession /
// router_core restore) write under r.mu.Lock(). This reader runs from
// cli.Wrapper.NewHistorySource factories which do NOT hold r.mu, so
// historyMu.RLock here is a defensive rope: it does not synchronise
// with the r.mu writers, but the slices.Clone in writers + the append
// pattern guarantee any value we observe was a complete prior snapshot
// (Go's memory model on slice header writes is per-word atomic on
// 64-bit). historyMu still serialises against the InjectHistory
// persistedHistory append path which lives next to prevSessionIDs in
// memory. A future cleanup is to take r.mu.RLock() here instead, but
// that requires plumbing the router pointer into ManagedSession and
// is not done as part of R230C-GO-1.
//
// Exported (Sprint 1a) so cli.Wrapper.NewHistorySource factories can
// pull the current chain at LoadBefore time without the cli package
// having to know about ManagedSession internals. Callers must not
// mutate the returned slice — the underlying append/clone defends
// against torn writes but not against caller-side modification.
func (s *ManagedSession) SnapshotChainIDs() []string {
	s.historyMu.RLock()
	defer s.historyMu.RUnlock()
	cur := s.getSessionID()
	n := len(s.prevSessionIDs)
	if cur != "" {
		n++
	}
	if n == 0 {
		return nil
	}
	out := make([]string, 0, n)
	out = append(out, s.prevSessionIDs...)
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// SetPrevSessionOrigins records the origin label for the most-recently
// appended chain segment (the trailing len(ids) entries of prevSessionIDs).
// Older origin entries are preserved with their existing value, defaulting
// to "manual" for any prefix that was set before origins tracking arrived.
//
// origin is one of: "manual" / "auto-spawn" / "auto-backfill" / "resume".
// Empty origin is rejected as a no-op (caller bug protection).
//
// Invariant (RFC v3 Arch-MINOR-1): prev_session_ids is append-only — every
// production write path (spawn chain rotation, auto-chain attach,
// RegisterCronStubWithChain replace, store restore) only grows the slice
// or replaces it wholesale. SetPrevSessionOrigins detects drift (origins
// longer than ids, or "ids" tail position negative) and rebuilds origins
// to all-"manual" rather than allowing a misaligned label to persist.
// The drift is metric-counted so a regression in a future writer is
// visible in production telemetry.
func (s *ManagedSession) SetPrevSessionOrigins(ids []string, origin string) {
	if origin == "" || len(ids) == 0 {
		return
	}
	s.historyMu.Lock()
	defer s.historyMu.Unlock()

	// Drift detection. start := len(ids in chain) - len(this batch).
	// Negative means the batch is longer than the chain — the batch
	// was not appended to the tail. Origins longer than IDs means a
	// past write left dangling labels. Both rebuild the parallel
	// slice from scratch with "manual" defaults so origin↔id never
	// misaligns silently.
	start := len(s.prevSessionIDs) - len(ids)
	driftLonger := len(s.prevSessionOrigins) > len(s.prevSessionIDs)
	if start < 0 || driftLonger {
		metrics.AutoChainOriginsLengthMismatch.Add(1)
		slog.Warn("auto-chain: prev_session_origins length drift; rebuilding to manual",
			"key", s.key,
			"prev_ids_len", len(s.prevSessionIDs),
			"prev_origins_len", len(s.prevSessionOrigins),
			"incoming_len", len(ids))
		rebuilt := make([]string, len(s.prevSessionIDs))
		for i := range rebuilt {
			rebuilt[i] = "manual"
		}
		s.prevSessionOrigins = rebuilt
		// Re-derive start with the now-clean baseline; if start was
		// negative the batch is meaningless against this chain — bail.
		if start < 0 {
			return
		}
	}

	// Grow origins to match the chain length, defaulting any older
	// untracked prefix to "manual" so the resulting slice is fully
	// populated.
	if len(s.prevSessionOrigins) < len(s.prevSessionIDs) {
		grown := make([]string, len(s.prevSessionIDs))
		copy(grown, s.prevSessionOrigins)
		for i := len(s.prevSessionOrigins); i < len(grown); i++ {
			grown[i] = "manual"
		}
		s.prevSessionOrigins = grown
	}

	// Stamp the trailing len(ids) entries with the supplied origin.
	for i := range ids {
		s.prevSessionOrigins[start+i] = origin
	}
}

// SnapshotPrevSessionOrigins returns a defensive copy of the parallel
// origins slice. Callers (storeStateLocked / dashboard introspection)
// must not mutate the result. Length is exactly len(prevSessionIDs);
// any unset entry is materialised as "manual" so consumers can always
// align positionally without nil-checks.
func (s *ManagedSession) SnapshotPrevSessionOrigins() []string {
	s.historyMu.RLock()
	defer s.historyMu.RUnlock()
	if len(s.prevSessionIDs) == 0 {
		return nil
	}
	out := make([]string, len(s.prevSessionIDs))
	for i := range out {
		if i < len(s.prevSessionOrigins) && s.prevSessionOrigins[i] != "" {
			out[i] = s.prevSessionOrigins[i]
		} else {
			out[i] = "manual"
		}
	}
	return out
}

// SnapshotPrevSessionIDs returns a defensive copy of the prevSessionIDs
// chain (oldest → newest). Read-only callers in router_*.go can use this
// instead of reaching into s.prevSessionIDs directly under historyMu —
// the accessor formalises the boundary required before splitting Router
// into sub-aggregates (R215-ARCH-P1-1, #545). Returns nil when the chain
// is empty (matches SnapshotPrevSessionOrigins shape).
func (s *ManagedSession) SnapshotPrevSessionIDs() []string {
	s.historyMu.RLock()
	defer s.historyMu.RUnlock()
	if len(s.prevSessionIDs) == 0 {
		return nil
	}
	return slices.Clone(s.prevSessionIDs)
}

// ReplacePrevSessionIDs swaps the prevSessionIDs chain wholesale and
// returns the replaced length. The supplied slice is cloned so the caller
// can reuse / mutate its argument. Origins are NOT touched here —
// SetPrevSessionOrigins is the dedicated path for parallel origin
// alignment and runs its own drift checks. Callers that need both must
// invoke ReplacePrevSessionIDs first then SetPrevSessionOrigins so the
// length-drift detector sees the post-replace baseline. R215-ARCH-P1-1.
func (s *ManagedSession) ReplacePrevSessionIDs(ids []string) {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	if len(ids) == 0 {
		s.prevSessionIDs = nil
		return
	}
	s.prevSessionIDs = slices.Clone(ids)
}

// RebuildChainFiltered atomically rebuilds prevSessionIDs and
// prevSessionOrigins under a SINGLE historyMu write hold, keeping only the
// indices where keepMask is true. Both parallel slices are filtered with the
// same mask so they stay positionally aligned — no reader can observe an
// intermediate state where the two slices differ in length.
//
// Why a dedicated method (RFC §9.2 v2.1, Go-BLOCKING-2): composing
// ReplacePrevSessionIDs + SetPrevSessionOrigins cannot achieve this — each
// takes historyMu independently, so between the two calls a reader running
// SnapshotPrevSessionOrigins would see len(prevSessionIDs) != len(origins)
// and synthesise wrong "manual" labels. Doing both mutations in one lock
// hold closes that window.
//
// keepMask must have len == len(prevSessionIDs); a mismatched mask is a
// caller bug and is treated as "keep nothing changed" (no-op) to avoid
// corrupting the chain. Returns the number of entries removed.
//
// Origins shorter than the ID chain (legacy / untracked prefix) are treated
// as "manual" for the surviving entries, matching SnapshotPrevSessionOrigins'
// positional fallback, so the rebuilt origins slice is exactly len(newIDs).
func (s *ManagedSession) RebuildChainFiltered(keepMask []bool) int {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()

	n := len(s.prevSessionIDs)
	if len(keepMask) != n {
		// Length mismatch — refuse to touch the chain rather than risk
		// misaligning the parallel slices.
		return 0
	}
	if n == 0 {
		return 0
	}

	newIDs := make([]string, 0, n)
	newOrigins := make([]string, 0, n)
	removed := 0
	for i := 0; i < n; i++ {
		if !keepMask[i] {
			removed++
			continue
		}
		newIDs = append(newIDs, s.prevSessionIDs[i])
		origin := "manual"
		if i < len(s.prevSessionOrigins) && s.prevSessionOrigins[i] != "" {
			origin = s.prevSessionOrigins[i]
		}
		newOrigins = append(newOrigins, origin)
	}

	if removed == 0 {
		// Nothing dropped — leave the slices untouched so we don't perturb
		// a possibly-shorter origins slice that callers tolerate.
		return 0
	}

	if len(newIDs) == 0 {
		s.prevSessionIDs = nil
		s.prevSessionOrigins = nil
		return removed
	}
	s.prevSessionIDs = newIDs
	s.prevSessionOrigins = newOrigins
	return removed
}

// SnapshotPersistedHistory returns a defensive copy of the persistedHistory
// ring. The result is safe to mutate without affecting the session. Returns
// nil when the ring is empty so callers don't pay a zero-length alloc.
// R215-ARCH-P1-1: pre-requisite accessor for splitting Router into
// sub-aggregates without leaking ManagedSession internals.
func (s *ManagedSession) SnapshotPersistedHistory() []cli.EventEntry {
	s.historyMu.RLock()
	defer s.historyMu.RUnlock()
	if len(s.persistedHistory) == 0 {
		return nil
	}
	out := make([]cli.EventEntry, len(s.persistedHistory))
	copy(out, s.persistedHistory)
	return out
}

// persistedHistoryBefore collects up to `limit` entries from persistedHistory
// strictly older than beforeMS. Returns entries in reverse-walk order (newest
// first). The second return value is true when persistedHistorySorted is set,
// meaning the history is Time-ascending and the backward walk therefore
// produces a strictly Time-descending result — the caller can obtain ascending
// order by a cheap slices.Reverse instead of a full sort. Only relevant when
// proc is nil; live-process sessions go through proc.EventEntriesBefore directly.
func (s *ManagedSession) persistedHistoryBefore(beforeMS int64, limit int) ([]cli.EventEntry, bool) {
	if limit <= 0 {
		return nil, false
	}
	s.historyMu.RLock()
	defer s.historyMu.RUnlock()
	if len(s.persistedHistory) == 0 {
		return nil, false
	}
	sorted := s.persistedHistorySorted
	// Walk backward collecting up to `limit` entries strictly older than
	// beforeMS. persistedHistory is not guaranteed to be sorted, so a full
	// linear walk is the conservative choice.
	out := make([]cli.EventEntry, 0, limit)
	for i := len(s.persistedHistory) - 1; i >= 0 && len(out) < limit; i-- {
		e := s.persistedHistory[i]
		if beforeMS > 0 && e.Time >= beforeMS {
			continue
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil, false
	}
	// When sorted==true the backward walk produced a Time-descending sequence.
	// Leave the order as-is; the caller decides whether to Reverse or full-sort.
	return out, sorted
}

// InjectHistory pre-populates the event log with historical entries.
// Entries are saved to persistedHistory so they survive process restarts.
func (s *ManagedSession) InjectHistory(entries []cli.EventEntry) {
	if len(entries) > maxPersistedHistory {
		slog.Debug("inject history: batch exceeds cap, truncating oldest",
			"key", s.key,
			"batch_len", len(entries),
			"cap", maxPersistedHistory,
			"dropped", len(entries)-maxPersistedHistory)
		entries = entries[len(entries)-maxPersistedHistory:]
	}
	// Scan the injected batch for prompt/activity summaries outside the lock:
	// the scan operates on the caller-supplied slice only (not persistedHistory),
	// and the only side-effects are atomic.Pointer[string] Store calls. Keeping
	// it out of historyMu lets concurrent readers (EventEntries / EventEntriesSince
	// / EventEntriesBefore) proceed during 500-entry JSONL replays at startup.
	// R61-PERF-9.
	prompt, activity, response := scanLastSummaries(entries)

	// Mutate persistedHistory AND read s.process under the same historyMu
	// hold so a concurrent attachProcessAndSnapshotPersisted (also serialised
	// on historyMu) cannot stamp seededLen between our load-process and our
	// forward-decision: either it ran first (we observe the new proc and
	// seededLen=full snapshot, forward only genuinely-new tail) or it runs
	// after (we observe proc=nil, defer forwarding to the upcoming attach,
	// which will snapshot our just-appended entries).
	//
	// proc.InjectHistory itself is invoked AFTER releasing historyMu — it
	// takes proc.eventLog.mu and we never want two long locks held at once.
	// R191-GO-M1's "reload proc after unlock" concern is no longer relevant:
	// a fresh proc replacing the current one happens through attach helpers
	// that share historyMu, so the in-lock loadProcess() is the authoritative
	// snapshot for this caller.
	//
	// Stale proc note (R231-CQ-7): if the proc captured here was already
	// orphaned by a concurrent storeProcess(nil) during ResetChat / Remove,
	// proc.InjectHistory below still mutates that orphan's EventLog ring,
	// but no one calls EventEntries() on an orphan — Router.loadProcess()
	// returns the new pointer and dashboards/cron snapshot through that.
	// The orphan ring is GC'd when the last reference (this closure)
	// drops, so the extra append is a harmless no-op rather than a leak.
	s.historyMu.Lock()
	// Monotonicity check (R237-PERF-12): when persistedHistory is empty
	// or already known sorted AND the appended batch is internally sorted
	// w.r.t. the existing tail, the flag stays/becomes true and
	// dead-session readers can skip the per-call stable sort. Out-of-order
	// entries leave the flag false, falling back to the lazy sort-on-read
	// path that EventEntriesSince still implements. Steady-state
	// Send/result append is monotonic by construction (Append/AppendBatch
	// stamp now), so the common path costs only this O(batch) scan.
	if s.persistedHistorySorted || len(s.persistedHistory) == 0 {
		monotonic := true
		var prevTime int64
		if n := len(s.persistedHistory); n > 0 {
			prevTime = s.persistedHistory[n-1].Time
		}
		for _, e := range entries {
			if e.Time < prevTime {
				monotonic = false
				break
			}
			prevTime = e.Time
		}
		if monotonic {
			s.persistedHistorySorted = true
		} else {
			s.persistedHistorySorted = false
		}
	}
	// R20260603150052-PERF-5: count user entries in the incoming batch before
	// appending so we can maintain persistedUserTurns incrementally (O(batch))
	// instead of rescanning the full history slice (O(N)) on every inject.
	var newUserEntries int64
	for i := range entries {
		if entries[i].Type == "user" {
			newUserEntries++
		}
	}
	s.persistedHistory = append(s.persistedHistory, entries...)
	var trimmedUserEntries int64
	if trimmed := len(s.persistedHistory) - maxPersistedHistory; trimmed > 0 {
		// Count user entries in the trimmed prefix before reslicing so the
		// incremental counter stays accurate. The prefix is still addressable
		// on the pre-trim backing array at this point.
		for i := 0; i < trimmed; i++ {
			if s.persistedHistory[i].Type == "user" {
				trimmedUserEntries++
			}
		}
		s.persistedHistory = s.persistedHistory[trimmed:]
		// Cap-trim shifts the prefix backwards; clamp seededLen so it keeps
		// pointing at "tail-end of what proc has already seen" rather than
		// past the new start.
		//
		// R231-CQ-9 — degrade-to-reseed semantic: when trimmed > seededLen
		// the clamp lands on 0, which means the next forward span below will
		// re-emit the entire post-trim ring (including some entries the proc
		// has already observed). This is intentional: after a cap-trim the
		// "exact already-seen prefix" is no longer recoverable (its leading
		// entries were dropped), so we choose duplicate forwarding over data
		// loss. The duplication only fires when the injected batch by itself
		// exceeds maxPersistedHistory minus what proc already saw — i.e.
		// boot-time JSONL replay of >cap entries; steady-state Send/result
		// flow stays well under the cap and preserves the no-duplicate
		// guarantee.
		if s.persistedSeededLen >= trimmed {
			s.persistedSeededLen -= trimmed
		} else {
			s.persistedSeededLen = 0
		}
	}
	proc := s.loadProcess()
	// R237-PERF-6 (#667): capture only the bounds of the forward window
	// under historyMu; defer the make+copy to AFTER Unlock so concurrent
	// EventEntries / EventEntriesSince RLockers do not stall on a
	// 500-entry replay's allocation+memcpy. Safety: subsequent
	// InjectHistory calls cannot mutate slots the captured `tail` slice
	// points at — append only writes past the current len, and cap-trim
	// merely reslices the header. A reallocating append leaves the old
	// backing array referenced by `tail` alive for GC; element data at
	// [seededLen..end) is never overwritten in place anywhere in the
	// codebase (verified by Grep on persistedHistory[ writes — only
	// `s.persistedHistory = …` reslices the header). seededLen is
	// committed under the lock so no second InjectHistory can re-forward
	// the same entries.
	var tail []cli.EventEntry
	if proc != nil && s.persistedSeededLen < len(s.persistedHistory) {
		tail = s.persistedHistory[s.persistedSeededLen:]
		s.persistedSeededLen = len(s.persistedHistory)
	}
	// R040034-PERF-6 (#1405): proc==nil branch is the only path that ever
	// reads persistedHistory directly (live procs serve their own EventLog
	// ring), so when proc is absent we can sort eagerly under the write
	// lock we already hold instead of deferring to the first
	// EventEntriesSince/Before reader. The deferred path took
	// historyMu.Lock on the WS push hot path and blocked concurrent
	// RLockers for an O(n log n) sort over up to 500 entries; absorbing
	// the cost here keeps reader fast-path RLock-only forever after.
	//
	// Gated on proc==nil to preserve the seededLen contract: when proc is
	// attached, persistedSeededLen indexes into a stable [0..n) prefix
	// that proc has already been seeded with — permuting persistedHistory
	// in place would invalidate that pointer and cause duplicate or
	// dropped tail forwarding (see attachProcessAndSnapshotPersisted /
	// adoptProcessAlreadySeeded). With proc==nil, persistedSeededLen has
	// either been reset to 0 (attach(nil) path) or will be on the next
	// attach via attachProcessAndSnapshotPersisted's snapshot-rebuild, so
	// reordering is safe.
	//
	// Sort lands on a fresh backing array, NOT in place: a prior
	// InjectHistory call may have captured a `tail` slice into the
	// existing backing array and released historyMu before its deferred
	// make+copy ran (R237-PERF-6). Permuting the shared backing array in
	// place would race that copy. Allocating fresh leaves the old array
	// alive for GC — its contents stay readable and unchanged for any
	// outstanding tail reader.
	if proc == nil && !s.persistedHistorySorted && len(s.persistedHistory) > 1 {
		sorted := make([]cli.EventEntry, len(s.persistedHistory))
		copy(sorted, s.persistedHistory)
		sortEntriesByTimeStable(sorted)
		s.persistedHistory = sorted
		s.persistedHistorySorted = true
	}
	// #1644 / R20260603150052-PERF-5: maintain persistedUserTurns incrementally
	// (O(batch)) rather than rescanning the full slice (O(N)). newUserEntries
	// and trimmedUserEntries were computed above from the incoming batch and the
	// trimmed prefix respectively; the sort branch does not add or remove
	// entries so no adjustment is needed there.
	s.persistedUserTurns.Add(newUserEntries - trimmedUserEntries)
	s.historyMu.Unlock()

	if len(tail) > 0 {
		// Defensive copy outside historyMu: proc.InjectHistory consumes
		// the slice and may outlive this call, while the caller's
		// entries slice and `tail`'s backing array are owned by us — a
		// fresh allocation severs both ties cleanly.
		forward := make([]cli.EventEntry, len(tail))
		copy(forward, tail)
		proc.InjectHistory(forward)
	}

	// Update cached snapshot values only if not yet set by Send. Each Store
	// is atomic so no lock is needed; the "only set if empty" check is a
	// benign TOCTOU — a concurrent Send writing the same field races, but
	// both values are "most recent" views and whichever lands is acceptable.
	if prompt != "" && loadAtomicString(&s.lastPrompt) == "" {
		storeAtomicString(&s.lastPrompt, prompt)
	}
	if activity != "" && loadAtomicString(&s.lastActivity) == "" {
		storeAtomicString(&s.lastActivity, activity)
	}
	// R110-P1: seed lastResponse alongside lastPrompt/lastActivity. Same
	// "only set if empty" guard so a concurrent live Send that already
	// stamped a fresher response doesn't get clobbered by historical replay.
	if response != "" && loadAtomicString(&s.lastResponse) == "" {
		storeAtomicString(&s.lastResponse, response)
	}
}
