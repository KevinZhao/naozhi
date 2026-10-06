package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/textutil"
)

// Result cache bounds (RFC §6.2.1).
const (
	maxResultBytes  = 16 << 10
	maxLogLineRunes = 500
	maxLogLines     = 200
	maxLogBytes     = 64 << 10
)

// ResultFile is the slim form of <session>/workflows/<runId>.json that CC
// writes when an attempt ends: script, args and previews are not declared,
// and items share the stream snapshot's type.
type ResultFile struct {
	TaskID           string                  `json:"taskId"`
	Status           string                  `json:"status"`
	StartTime        int64                   `json:"startTime"`
	Result           json.RawMessage         `json:"result"`
	Logs             []string                `json:"logs"`
	TotalTokens      int64                   `json:"totalTokens"`
	TotalToolCalls   int                     `json:"totalToolCalls"`
	DurationMs       int64                   `json:"durationMs"`
	Phases           []resultPhase           `json:"phases"`
	WorkflowProgress []clievent.WorkflowItem `json:"workflowProgress"`
}

type resultPhase struct {
	Title string `json:"title"`
}

// ParseResultFile decodes a result file. A type error inside
// workflowProgress drops the items (the stream rows stay) instead of the
// file; so do items that fail the identity check. Any other error fails.
func ParseResultFile(data []byte) (*ResultFile, error) {
	var rf ResultFile
	err := json.Unmarshal(data, &rf)
	if err != nil {
		var ute *json.UnmarshalTypeError
		if !errors.As(err, &ute) || !strings.HasPrefix(ute.Field, "workflowProgress") {
			return nil, err
		}
		// encoding/json reports only the first type error: decode again with
		// the items skipped, so an error elsewhere still fails the file.
		var shadow struct {
			ResultFile
			WorkflowProgress json.RawMessage `json:"workflowProgress"`
		}
		if err := json.Unmarshal(data, &shadow); err != nil {
			return nil, err
		}
		rf, rf.WorkflowProgress = shadow.ResultFile, nil
	}
	if !clievent.WorkflowItemsValid(rf.WorkflowProgress) {
		rf.WorkflowProgress = nil
	}
	return &rf, nil
}

// ResultCache is a terminal workflow's result and logs, redacted and
// bounded, as the HTTP endpoint serves them. Immutable.
type ResultCache struct {
	Result          string
	ResultTruncated bool
	Logs            []string // oldest first
	LogsTruncated   bool
}

// NewResultCache trims a result file's result to 16KB of JSON text and its
// logs to the newest that fit 200 lines and 64KiB, each line ≤ 500 runes.
func NewResultCache(rf *ResultFile) *ResultCache {
	c := &ResultCache{}
	if raw := bytes.TrimSpace(rf.Result); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		var buf bytes.Buffer
		text := string(raw)
		if json.Compact(&buf, raw) == nil {
			text = buf.String()
		}
		text = redactSecrets(text)
		if cut := textutil.TruncateAtRuneBoundary(text, maxResultBytes); cut < len(text) {
			text, c.ResultTruncated = text[:cut], true
		}
		c.Result = text
	}
	size := 0
	for i := len(rf.Logs) - 1; i >= 0; i-- {
		line := clip(rf.Logs[i], maxLogLineRunes)
		if len(c.Logs) == maxLogLines || size+len(line) > maxLogBytes {
			c.LogsTruncated = true
			break
		}
		size += len(line)
		c.Logs = append(c.Logs, line)
	}
	for i, j := 0, len(c.Logs)-1; i < j; i, j = i+1, j-1 {
		c.Logs[i], c.Logs[j] = c.Logs[j], c.Logs[i]
	}
	return c
}

