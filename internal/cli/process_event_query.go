package cli

// process_event_query.go — read-only EventLog accessors + Linker
// lifecycle + InjectHistory seeding.
//
// Most methods are thin passthroughs to p.eventLog. InjectHistory is
// the heaviest — it seeds the SubagentLinker so dashboard drill-in
// for live agent tasks survives a naozhi restart.
//
// R227-ARCH-19: dropped the "Phase 6 of process-split / zero semantic
// change" preamble; refactor is complete, history lives in git log.

import (
	"time"

	"github.com/naozhi/naozhi/internal/textutil"
)

// InjectHistory pre-populates the event log with historical entries. Also
// seeds the SubagentLinker so team agent rows in the dashboard banner can
// resume the task_id → jsonl mapping established in a previous process
// lifetime (RFC v4 agent-team-ui §3.3.7 — A3 defence against CLI-dead
// respawn changing session_uuid).
func (p *Process) InjectHistory(entries []EventEntry) {
	// Replay path: AppendBatchReplay skips applyEntryStateLocked under l.mu.
	// SetPersistSink runs AFTER InjectHistory and the on-task-done callback
	// is wired by the server tailer registry only on live spawn, so replay
	// can never produce an observable side effect. R240-PERF-3 (#1042).
	p.eventLog.AppendBatchReplay(entries)
	if p.linker == nil {
		return
	}
	p.linker.SeedFromHistory(entries)

	// A3 second-leg: entries persisted by an older naozhi (before the RFC v4
	// §3.2.2 four-field backfill shipped) lack InternalAgentID / JSONLPath,
	// so SeedFromHistory skips them. Yet the in-process team agents those
	// entries describe may still be running under the live shim — their
	// task_id → jsonl mapping has to come from an active Resolve. Walk the
	// batch for terminal-but-unresolved "agent" / "task_start" pairs and
	// kick off async Resolves so the dashboard drill-in has something to
	// serve when the user clicks.
	//
	// agent: agent entries carry {ToolUseID, Subagent, Background};
	//        task_start entries carry {TaskID, ToolUseID} and usually arrive
	//        right after the paired agent entry.
	// We index task_start by ToolUseID so a single pass assembles the args
	// Resolve wants: (taskID, toolUseID, name, description, agentToolUseMS).
	// De-dupe by task_id so we fire at most one Resolve per unique task.
	// Ring-buffered history commonly carries many task_progress entries
	// for the same task_id; the fast path inside Resolve is cached after
	// the first hit but we want to avoid the goroutine churn.
	seen := make(map[string]struct{})
	taskStartByToolUse := make(map[string]EventEntry, len(entries))
	for _, e := range entries {
		if e.Type == "task_start" && e.ToolUseID != "" {
			taskStartByToolUse[e.ToolUseID] = e
		}
	}
	kick := func(taskID, toolUseID, name, desc string, wallclock int64) {
		if taskID == "" {
			return
		}
		if _, ok := seen[taskID]; ok {
			return
		}
		seen[taskID] = struct{}{}
		linker := p.linker
		// R241-PERF-5 (#478): skip goroutine spawn if linker has already
		// resolved this task_id. InjectHistory replay can repeatedly hit
		// the same backfill path during dashboard reconnects; Query is
		// an O(1) RLock+map-read so it pays the same cost as the redundant
		// resolveByTaskIDFast cache check inside Resolve, without the
		// goroutine schedule + closure allocation cost.
		if info, ok := linker.Query(taskID); ok && info.Resolved {
			return
		}
		// R260528-PERF-7 (#1354): also skip if a Resolve goroutine is
		// already in-flight for this taskID. InjectHistory's per-entry
		// `seen` map dedupes within a single batch, but two adjacent
		// InjectHistory calls (server reconnect → replay-then-tail) can
		// race on the same taskID and each escape into the goroutine
		// fork before the first completes. The cross-batch in-flight
		// gate is the canonical dedup; the local `seen` map remains as
		// a cheap intra-batch fast path (less hashing per kick).
		if !linker.TryMarkResolveInflight(taskID) {
			return
		}
		// R225-CR-10: cap description (matches process_readloop hot path)
		// so the Resolve goroutine doesn't pin multi-KB strings until the
		// resolveSem slot frees.
		desc = textutil.TruncateRunes(desc, eventDetailMaxRunes)
		// R214-PERF-6 (#415): use the linker's bounded dispatch pool
		// rather than spawning a per-event goroutine. InjectHistory's
		// replay can fan in dozens of task_started entries on shim
		// reconnect; the pool keeps that bounded.
		linker.DispatchResolve(p.lifecycleContext(), taskID, toolUseID, name, desc, wallclock)
	}
	for _, e := range entries {
		switch e.Type {
		case "agent":
			if e.ToolUseID == "" || e.InternalAgentID != "" {
				continue
			}
			ts, ok := taskStartByToolUse[e.ToolUseID]
			if !ok || ts.TaskID == "" {
				continue
			}
			name := e.Subagent
			if name == "" {
				name = e.TeamName
			}
			kick(ts.TaskID, e.ToolUseID, name, e.Summary, e.Time)
		case "task_start", "task_progress":
			// Orphan task path — the originating agent entry was evicted
			// from the ring buffer before the replay window. Without this
			// the dashboard sees a banner row (rebuilt from task_progress
			// via the frontend) but Linker.Query returns ok=false for the
			// task_id, so the HTTP endpoint serves 202 forever. Fast-path
			// Resolve by task_id works because Claude 2.1.132 names the
			// jsonl file after the task_id directly.
			if e.TaskID == "" || e.InternalAgentID != "" {
				continue
			}
			kick(e.TaskID, e.ToolUseID, e.Subagent, e.Summary, e.Time)
		}
	}
}

