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
)

// getSessionID returns the session ID lock-free via atomic.Pointer[string].
//
// R230C-CR-5: there are three SessionID-shaped accessors across two
// packages — keep them in mind so future refactors don't accidentally
// drop one or introduce a fourth:
//
//   - ManagedSession.getSessionID — package-private; canonical lock-free
//     read of the session-level atomic. Used internally inside this file.
//   - ManagedSession.SessionID — public alias of getSessionID; satisfies
//     the cli.HistorySessionView interface (Wrapper.NewHistorySource
//     factory wiring) and is the right entry point for cross-package
//     callers in internal/server / internal/dispatch.
//   - cli.Process.GetSessionID — different layer entirely. Reads the CLI
//     subprocess's most-recently-observed session ID off the live event
//     stream. The two layers may briefly disagree during a /resume
//     handshake or first-Send capture; callers picking between them
//     should choose by intent: "what does naozhi remember for this chat
//     key" → ManagedSession.SessionID; "what does the CLI think the
//     active session is right now" → Process.GetSessionID.
func (s *ManagedSession) getSessionID() string {
	return loadAtomicString(&s.sessionID)
}

// SessionID returns the current CLI session ID, lock-free. Public alias
// for getSessionID used by the cli.HistorySessionView interface
// (Sprint 1a, Wrapper.NewHistorySource factory wiring) and any future
// caller that needs the current ID without taking r.mu. See
// getSessionID's godoc for the relationship with cli.Process.GetSessionID
// (R230C-CR-5).
func (s *ManagedSession) SessionID() string { return s.getSessionID() }

// setSessionID stores the session ID atomically.
func (s *ManagedSession) setSessionID(id string) {
	storeAtomicString(&s.sessionID, id)
}

// parseKeyParts lazily parses the immutable session key into cached components.
// Hand-rolled split avoids the []string allocation that strings.SplitN would
// produce — every new session triggers exactly one parseKeyParts on its first
// Snapshot, and dashboards poll dozens of sessions per second. (R227-PERF-13)
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
// session, regardless of liveness. Returns true even for processes
// that have exited but not yet been detached by the readLoop cleanup.
// Callers needing liveness should use isAlive() (private) or check
// State() == "ready"/"busy" via the Snapshot path. Lock-free read of
// the atomic.Pointer[processBox] backing field.
func (s *ManagedSession) HasProcess() bool {
	return s.loadProcess() != nil
}

// State returns just the live process state ("ready" / "busy" / etc.)
// without performing the SetModel mirror or building a full
// SessionSnapshot. Lock-free hot path for high-frequency observers
// (R230C-PERF-1: connector_subscribe ticks per agent_message_chunk
// event ~10-50/s and only needs State + DeathReason). Returns "ready"
// when no process is attached, mirroring Snapshot's no-proc branch.
func (s *ManagedSession) State() string {
	proc := s.loadProcess()
	if proc == nil {
		return "ready"
	}
	return proc.GetState().String()
}

// DeathReason returns the recorded death cause string ("" when the
// session is healthy or has not died yet). Companion to State() for
// connector_subscribe's session_state push so the change-detection
// branch can avoid a full Snapshot. R230C-PERF-1.
func (s *ManagedSession) DeathReason() string {
	return loadAtomicString(&s.deathReason)
}

