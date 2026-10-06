package workflow

import (
	"bytes"
	"encoding/json"
	"hash/maphash"
	"slices"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/textutil"
)

// Rune caps for model-generated and pass-through strings (RFC §5.3).
const (
	maxLabelRunes       = 120 // agent label, phase title, workflow name
	maxModelRunes       = 64
	maxLastToolRunes    = 64
	maxToolSummaryRunes = 200
	maxErrorRunes       = 400
	maxHeaderTextRunes  = 200 // Description, Current, NotifySummary
	maxRawRunes         = 32  // RawState, RawStatus
	maxPrevAgentIDs     = 8
	maxAgents           = 2000
	maxPhases           = 200
)

// redactSecrets is the redactor every clip runs; tests swap it to count calls.
var redactSecrets = textutil.RedactSecrets

var memoSeed = maphash.MakeSeed()

// clip redacts s, then truncates it: truncating first could cut a key below
// RedactSecrets' minimum tail and leak the stub.
func clip(s string, maxRunes int) string {
	if s == "" {
		return ""
	}
	return textutil.TruncateRunes(redactSecrets(s), maxRunes)
}

// memo caches one string field's normalized value keyed by the hash of its
// raw value, so a snapshot repeating a row's text costs one hash, not a
// redact. A collision only shows a stale value, never an unredacted one.
type memo struct {
	h   uint64
	set bool
	out string
}

func (m *memo) clip(s string, maxRunes int) string {
	if s == "" {
		return ""
	}
	h := maphash.String(memoSeed, s)
	if !m.set || m.h != h {
		m.h, m.set, m.out = h, true, clip(s, maxRunes)
	}
	return m.out
}

// clipRaw is clip for a JSON value of unknown type: a string is unquoted,
// null is empty, anything else is kept as its JSON text.
func (m *memo) clipRaw(raw json.RawMessage, maxRunes int) string {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	h := maphash.Bytes(memoSeed, raw)
	if !m.set || m.h != h {
		m.h, m.set, m.out = h, true, clip(rawText(raw), maxRunes)
	}
	return m.out
}

