package session

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/textutil"
)

// getSessionID returns the session ID lock-free via atomic.Pointer[string].
//
// Distinct from cli.Process.GetSessionID, which reads the CLI subprocess's
// most-recently-observed ID off the live event stream; the two may briefly
// disagree during a /resume handshake or first-Send capture. Pick by
// intent: "what naozhi remembers for this chat key" → this / SessionID;
// "what the CLI thinks is active right now" → Process.GetSessionID.
func (s *ManagedSession) getSessionID() string {
	return loadAtomicString(&s.sessionID)
}

// SessionID returns the current CLI session ID, lock-free. Public alias for
// getSessionID satisfying cli.HistorySessionView and cross-package callers
// that need the ID without taking r.mu.
func (s *ManagedSession) SessionID() string { return s.getSessionID() }

// setSessionID stores the session ID atomically.
func (s *ManagedSession) setSessionID(id string) {
	storeAtomicString(&s.sessionID, id)
}

// parseKeyParts lazily parses the immutable session key into cached components.
// Hand-rolled split avoids the []string allocation of strings.SplitN on the
// dashboard poll hot path.
func (s *ManagedSession) parseKeyParts() {
	s.keyOnce.Do(func() {
		k := s.key
		idx := strings.IndexByte(k, ':')
		if idx < 0 {
			s.keyPlatform = k
			return
		}
		s.keyPlatform = k[:idx]
		k = k[idx+1:]
		idx = strings.IndexByte(k, ':')
		if idx < 0 {
			s.keyChatType = k
			return
		}
		s.keyChatType = k[:idx]
		k = k[idx+1:]
		idx = strings.IndexByte(k, ':')
		if idx < 0 {
			s.keyChatID = k
			return
		}
		s.keyChatID = k[:idx]
		s.keyAgentID = k[idx+1:]
	})
}

// HasProcess reports whether a process is currently attached to this
// session, regardless of liveness: true even for an exited process not yet
// detached by readLoop cleanup. Use isAlive() or State() for liveness.
func (s *ManagedSession) HasProcess() bool {
	return s.loadProcess() != nil
}

// State returns just the live process state ("ready" / "busy" / etc.)
// without the SetModel mirror or a full SessionSnapshot — lock-free hot
// path for high-frequency observers. Returns "ready" when no process is
// attached, mirroring Snapshot's no-proc branch.
func (s *ManagedSession) State() string {
	proc := s.loadProcess()
	if proc == nil {
		return "ready"
	}
	return proc.State().String()
}

// DeathReason returns the recorded death cause string ("" when the
// session is healthy or has not died yet).
func (s *ManagedSession) DeathReason() string {
	return loadAtomicString(&s.deathReason)
}

// Snapshot returns a point-in-time view of this session.
//
// Side effect: a live process Model() that differs from the persisted
// s.model is mirrored back via SetModel so the dashboard chip tracks what
// the CLI actually uses. Use snapshotReadOnly for a pure read.
//
// Performance contract (#411): Snapshot MUST NOT copy persistedHistory or
// any other O(N) structure — dashboards poll at 1Hz × N tabs × M sessions.
// Scalar fields are atomic caches so the call is O(1); the event log is
// reached via EventEntries / EventLastN / EventEntriesSince.
func (s *ManagedSession) Snapshot() SessionSnapshot {
	return s.snapshot(true)
}

// snapshotReadOnly is Snapshot without the SetModel mirror: snap.Model is
// still resolved from the live process (falling back to the persisted
// value) but nothing is written. VisitSessions runs under r.mu.RLock for
// every live session, and a write on that read path is both unnecessary
// and harder to reason about (#1577). The dashboard poll path keeps the
// mirroring Snapshot() so the live model still lands in sessions.json.
func (s *ManagedSession) snapshotReadOnly() SessionSnapshot {
	return s.snapshot(false)
}