// Snapshot returns a point-in-time view of this session.
//
// Side effect (R230C-CR-Diag / R229-GO-2): when a live process reports a
// non-empty Model() that disagrees with the persisted s.model field,
// Snapshot writes the live value back via SetModel before returning the
// view. This keeps the dashboard's model chip in sync with what the CLI
// is actually using (kiro reports the model only after session/new
// completes, not at spawn time). Callers that need a strictly read-only
// snapshot should not rely on this path; a future SnapshotReadOnly
// variant is tracked under R229-GO-2 once the dashboard polling cadence
// is moved to a dedicated mirror.
//
// Performance contract (R214-PERF-2 #411): Snapshot MUST NOT copy
// persistedHistory or any other O(N) backing structure. Dashboards poll
// at 1Hz × N tabs × M sessions, and at 500 entries × ~400 B each the
// per-call copy would burn ~200 KB of allocation per session per second.
// Scalar fields are cached via atomic.Pointer[string] (lastPrompt,
// lastActivity, lastResponse, deathReason, model) so the snapshot is
// O(1) regardless of session age. Callers that need the full event log
// must call EventEntries / EventLastN / EventEntriesSince explicitly,
// which is the cheap-rare-call path versus this hot poll path.
// snapshot_no_history_copy_test.go pins the contract.
func (s *ManagedSession) Snapshot() SessionSnapshot {
	s.parseKeyParts()
	// R215-ARCH-P2-7: pull backend/cliName/cliVersion in one atomic Load
	// instead of three sequential Loads — Snapshot is the 1 Hz × N tabs ×
	// N sessions hot path, so collapsing redundant atomic reads here
	// adds up at scale.
	id := s.loadCLIIdentity()
	snap := SessionSnapshot{
		Key:         s.key,
		Platform:    s.keyPlatform,
		ChatType:    s.keyChatType,
		ChatID:      s.keyChatID,
		Agent:       s.keyAgentID,
		SessionID:   s.getSessionID(),
		LastActive:  s.LastActive().UnixMilli(),
		CreatedAt:   s.createdAtMillis(),
		Workspace:   s.Workspace(),
		Backend:     id.backend,
		CLIName:     id.cliName,
		CLIVersion:  id.cliVersion,
		UserLabel:   s.UserLabel(),
		LabelOrigin: s.LabelOrigin(),
		// UI Round 5 R5-3: seed Model from persisted ManagedSession; the
		// proc-bearing branch below will overwrite if live proc has a
		// fresher value. No-proc snapshots (evicted / pre-spawn) keep
		// the persisted value so dashboard doesn't blink to
		// "(模型未配置)" during restart-reattach.
		Model: s.Model(),
	}
	snap.DeathReason = loadAtomicString(&s.deathReason)
	// R176-ARCH-N4 (#432): expose the explicit lifecycle state as a single
	// derived field so the dashboard stops re-deriving it from
	// State()+SessionID+exempt. ManagedState() reuses the same atomic reads
	// this method already performs; the only extra cost is in the rare
	// stub/dead fallback (proc==nil && no session id), which is not the
	// active-session hot poll path.
	snap.Lifecycle = s.ManagedState().String()

	proc := s.loadProcess()
	sessCost := loadTotalCost(&s.totalCost)
	if proc == nil {
		snap.TotalCost = sessCost
		snap.State = "ready"
	} else {
		snap.State = proc.GetState().String()
		snap.Protocol = proc.ProtocolName()
		// UI Round 5 R5-3: model resolution priority
		//   1. live proc.Model() (claude system/init or kiro SpawnOptions)
		//   2. persisted s.Model() (post-restart, before next init)
		// When proc reports a model and it differs from / is more
		// recent than what we persisted, mirror it back so the next
		// saveStore tick captures it. Empty live → keep persisted.
		//
		// R226-CR-13: this SetModel is an intentional read-side write —
		// dashboard polls Snapshot at 1Hz and proc-reported model is the
		// authoritative source we need to ship into sessions.json.
		// Snapshot is otherwise read-only; if a future caller needs a
		// pure-read variant, factor a SnapshotReadOnly that skips this
		// mirror rather than dropping it (skipping silently regresses to
		// the symptom Round 5 R5-3 fixed: dashboard "model 未配置" blink
		// after spawn until the first result event triggered a save).
		liveModel := proc.Model()
		if liveModel != "" {
			// R236-PERF-13 (#534): the previous comment claimed SetModel
			// short-circuits internally — it does NOT. storeAtomicString
			// always swaps the pointer, which dirties the cache line and
			// at 1Hz × N tabs × M sessions costs an avoidable atomic
			// store per poll on what is otherwise a pure-read path.
			// Compare the cached value first and only mirror on change.
			if cached := s.Model(); cached != liveModel {
				s.SetModel(liveModel)
			}
			snap.Model = liveModel
		} else {
			snap.Model = s.Model()
		}
		// Prefer whichever is larger: a freshly resumed process reports 0
		// until the first `result` event arrives, but s.totalCost carries
		// the historical cumulative value restored from sessions.json.
		// Claude CLI's total_cost_usd under --resume is cumulative, so once
		// the next result lands, proc.TotalCost() will be >= s.totalCost
		// and the display won't regress.
		if pc := proc.TotalCost(); pc > sessCost {
			snap.TotalCost = pc
		} else {
			snap.TotalCost = sessCost
		}
		snap.Subagents = proc.TurnAgents()
		// Prefer the EventLog-maintained summary (updated lock-free on every
		// event) so we don't need a wrapper closure around Send just to track
		// lastActivity.
		snap.LastActivity = proc.LastActivitySummary()
		// R110-P1: live process is the authoritative source for the most-recent
		// assistant text reply. Empty when no text block has streamed yet
		// (post-spawn pre-result window); the s.lastResponse fallback below
		// covers the post-restart / pre-replay case via scanLastSummaries seed.
		snap.LastResponse = proc.LastResponseSummary()
		// MessageCount is the cumulative user turn count observed by the
		// current Process since its last spawn. proc==nil branch leaves the
		// field at zero so UI code can gate visibility on `> 0` and skip the
		// chip for brand-new sessions that haven't yet received a prompt.
		snap.MessageCount = proc.UserTurnCount()

		// Normalize layer (docs/rfc/multi-backend.md §8.8). Process getters
		// return zero values for fields the backend never reports, so
		// `> 0` gating in dashboard.js works for both claude (most fields
		// zero today) and kiro (all fields populated).
		snap.ContextUsagePercent = proc.ContextUsagePercent()
		snap.TurnDurationMs = proc.TurnDurationMs()
		snap.MeteringUsage = proc.MeteringUsage()
	}

	// CostUnit is derived from backend even when proc is nil so an evicted
	// session still renders the right cost label until pruning. claude is
	// the default for legacy stores predating the Backend field.
	snap.CostUnit = costUnitForBackend(snap.Backend)

	// UI Round 5 R5-4: when CostUnit is "credits" (kiro family) the
	// dashboard's header cost cell should show the SESSION-level total,
	// not per-turn. claude path keeps snap.TotalCost from CLI's own
	// running total (USD). For kiro we derive it from the accumulated
	// MeteringUsage (Process.applyMetadata is now session-level).
	if snap.CostUnit == "credits" && len(snap.MeteringUsage) > 0 {
		var credits float64
		for _, m := range snap.MeteringUsage {
			if m.Unit == "credit" || m.Unit == "credits" {
				credits += m.Value
			}
		}
		// Only override when we found a credit-typed entry; if kiro ever
		// emits a non-credit unit (token / cost) under cost_unit=credits,
		// don't silently zero the running total.
		if credits > 0 {
			snap.TotalCost = credits
		}
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
	// R110-P1: only fall back to the cached lastResponse when the live process
	// hasn't yet reported one. Mirrors the LastPrompt/LastActivity priority
	// (live wins, cache survives restart). Empty cache + empty live leaves the
	// field unset → JSON omitempty hides the dim line on brand-new sessions.
	if snap.LastResponse == "" {
		if lr := loadAtomicString(&s.lastResponse); lr != "" {
			snap.LastResponse = lr
		}
	}

	return snap
}

// hasInjectedHistory reports whether persistedHistory contains any entries.
// Used by the startup history loader (R53-ARCH-001 fix) to decide whether
// the deferred JSONL backfill path is needed: if ReconnectShims already
// injected history via proc.InjectHistory → s.InjectHistory's
// persistedHistory append, the flag is set and we skip the redundant FS
// read. Read-only, no copy — callers just need a boolean.
func (s *ManagedSession) hasInjectedHistory() bool {
	s.historyMu.RLock()
	defer s.historyMu.RUnlock()
	return len(s.persistedHistory) > 0
}

// EventEntries returns the event log entries for this session.
// Returns persisted history when the process is nil or dead.
func (s *ManagedSession) EventEntries() []cli.EventEntry {
	proc := s.loadProcess()
	if proc != nil {
		return proc.EventEntries()
	}
	s.historyMu.RLock()
	out := make([]cli.EventEntry, len(s.persistedHistory))
	copy(out, s.persistedHistory)
	s.historyMu.RUnlock()
	return out
}

// SubagentLinker returns the SubagentLinker owned by the live *cli.Process,
// or nil when the session is not backed by a live Claude-CLI process (fake
// test process, dead process, ACP protocol, etc.). Callers must guard the
// nil return — the agent-team UI endpoints downgrade to 404 in that case.
//
// Intentionally type-asserts rather than widening processIface so the fake
// processes in router/managed tests don't need to implement the full Linker
// surface. The downside — a test process that wants real linker behaviour
// must wrap *cli.Process directly — is acceptable because the linker's own
// unit tests in internal/cli/subagent_link_test.go are the canonical spot
// for that coverage.
//
// R239-ARCH-I: the consumer-facing interface lives at
// internal/session/agentlink.AgentLinker — server stores wired linkers
// keyed on that interface. ManagedSession still returns the concrete
// *cli.SubagentLinker so callers that need the full linker surface
// (SeedFromHistory / Resolve / SetContext / ConfigureForTest, used by
// the cli package itself plus its tests) keep working without an extra
// type assertion. The interface widens only at the server boundary.
//
// TODO: introduce AgentIntrospector interface when a second backend needs
// agent-view support. Tracked in docs/TODO.md (R214-CODE-6 / R217-ARCH-2 /
// R219-ARCH-3 — the lifecycle question, distinct from the consumer-side
// interface R239-ARCH-I now solves). The three live anchors above cover
// the same root; the orphan-id reference (R245-CR-008) was retired in
// favour of pointing at the live anchors directly.
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
func (s *ManagedSession) EventLastN(n int) []cli.EventEntry {
	proc := s.loadProcess()
	if proc != nil {
		return proc.EventLastN(n)
	}
	s.historyMu.RLock()
	defer s.historyMu.RUnlock()
	if n <= 0 || n >= len(s.persistedHistory) {
		out := make([]cli.EventEntry, len(s.persistedHistory))
		copy(out, s.persistedHistory)
		return out
	}
	start := len(s.persistedHistory) - n
	out := make([]cli.EventEntry, n)
	copy(out, s.persistedHistory[start:])
	return out
}

// sortEntriesByTimeStable sorts entries in-place by Time ascending using a
// stable sort so that entries sharing the same Time keep their insertion
// order (matters for InjectHistory batches where a whole chain replay may
// collapse to a single default timestamp). Callers of EventEntriesSince /
// EventEntriesBefore depend on chronological output — the ring buffer and
// persistedHistory themselves don't guarantee strict ordering because
// (a) InjectHistory may interleave segments from multiple session chains
// and (b) AppendBatch assigns a single wall-clock to zero-Time entries
// while older entries might still arrive with real earlier timestamps
// from resume paths.
func sortEntriesByTimeStable(entries []cli.EventEntry) {
	if len(entries) < 2 {
		return
	}
	slices.SortStableFunc(entries, func(a, b cli.EventEntry) int {
		return cmp.Compare(a.Time, b.Time)
	})
}

// EventEntriesSince returns the event log entries with Time > afterMS in
// chronological order.
//
// Live-process branch: proc.EventEntriesSince is backed by cli.EventLog's
// ring buffer, which records entries in strict append order. Append stamps
// zero-Time entries with now and AppendBatch uses a single now for the
// batch, so Time is weakly monotonic by construction and no re-sort is
// needed. This is the WS push hot path (wshub.go emits on every notify
// tick), so avoiding an O(n)+sort here matters.
//
// Dead-session branch: persistedHistory is NOT guaranteed sorted because
// InjectHistory may interleave segments from multiple session chains
// (startup backfill replays prev-session IDs in reverse-chain order).
// We do a full linear scan + stable sort so paginated fetches see
// chronological output.
func (s *ManagedSession) EventEntriesSince(afterMS int64) []cli.EventEntry {
	proc := s.loadProcess()
	if proc != nil {
		return proc.EventEntriesSince(afterMS)
	}
	// Skip the stable sort once the maintained invariant says
	// persistedHistory is already in Time order. Steady-state dashboard
	// polling (1Hz × N tabs × M dead sessions) used to pay this every
	// call; the in-place sort under historyMu also blocks concurrent
	// readers. R237-PERF-12.
	//
	// R040034-PERF-6 (#1405): production mutations now sort eagerly in
	// InjectHistory under the existing write lock, so this fallback only
	// fires for test fixtures that assign s.persistedHistory directly and
	// leave the flag false. Promote to the write lock once, sort in place,
	// set the flag, and downgrade — subsequent reads then take the cheap
	// RLock-only path.
	s.historyMu.RLock()
	// Fast path: empty history needs no work; when sorted, the last entry
	// holds the max Time so we can skip the linear scan if it's already
	// <= afterMS. Idle dashboard poll (afterMS = last seen) on dead
	// sessions used to scan the entire persistedHistory every tick even
	// though every entry was older than afterMS. R260528-PERF-4.
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
		// Re-check the short-circuit now that we're sorted — the sort
		// could only have rearranged entries, not added new ones, but
		// take the same fast exit if last.Time <= afterMS to avoid the
		// scan-and-allocate on the steady-state poll.
		if n := len(s.persistedHistory); n == 0 || s.persistedHistory[n-1].Time <= afterMS {
			s.historyMu.RUnlock()
			return nil
		}
	}
	// R20260531-PERF-1: small initial cap and let append grow naturally.
	// The steady-state dashboard poll (1Hz × N-tab × dead-session) is an
	// incremental query with a recent afterMS that matches only the last
	// handful of entries, so presizing to len(persistedHistory) (up to the
	// full ring, default 500) over-allocates ~500 slots per poll for a
	// 0-5-entry result. afterMS=0 full replay still happens (e.g. first
	// load of a dead session) and will pay a few reallocations growing past
	// 16, but that path is rare; we trade it for the common case.
	out := make([]cli.EventEntry, 0, 16)
	for _, e := range s.persistedHistory {
		if e.Time > afterMS {
			out = append(out, e)
		}
	}
	s.historyMu.RUnlock()
	return out
}