func rawText(raw json.RawMessage) string {
	var s string
	if raw[0] == '"' && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// agentMemo is a builder's per-index state: the sticky agentId history and
// the memoized strings of the row's last normalization.
type agentMemo struct {
	agentID                                  string
	prev                                     []string // shared with published rows; replaced, never mutated
	label, model, lastTool, toolSummary, err memo
}

// stick folds an observed agentId into the history: an empty one keeps the
// last, a new one pushes the last into prev.
func (am *agentMemo) stick(id string) {
	if id == "" || id == am.agentID {
		return
	}
	if am.agentID != "" {
		prev := make([]string, 0, len(am.prev)+1)
		for _, p := range am.prev {
			if p != am.agentID && p != id {
				prev = append(prev, p)
			}
		}
		prev = append(prev, am.agentID)
		if n := len(prev) - maxPrevAgentIDs; n > 0 {
			prev = prev[n:]
		}
		am.prev = prev
	} else if slices.Contains(am.prev, id) {
		am.prev = slices.DeleteFunc(slices.Clone(am.prev), func(p string) bool { return p == id })
	}
	am.agentID = id
}

// itemState normalizes an agent item's state, first match wins (RFC §4.2):
// terminal states before queued, queued before running.
func itemState(it *clievent.WorkflowItem) (AgentState, string) {
	switch {
	case it.State == "done":
		return AgentDone, ""
	case it.State == "error" && (it.Skipped || isSkippedByUser(it.Error)):
		return AgentSkipped, ""
	case it.State == "error" || it.State == "failed":
		return AgentFailed, ""
	case it.StartedAt == 0 && it.AgentID == "":
		return AgentQueued, ""
	case it.State == "start" || it.State == "progress":
		return AgentRunning, ""
	}
	return AgentUnknown, clip(it.State, maxRawRunes)
}

func isSkippedByUser(raw json.RawMessage) bool {
	return bytes.Equal(raw, []byte(`"skipped by user"`))
}

// row normalizes one agent item against its index's memo.
func (am *agentMemo) row(it *clievent.WorkflowItem) Agent {
	st, raw := itemState(it)
	am.stick(it.AgentID)
	return Agent{
		Index:           it.Index,
		PhaseIndex:      it.PhaseIndex,
		Label:           am.label.clip(it.Label, maxLabelRunes),
		AgentID:         am.agentID,
		PrevAgentIDs:    am.prev,
		Model:           am.model.clip(it.Model, maxModelRunes),
		State:           st,
		RawState:        raw,
		Blocked:         it.Blocked,
		Attempt:         it.Attempt,
		Cached:          it.Cached,
		QueuedAt:        it.QueuedAt,
		StartedAt:       it.StartedAt,
		LastProgressAt:  it.LastProgressAt,
		DurationMS:      it.DurationMs,
		Tokens:          it.Tokens,
		ToolCalls:       it.ToolCalls,
		LastTool:        am.lastTool.clip(it.LastToolName, maxLastToolRunes),
		LastToolSummary: am.toolSummary.clip(it.LastToolSummary, maxToolSummaryRunes),
		Error:           am.err.clipRaw(it.Error, maxErrorRunes),
	}
}

func (c *Counts) add(st AgentState) {
	c.Total++
	switch st {
	case AgentQueued:
		c.Queued++
	case AgentRunning:
		c.Running++
	case AgentDone:
		c.Done++
	case AgentFailed:
		c.Failed++
	case AgentSkipped:
		c.Skipped++
	case AgentStopped:
		c.Stopped++
	}
}

// snapshot is a normalized workflow_progress array.
type snapshot struct {
	phases       []Phase
	agents       []Agent // nil when rows are not kept
	counts       Counts
	capped       bool
	phasesCapped bool
	earliest     int64 // earliest agent queuedAt / startedAt; 0 = none
}

// normalizeItems turns snapshot items into phases, counts and, when
// withRows, rows keyed through memos. Indexes past the caps are dropped,
// lowest first kept; Counts still cover every agent.
func normalizeItems(items []clievent.WorkflowItem, memos map[int]*agentMemo, phaseMemos map[int]*memo, withRows bool) snapshot {
	var s snapshot
	if withRows {
		s.agents = make([]Agent, 0, len(items))
	}
	perPhase := map[int]*Counts{}
	for i := range items {
		it := &items[i]
		switch it.Type {
		case clievent.WorkflowItemPhase:
			m := phaseMemos[it.Index]
			if m == nil {
				m = &memo{}
				phaseMemos[it.Index] = m
			}
			s.phases = append(s.phases, Phase{Index: it.Index, Title: m.clip(it.Title, maxLabelRunes)})
		case clievent.WorkflowItemAgent:
			st, _ := itemState(it)
			s.counts.add(st)
			pc := perPhase[it.PhaseIndex]
			if pc == nil {
				pc = &Counts{}
				perPhase[it.PhaseIndex] = pc
			}
			pc.add(st)
			for _, t := range [...]int64{it.QueuedAt, it.StartedAt} {
				if t > 0 && (s.earliest == 0 || t < s.earliest) {
					s.earliest = t
				}
			}
			if withRows {
				am := memos[it.Index]
				if am == nil {
					am = &agentMemo{}
					memos[it.Index] = am
				}
				s.agents = append(s.agents, am.row(it))
			}
		}
	}
	sortByIndex(s.phases, func(p *Phase) int { return p.Index })
	if len(s.phases) > maxPhases {
		s.phases, s.phasesCapped = s.phases[:maxPhases:maxPhases], true
	}
	for i := range s.phases {
		if pc := perPhase[s.phases[i].Index]; pc != nil {
			s.phases[i].Counts = *pc
		}
	}
	if withRows {
		sortByIndex(s.agents, func(a *Agent) int { return a.Index })
		if len(s.agents) > maxAgents {
			s.agents, s.capped = s.agents[:maxAgents:maxAgents], true
		}
	} else if s.counts.Total > maxAgents {
		s.capped = true
	}
	return s
}

// sortByIndex sorts unless already ascending, CC's normal order.
func sortByIndex[T any](xs []T, index func(*T) int) {
	for i := 1; i < len(xs); i++ {
		if index(&xs[i-1]) > index(&xs[i]) {
			slices.SortStableFunc(xs, func(a, b T) int { return index(&a) - index(&b) })
			return
		}
	}
}

// stopAll marks a terminal workflow's queued and running agents stopped,
// in its rows and in its counts (which also cover rows past the cap).
func stopAll(w *Workflow) {
	if i := slices.IndexFunc(w.Agents, isLive); i >= 0 {
		rows := slices.Clone(w.Agents)
		for j := i; j < len(rows); j++ {
			if isLive(rows[j]) {
				rows[j].State = AgentStopped
			}
		}
		w.Agents = rows
	}
	w.Counts = w.Counts.stopped()
	if slices.ContainsFunc(w.Phases, func(p Phase) bool { return p.Queued+p.Running > 0 }) {
		phases := slices.Clone(w.Phases)
		for i := range phases {
			phases[i].Counts = phases[i].Counts.stopped()
		}
		w.Phases = phases
	}
}

func isLive(a Agent) bool { return a.State == AgentQueued || a.State == AgentRunning }

func (c Counts) stopped() Counts {
	c.Stopped += c.Queued + c.Running
	c.Queued, c.Running = 0, 0
	return c
}

// patchStatus maps task_updated.patch.status.
func patchStatus(s string) (Status, string) {
	switch s {
	case "completed", "failed", "killed", "paused", "running":
		return Status(s), ""
	case "pending":
		return StatusRunning, ""
	}
	return StatusUnknown, clip(s, maxRawRunes)
}

// notificationStatus maps task_notification.status; CC reports a killed
// run as stopped there.
func notificationStatus(s string) (Status, string) {
	switch s {
	case "completed", "failed":
		return Status(s), ""
	case "stopped":
		return StatusKilled, ""
	}
	return StatusUnknown, clip(s, maxRawRunes)
}

// resultFileStatus maps a result file's status; CC writes only these.
func resultFileStatus(s string) (Status, bool) {
	switch s {
	case "completed", "failed", "killed":
		return Status(s), true
	}
	return "", false
}
