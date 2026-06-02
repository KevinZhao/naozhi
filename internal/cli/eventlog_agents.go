// File eventlog_agents.go: per-turn subagent tracking — applyEntryStateLocked, the O(1) sidecar
// indexes (taskIndex / toolUseIndex / agentRingByToolUse), task_done
// callbacks, and the TurnAgents / Subagents / BgSubagents accessors.
// Split from eventlog.go per docs/rfc/eventlog-split.md (ARCH-EVENTLOG-SPLIT);
// the EventLog struct and constructor live in eventlog.go.

package cli

import (
	"log/slog"
	"sync"
)

// subagentRef points to a SubagentInfo entry inside either turnAgents or
// bgAgents. The taskIndex sidecar uses this to skip the O(N) range-scan in
// applyEntryStateLocked's task_progress / task_done arms when a TeamCreate
// fan-out has spawned 8+ subagents emitting 5+ progress events/s each.
// R260528-PERF-6 (#1353).
type subagentRef struct {
	background bool // true ⇒ index into bgAgents, false ⇒ turnAgents
	index      int
}

// agentRingPos pins the ring-buffer slots that hold the "agent" and
// "task_start" entries for one ToolUseID, so SetAgentInternalID can reach
// them in O(1). -1 means "not yet appended" (the agent entry typically
// lands first; task_start arrives 0-200ms later via system.task_started).
// The pair is enough because SetAgentInternalID writes exactly those two
// entry types — no other ring entry needs the InternalAgentID/JSONLPath/
// FirstPromptID linkage. R260528-PERF-22 (#1360).
type agentRingPos struct {
	agentIdx     int
	taskStartIdx int
}

// noAgentRingPos is the zero value used for fresh map inserts: both
// slots unknown.
var noAgentRingPos = agentRingPos{agentIdx: -1, taskStartIdx: -1}