// EventEntriesBefore returns up to `limit` entries with Time < beforeMS
// drawn from the in-memory log (live process ring or persistedHistory).
// Entries are returned in chronological order.
//
// Scope: memory-tier only. Does NOT consult the backend's disk-tier
// history.Source — callers that need complete historical coverage should
// use EventEntriesBeforeCtx which falls back to disk when memory is
// exhausted. This split preserves the legacy call sites (tests, internal
// helpers) that can't easily thread a context through.
//
// The live-process branch relies on EventLog's insertion-order ring which
// is already chronological (Append/AppendBatch assign monotonic Time to
// zero-Time entries), so it returns without re-sorting. Only the
// persistedHistory branch pays for a stable sort because startup chain
// replay may interleave segments.
//
// beforeMS <= 0 is treated as "no upper bound" — equivalent to the tail
// of the log, matching EventLastN semantics. limit <= 0 returns nil.
func (s *ManagedSession) EventEntriesBefore(beforeMS int64, limit int) []cli.EventEntry {
	if limit <= 0 {
		return nil
	}
	proc := s.loadProcess()
	if proc != nil {
		return proc.EventEntriesBefore(beforeMS, limit)
	}
	out := s.persistedHistoryBefore(beforeMS, limit)
	sortEntriesByTimeStable(out)
	return out
}