// snapshot is the shared core for Snapshot / snapshotReadOnly; mirrorModel
// gates the one intentional read-side write. snap.Model resolution is
// identical in both modes.
func (s *ManagedSession) snapshot(mirrorModel bool) SessionSnapshot {
	s.parseKeyParts()
	// One atomic Load for backend/cliName/cliVersion instead of three.
	id := s.loadCLIIdentity()
	snap := SessionSnapshot{
		Key:           s.key,
		Platform:      s.keyPlatform,
		ChatType:      s.keyChatType,
		ChatID:        s.keyChatID,
		Agent:         s.keyAgentID,
		SessionID:     s.getSessionID(),
		LastActive:    s.LastActive().UnixMilli(),
		CreatedAt:     s.createdAtMillis(),
		Workspace:     s.Workspace(),
		Backend:       id.backend,
		AccessProfile: id.accessProfile,
		CLIName:       id.cliName,
		CLIVersion:    id.cliVersion,
		UserLabel:     s.UserLabel(),
		LabelOrigin:   s.LabelOrigin(),
		// Seed from the persisted value; the proc branch below overwrites
		// with a fresher live value. No-proc snapshots (evicted / pre-spawn)
		// keep it so the dashboard doesn't blink to "(模型未配置)".
		Model: s.Model(),
	}
	snap.DeathReason = loadAtomicString(&s.deathReason)

	proc := s.loadProcess()
	sessCost := loadTotalCost(&s.totalCost)
	// Credit-unit sum of snap.MeteringUsage, computed once per metering
	// generation alongside the cached rows (#2345).
	var meteringCredits float64
	// costSpent (sum of per-turn deltas, monotonic across resume/restart) is
	// the authoritative total. spent==0 also covers legacy stores without
	// cost_spent and the pre-first-turn window after resume, so fall back to
	// sessCost there; a zero-spend session's sessCost is ~0 too, so the
	// ambiguity is benign. See runhistory.TurnCostDelta and finishRun.
	spent := loadTotalCost(&s.costSpent)
	if spent <= 0 {
		spent = sessCost
	}
	if proc == nil {
		snap.TotalCost = spent
		snap.State = "ready"
		// No proc to report UserTurnCount: count persisted "user" entries
		// so AutoTitler's minUserTurns gate still sees idle-evicted
		// sessions (#1644).
		snap.MessageCount = s.persistedUserTurns.Load()
	} else {
		snap.State = proc.State().String()
		snap.Protocol = proc.ProtocolName()
		// Model priority: live proc.Model() over persisted s.Model(). A
		// differing live value is mirrored back so the next saveStore
		// captures it; empty live keeps persisted. Compare before storing:
		// storeAtomicString always swaps the pointer, and an unconditional
		// store per 1Hz poll dirties the cache line for nothing (#534).
		liveModel := proc.Model()
		if liveModel != "" {
			if mirrorModel {
				if cached := s.Model(); cached != liveModel {
					s.SetModel(liveModel)
				}
			}
			snap.Model = liveModel
		} else {
			snap.Model = s.Model()
		}
		// CLI version: live proc.LiveVersion() (system/init frame, the
		// binary THIS process exec'd) over persisted id.cliVersion (detected
		// once at naozhi startup, so stale after a host upgrade; also the
		// only source for ACP backends). Same mirror discipline as model.
		if liveVersion := proc.LiveVersion(); liveVersion != "" {
			if mirrorModel {
				if id.cliVersion != liveVersion {
					s.SetCLIVersion(liveVersion)
				}
			}
			snap.CLIVersion = liveVersion
		}
		// Use the monotonic costSpent, not proc.TotalCost(): the CLI's
		// per-incarnation running total RESETS on resume, so it would freeze
		// the session total at the pre-resume value until it climbed back.
		snap.TotalCost = spent
		snap.Subagents = proc.TurnAgents()
		// EventLog-maintained summaries (updated lock-free on every event).
		snap.LastActivity = proc.LastActivitySummary()
		// Empty until a text block has streamed; the s.lastResponse fallback
		// below covers the post-restart / pre-replay case.
		snap.LastResponse = proc.LastResponseSummary()
		// User turns observed by the current Process since its spawn; the
		// dashboard gates the chip on `> 0`.
		snap.MessageCount = proc.UserTurnCount()

		// Normalize layer (docs/rfc/multi-backend.md §8.8): getters return
		// zero for fields the backend never reports so `> 0` gating in
		// dashboard.js works for both claude and kiro.
		snap.ContextUsagePercent = proc.ContextUsagePercent()
		snap.TurnDurationMs = proc.TurnDurationMs()
		snap.MeteringUsage, meteringCredits = s.meteringView(proc)
		// Effort is a runtime observation, never persisted to sessions.json,
		// so unlike Model there is nothing to seed for an evicted session and
		// the dashboard tag hides (docs/rfc/kiro-effort-visibility.md §9).
		// EffortTier backends pre-seed it from the spawn pin
		// (cli.Process.seedEffort); kiro's metadata report overwrites.
		snap.Effort = proc.Effort()
		if diags := proc.SpawnDiags(); len(diags) > 0 {
			snap.SpawnDiags = diags
		}
	}
	if snap.SpawnDiags == nil {
		// Contract: spawn_diags is always an array in /api/sessions.
		snap.SpawnDiags = []cli.SpawnDiag{}
	}
	if d := s.overlayDrift.Load(); d != nil {
		snap.OverlayDrift = *d
	}
	if snap.OverlayDrift == nil {
		// Same always-an-array contract as spawn_diags.
		snap.OverlayDrift = []OverlayFieldDrift{}
	}

	// Derived from backend even when proc is nil so an evicted session still
	// renders the right cost label; claude is the default for legacy stores.
	snap.CostUnit = costUnitForBackend(snap.Backend)

	// For credit-unit backends (kiro) the header shows the SESSION-level
	// credit sum from the metering cache (#2345); claude keeps the USD total.
	// Only override when a credit-typed entry exists so a non-credit unit
	// under cost_unit=credits doesn't silently zero the running total.
	if snap.CostUnit == "credits" && meteringCredits > 0 {
		snap.TotalCost = meteringCredits
	}

	// Read cached values instead of copying the full event log.
	if lp := loadAtomicString(&s.lastPrompt); lp != "" {
		snap.LastPrompt = lp
	}
	if snap.LastActivity == "" {
		if la := loadAtomicString(&s.lastActivity); la != "" {
			snap.LastActivity = la
		}
	}
	// Live wins, cache survives restart; both empty leaves the field unset
	// so JSON omitempty hides the dim line on brand-new sessions.
	if snap.LastResponse == "" {
		if lr := loadAtomicString(&s.lastResponse); lr != "" {
			snap.LastResponse = lr
		}
	}

	return snap
}