// InitLinker wires a SubagentLinker into the process. Called by Wrapper.Spawn
// once the working directory is known. Safe to call before the first system.init
// event — the Linker is context-free until SetContext fires from readLoop's
// init handler.
//
// The OnResolve callback writes the resolved (internal_agent_id, jsonl_path,
// first_prompt_id) tuple back onto the matching "agent" / "task_start"
// EventEntry so persistHistory flushes a self-contained record.
func (p *Process) InitLinker(cwd string) {
	p.cwd = cwd
	p.cachedProjectDir = resolveProjectDir(cwd)
	p.linker = NewSubagentLinker()
	// R20260603030037-GO-2 (#1661): bind the resolve worker-pool lifetime to
	// the process-scoped ctx up front, so it never captures the per-request
	// ctx of whichever DispatchResolve caller happens to fire first.
	p.linker.SetPoolContext(p.lifecycleContext())
	log := p.eventLog
	p.linker.OnResolve(func(taskID, toolUseID, internalAgentID string) {
		if toolUseID == "" || log == nil {
			return
		}
		info, _ := p.linker.Query(taskID)
		log.SetAgentInternalID(toolUseID, internalAgentID, info.JSONLPath, info.FirstPromptID)
	})
}

// Linker returns the SubagentLinker, or nil when none has been installed
// (test fakes / unusual spawn paths).
func (p *Process) Linker() *SubagentLinker {
	return p.linker
}

// EventLog returns the process's underlying *EventLog so the server-side
// tailer registry can register its SetOnAgentTaskDone callback. Returning
// the concrete type is a minor API widening but stays symmetric with
// Linker() above — both are escape hatches for tightly-coupled server
// integrations that the public method set does not otherwise expose.
func (p *Process) EventLog() *EventLog {
	return p.eventLog
}

// SetCwdForLinker plumbs the working directory into the Linker after a shim
// reconnect. SpawnReconnect does not have cwd handy (shim owns it); the
// session router provides it once the session record is re-read. Safe to
// call any time — updates projectDir and, when the reconnect handshake
// already carried a session_id, also populates parentSessionID so Resolve
// can succeed without waiting for the next live system.init event (which
// never fires when the CLI is idle post-reconnect).
func (p *Process) SetCwdForLinker(cwd string) {
	if p.linker == nil || cwd == "" {
		return
	}
	p.cwd = cwd
	projectDir := resolveProjectDir(cwd)
	p.cachedProjectDir = projectDir
	p.linker.mu.RLock()
	session := p.linker.parentSessionID
	p.linker.mu.RUnlock()
	// On reconnect the wrapper populates proc.sessionID from
	// handle.Hello.SessionID BEFORE the first live init event arrives —
	// mirror that into the linker so Resolve works immediately on
	// historical agent tasks replayed via InjectHistory. If readLoop
	// later ingests an init with a different id, the normal SetContext
	// call in the readLoop path updates it.
	//
	// Reads sessionID under p.mu via the GetSessionID accessor rather than
	// the bare field so the cross-package locking contract still holds —
	// wrapper.go's reconnect store path holds p.mu.Lock when populating
	// sessionID, GetSessionID's RLock pairs with that.
	if sid := p.SessionID(); session == "" && sid != "" {
		session = sid
	}
	p.linker.SetContext(projectDir, session)
}