// EventEntriesBeforeCtx extends EventEntriesBefore with a disk-tier
// fallback. When the in-memory log has no entries strictly older than
// beforeMS, the session's history.Source is consulted. This is the path
// the dashboard pagination handler takes; legacy non-ctx callers still
// use the memory-only variant.
//
// The two tiers are never merged: the memory tier is authoritative for
// any range it covers (since it includes naozhi-synthesized events like
// LogSystemEvent that never reach disk), and falling through to disk
// only when memory is empty keeps the result strictly chronological
// without a deduplication step. The trade-off is one extra round trip
// on the page that straddles the memory-bottom; on all subsequent pages
// memory returns empty and disk is queried directly.
func (s *ManagedSession) EventEntriesBeforeCtx(ctx context.Context, beforeMS int64, limit int) []cli.EventEntry {
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
		// Treat as end-of-history — logging (not propagating) matches the
		// existing JSONL load sites in router.go which also degrade silently
		// on read errors.
		slog.Warn("history source load failed", "key", s.key, "err", err)
		return nil
	}
	sortEntriesByTimeStable(entries)
	return entries
}

// countVisibleEntries returns how many entries the dashboard would render as
// chat bubbles (the inverse of the INTERNAL_EVENT_TYPES filter). Shared by the
// visible-aware reader below.
func countVisibleEntries(entries []cli.EventEntry) int {
	n := 0
	for i := range entries {
		if cli.IsVisibleEntry(entries[i]) {
			n++
		}
	}
	return n
}