// hasInjectedHistory reports whether persistedHistory contains any entries,
// letting the startup history loader skip the JSONL backfill when
// ReconnectShims already injected history. Read-only, no copy.
func (s *ManagedSession) hasInjectedHistory() bool {
	s.historyMu.RLock()
	defer s.historyMu.RUnlock()
	return len(s.persistedHistory) > 0
}

// recountPersistedUserTurnsLocked recomputes persistedUserTurns from the
// current persistedHistory slice. Caller MUST hold s.historyMu (read or
// write — the scan only reads the slice, the result is stored atomically)
// so the proc==nil snapshot branch can read the count lock-free (#1644).
func (s *ManagedSession) recountPersistedUserTurnsLocked() {
	s.persistedUserTurns.Store(countUserTurns(s.persistedHistory))
}

// EventEntries returns the event log entries for this session.
// Returns persisted history when the process is nil or dead.
func (s *ManagedSession) EventEntries() []clievent.EventEntry {
	proc := s.loadProcess()
	if proc != nil {
		return proc.EventEntries()
	}
	s.historyMu.RLock()
	out := make([]clievent.EventEntry, len(s.persistedHistory))
	copy(out, s.persistedHistory)
	s.historyMu.RUnlock()
	return out
}

// EventEntriesAppend is the buffer-reusing variant of EventEntries: it appends
// this session's full event log onto dst and returns the grown slice, so
// callers iterating O(N) dead sessions can reuse one pooled buffer (#1885).
//
// Ownership mirrors EventEntriesSinceAppend: the caller must not retain dst
// across calls; the returned slice shares the backing array with dst. dst's
// prefix is preserved in every branch.
func (s *ManagedSession) EventEntriesAppend(dst []clievent.EventEntry) []clievent.EventEntry {
	proc := s.loadProcess()
	if proc != nil {
		return append(dst, proc.EventEntries()...)
	}
	s.historyMu.RLock()
	dst = append(dst, s.persistedHistory...)
	s.historyMu.RUnlock()
	return dst
}