// SubagentInfo holds display information about an active sub-agent in the current turn.
// Fields below "Background" are added by RFC v4 agent-team-ui §3.2.2 to surface
// per-agent linkage (task_id/tool_use_id), lifecycle status, and aggregator
// metrics. All values are derived from EventEntry fields or server-side tailer
// state (§3.5.4 enrichSnapshot); none are persisted independently — the
// canonical source remains the ring-buffered EventEntry list.
type SubagentInfo struct {
	Name       string `json:"name"`
	Activity   string `json:"activity,omitempty"`   // task description from agent event
	Background bool   `json:"background,omitempty"` // true for run_in_background agents
	TaskID     string `json:"task_id,omitempty"`
	ToolUseID  string `json:"tool_use_id,omitempty"`
	TaskType   string `json:"task_type,omitempty"`
	// InternalAgentID mirrors EventEntry.InternalAgentID once SubagentLinker
	// resolves the task_id → on-disk agent-<hex>.jsonl mapping. Empty before
	// async Resolve completes (~0.1-3s grace) and on tombstoned tasks.
	InternalAgentID string `json:"internal_agent_id,omitempty"`
	Status          string `json:"status,omitempty"`        // "spawned" | "running" | "completed" | "error"
	StartedAtMS     int64  `json:"started_at_ms,omitempty"` // task_start wall-clock
	// Aggregator-injected fields (server.enrichSnapshot). LastTool/LastDetail
	// come from the silent tailer's parse of the agent transcript; ToolUses
	// and DurationMS use task_notification's usage payload when present,
	// otherwise the tailer's running counters.
	LastTool   string `json:"last_tool,omitempty"`
	LastDetail string `json:"last_detail,omitempty"`
	ToolUses   int    `json:"tool_uses,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

// pendingTaskDone captures a task_done callback invocation that
// applyEntryStateLocked wants to run *after* the caller has released l.mu.
// Deferring the dispatch keeps Append / AppendBatch's "one lock acquisition
// per call" contract intact — firing inline and re-acquiring would let a
// concurrent Append slip between batch entries and interleave ring-buffer
// writes. R201-CRIT-1.
type pendingTaskDone struct {
	TaskID string
	Status string
}

// applyEntryStateLocked updates per-turn agent tracking for a single entry.
// Caller MUST hold l.mu. Summary atomic writes are the caller's responsibility
// so that AppendBatch can coalesce multiple per-type updates into one Store.
//
// Returns (true, pending) when the entry is a "task_done" event that warrants
// an external callback dispatch; callers should accumulate pending patches
// and fire them after releasing l.mu via fireTaskDoneCallbacks.
// entryAffectsAgentState reports whether an entry's Type causes
// applyEntryStateLocked to perform any work. The hot path is dominated
// by `assistant_text` / `tool_use` / `tool_result` / `system` events
// which fall through the switch's default arm with zero work; gating
// the call site on this predicate avoids the O(N) turnAgents/bgAgents
// scans that the default arm would still trigger inside the switch
// dispatch when called per-entry under l.mu (R240-PERF-3 / R240-PERF-2
// — the AppendBatch replay path runs 500-entry InjectHistory bursts
// where typically <5% are agent-state events). The predicate must
// stay in lockstep with applyEntryStateLocked's case labels.
func entryAffectsAgentState(t string) bool {
	switch t {
	case "agent", "task_start", "task_progress", "task_done", "result", "user":
		return true
	}
	return false
}

func (l *EventLog) applyEntryStateLocked(e EventEntry) (fire bool, pending pendingTaskDone) {
	switch e.Type {
	case "agent":
		label := e.Subagent
		if label == "" {
			label = e.TeamName
		}
		if label == "" {
			label = "agent"
		}
		// task_start matching below requires a non-empty ToolUseID
		// (`l.turnAgents[i].ToolUseID != ""` guard at line ~793). In
		// production Claude CLI always emits ToolUseID with the agent
		// tool_use; an empty value here means the entry will live in
		// turnAgents/bgAgents but never link to its task_start, leaving
		// Status stuck at "spawned" indefinitely. Surface this as a
		// warn so the upstream emitter can be diagnosed (R20260527-COR-14).
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
		// task_started arrives 0-200ms after the "agent" tool_use. Match
		// by ToolUseID (authoritative; Agent tool_use → system.task_started
		// carries the same id). RFC §3.2 deliberately skips InternalAgentID
		// here — SubagentLinker.Resolve is async and fills it via
		// SetAgentInternalID below once the on-disk jsonl is located.
		//
		// R260528-PERF-6 (#1353): seed taskIndex sidecar so the
		// task_progress/task_done arms can find the SubagentInfo by TaskID
		// without scanning turnAgents/bgAgents linearly.
		// R240-PERF-2 (#1041): same ToolUseID→ref sidecar avoids the
		// linear scan here too. Falls through to the legacy scan when the
		// sidecar misses (e.g. agent entry was injected via AppendBatch
		// history replay before the sidecar was populated).
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
		// Update live counters from the parent stream. Aggregator in
		// agent_tailer.go may also push meta, but the parent stream is
		// authoritative for totals when present.
		//
		// R260528-PERF-6 (#1353): O(1) sidecar lookup. Ref index is stable
		// for the turn — turnAgents/bgAgents only grow via append between
		// result/user resets, never reorder. Falls through to the linear
		// scan if the sidecar is stale (e.g. taskIndex reset by an
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
		// Fallback linear scan preserves byte-identical behaviour for
		// out-of-order progress events whose task_start did not seed the
		// sidecar (TaskID then was "", or already cleared on result reset).
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
		// R260528-PERF-6 (#1353): O(1) sidecar lookup before the scan.
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
		// R230-PERF-5: a turn that spawned dozens of subagents (e.g. a
		// TeamCreate fan-out) inflates the backing array; subsequent
		// SnapshotTurnAgents copies pay len*sizeof on every Snapshot even
		// when the live count is zero. Drop the array when it grew past a
		// typical-turn threshold so the next turn re-grows from scratch.
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
		// R260528-PERF-6 (#1353) / R240-PERF-2 (#1041): reset sidecars
		// in lockstep with the slices they index. Drop the map when it
		// grew past the typical turn cap so a fan-out turn doesn't pin
		// the bucket array; small maps reuse via Go-1.21+ runtime mapclear.
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
		// R260528-PERF-22 (#1360): reset alongside sibling sidecars.
		// A turn boundary makes prior ring positions semantically dead —
		// any later ToolUseID rebind would land on a fresh "agent"
		// Append that re-seeds the entry above. Drop the map when it
		// grew past the typical fan-out cap so a TeamCreate with 8+
		// subagents doesn't pin the bucket array indefinitely.
		if len(l.agentRingByToolUse) > subagentTurnRetainCap {
			l.agentRingByToolUse = nil
		} else {
			for k := range l.agentRingByToolUse {
				delete(l.agentRingByToolUse, k)
			}
		}
		// Most non-agent turns leave turnAgentCount at zero already;
		// skipping the redundant atomic Store avoids cache-coherence
		// traffic on every result event in agent-free workloads.
		// (R227-PERF-14)
		if l.turnAgentCount.Load() != 0 {
			l.turnAgentCount.Store(0)
		}
	}
	return false, pendingTaskDone{}
}

// SetOnAgentTaskDone installs a callback that fires when a "task_done"
// EventEntry is appended. Atomic store — multiple subscribers are
// forbidden (setting a second time replaces the first). Used by the
// server-side tailer registry to stop tailers promptly once the parent
// stream marks an agent task finished. nil clears.
//
// Prefer OnAgentTaskDone for new code: it returns a cancel func that
// matches the Subscribe() idiom, so callback registration and channel
// subscription share one mental model (R246-ARCH-20 / #802 P0 subset).
// The set/clear semantics are unchanged here -- a future PR can promote
// the field to a slice + EventFilter and unify the dispatch path itself.
func (l *EventLog) SetOnAgentTaskDone(fn func(taskID, status string)) {
	if fn == nil {
		l.onAgentTaskDoneFn.Store(nil)
		return
	}
	l.onAgentTaskDoneFn.Store(&fn)
}

// OnAgentTaskDone is the cancel-func form of SetOnAgentTaskDone per
// R246-ARCH-20 / #802 (P0 subset). Registration shape now matches
// Subscribe(): both return a cancel func that detaches the consumer
// without the caller needing to remember "pass nil to clear".
//
// Multi-subscriber semantics still resolve to last-writer-wins because
// the underlying single-pointer storage is unchanged in this P0 step --
// the goal here is to unify the *registration idiom* so the eventlog
// API surface stops asking callers to choose between Set/clear-via-nil
// (callback channel) vs Subscribe/cancel (notification channel). A
// follow-up PR can promote the storage to a []func + filter and the
// idiom does not have to change again.
//
// The returned cancel is idempotent. If fn is nil, the call is a no-op
// and the returned cancel is also a no-op (mirrors Subscribe's pre-
// closed-channel-on-CloseSubscribers branch in spirit: callers can
// safely defer cancel without conditional checks).
func (l *EventLog) OnAgentTaskDone(fn func(taskID, status string)) func() {
	if fn == nil {
		return func() {}
	}
	stored := &fn
	l.onAgentTaskDoneFn.Store(stored)
	var cancelOnce sync.Once
	return func() {
		cancelOnce.Do(func() {
			// CompareAndSwap so a later SetOnAgentTaskDone / OnAgentTaskDone
			// caller's installed pointer is not clobbered by a stale cancel
			// from this registration. Last-writer-wins is preserved without
			// the cancel func acting as a delayed nil-clear on whatever the
			// next consumer just installed.
			l.onAgentTaskDoneFn.CompareAndSwap(stored, nil)
		})
	}
}

// loadAgentTaskDoneFn returns the current on-task-done callback so the
// dispatch loops (single + batch) below can read it without taking a
// lock. Returns nil when no callback is wired — callers must treat
// that as a no-op. R233B-PERF-6.
func (l *EventLog) loadAgentTaskDoneFn() func(taskID, status string) {
	if p := l.onAgentTaskDoneFn.Load(); p != nil {
		return *p
	}
	return nil
}

// fireTaskDoneCallbacks dispatches previously-collected task_done callbacks
// outside l.mu. Append/AppendBatch accumulate pendingTaskDone entries while
// holding l.mu, release the lock cleanly, and then call this helper — so a
// slow callback (e.g. tailer registry closing 50 tailers) cannot block
// concurrent Appends or interleave ring-buffer writes. R201-CRIT-1.
//
// Safe to call with an empty slice; common case on non-task_done appends.
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

// fireOneTaskDoneCallback is the single-entry fast path used by Append's
// hot path to avoid a one-slot slice literal escape. Append observes at
// most one pending task_done per call (a single Event maps to one
// EventEntry), so the batch-shaped helper above is unnecessary overhead
// here. AppendBatch keeps using the slice variant because it accumulates
// across multi-entry batches. R224-PERF-1 / R232-CR-16.
func (l *EventLog) fireOneTaskDoneCallback(pending pendingTaskDone) {
	fn := l.loadAgentTaskDoneFn()
	if fn == nil {
		return
	}
	fn(pending.TaskID, pending.Status)
}

// recordAgentRingPosLocked stores the ring index of an agent / task_start
// entry that was just appended (slot = ringIdx) so SetAgentInternalID can
// hop straight to it. Caller MUST hold l.mu and have already advanced
// l.head past the slot. Skips entries with empty ToolUseID (those have no
// linker-resolved payload to backfill anyway). R260528-PERF-22 (#1360).
//
// The map is created lazily because most sessions never spawn an agent;
// allocating an empty map per EventLog would burn ~64B per idle session
// across the 50-500-session deployments naozhi targets.
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

// SetAgentInternalID writes the SubagentLinker-resolved linkage back into
// the most recent matching "agent" / "task_start" EventEntry and the live
// SubagentInfo. Called from the Linker's OnResolve callback.
//
// All four fields are written together so persistHistory's next flush captures
// a self-contained record that SeedFromHistory can re-consume on restart
// (RFC v4 §3.3.7). Idempotent: repeated calls with the same values are no-ops;
// distinct internal_agent_id for the same tool_use_id overwrites (Resolve
// should never produce divergent values for the same tool_use_id, but the
// guard keeps the state machine simple if it ever does).
// backfillSubagentInternalID writes internalAgentID into the live
// SubagentInfo for toolUseID via the O(1) toolUseIndex sidecar. Returns true
// when the indexed slot validated and was written; false (caller falls back to
// the linear scan) when the sidecar lacks the key or the indexed slot no longer
// matches the ToolUseID. Callers must hold l.mu. R164029-PERF-7 (#1597).
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

func (l *EventLog) SetAgentInternalID(toolUseID, internalAgentID, jsonlPath, firstPromptID string) {
	if toolUseID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	// Backfill live SubagentInfo first (hot read path for Snapshot).
	//
	// R164029-PERF-7 (#1597): the toolUseIndex sidecar (keyed by ToolUseID,
	// populated on the "agent" Append) gives an O(1) slot lookup, so a
	// TeamCreate fan-out's per-resolve OnResolve callback no longer walks
	// turnAgents+bgAgents linearly under wlock while readLoop's hot Append
	// path queues behind it. Re-validate ToolUseID at the indexed slot (the
	// slices only grow within a turn and the sidecar is reset on the same
	// turn boundary, but the guard keeps a stale index from corrupting an
	// unrelated row) and fall back to the linear scan on any miss.
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

	// R260528-PERF-22 (#1360): O(1) ring-slot lookup via the
	// agentRingByToolUse sidecar. Every "agent" and "task_start"
	// Append/AppendBatch call records its ring index here, so the
	// linker's OnResolve callback no longer walks up to 50 ring slots
	// under wlock per resolve. A TeamCreate fan-out with 8 subagents
	// previously paid 8×O(50)=400 slot reads holding the write lock;
	// this collapses to two direct slot writes. The legacy bounded
	// scan stays as the fallback path so callers that lost the sidecar
	// (entry replayed via injectHistory before this PR's deploy, or
	// the rare ring-rotation case where the agent slot was overwritten
	// before resolve) keep working unchanged.
	var foundAgent, foundTaskStart bool
	if pos, ok := l.agentRingByToolUse[toolUseID]; ok {
		if pos.agentIdx >= 0 && pos.agentIdx < l.maxSize {
			e := &l.entries[pos.agentIdx]
			// Re-validate Type+ToolUseID at the indexed slot so a
			// concurrent ring rotation that overwrote the original
			// "agent" entry with an unrelated event cannot leak the
			// linker payload into the wrong row.
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

	// Fallback: bounded reverse scan for entries the sidecar did not
	// pin (e.g. a legacy persisted-history replay before the sidecar
	// was wired, or a stale entry whose ring slot got overwritten by
	// a turn-spanning event burst). R225-PERF-13: cap at
	// setAgentInternalIDMaxScan and break once both expected entries
	// (one "agent" + one "task_start" with this ToolUseID) have been
	// backfilled so the wlock isn't held for an O(maxSize) walk while
	// every Append call queues behind it.
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
// in the current turn. Both are cleared on turn boundaries (result/user events).
// Returns nil when no agents are active.
//
// Fast path: most sessions have no active sub-agents at any given time, so
// the atomic turnAgentCount lets Snapshot skip the RLock + 0-length slice
// allocation on the common empty read. R220-PERF-6.
func (l *EventLog) TurnAgents() []SubagentInfo {
	if l.turnAgentCount.Load() == 0 {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	total := len(l.turnAgents) + len(l.bgAgents)
	if total == 0 {
		return nil
	}
	out := make([]SubagentInfo, total)
	copy(out, l.turnAgents)
	copy(out[len(l.turnAgents):], l.bgAgents)
	return out
}

// Subagents returns a copy of foreground turn agents only. Used by dashboard
// snapshot enrichment (server.enrichSnapshot) where banner solo/team rows
// need to stay separated from long-lived [bg] tags. Tests also use this to
// pin per-agent lifecycle state without the foreground/background merge.
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
// Symmetric with Subagents — see that method's doc for rationale.
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