// EventLastNVisibleCtx is the initial-history entry point for the dashboard.
// It returns the tail of the session's history guaranteed to carry enough
// VISIBLE entries (chat bubbles) that the dashboard's initial render never
// degrades to the blank "该会话最近仅有 agent 活动" placeholder — the symptom
// of a parallel agent team flooding the trailing window with internal events.
//
// Two tiers, mirroring EventEntriesBeforeCtx's memory-then-disk strategy:
//
//  1. Memory tier: the live ring (Process.EventLastNVisible) or, for a
//     suspended/dead session, the tail of persistedHistory. The memory slice
//     is a contiguous run so the dashboard can rebuild turnState / the running
//     banner from the interleaved internal events.
//  2. Disk tier: when the ring alone can't reach visibleTarget (the 500-entry
//     ring is entirely internal — exactly the bug scenario), walk backward
//     through the backend's history.Source one page at a time, prepending
//     older entries until the combined visible count reaches the target or a
//     page/total/byte ceiling trips.
//
// The two tiers never overlap: disk is queried strictly older than the
// earliest in-memory Time, reusing the non-merging contract documented on
// EventEntriesBeforeCtx, so no dedup is required. Non-claude backends carry a
// Noop source and simply return the memory tier.
//
// visibleTarget <= 0 falls back to a plain EventLastN(maxTotal). The ctx
// bounds disk I/O — callers on the WS subscribe handshake pass a short
// timeout so a slow filesystem can't stall the first frame.
func (s *ManagedSession) EventLastNVisibleCtx(ctx context.Context, visibleTarget, maxTotal int) []cli.EventEntry {
	if maxTotal <= 0 {
		maxTotal = maxVisibleTotal
	}
	// Memory tier: contiguous tail carrying up to visibleTarget visible entries.
	var mem []cli.EventEntry
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
	var older []cli.EventEntry
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
		// chunk is chronological and strictly older than `before`; prepend it
		// ahead of everything collected so far.
		older = append(chunk, older...)
		vis += countVisibleEntries(chunk)
		before = chunk[0].Time
		if len(older)+len(mem) >= maxTotal {
			break // total payload ceiling
		}
	}
	if len(older) == 0 {
		return mem
	}
	return append(older, mem...)
}