// SubagentLinker returns the SubagentLinker owned by the live *cli.Process,
// or nil when the session is not backed by one (fake test process, dead
// process, ACP protocol). Callers must guard the nil return.
//
// Intentionally type-asserts rather than widening processIface so fake test
// processes need not implement the Linker surface; the agentlink.AgentLinker
// interface widens only at the server boundary. TODO: AgentIntrospector
// interface when a second backend needs agent-view support (docs/TODO.md).
func (s *ManagedSession) SubagentLinker() *cli.SubagentLinker {
	if real := s.loadCliProcess(); real != nil {
		return real.Linker()
	}
	return nil
}

// AgentEventLog exposes the live *cli.EventLog so the server-side tailer
// registry can install its task_done hook. nil for fake processes / dead
// sessions, same policy as SubagentLinker above.
func (s *ManagedSession) AgentEventLog() *cli.EventLog {
	if real := s.loadCliProcess(); real != nil {
		return real.EventLog()
	}
	return nil
}

// loadCliProcess returns the live *cli.Process when the session is backed by
// one, nil otherwise (fake test process, dead session, ACP protocol, etc.).
func (s *ManagedSession) loadCliProcess() *cli.Process {
	proc := s.loadProcess()
	if proc == nil {
		return nil
	}
	real, ok := proc.(*cli.Process)
	if !ok {
		return nil
	}
	return real
}

// EventLastN returns the most recent n event entries.
func (s *ManagedSession) EventLastN(n int) []clievent.EventEntry {
	proc := s.loadProcess()
	if proc != nil {
		return proc.EventLastN(n)
	}
	s.historyMu.RLock()
	defer s.historyMu.RUnlock()
	if n <= 0 || n >= len(s.persistedHistory) {
		out := make([]clievent.EventEntry, len(s.persistedHistory))
		copy(out, s.persistedHistory)
		return out
	}
	start := len(s.persistedHistory) - n
	out := make([]clievent.EventEntry, n)
	copy(out, s.persistedHistory[start:])
	return out
}

// sortEntriesByTimeStable sorts entries in-place by Time ascending. Stable so
// entries sharing a Time keep insertion order (InjectHistory batches may
// collapse a whole chain replay to one timestamp). persistedHistory is not
// ordered by construction: InjectHistory may interleave segments from
// multiple session chains, and AppendBatch stamps zero-Time entries with a
// single wall-clock while resume paths deliver real earlier timestamps.
func sortEntriesByTimeStable(entries []clievent.EventEntry) {
	if len(entries) < 2 {
		return
	}
	slices.SortStableFunc(entries, func(a, b clievent.EventEntry) int {
		return cmp.Compare(a.Time, b.Time)
	})
}

// EventEntriesSince returns the event log entries with Time > afterMS in
// chronological order.
//
// Live branch: cli.EventLog's ring is weakly Time-monotonic by construction
// (Append stamps zero-Time entries with now; AppendBatch uses one now), so
// no re-sort on this WS push hot path. Dead branch: persistedHistory is
// sorted lazily under historyMu if the sorted flag is unset.
func (s *ManagedSession) EventEntriesSince(afterMS int64) []clievent.EventEntry {
	proc := s.loadProcess()
	if proc != nil {
		return proc.EventEntriesSince(afterMS)
	}
	// InjectHistory sorts eagerly under the write lock (#1405), so the
	// promote-sort-downgrade fallback below only fires for fixtures that
	// assign persistedHistory directly; steady-state reads stay RLock-only.
	s.historyMu.RLock()
	// Fast path: when sorted, the last entry holds the max Time, so an idle
	// poll (afterMS = last seen) skips the linear scan entirely.
	if n := len(s.persistedHistory); n == 0 || (s.persistedHistorySorted && s.persistedHistory[n-1].Time <= afterMS) {
		s.historyMu.RUnlock()
		return nil
	}
	if !s.persistedHistorySorted {
		s.historyMu.RUnlock()
		s.historyMu.Lock()
		// Re-check under the write lock — another reader may have already
		// sorted between the unlock and re-acquire.
		if !s.persistedHistorySorted {
			sortEntriesByTimeStable(s.persistedHistory)
			s.persistedHistorySorted = true
		}
		s.historyMu.Unlock()
		s.historyMu.RLock()
		// Same fast exit now that we're sorted (the sort rearranged, never
		// added, entries).
		if n := len(s.persistedHistory); n == 0 || s.persistedHistory[n-1].Time <= afterMS {
			s.historyMu.RUnlock()
			return nil
		}
	}
	// Small cap: the steady-state poll matches only the last few entries,
	// so presizing to len(persistedHistory) would over-allocate per poll.
	out := make([]clievent.EventEntry, 0, 16)
	for _, e := range s.persistedHistory {
		if e.Time > afterMS {
			out = append(out, e)
		}
	}
	s.historyMu.RUnlock()
	return out
}

