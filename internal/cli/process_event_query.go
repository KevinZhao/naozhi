package cli

// process_event_query.go — read-only EventLog accessors, Linker lifecycle and
// InjectHistory seeding (so dashboard drill-in survives a naozhi restart).

import (
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/textutil"
)

// InjectHistory pre-populates the event log with historical entries and seeds
// the SubagentLinker so dashboard team-agent rows resume the task_id → jsonl
// mapping from a previous process lifetime (RFC v4 agent-team-ui §3.3.7).
func (p *Process) InjectHistory(entries []clievent.EventEntry) {
	// Replay path skips applyEntryStateLocked; SetPersistSink and the task-done
	// callback are wired only after/on live spawn, so no side effects (#1042).
	p.eventLog.AppendBatchReplay(entries)
	if p.linker == nil {
		return
	}
	p.linker.SeedFromHistory(entries)

	// Older-naozhi entries lack InternalAgentID / JSONLPath so SeedFromHistory
	// skips them, yet their agents may still run under the live shim. Pair agent
	// entries with task_start by ToolUseID and Resolve once per task_id.
	seen := make(map[string]struct{})
	taskStartByToolUse := make(map[string]clievent.EventEntry, len(entries))
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
		// Already resolved: skip the dispatch (Query is an O(1) RLock+map read, #478).
		if info, ok := linker.Query(taskID); ok && info.Resolved {
			return
		}
		// Cross-batch in-flight gate: adjacent InjectHistory calls (reconnect →
		// replay-then-tail) can race on a taskID; `seen` is intra-batch only (#1354).
		if !linker.TryMarkResolveInflight(taskID) {
			return
		}
		// Cap so the Resolve goroutine doesn't pin multi-KB strings while queued.
		desc = textutil.TruncateRunes(desc, EventDetailMaxRunes)
		// Bounded pool: replay can fan in dozens of task_started on reconnect (#415).
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
			// Orphan task: the agent entry was evicted from the ring before the replay
			// window; without this Linker.Query stays ok=false forever (HTTP 202).
			// Resolve by task_id works because Claude names the jsonl after it.
			if e.TaskID == "" || e.InternalAgentID != "" {
				continue
			}
			kick(e.TaskID, e.ToolUseID, e.Subagent, e.Summary, e.Time)
		}
	}
}

// InitLinker wires a SubagentLinker into the process (called by Wrapper.Spawn
// once cwd is known; the Linker is context-free until readLoop's init handler
// calls SetContext). OnResolve writes the resolved (internal_agent_id,
// jsonl_path, first_prompt_id) back onto the matching EventEntry for persistHistory.
func (p *Process) InitLinker(cwd string) {
	p.cwd = cwd
	p.cachedProjectDir = resolveProjectDir(cwd)
	p.linker = NewSubagentLinker()
	// Bind the resolve pool lifetime to the process-scoped ctx up front so it
	// never captures a DispatchResolve caller's per-request ctx (#1661).
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

// Linker returns the SubagentLinker, or nil when none is installed (test fakes).
func (p *Process) Linker() *SubagentLinker {
	return p.linker
}

// EventLog returns the underlying *EventLog so the server-side tailer registry
// can register SetOnAgentTaskDone — an escape hatch symmetric with Linker().
func (p *Process) EventLog() *EventLog {
	return p.eventLog
}

// SetCwdForLinker plumbs the working directory into the Linker after a shim
// reconnect (SpawnReconnect lacks cwd; the router supplies it once the session
// record is re-read). Also seeds parentSessionID from the reconnect handshake so
// Resolve works without a live system.init, which never fires while the CLI idles.
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
	// The wrapper sets proc.sessionID from Hello BEFORE any live init; mirror it so
	// Resolve works immediately on replayed tasks (a later init updates it via
	// SetContext). SessionID() reads under p.mu, pairing with wrapper.go's store.
	if sid := p.SessionID(); session == "" && sid != "" {
		session = sid
	}
	p.linker.SetContext(projectDir, session)
}

// EventEntries returns a copy of all event log entries.
func (p *Process) EventEntries() []clievent.EventEntry {
	return p.eventLog.Entries()
}

// EventLastN returns the most recent n event log entries.
func (p *Process) EventLastN(n int) []clievent.EventEntry {
	return p.eventLog.LastN(n)
}

// EventLastNVisible returns a contiguous tail carrying at least visibleTarget
// visible entries (or up to maxTotal); see EventLog.LastNVisible for the contract.
func (p *Process) EventLastNVisible(visibleTarget, maxTotal int) []clievent.EventEntry {
	return p.eventLog.LastNVisible(visibleTarget, maxTotal)
}

// EventEntriesSince returns event log entries after the given unix ms timestamp.
func (p *Process) EventEntriesSince(afterMS int64) []clievent.EventEntry {
	return p.eventLog.EntriesSince(afterMS)
}

// EventEntriesSinceAppend is the buffer-reusing variant of EventEntriesSince,
// so the live-session WS backfill path avoids a fresh slice per notify wave (#1740).
func (p *Process) EventEntriesSinceAppend(dst []clievent.EventEntry, afterMS int64) []clievent.EventEntry {
	return p.eventLog.EntriesSinceAppend(dst, afterMS)
}

// EventEntriesBefore returns up to `limit` entries strictly older than
// beforeMS, in chronological order (dashboard pagination).
func (p *Process) EventEntriesBefore(beforeMS int64, limit int) []clievent.EventEntry {
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
// entry (dashboard sidebar second-line preview).
func (p *Process) LastResponseSummary() string {
	return p.eventLog.LastResponseSummary()
}

// LastEventAt returns the wall-clock time of the most recent live event (zero
// if none yet). Router.Cleanup uses it to treat a long-running turn as active
// while the CLI keeps emitting events, instead of flagging it stuck because
// session.lastActive was last touched at Send entry. Lock-free.
func (p *Process) LastEventAt() time.Time {
	return p.eventLog.LastEventAt()
}

// UserTurnCount returns the cumulative count of "user" entries since spawn;
// ManagedSession.Snapshot uses it for SessionSnapshot.MessageCount.
func (p *Process) UserTurnCount() int64 {
	return p.eventLog.UserTurnCount()
}

// SubscribeEvents returns a notification channel and unsubscribe function.
// Prefer SubscribeEventsTyped for new callers: it bundles (channel, cancel) so
// the channel-close contract stays internal to the eventlog (#792).
func (p *Process) SubscribeEvents() (<-chan struct{}, func()) {
	return p.eventLog.Subscribe()
}

// SubscribeEventsTyped is the typed form of SubscribeEvents (#792): the
// EventSubscription owns both the notify channel and the cancel callback, so
// callers need not know who closes the channel — they just call Cancel().
func (p *Process) SubscribeEventsTyped() EventSubscription {
	return p.eventLog.SubscribeNew()
}