// persistedHistoryTailVisible returns a contiguous tail of persistedHistory
// carrying at least visibleTarget visible entries (or up to maxTotal entries).
// The no-process analogue of EventLog.LastNVisible. Read-only copy under the
// history lock.
func (s *ManagedSession) persistedHistoryTailVisible(visibleTarget, maxTotal int) []cli.EventEntry {
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
	out := make([]cli.EventEntry, n-start)
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
// summary text to this session's event log and notifies subscribers. Used by
// off-main-path writers (e.g. upstream/connector's async Send goroutine)
// that would otherwise lose errors to log.Warn while the primary has
// already told the UI "accepted". Dashboard renders system events as
// esc(e.summary), so the text is safe to contain arbitrary error messages.
//
// Semantics:
//   - proc != nil: appends to the live EventLog; push-subscribers (WS
//     eventPushLoop) wake immediately.
//   - proc == nil (suspended session): appends to persistedHistory so the
//     entry shows up on the next subscribe/snapshot. Still bounded by
//     maxPersistedHistory; the oldest entry is dropped if full.
//
// Empty summary is rejected (no-op) to avoid polluting the log with blank
// system lines on programmer error. R49-REL-CONNECTOR-SEND-RESULT-LOSS.
func (s *ManagedSession) LogSystemEvent(summary string) {
	if summary == "" {
		return
	}
	entry := cli.EventEntry{
		Time:    time.Now().UnixMilli(),
		Type:    "system",
		Summary: summary,
	}
	// Reuse InjectHistory so proc/persistedHistory routing stays in one
	// place and subscribers wake via the existing notifySubscribers path.
	s.InjectHistory([]cli.EventEntry{entry})
}

// extractLastPromptFromProcess scans the attached process's event log to populate
// lastPrompt, lastActivity, and lastResponse when they haven't been set yet
// (e.g. after shim reconnect where events were injected directly into the
// process, bypassing InjectHistory).
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
	prompt, activity, response := scanLastSummaries(p.EventEntries())
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
// Stops early once all three are found. Used by InjectHistory and
// extractLastPromptFromProcess to seed the atomic caches after replay.
//
// R110-P1: response capture extends the existing prompt/activity scan so
// suspended/dead sessions still surface a sidebar second-line preview after
// shim reconnect (which replays history into a fresh EventLog).
func scanLastSummaries(entries []cli.EventEntry) (prompt, activity, response string) {
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if prompt == "" && e.Type == "user" {
			prompt = e.Summary
		}
		if activity == "" && cli.IsActivityType(e.Type) {
			activity = e.Summary
		}
		if response == "" && e.Type == "text" {
			response = e.Summary
		}
		if prompt != "" && activity != "" && response != "" {
			break
		}
	}
	return prompt, activity, response
}