// EventEntriesSinceAppend is the buffer-reusing variant of EventEntriesSince
// for both the live-process and dead-session (persistedHistory) paths, so
// 1Hz WS pollers can append the common 0-5 new entries into existing
// capacity (#1740). Ownership: the caller must not retain dst across calls;
// the returned slice shares its backing array with dst.
func (s *ManagedSession) EventEntriesSinceAppend(dst []clievent.EventEntry, afterMS int64) []clievent.EventEntry {
	proc := s.loadProcess()
	if proc != nil {
		// Empty dst is the hot path: forward straight into the append-mode
		// query so the EventLog reuses dst's backing capacity.
		if len(dst) == 0 {
			return proc.EventEntriesSinceAppend(dst, afterMS)
		}
		// Non-empty dst: EventLog.EntriesSinceAppend re-slices its argument
		// to [:0], so passing dst would OVERWRITE the prefix. dst[len(dst):]
		// writes into spare capacity past the prefix instead; the final
		// append is then an in-place no-op (capacity sufficed) or a grow
		// that folds in the fresh slice. Prefix preserved either way (#1922).
		appended := proc.EventEntriesSinceAppend(dst[len(dst):], afterMS)
		return append(dst, appended...)
	}
	s.historyMu.RLock()
	if n := len(s.persistedHistory); n == 0 || (s.persistedHistorySorted && s.persistedHistory[n-1].Time <= afterMS) {
		s.historyMu.RUnlock()
		if len(dst) == 0 {
			return nil
		}
		return dst[:0]
	}
	if !s.persistedHistorySorted {
		s.historyMu.RUnlock()
		s.historyMu.Lock()
		if !s.persistedHistorySorted {
			sortEntriesByTimeStable(s.persistedHistory)
			s.persistedHistorySorted = true
		}
		s.historyMu.Unlock()
		s.historyMu.RLock()
		if n := len(s.persistedHistory); n == 0 || s.persistedHistory[n-1].Time <= afterMS {
			s.historyMu.RUnlock()
			if len(dst) == 0 {
				return nil
			}
			return dst[:0]
		}
	}
	// Here persistedHistory is sorted, non-empty, and its last element is
	// strictly > afterMS, so binary-search the first Time > afterMS and
	// bulk-append the tail.
	i, _ := slices.BinarySearchFunc(s.persistedHistory, afterMS, func(e clievent.EventEntry, t int64) int {
		if e.Time <= t {
			return -1
		}
		return 1
	})
	dst = append(dst, s.persistedHistory[i:]...)
	s.historyMu.RUnlock()
	return dst
}

// EventEntriesBefore returns up to `limit` entries with Time < beforeMS
// from the in-memory log (live ring or persistedHistory), chronological.
// Memory-tier only: use EventEntriesBeforeCtx for the disk fallback.
// beforeMS <= 0 means "no upper bound" (tail of the log, like EventLastN);
// limit <= 0 returns nil.
func (s *ManagedSession) EventEntriesBefore(beforeMS int64, limit int) []clievent.EventEntry {
	if limit <= 0 {
		return nil
	}
	proc := s.loadProcess()
	if proc != nil {
		return proc.EventEntriesBefore(beforeMS, limit)
	}
	out, descSorted := s.persistedHistoryBefore(beforeMS, limit)
	if descSorted {
		// Backward walk over ascending input yields descending output; an
		// O(n) reverse beats the O(n log n) stable sort.
		slices.Reverse(out)
	} else {
		sortEntriesByTimeStable(out)
	}
	return out
}

