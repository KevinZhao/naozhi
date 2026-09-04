package session

import (
	"log/slog"
	"slices"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/metrics"
)

// SnapshotChainIDs returns the session-ID chain (oldest → newest). The current
// session ID is appended only when non-empty, matching how the router builds
// the chain for JSONL loads. Callers must not mutate the returned slice.
//
// Lock contract: writers of prevSessionIDs hold r.mu; readers either hold r.mu
// or accept a stale-but-not-torn snapshot. This reader runs from
// cli.Wrapper.NewHistorySource factories which do NOT hold r.mu, so
// historyMu.RLock here does not synchronise with r.mu writers — the
// slices.Clone-then-assign pattern in writers guarantees any observed value is
// a complete prior snapshot. historyMu still serialises against InjectHistory.
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
// Older entries keep their value, defaulting to "manual". origin is one of
// "manual" / "auto-spawn" / "auto-backfill" / "resume"; empty is a no-op.
//
// Invariant: prev_session_ids is append-only — every write path only grows the
// slice or replaces it wholesale. Drift (origins longer than ids, or a
// negative tail position) rebuilds origins to all-"manual" rather than letting
// a misaligned label persist, and is metric-counted.
func (s *ManagedSession) SetPrevSessionOrigins(ids []string, origin string) {
	if origin == "" || len(ids) == 0 {
		return
	}
	s.historyMu.Lock()
	defer s.historyMu.Unlock()

	// Drift detection: start < 0 means the batch was not appended to the tail;
	// origins longer than IDs means a past write left dangling labels. Both
	// rebuild the parallel slice with "manual" defaults so origin↔id never
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
		// A negative start is meaningless against this chain — bail.
		if start < 0 {
			return
		}
	}

	// Grow origins to match the chain length, defaulting the untracked prefix
	// to "manual".
	if len(s.prevSessionOrigins) < len(s.prevSessionIDs) {
		grown := make([]string, len(s.prevSessionIDs))
		copy(grown, s.prevSessionOrigins)
		for i := len(s.prevSessionOrigins); i < len(grown); i++ {
			grown[i] = "manual"
		}
		s.prevSessionOrigins = grown
	}

	for i := range ids {
		s.prevSessionOrigins[start+i] = origin
	}
	// Bump the chain generation so the store marshal cache (#2346) can
	// detect this mutation via an O(1) counter compare instead of slices.Equal.
	s.prevHistoryGen.Add(1)
}

// SnapshotPrevSessionOrigins returns a defensive copy of the parallel origins
// slice; callers must not mutate the result. Length is exactly
// len(prevSessionIDs), with unset entries materialised as "manual" so
// consumers can align positionally without nil-checks.
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

// SnapshotPrevSessionIDs returns a defensive copy of the prevSessionIDs chain
// (oldest → newest) for read-only callers in router_*.go. Returns nil when the
// chain is empty (matches SnapshotPrevSessionOrigins shape).
func (s *ManagedSession) SnapshotPrevSessionIDs() []string {
	s.historyMu.RLock()
	defer s.historyMu.RUnlock()
	if len(s.prevSessionIDs) == 0 {
		return nil
	}
	return slices.Clone(s.prevSessionIDs)
}

// ReplacePrevSessionIDs swaps the prevSessionIDs chain wholesale. The supplied
// slice is cloned so the caller can reuse its argument. Origins are NOT
// touched — callers that need both must call ReplacePrevSessionIDs first, then
// SetPrevSessionOrigins, so its drift detector sees the post-replace baseline.
func (s *ManagedSession) ReplacePrevSessionIDs(ids []string) {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	if len(ids) == 0 {
		s.prevSessionIDs = nil
		s.prevHistoryGen.Add(1)
		return
	}
	s.prevSessionIDs = slices.Clone(ids)
	s.prevHistoryGen.Add(1)
}