// costUnitForBackend returns the SessionSnapshot.CostUnit value for a given
// backend. claude-class backends report cost in USD via Process.TotalCost();
// ACP-class kiro reports per-turn metering in credits via _kiro.dev/metadata.
// Empty backend (legacy stores predating the Backend field) defaults to USD
// because such stores are necessarily claude-only.
//
// The actual unit string lives on backend.Profile.CostUnit, looked up via
// backend.Get. Adding a new backend means setting CostUnit on its profile —
// no edit here required (R225-CR-4 / R224-ARCH-1). The dashboard reads this
// value as the source of truth for cost-cell formatting (see
// docs/rfc/multi-backend.md §8.3 D5).
func costUnitForBackend(backendID string) string {
	// Legacy stores predating the Backend field — claude-only.
	if backendID == "" {
		backendID = "claude"
	}
	// Lazy bootstrap pattern (matches server.replyTagForBackend): production
	// wires backend.RegisterDefaults() in cmd/naozhi/main.go before any
	// session is constructed. Tests that build a Snapshot without calling
	// RegisterDefaults would otherwise see backend.Get return false and lose
	// the unit — costUnitForBackendOnce ensures one-shot lazy registration so
	// tests stay green. Guard with a registry-empty check so we cooperate
	// with sibling tests (server pkg withDefaultBackends) that already
	// pre-registered, rather than panicking on duplicate Register.
	costUnitForBackendOnce.Do(func() {
		if len(backend.All()) == 0 {
			backend.RegisterDefaults()
		}
	})
	if p, ok := backend.Get(backendID); ok {
		return p.CostUnit
	}
	// Unregistered backend ID (e.g. config typo, in-progress backend not
	// yet wired into RegisterDefaults). Returning "" makes the dashboard
	// hide the cost cell rather than render a misleading unit.
	return ""
}

var costUnitForBackendOnce sync.Once