// EventEntriesBeforeCtx extends EventEntriesBefore with a disk-tier
// fallback: when memory has no entries strictly older than beforeMS, the
// session's history.Source is consulted.
//
// The two tiers are never merged: memory is authoritative for any range it
// covers (it includes naozhi-synthesized events like LogSystemEvent that
// never reach disk), and falling through only when memory is empty keeps
// the result chronological without dedup. Cost: one extra round trip on
// the page that straddles the memory bottom.
func (s *ManagedSession) EventEntriesBeforeCtx(ctx context.Context, beforeMS int64, limit int) []clievent.EventEntry {
	if limit <= 0 {
		return nil
	}
	if mem := s.EventEntriesBefore(beforeMS, limit); len(mem) > 0 {
		return mem
	}
	src := s.loadHistorySource()
	if src == nil {
		return nil
	}
	entries, err := src.LoadBefore(ctx, beforeMS, limit)
	if err != nil {
		// Treat as end-of-history, matching the JSONL load sites in router.go.
		slog.Warn("history source load failed", "key", s.key, "err", err)
		return nil
	}
	sortEntriesByTimeStable(entries)
	return entries
}

// countVisibleEntries returns how many entries the dashboard would render as
// chat bubbles (the inverse of the INTERNAL_EVENT_TYPES filter).
func countVisibleEntries(entries []clievent.EventEntry) int {
	n := 0
	for i := range entries {
		if cli.IsVisibleEntry(entries[i]) {
			n++
		}
	}
	return n
}

// EventLastNVisibleCtx is the dashboard's initial-history entry point: the
// history tail carrying enough VISIBLE entries (chat bubbles) that the
// initial render never degrades to the blank "该会话最近仅有 agent 活动"
// placeholder when internal events flood the trailing window.
//
// Memory tier first (contiguous, so the dashboard can rebuild turnState),
// then disk pages strictly older than the earliest in-memory Time are
// prepended until visibleTarget or a page/total/byte ceiling is reached;
// tiers never overlap. visibleTarget <= 0 falls back to EventLastN(maxTotal).
// ctx bounds disk I/O so a slow filesystem can't stall the WS first frame.
func (s *ManagedSession) EventLastNVisibleCtx(ctx context.Context, visibleTarget, maxTotal int) []clievent.EventEntry {
	return s.eventLastNVisibleCtx(ctx, visibleTarget, maxTotal)
}

// EventInitialPageCtx returns the dashboard's initial-history slice plus a
// hasMore flag: whether any entry strictly older than the slice exists (ring
// or disk). Decided server-side because the server truncates by visible
// bubble count, which a client total-count heuristic cannot see. The probe
// is one limit=1 reverse lookup anchored at the earliest returned entry via
// EventEntriesBeforeCtx (so it sees disk even when the ring is short). An
// empty slice reports hasMore=false. ctx bounds both the read and the probe.
func (s *ManagedSession) EventInitialPageCtx(ctx context.Context, visibleTarget, maxTotal int) ([]clievent.EventEntry, bool) {
	entries := s.eventLastNVisibleCtx(ctx, visibleTarget, maxTotal)
	if len(entries) == 0 {
		return entries, false
	}
	oldest := entries[0].Time
	if older := s.EventEntriesBeforeCtx(ctx, oldest, 1); len(older) > 0 {
		return entries, true
	}
	// EventEntriesBeforeCtx swallows ctx cancellation as nil, and the walk
	// above shares the ctx budget, so a starved probe looks like end-of-
	// history. Fail OPEN: a "load earlier" button on exhausted history is a
	// benign no-op, a wrongly hidden one is unrecoverable. Only a clean
	// (non-cancelled) empty probe means "no more".
	if ctx.Err() != nil {
		return entries, true
	}
	return entries, false
}