// EventEntries returns a copy of all event log entries.
func (p *Process) EventEntries() []EventEntry {
	return p.eventLog.Entries()
}

// EventLastN returns the most recent n event log entries.
func (p *Process) EventLastN(n int) []EventEntry {
	return p.eventLog.LastN(n)
}

// EventLastNVisible returns a contiguous tail of the event log carrying at
// least visibleTarget visible entries (or up to maxTotal entries, whichever
// trips first). See EventLog.LastNVisible for the full contract — this is the
// thin Process-level forwarder used by the dashboard initial-subscribe path.
func (p *Process) EventLastNVisible(visibleTarget, maxTotal int) []EventEntry {
	return p.eventLog.LastNVisible(visibleTarget, maxTotal)
}

// EventEntriesSince returns event log entries after the given unix ms timestamp.
func (p *Process) EventEntriesSince(afterMS int64) []EventEntry {
	return p.eventLog.EntriesSince(afterMS)
}

// EventEntriesSinceAppend is the buffer-reusing variant of EventEntriesSince:
// it forwards to EventLog.EntriesSinceAppend so the live-session WS backfill
// path can reuse a per-subscription buffer instead of allocating a fresh
// []EventEntry per notify wave. R20260604-PERF-25 (#1740).
func (p *Process) EventEntriesSinceAppend(dst []EventEntry, afterMS int64) []EventEntry {
	return p.eventLog.EntriesSinceAppend(dst, afterMS)
}

// EventEntriesBefore returns up to `limit` event log entries strictly older
// than beforeMS, in chronological order. Used by dashboard pagination to
// load earlier pages of history.
func (p *Process) EventEntriesBefore(beforeMS int64, limit int) []EventEntry {
	return p.eventLog.EntriesBefore(beforeMS, limit)
}

// TurnAgents returns the sub-agent types spawned in the current turn.
func (p *Process) TurnAgents() []SubagentInfo {
	return p.eventLog.TurnAgents()
}

// LastActivitySummary returns the summary of the most recent tool_use/thinking
// entry, as maintained atomically by EventLog.Append.
func (p *Process) LastActivitySummary() string {
	return p.eventLog.LastActivitySummary()
}

// LastResponseSummary returns the summary of the most recent assistant "text"
// entry, as maintained atomically by EventLog.Append. Used by the dashboard
// sidebar's R110-P1 30-rune second-line preview.
func (p *Process) LastResponseSummary() string {
	return p.eventLog.LastResponseSummary()
}

// LastEventAt returns the wall-clock time of the most recent live event
// observed by this process's EventLog. Zero Time means no live event has
// landed yet. Consumed by Router.Cleanup to treat a long-running turn
// (e.g. a 20-minute code analysis) as still active as long as the CLI
// keeps emitting tool_use / thinking / assistant events, rather than
// falsely flagging it as stuck when session.lastActive was last touched
// at Send entry. Lock-free.
func (p *Process) LastEventAt() time.Time {
	return p.eventLog.LastEventAt()
}

// UserTurnCount returns the cumulative count of "user" entries this Process
// has observed since spawn. Pass-through to EventLog; consumed by
// ManagedSession.Snapshot to populate SessionSnapshot.MessageCount for
// sidebar / main-header chip display.
func (p *Process) UserTurnCount() int64 {
	return p.eventLog.UserTurnCount()
}

// SubscribeEvents returns a notification channel and unsubscribe function.
//
// Prefer SubscribeEventsTyped for new callers: it returns an
// EventSubscription value that bundles the (channel, cancel) pair so the
// channel-close contract stays internal to the cli/eventlog package rather
// than a cross-package convention each consumer must internalize
// (R246-ARCH-12 / #792).
func (p *Process) SubscribeEvents() (<-chan struct{}, func()) {
	return p.eventLog.Subscribe()
}

// SubscribeEventsTyped is the typed form of SubscribeEvents per
// R246-ARCH-12 / #792 (P0 subset). The returned EventSubscription owns
// both the notify channel and the cancel callback, so cross-package
// callers no longer need to know that the channel is closed by
// CloseSubscribers / unsub -- they just hold the value and call Cancel()
// when done.
func (p *Process) SubscribeEventsTyped() EventSubscription {
	return p.eventLog.SubscribeNew()
}