// RebuildChainFiltered atomically rebuilds prevSessionIDs and
// prevSessionOrigins under a SINGLE historyMu write hold, keeping only the
// indices where keepMask is true, so no reader can observe the two parallel
// slices differing in length. Composing ReplacePrevSessionIDs +
// SetPrevSessionOrigins cannot achieve this: each takes historyMu
// independently and a reader in between would synthesise wrong "manual"
// labels (RFC §9.2 v2.1). keepMask must have len == len(prevSessionIDs); a
// mismatch is a caller bug and is a no-op. Origins shorter than the chain are
// treated as "manual" for surviving entries. Returns the number removed.
func (s *ManagedSession) RebuildChainFiltered(keepMask []bool) int {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()

	n := len(s.prevSessionIDs)
	if len(keepMask) != n {
		// Refuse to touch the chain rather than misalign the parallel slices.
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
		// Leave the slices untouched so a possibly-shorter origins slice that
		// callers tolerate is not perturbed.
		return 0
	}

	if len(newIDs) == 0 {
		s.prevSessionIDs = nil
		s.prevSessionOrigins = nil
		s.prevHistoryGen.Add(1)
		return removed
	}
	s.prevSessionIDs = newIDs
	s.prevSessionOrigins = newOrigins
	s.prevHistoryGen.Add(1)
	return removed
}

// SnapshotPersistedHistory returns a defensive copy of the persistedHistory
// ring, safe to mutate. Returns nil when the ring is empty so callers don't
// pay a zero-length alloc.
func (s *ManagedSession) SnapshotPersistedHistory() []clievent.EventEntry {
	s.historyMu.RLock()
	defer s.historyMu.RUnlock()
	if len(s.persistedHistory) == 0 {
		return nil
	}
	out := make([]clievent.EventEntry, len(s.persistedHistory))
	copy(out, s.persistedHistory)
	return out
}

// persistedHistoryBefore collects up to `limit` entries from persistedHistory
// strictly older than beforeMS, in reverse-walk order (newest first). The
// second return value is true when persistedHistorySorted is set, meaning the
// result is strictly Time-descending and the caller can obtain ascending order
// by a cheap slices.Reverse instead of a full sort. Only relevant when proc is
// nil; live-process sessions go through proc.EventEntriesBefore directly.
func (s *ManagedSession) persistedHistoryBefore(beforeMS int64, limit int) ([]clievent.EventEntry, bool) {
	if limit <= 0 {
		return nil, false
	}
	s.historyMu.RLock()
	defer s.historyMu.RUnlock()
	if len(s.persistedHistory) == 0 {
		return nil, false
	}
	sorted := s.persistedHistorySorted
	// persistedHistory is not guaranteed to be sorted, so a full linear
	// backward walk is the conservative choice.
	out := make([]clievent.EventEntry, 0, limit)
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
	// Order left as-is; the caller decides whether to Reverse or full-sort.
	return out, sorted
}

// InjectHistory pre-populates the event log with entries from an earlier
// conversation, saved to persistedHistory so they survive process restarts.
func (s *ManagedSession) InjectHistory(entries []clievent.EventEntry) {
	s.injectHistory(entries, false)
}

// InjectHistoryIfEmpty atomically injects entries only when persistedHistory is
// currently empty, returning true if the injection happened. The emptiness
// check and the append run under a single historyMu hold so concurrent startup
// loaders (router_core.go Tier1/Tier2) and ReconnectShims cannot both pass a
// separate hasInjectedHistory() check and double-append the same conversation
// (#1812).
func (s *ManagedSession) InjectHistoryIfEmpty(entries []clievent.EventEntry) bool {
	return s.injectHistory(entries, true)
}