func (s *ManagedSession) eventLastNVisibleCtx(ctx context.Context, visibleTarget, maxTotal int) []clievent.EventEntry {
	if maxTotal <= 0 {
		maxTotal = maxVisibleTotal
	}
	// Memory tier: contiguous tail carrying up to visibleTarget visible entries.
	var mem []clievent.EventEntry
	if proc := s.loadProcess(); proc != nil {
		mem = proc.EventLastNVisible(visibleTarget, maxTotal)
	} else {
		mem = s.persistedHistoryTailVisible(visibleTarget, maxTotal)
	}

	if visibleTarget <= 0 {
		return mem
	}
	vis := countVisibleEntries(mem)
	if vis >= visibleTarget || len(mem) >= maxTotal {
		return mem
	}

	// Disk tier: the ring couldn't satisfy the target. Page backward through
	// the durable source, strictly older than the earliest in-memory entry.
	src := s.loadHistorySource()
	if src == nil {
		return mem
	}
	before := int64(0)
	if len(mem) > 0 {
		before = mem[0].Time
	}
	// Pages accumulate newest-first and are concatenated in reverse after the
	// loop, avoiding O(n²) prepends. runningOlder keeps the ceiling check O(1).
	var pages [][]clievent.EventEntry
	runningOlder := 0
	for page := 0; page < maxVisibleDiskPages && vis < visibleTarget; page++ {
		if ctx.Err() != nil {
			break
		}
		chunk, err := src.LoadBefore(ctx, before, visibleDiskPageSize)
		if err != nil {
			slog.Warn("visible history source load failed", "key", s.key, "err", err)
			break
		}
		if len(chunk) == 0 {
			break // disk exhausted
		}
		sortEntriesByTimeStable(chunk)
		pages = append(pages, chunk)
		vis += countVisibleEntries(chunk)
		before = chunk[0].Time
		runningOlder += len(chunk)
		if len(mem)+runningOlder >= maxTotal {
			break // total payload ceiling
		}
	}
	if len(pages) == 0 {
		return mem
	}
	totalOlder := 0
	for _, p := range pages {
		totalOlder += len(p)
	}
	older := make([]clievent.EventEntry, 0, totalOlder)
	for i := len(pages) - 1; i >= 0; i-- {
		older = append(older, pages[i]...)
	}
	return append(older, mem...)
}

// persistedHistoryTailVisible returns a contiguous tail of persistedHistory
// carrying at least visibleTarget visible entries (or up to maxTotal entries).
// The no-process analogue of EventLog.LastNVisible. Read-only copy under the
// history lock.
func (s *ManagedSession) persistedHistoryTailVisible(visibleTarget, maxTotal int) []clievent.EventEntry {
	s.historyMu.RLock()
	defer s.historyMu.RUnlock()
	n := len(s.persistedHistory)
	if n == 0 {
		return nil
	}
	limit := maxTotal
	if limit <= 0 || limit > n {
		limit = n
	}
	// Walk backward from the newest entry until we have visibleTarget visible
	// entries or hit the length ceiling.
	visible := 0
	start := n // exclusive lower bound of the tail we keep
	for i := n - 1; i >= 0 && (n-i) <= limit; i-- {
		start = i
		if cli.IsVisibleEntry(s.persistedHistory[i]) {
			visible++
			if visibleTarget > 0 && visible >= visibleTarget {
				break
			}
		}
	}
	out := make([]clievent.EventEntry, n-start)
	copy(out, s.persistedHistory[start:])
	return out
}

// SubscribeEvents subscribes to event log notifications for this session.
// If the session has no process, returns a closed channel and a no-op unsubscribe.
func (s *ManagedSession) SubscribeEvents() (<-chan struct{}, func()) {
	proc := s.loadProcess()
	if proc == nil {
		ch := make(chan struct{})
		close(ch)
		return ch, func() {}
	}
	return proc.SubscribeEvents()
}