// MergeResultFile returns w with rf merged when rf.TaskID names it, and
// false (w unchanged) otherwise. The board uses it for entries no live
// Tracker holds; Tracker.ApplyResultFile shares the implementation.
func MergeResultFile(w *Workflow, rf *ResultFile) (*Workflow, bool) {
	memos := make(map[int]*agentMemo, len(w.Agents))
	for i := range w.Agents {
		a := &w.Agents[i]
		memos[a.Index] = &agentMemo{agentID: a.AgentID, prev: a.PrevAgentIDs}
	}
	return mergeResult(w, rf, memos, map[int]*memo{})
}

// mergeResult overlays a result file: status, totals and StartedAt from
// the file, rows merged by index (file rows win, others stay), phases from
// the file when it lists any. A terminal status stops the remaining rows.
func mergeResult(w *Workflow, rf *ResultFile, memos map[int]*agentMemo, phaseMemos map[int]*memo) (*Workflow, bool) {
	if rf == nil || rf.TaskID == "" || rf.TaskID != w.TaskID {
		return w, false
	}
	n := *w
	n.Status, n.RawStatus = resultFileStatus(rf.Status)
	setStarted(&n, rf.StartTime, StartedFromResultFile)
	if n.EndedAt == 0 && IsTerminal(n.Status) && rf.StartTime > 0 {
		n.EndedAt = rf.StartTime + rf.DurationMs
	}
	n.Tokens, n.ToolCalls, n.DurationMS = rf.TotalTokens, rf.TotalToolCalls, rf.DurationMs

	s := normalizeItems(rf.WorkflowProgress, memos, phaseMemos, true)
	phases := s.phases
	if len(phases) == 0 && len(rf.Phases) > 0 {
		phases = make([]Phase, 0, min(len(rf.Phases), maxPhases))
		for i := 0; i < len(rf.Phases) && i < maxPhases; i++ {
			phases = append(phases, Phase{Index: i + 1, Title: clip(rf.Phases[i].Title, maxLabelRunes)})
		}
		s.phasesCapped = len(rf.Phases) > maxPhases
	}
	if len(phases) == 0 {
		phases = n.Phases
	}
	rows := mergeRows(n.Agents, s.agents)
	capped := n.AgentsCapped || s.capped
	if len(rows) > maxAgents {
		rows, capped = rows[:maxAgents:maxAgents], true
	}
	n.Agents, n.AgentsCapped = rows, capped
	n.Counts, n.Phases = recount(rows, phases)
	if capped && s.counts.Total > n.Counts.Total {
		n.Counts = s.counts
	}
	if IsTerminal(n.Status) {
		stopAll(&n)
	}
	n.Source, n.ResultLoaded = SourceResultFile, true
	n.Degraded = ""
	if s.phasesCapped {
		n.Degraded = DegradedPhasesCapped
	}
	return &n, true
}

// mergeRows unions two index-sorted row sets; on a shared index, upd wins.
func mergeRows(base, upd []Agent) []Agent {
	if len(upd) == 0 {
		return base
	}
	out := make([]Agent, 0, max(len(base), len(upd)))
	i, j := 0, 0
	for i < len(base) || j < len(upd) {
		switch {
		case j == len(upd) || (i < len(base) && base[i].Index < upd[j].Index):
			out = append(out, base[i])
			i++
		case i == len(base) || upd[j].Index < base[i].Index:
			out = append(out, upd[j])
			j++
		default:
			out = append(out, upd[j])
			i, j = i+1, j+1
		}
	}
	return out
}

// recount derives Counts and the phases' counts from rows.
func recount(rows []Agent, phases []Phase) (Counts, []Phase) {
	var c Counts
	per := make(map[int]*Counts, len(phases))
	out := make([]Phase, len(phases))
	for i, p := range phases {
		out[i] = Phase{Index: p.Index, Title: p.Title}
		per[p.Index] = &out[i].Counts
	}
	for i := range rows {
		c.add(rows[i].State)
		if pc := per[rows[i].PhaseIndex]; pc != nil {
			pc.add(rows[i].State)
		}
	}
	return c, out
}
