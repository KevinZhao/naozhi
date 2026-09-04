// File eventlog_agents.go: per-turn subagent tracking — applyEntryStateLocked,
// the O(1) sidecar indexes (taskIndex / toolUseIndex / agentRingByToolUse),
// task_done callbacks, and the TurnAgents / Subagents / BgSubagents accessors.

package cli

import (
	"log/slog"
	"sync"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// subagentRef points to a SubagentInfo entry inside either turnAgents or
// bgAgents; the taskIndex sidecar uses it to skip the linear scan in
// applyEntryStateLocked's task_progress / task_done arms (#1353).
type subagentRef struct {
	background bool // true ⇒ index into bgAgents, false ⇒ turnAgents
	index      int
}

// agentRingPos pins the ring slots holding the "agent" and "task_start"
// entries for one ToolUseID so SetAgentInternalID reaches them in O(1).
// -1 means "not yet appended" (task_start lands 0-200ms after agent). Only
// these two entry types carry the linker payload (#1360).
type agentRingPos struct {
	agentIdx     int
	taskStartIdx int
}

// noAgentRingPos is the initial value for fresh map inserts: both slots unknown.
var noAgentRingPos = agentRingPos{agentIdx: -1, taskStartIdx: -1}

// SubagentInfo holds display information about an active sub-agent in the current turn.
// Everything is derived from EventEntry fields or server-side tailer state
// (enrichSnapshot); nothing is persisted independently — the ring-buffered
// EventEntry list stays canonical.
type SubagentInfo struct {
	Name       string `json:"name"`
	Activity   string `json:"activity,omitempty"`   // task description from agent event
	Background bool   `json:"background,omitempty"` // true for run_in_background agents
	TaskID     string `json:"task_id,omitempty"`
	ToolUseID  string `json:"tool_use_id,omitempty"`
	TaskType   string `json:"task_type,omitempty"`
	// InternalAgentID mirrors EventEntry.InternalAgentID once SubagentLinker
	// resolves the on-disk agent-<hex>.jsonl; empty until async Resolve
	// completes and on tombstoned tasks.
	InternalAgentID string `json:"internal_agent_id,omitempty"`
	Status          string `json:"status,omitempty"`        // "spawned" | "running" | "completed" | "error"
	StartedAtMS     int64  `json:"started_at_ms,omitempty"` // task_start wall-clock
	// Aggregator-injected (server.enrichSnapshot): LastTool/LastDetail from the
	// silent tailer; ToolUses/DurationMS from task_notification usage when
	// present, else the tailer's running counters.
	LastTool   string `json:"last_tool,omitempty"`
	LastDetail string `json:"last_detail,omitempty"`
	ToolUses   int    `json:"tool_uses,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

// pendingTaskDone is a task_done callback that applyEntryStateLocked defers
// until the caller has released l.mu, preserving Append/AppendBatch's single
// lock acquisition (firing inline and re-locking would let a concurrent
// Append interleave ring writes mid-batch).
type pendingTaskDone struct {
	TaskID string
	Status string
}

// entryAffectsAgentState reports whether applyEntryStateLocked does any work
// for this Type. Hot-path types (assistant_text / tool_use / tool_result /
// system) fall through its default arm, so callers gate on this to skip the
// dispatch under l.mu. Must stay in lockstep with the case labels below.
func entryAffectsAgentState(t string) bool {
	switch t {
	case "agent", "task_start", "task_progress", "task_done", "result", "user":
		return true
	}
	return false
}

// applyEntryStateLocked updates per-turn agent tracking for a single entry.
// Caller MUST hold l.mu. Summary atomics are the caller's job so AppendBatch
// can coalesce them into one Store. Returns (true, pending) for a "task_done"
// entry that warrants a callback; callers fire it after releasing l.mu via
// fireTaskDoneCallbacks.
func (l *EventLog) applyEntryStateLocked(e clievent.EventEntry) (fire bool, pending pendingTaskDone) {
	switch e.Type {
	case "agent":
		label := e.Subagent
		if label == "" {
			label = e.TeamName
		}
		if label == "" {
			label = "agent"
		}
		// task_start matching requires a non-empty ToolUseID; without it the
		// entry never links and Status sticks at "spawned". Warn so the
		// upstream emitter can be diagnosed.
		if e.ToolUseID == "" {
			slog.Warn("cli/eventlog: agent entry missing ToolUseID; task_start linkage will be dropped",
				"name", label,
				"background", e.Background,
				"task_type", e.TaskType,
			)
		}
		info := SubagentInfo{
			Name:       label,
			Activity:   e.Summary,
			Background: e.Background,
			ToolUseID:  e.ToolUseID,
			TaskType:   e.TaskType,
			Status:     "spawned",
		}
		if e.Background {
			l.bgAgents = append(l.bgAgents, info)
			if e.ToolUseID != "" {
				if l.toolUseIndex == nil {
					l.toolUseIndex = make(map[string]subagentRef, 8)
				}
				l.toolUseIndex[e.ToolUseID] = subagentRef{background: true, index: len(l.bgAgents) - 1}
			}
		} else {
			l.turnAgents = append(l.turnAgents, info)
			if e.ToolUseID != "" {
				if l.toolUseIndex == nil {
					l.toolUseIndex = make(map[string]subagentRef, 8)
				}
				l.toolUseIndex[e.ToolUseID] = subagentRef{background: false, index: len(l.turnAgents) - 1}
			}
		}
		l.turnAgentCount.Store(int32(len(l.turnAgents) + len(l.bgAgents)))
	case "task_start":
		// Match by ToolUseID (Agent tool_use → system.task_started carry the
		// same id). InternalAgentID is filled later by SetAgentInternalID once
		// the async linker locates the jsonl. Try the toolUseIndex sidecar
		// first and seed taskIndex; fall back to the scan when the sidecar
		// misses (e.g. agent entry arrived via history replay).
		if e.ToolUseID != "" {
			if ref, ok := l.toolUseIndex[e.ToolUseID]; ok {
				var slice []SubagentInfo
				if ref.background {
					slice = l.bgAgents
				} else {
					slice = l.turnAgents
				}
				if ref.index < len(slice) && slice[ref.index].ToolUseID == e.ToolUseID {
					slice[ref.index].TaskID = e.TaskID
					slice[ref.index].Status = "running"
					slice[ref.index].StartedAtMS = e.Time
					if e.TaskID != "" {
						if l.taskIndex == nil {
							l.taskIndex = make(map[string]subagentRef, 8)
						}
						l.taskIndex[e.TaskID] = ref
					}
					return false, pendingTaskDone{}
				}
			}
		}
		for i := range l.turnAgents {
			if l.turnAgents[i].ToolUseID != "" && l.turnAgents[i].ToolUseID == e.ToolUseID {
				l.turnAgents[i].TaskID = e.TaskID
				l.turnAgents[i].Status = "running"
				l.turnAgents[i].StartedAtMS = e.Time
				if e.TaskID != "" {
					if l.taskIndex == nil {
						l.taskIndex = make(map[string]subagentRef, 8)
					}
					l.taskIndex[e.TaskID] = subagentRef{background: false, index: i}
				}
				return false, pendingTaskDone{}
			}
		}
		for i := range l.bgAgents {
			if l.bgAgents[i].ToolUseID != "" && l.bgAgents[i].ToolUseID == e.ToolUseID {
				l.bgAgents[i].TaskID = e.TaskID
				l.bgAgents[i].Status = "running"
				l.bgAgents[i].StartedAtMS = e.Time
				if e.TaskID != "" {
					if l.taskIndex == nil {
						l.taskIndex = make(map[string]subagentRef, 8)
					}
					l.taskIndex[e.TaskID] = subagentRef{background: true, index: i}
				}
				return false, pendingTaskDone{}
			}
		}
	case "task_progress":
		// The parent stream is authoritative for totals when present. Sidecar
		// index is stable within a turn (slices only grow between resets);
		// fall back to the scan if it is stale (e.g. taskIndex reset by an
		// out-of-order result before a stray progress event).
		if ref, ok := l.taskIndex[e.TaskID]; ok && e.TaskID != "" {
			var slice []SubagentInfo
			if ref.background {
				slice = l.bgAgents
			} else {
				slice = l.turnAgents
			}
			if ref.index < len(slice) && slice[ref.index].TaskID == e.TaskID {
				if e.LastTool != "" {
					slice[ref.index].LastTool = e.LastTool
				}
				if e.ToolUses > 0 {
					slice[ref.index].ToolUses = e.ToolUses
				}
				if e.DurationMS > 0 {
					slice[ref.index].DurationMS = e.DurationMS
				}
				return false, pendingTaskDone{}
			}
		}
		for i := range l.turnAgents {
			if l.turnAgents[i].TaskID != "" && l.turnAgents[i].TaskID == e.TaskID {
				if e.LastTool != "" {
					l.turnAgents[i].LastTool = e.LastTool
				}
				if e.ToolUses > 0 {
					l.turnAgents[i].ToolUses = e.ToolUses
				}
				if e.DurationMS > 0 {
					l.turnAgents[i].DurationMS = e.DurationMS
				}
				return false, pendingTaskDone{}
			}
		}
		for i := range l.bgAgents {
			if l.bgAgents[i].TaskID != "" && l.bgAgents[i].TaskID == e.TaskID {
				if e.LastTool != "" {
					l.bgAgents[i].LastTool = e.LastTool
				}
				if e.ToolUses > 0 {
					l.bgAgents[i].ToolUses = e.ToolUses
				}
				if e.DurationMS > 0 {
					l.bgAgents[i].DurationMS = e.DurationMS
				}
				return false, pendingTaskDone{}
			}
		}
	case "task_done":
		status := e.Status
		if status == "" {
			status = "completed"
		}
		matched := false
		if ref, ok := l.taskIndex[e.TaskID]; ok && e.TaskID != "" {
			var slice []SubagentInfo
			if ref.background {
				slice = l.bgAgents
			} else {
				slice = l.turnAgents
			}
			if ref.index < len(slice) && slice[ref.index].TaskID == e.TaskID {
				slice[ref.index].Status = status
				if e.DurationMS > 0 {
					slice[ref.index].DurationMS = e.DurationMS
				}
				if e.ToolUses > 0 {
					slice[ref.index].ToolUses = e.ToolUses
				}
				delete(l.taskIndex, e.TaskID)
				matched = true
			}
		}
		if !matched {
			for i := range l.turnAgents {
				if l.turnAgents[i].TaskID != "" && l.turnAgents[i].TaskID == e.TaskID {
					l.turnAgents[i].Status = status
					if e.DurationMS > 0 {
						l.turnAgents[i].DurationMS = e.DurationMS
					}
					if e.ToolUses > 0 {
						l.turnAgents[i].ToolUses = e.ToolUses
					}
					matched = true
					break
				}
			}
		}
		if !matched {
			for i := range l.bgAgents {
				if l.bgAgents[i].TaskID != "" && l.bgAgents[i].TaskID == e.TaskID {
					l.bgAgents[i].Status = status
					if e.DurationMS > 0 {
						l.bgAgents[i].DurationMS = e.DurationMS
					}
					if e.ToolUses > 0 {
						l.bgAgents[i].ToolUses = e.ToolUses
					}
					break
				}
			}
		}
		if e.TaskID != "" {
			return true, pendingTaskDone{TaskID: e.TaskID, Status: status}
		}
		return false, pendingTaskDone{}
	case "result", "user":
		// Turn boundary. Drop backing arrays/maps that grew past a typical
		// turn so a TeamCreate fan-out doesn't pin them (and inflate every
		// later Snapshot copy); small ones are reused in place.
		const subagentTurnRetainCap = 8
		if cap(l.turnAgents) > subagentTurnRetainCap {
			l.turnAgents = nil
		} else {
			l.turnAgents = l.turnAgents[:0]
		}
		if cap(l.bgAgents) > subagentTurnRetainCap {
			l.bgAgents = nil
		} else {
			l.bgAgents = l.bgAgents[:0]
		}
		// Sidecars reset in lockstep with the slices they index.
		if len(l.taskIndex) > subagentTurnRetainCap {
			l.taskIndex = nil
		} else {
			for k := range l.taskIndex {
				delete(l.taskIndex, k)
			}
		}
		if len(l.toolUseIndex) > subagentTurnRetainCap {
			l.toolUseIndex = nil
		} else {
			for k := range l.toolUseIndex {
				delete(l.toolUseIndex, k)
			}
		}
		// Prior ring positions are dead after a turn boundary; a later
		// ToolUseID rebind lands on a fresh "agent" Append that re-seeds.
		if len(l.agentRingByToolUse) > subagentTurnRetainCap {
			l.agentRingByToolUse = nil
		} else {
			for k := range l.agentRingByToolUse {
				delete(l.agentRingByToolUse, k)
			}
		}
		// Skip the redundant atomic Store on agent-free turns.
		if l.turnAgentCount.Load() != 0 {
			l.turnAgentCount.Store(0)
		}
	}
	return false, pendingTaskDone{}
}

// SetOnAgentTaskDone installs a callback that fires when a "task_done"
// EventEntry is appended. Single subscriber: setting again replaces the
// previous callback; nil clears. Used by the server-side tailer registry to
// stop tailers once the parent stream marks a task finished. Prefer
// OnAgentTaskDone for new code (cancel-func idiom matching Subscribe).
func (l *EventLog) SetOnAgentTaskDone(fn func(taskID, status string)) {
	if fn == nil {
		l.onAgentTaskDoneFn.Store(nil)
		return
	}
	l.onAgentTaskDoneFn.Store(&fn)
}

// OnAgentTaskDone is the cancel-func form of SetOnAgentTaskDone, matching
// Subscribe's registration idiom (#802). Storage is still a single pointer,
// so semantics remain last-writer-wins. The returned cancel is idempotent;
// a nil fn is a no-op with a no-op cancel so callers can defer it blindly.
func (l *EventLog) OnAgentTaskDone(fn func(taskID, status string)) func() {
	if fn == nil {
		return func() {}
	}
	stored := &fn
	l.onAgentTaskDoneFn.Store(stored)
	var cancelOnce sync.Once
	return func() {
		cancelOnce.Do(func() {
			// CompareAndSwap so a stale cancel cannot clear a callback that a
			// later registration installed.
			l.onAgentTaskDoneFn.CompareAndSwap(stored, nil)
		})
	}
}

// loadAgentTaskDoneFn returns the current on-task-done callback (nil when
// none is wired) without taking a lock.
func (l *EventLog) loadAgentTaskDoneFn() func(taskID, status string) {
	if p := l.onAgentTaskDoneFn.Load(); p != nil {
		return *p
	}
	return nil
}

// fireTaskDoneCallbacks dispatches task_done callbacks collected under l.mu
// after the lock has been released, so a slow callback (e.g. closing 50
// tailers) cannot block concurrent Appends. Safe with an empty slice.
func (l *EventLog) fireTaskDoneCallbacks(pending []pendingTaskDone) {
	if len(pending) == 0 {
		return
	}
	fn := l.loadAgentTaskDoneFn()
	if fn == nil {
		return
	}
	for _, p := range pending {
		fn(p.TaskID, p.Status)
	}
}

// fireOneTaskDoneCallback is Append's single-entry fast path (one Event maps
// to at most one task_done), avoiding a heap-escaping one-slot slice.
func (l *EventLog) fireOneTaskDoneCallback(pending pendingTaskDone) {
	fn := l.loadAgentTaskDoneFn()
	if fn == nil {
		return
	}
	fn(pending.TaskID, pending.Status)
}

// recordAgentRingPosLocked stores the ring index of a just-appended agent /
// task_start entry so SetAgentInternalID can hop straight to it. Caller MUST
// hold l.mu. Entries without ToolUseID have no linker payload to backfill and
// are skipped. The map is lazy because most sessions never spawn an agent.
func (l *EventLog) recordAgentRingPosLocked(entryType, toolUseID string, ringIdx int) {
	if toolUseID == "" {
		return
	}
	if entryType != "agent" && entryType != "task_start" {
		return
	}
	if l.agentRingByToolUse == nil {
		l.agentRingByToolUse = make(map[string]agentRingPos, 8)
	}
	pos, ok := l.agentRingByToolUse[toolUseID]
	if !ok {
		pos = noAgentRingPos
	}
	if entryType == "agent" {
		pos.agentIdx = ringIdx
	} else {
		pos.taskStartIdx = ringIdx
	}
	l.agentRingByToolUse[toolUseID] = pos
}

// backfillSubagentInternalID writes internalAgentID into the live
// SubagentInfo for toolUseID via the toolUseIndex sidecar. Returns false when
// the sidecar lacks the key or the indexed slot does not match, and the
// caller falls back to the linear scan. Caller must hold l.mu (#1597).
func (l *EventLog) backfillSubagentInternalID(toolUseID, internalAgentID string) bool {
	ref, ok := l.toolUseIndex[toolUseID]
	if !ok {
		return false
	}
	slice := l.turnAgents
	if ref.background {
		slice = l.bgAgents
	}
	if ref.index < 0 || ref.index >= len(slice) {
		return false
	}
	if slice[ref.index].ToolUseID != toolUseID {
		return false
	}
	slice[ref.index].InternalAgentID = internalAgentID
	return true
}

// SetAgentInternalID writes the SubagentLinker-resolved linkage into the most
// recent matching "agent" / "task_start" EventEntry and the live SubagentInfo.
// Called from the Linker's OnResolve callback. All fields are written together
// so the next persist flush is a self-contained record SeedFromHistory can
// re-consume. Idempotent; a different id for the same tool_use_id overwrites.
func (l *EventLog) SetAgentInternalID(toolUseID, internalAgentID, jsonlPath, firstPromptID string) {
	if toolUseID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	// Live SubagentInfo first (Snapshot's hot read path): O(1) sidecar with
	// slot re-validation, linear scan on miss.
	if !l.backfillSubagentInternalID(toolUseID, internalAgentID) {
		for i := range l.turnAgents {
			if l.turnAgents[i].ToolUseID == toolUseID {
				l.turnAgents[i].InternalAgentID = internalAgentID
				break
			}
		}
		for i := range l.bgAgents {
			if l.bgAgents[i].ToolUseID == toolUseID {
				l.bgAgents[i].InternalAgentID = internalAgentID
				break
			}
		}
	}

	// Ring entries via the agentRingByToolUse sidecar. Re-validate
	// Type+ToolUseID at each slot so a ring rotation that overwrote the
	// original entry cannot leak the payload into an unrelated row (#1360).
	var foundAgent, foundTaskStart bool
	if pos, ok := l.agentRingByToolUse[toolUseID]; ok {
		if pos.agentIdx >= 0 && pos.agentIdx < l.maxSize {
			e := &l.entries[pos.agentIdx]
			if e.Type == "agent" && e.ToolUseID == toolUseID {
				e.InternalAgentID = internalAgentID
				e.JSONLPath = jsonlPath
				e.FirstPromptID = firstPromptID
				foundAgent = true
			}
		}
		if pos.taskStartIdx >= 0 && pos.taskStartIdx < l.maxSize {
			e := &l.entries[pos.taskStartIdx]
			if e.Type == "task_start" && e.ToolUseID == toolUseID {
				e.InternalAgentID = internalAgentID
				e.JSONLPath = jsonlPath
				e.FirstPromptID = firstPromptID
				foundTaskStart = true
			}
		}
		if foundAgent && foundTaskStart {
			return
		}
	}

	// Fallback for entries the sidecar did not pin (history replay, or a slot
	// overwritten by a burst): bounded reverse scan, stopping once both the
	// agent and task_start entries are backfilled, so the wlock is never held
	// for an O(maxSize) walk.
	start := (l.head - l.count + l.maxSize) % l.maxSize
	scanLimit := l.count
	if scanLimit > setAgentInternalIDMaxScan {
		scanLimit = setAgentInternalIDMaxScan
	}
	for i := 0; i < scanLimit; i++ {
		if foundAgent && foundTaskStart {
			break
		}
		idx := (start + l.count - 1 - i) % l.maxSize
		e := &l.entries[idx]
		if e.ToolUseID != toolUseID {
			continue
		}
		switch e.Type {
		case "agent":
			if foundAgent {
				continue
			}
			foundAgent = true
		case "task_start":
			if foundTaskStart {
				continue
			}
			foundTaskStart = true
		default:
			continue
		}
		e.InternalAgentID = internalAgentID
		e.JSONLPath = jsonlPath
		e.FirstPromptID = firstPromptID
	}
}

// TurnAgents returns a copy of all currently active agents (foreground + background)
// in the current turn, or nil when none. Both sets clear on turn boundaries.
// The atomic turnAgentCount lets the common empty read skip RLock + alloc.
func (l *EventLog) TurnAgents() []SubagentInfo {
	if l.turnAgentCount.Load() == 0 {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	nTurn := len(l.turnAgents)
	nBg := len(l.bgAgents)
	if nTurn == 0 && nBg == 0 {
		return nil
	}
	// Single-side fast paths: one copy, no merge allocation.
	if nBg == 0 {
		return append([]SubagentInfo(nil), l.turnAgents...)
	}
	if nTurn == 0 {
		return append([]SubagentInfo(nil), l.bgAgents...)
	}
	out := make([]SubagentInfo, nTurn+nBg)
	copy(out, l.turnAgents)
	copy(out[nTurn:], l.bgAgents)
	return out
}

// Subagents returns a copy of foreground turn agents only, for snapshot
// enrichment that keeps banner rows separate from long-lived [bg] tags.
func (l *EventLog) Subagents() []SubagentInfo {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if len(l.turnAgents) == 0 {
		return nil
	}
	out := make([]SubagentInfo, len(l.turnAgents))
	copy(out, l.turnAgents)
	return out
}

// BgSubagents returns a copy of background (run_in_background) turn agents.
func (l *EventLog) BgSubagents() []SubagentInfo {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if len(l.bgAgents) == 0 {
		return nil
	}
	out := make([]SubagentInfo, len(l.bgAgents))
	copy(out, l.bgAgents)
	return out
}