// injectHistory is the shared implementation behind InjectHistory /
// InjectHistoryIfEmpty. When onlyIfEmpty is true it returns false without
// mutating state if persistedHistory already holds entries (checked under the
// same lock that performs the append); otherwise it always injects and
// returns true.
func (s *ManagedSession) injectHistory(entries []clievent.EventEntry, onlyIfEmpty bool) bool {
	if len(entries) > maxPersistedHistory {
		slog.Debug("inject history: batch exceeds cap, truncating oldest",
			"key", s.key,
			"batch_len", len(entries),
			"cap", maxPersistedHistory,
			"dropped", len(entries)-maxPersistedHistory)
		entries = entries[len(entries)-maxPersistedHistory:]
	}
	// Scan the batch for summaries outside the lock: it reads only the
	// caller-supplied slice and its only side effects are atomic Stores, so
	// concurrent readers (EventEntries / EventEntriesSince / EventEntriesBefore)
	// proceed during 500-entry JSONL replays at startup.
	prompt, activity, response := scanLastSummaries(entries)

	// Mutate persistedHistory AND read s.process under the same historyMu hold
	// so a concurrent attachProcessAndSnapshotPersisted (also on historyMu)
	// cannot stamp seededLen between our load-process and forward decision.
	// proc.InjectHistory runs AFTER Unlock (it takes proc.eventLog.mu; never
	// hold two long locks); an append onto an orphaned proc's ring is harmless.
	s.historyMu.Lock()
	// Atomic check-then-act (#1812): bail under the lock if another loader
	// already populated persistedHistory, so two startup goroutines that both
	// observed an empty session cannot both append.
	if onlyIfEmpty && len(s.persistedHistory) > 0 {
		s.historyMu.Unlock()
		return false
	}
	// Monotonicity check: when persistedHistory is empty or known sorted AND
	// the batch is sorted w.r.t. the existing tail, the flag stays/becomes true
	// and dead-session readers skip the per-call stable sort; out-of-order
	// entries leave it false (lazy sort-on-read). Common path: one O(batch) scan.
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
	// persistedUserTurns is maintained incrementally (#1644): oldCount +
	// usersInBatch - usersInTrimmedPrefix, O(batch+trimmed) rather than an O(n)
	// rescan. The proc==nil sort below only permutes order and leaves the total
	// unchanged. Equivalence with recountPersistedUserTurnsLocked is asserted in
	// persisted_user_turns_incremental_test.go.
	userTurns := s.persistedUserTurns.Load()
	for i := range entries {
		if entries[i].Type == "user" {
			userTurns++
		}
	}
	s.persistedHistory = append(s.persistedHistory, entries...)
	if trimmed := len(s.persistedHistory) - maxPersistedHistory; trimmed > 0 {
		for i := 0; i < trimmed; i++ {
			if s.persistedHistory[i].Type == "user" {
				userTurns--
			}
		}
		s.persistedHistory = s.persistedHistory[trimmed:]
		// Cap-trim shifts the prefix; clamp seededLen so it still points at
		// "tail-end of what proc has already seen". When trimmed > seededLen the
		// clamp lands on 0 and the forward span below re-emits the whole
		// post-trim ring: the exact already-seen prefix is unrecoverable, so we
		// choose duplicate forwarding over data loss (boot-time >cap replay only).
		if s.persistedSeededLen >= trimmed {
			s.persistedSeededLen -= trimmed
		} else {
			s.persistedSeededLen = 0
		}
	}
	proc := s.loadProcess()
	// Capture only the bounds of the forward window under historyMu; the
	// make+copy happens AFTER Unlock so RLockers don't stall on a 500-entry
	// memcpy (#667). Safe because element data at [seededLen..end) is never
	// overwritten in place — append writes past len, cap-trim only reslices the
	// header. seededLen is committed under the lock so no second call re-forwards.
	var tail []clievent.EventEntry
	if proc != nil && s.persistedSeededLen < len(s.persistedHistory) {
		tail = s.persistedHistory[s.persistedSeededLen:]
		s.persistedSeededLen = len(s.persistedHistory)
	}
	// proc==nil is the only path that reads persistedHistory directly, so sort
	// eagerly under the lock already held rather than on the WS-push read path
	// (#1405). Gated on proc==nil: persistedSeededLen indexes a stable prefix
	// proc was seeded with, so permuting in place would break tail forwarding.
	// FRESH array: a prior call's `tail` may still be copying after Unlock.
	if proc == nil && !s.persistedHistorySorted && len(s.persistedHistory) > 1 {
		sorted := make([]clievent.EventEntry, len(s.persistedHistory))
		copy(sorted, s.persistedHistory)
		sortEntriesByTimeStable(sorted)
		s.persistedHistory = sorted
		s.persistedHistorySorted = true
	}
	// Commit the user-turn count under historyMu after every persistedHistory
	// mutation so it stays consistent with the slice dead-session readers see
	// (feeds AutoTitler's min-turn gate, #1644).
	s.persistedUserTurns.Store(userTurns)
	s.historyMu.Unlock()

	if len(tail) > 0 {
		// Defensive copy outside historyMu: proc.InjectHistory consumes the
		// slice and may outlive this call; a fresh allocation severs ties to
		// both the caller's entries and `tail`'s backing array.
		forward := make([]clievent.EventEntry, len(tail))
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
	// Same guard: a fresher response stamped by a live Send wins over replay.
	if response != "" && loadAtomicString(&s.lastResponse) == "" {
		storeAtomicString(&s.lastResponse, response)
	}
	return true
}