// LogSystemEvent appends a single "system"-typed EventEntry with the given
// summary to this session's event log and notifies subscribers, so
// off-main-path writers (e.g. the connector's async Send goroutine) surface
// errors in the UI instead of only in logs. The dashboard renders system
// events escaped, so arbitrary error text is safe. With a live proc it goes
// to the EventLog (WS subscribers wake); without one it lands in
// persistedHistory (bounded by maxPersistedHistory). Empty summary is a no-op.
func (s *ManagedSession) LogSystemEvent(summary string) {
	if summary == "" {
		return
	}
	entry := clievent.EventEntry{
		Time:    time.Now().UnixMilli(),
		Type:    "system",
		Summary: summary,
	}
	// InjectHistory owns proc/persistedHistory routing and subscriber wakeup.
	s.InjectHistory([]clievent.EventEntry{entry})
}

// extractLastPromptFromProcess scans the attached process's event log to
// populate lastPrompt, lastActivity, and lastResponse when unset (e.g. after
// shim reconnect where events bypassed InjectHistory). Only the tail is
// needed since scanLastSummaries stops once all three are found; EventLastN
// avoids the full-ring copy EventEntries would allocate.
const extractLastPromptScanN = 64

func (s *ManagedSession) extractLastPromptFromProcess() {
	if loadAtomicString(&s.lastPrompt) != "" &&
		loadAtomicString(&s.lastActivity) != "" &&
		loadAtomicString(&s.lastResponse) != "" {
		return
	}
	p := s.loadProcess()
	if p == nil {
		return
	}
	prompt, activity, response := scanLastSummaries(p.EventLastN(extractLastPromptScanN))
	if prompt != "" && loadAtomicString(&s.lastPrompt) == "" {
		storeAtomicString(&s.lastPrompt, prompt)
	}
	if activity != "" && loadAtomicString(&s.lastActivity) == "" {
		storeAtomicString(&s.lastActivity, activity)
	}
	if response != "" && loadAtomicString(&s.lastResponse) == "" {
		storeAtomicString(&s.lastResponse, response)
	}
}

// scanLastSummaries walks entries in reverse, returning the most-recent
// user-prompt summary, activity summary, and assistant response summary.
// Stops early once all three are found. Seeds the atomic caches after
// replay so suspended/dead sessions still show a sidebar preview.
func scanLastSummaries(entries []clievent.EventEntry) (prompt, activity, response string) {
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if prompt == "" && e.Type == "user" {
			prompt = e.Summary
		}
		if activity == "" && cli.IsActivityType(e.Type) {
			activity = e.Summary
		}
		if response == "" && e.Type == "text" {
			// Mirrors EventLog's store-time strip so the replay-seeded
			// cache and the live summary render identically (#2435).
			response = textutil.StripMarkdown(e.Summary)
		}
		if prompt != "" && activity != "" && response != "" {
			break
		}
	}
	return prompt, activity, response
}

// costUnitForBackend returns the SessionSnapshot.CostUnit value for a given
// backend, read from backend.Profile.CostUnit (docs/rfc/multi-backend.md
// §8.3 D5). Empty backend (legacy stores predating the Backend field) means
// claude, hence USD.
func costUnitForBackend(backendID string) string {
	if backendID == "" {
		backendID = "claude"
	}
	// Fast path: production registers defaults before any session exists.
	if p, ok := backend.Get(backendID); ok {
		return p.CostUnit
	}
	// Lazy bootstrap for tests that never call RegisterDefaults (#890). Only
	// when the registry is completely empty: a partially-populated one (a
	// sibling test's custom backend) would panic on duplicate IDs. recover
	// covers the race with a concurrent wireup.RegisterCLIBackends between
	// the empty-check and RegisterDefaults — benign, registry ends populated.
	costUnitForBackendOnce.Do(func() {
		if len(backend.All()) != 0 {
			return
		}
		defer func() {
			if r := recover(); r != nil {
				// Logged so unexpected (non-duplicate) panics stay visible.
				slog.Debug("costUnitForBackend: recovered panic in RegisterDefaults",
					"recovered", r)
			}
		}()
		backend.RegisterDefaults()
	})
	if p, ok := backend.Get(backendID); ok {
		return p.CostUnit
	}
	// Unregistered ID (config typo, unwired backend): "" makes the dashboard
	// hide the cost cell rather than render a misleading unit.
	return ""
}

var costUnitForBackendOnce sync.Once
