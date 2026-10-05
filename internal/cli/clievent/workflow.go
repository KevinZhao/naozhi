// workflow.go — the Workflow tool's (ultracode) background-run fields on
// system/task_* and user frames: the per-agent snapshot CC attaches to
// task_progress, the task_updated patch and the launch receipt. Decoding
// tolerance lives in ClaudeProtocol.ReadEventInto; the rules it applies to a
// json type error are here so every reader of a snapshot shares them.
// See docs/rfc/workflow-dashboard.md §4.1.

package clievent

import (
	"encoding/json"
	"errors"
	"strings"
)

// WorkflowProgressKey is the task_progress key that carries the snapshot.
const WorkflowProgressKey = "workflow_progress"

// TaskPatch is a system/task_updated frame's patch.
type TaskPatch struct {
	Status  string `json:"status,omitempty"`
	EndTime int64  `json:"end_time,omitempty"`
}

// WorkflowLaunch is the Workflow tool_result's tool_use_result receipt
// (status "async_launched", taskType "local_workflow"). The script path is
// deliberately not kept: no field of it may reach the wire.
type WorkflowLaunch struct {
	TaskID, WorkflowName, RunID, Summary, TranscriptDir string
}

// WorkflowItem is one entry of a workflow_progress snapshot: a phase or an
// agent, identified by (Type, Index). Every field is optional on the wire.
// promptPreview / resultPreview / promptFramed are not declared, so the
// decoder skips the bulk of a snapshot instead of copying it.
type WorkflowItem struct {
	Type            string          `json:"type"`
	Index           int             `json:"index"`
	Title           string          `json:"title"`
	Label           string          `json:"label"`
	PhaseIndex      int             `json:"phaseIndex"`
	PhaseTitle      string          `json:"phaseTitle"`
	AgentID         string          `json:"agentId"`
	Model           string          `json:"model"`
	State           string          `json:"state"`
	Attempt         int             `json:"attempt"`
	Cached          bool            `json:"cached"`
	Blocked         bool            `json:"blocked"`
	Skipped         bool            `json:"skipped"`
	QueuedAt        int64           `json:"queuedAt"`
	StartedAt       int64           `json:"startedAt"`
	LastProgressAt  int64           `json:"lastProgressAt"`
	DurationMs      int64           `json:"durationMs"`
	Tokens          int64           `json:"tokens"`
	ToolCalls       int             `json:"toolCalls"`
	LastToolName    string          `json:"lastToolName"`
	LastToolSummary string          `json:"lastToolSummary"`
	Error           json.RawMessage `json:"error"`
}

// Workflow item types; any other Type is ignored by readers.
const (
	WorkflowItemPhase = "workflow_phase"
	WorkflowItemAgent = "workflow_agent"
)

// WorkflowDecode grades how an Event's WorkflowProgress decoded.
type WorkflowDecode uint8

const (
	// WorkflowDecodeOK: no snapshot, or one that decoded cleanly.
	WorkflowDecodeOK WorkflowDecode = iota
	// WorkflowDecodePartial: some non-identity field had the wrong type and
	// was zeroed; the items are usable.
	WorkflowDecodePartial
	// WorkflowDecodeFailed: the key was there but no item can be trusted;
	// WorkflowProgress is nil and readers keep their previous rows.
	WorkflowDecodeFailed
)

func (d WorkflowDecode) String() string {
	switch d {
	case WorkflowDecodeOK:
		return "ok"
	case WorkflowDecodePartial:
		return "partial"
	case WorkflowDecodeFailed:
		return "failed"
	}
	return "unknown"
}

// workflowIdentityFields are the item keys rows are matched on. A type error
// on one zeroes it in every item, so the snapshot cannot replace any row.
var workflowIdentityFields = map[string]bool{
	"type": true, "index": true, "phaseIndex": true, "agentId": true, "state": true,
}

// WorkflowDecodeFromError classifies a json.Unmarshal error of a frame whose
// Event declares WorkflowProgress. ok is false when the error is not a type
// error under workflow_progress, i.e. the frame is as broken as before the
// field existed. encoding/json reports only the first type error, so the
// caller must still re-check the rest of the frame (ReadEventInto does).
// Field is "workflow_progress.tokens" on go1.26 and "workflow_progress.0.tokens"
// on go1.27, so only its last segment is looked at.
func WorkflowDecodeFromError(err error) (d WorkflowDecode, ok bool) {
	var ute *json.UnmarshalTypeError
	if !errors.As(err, &ute) {
		return WorkflowDecodeOK, false
	}
	if ute.Field == WorkflowProgressKey {
		return WorkflowDecodeFailed, true
	}
	rest, under := strings.CutPrefix(ute.Field, WorkflowProgressKey+".")
	if !under {
		return WorkflowDecodeOK, false
	}
	last := rest[strings.LastIndexByte(rest, '.')+1:]
	if workflowIdentityFields[last] || isASCIIDigits(last) {
		return WorkflowDecodeFailed, true
	}
	return WorkflowDecodePartial, true
}

func isASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// WorkflowItemsValid reports whether items can stand in for a snapshot's
// rows: every item has a Type, agent indexes are ≥ 1 and unique, phase
// indexes are unique. It catches an identity type error hidden behind an
// earlier non-identity one (encoding/json reports only the first).
func WorkflowItemsValid(items []WorkflowItem) bool {
	// CC numbers both kinds in ascending order, so the common snapshot is
	// proven unique without a set.
	lastAgent, lastPhase, ascending := 0, -1, true
	for i := range items {
		switch it := &items[i]; it.Type {
		case "":
			return false
		case WorkflowItemAgent:
			if it.Index < 1 {
				return false
			}
			ascending = ascending && it.Index > lastAgent
			lastAgent = it.Index
		case WorkflowItemPhase:
			ascending = ascending && it.Index > lastPhase
			lastPhase = it.Index
		}
	}
	if ascending {
		return true
	}
	seen := make(map[[2]int]struct{}, len(items))
	for i := range items {
		var kind int
		switch items[i].Type {
		case WorkflowItemAgent:
			kind = 1
		case WorkflowItemPhase:
			kind = 2
		default:
			continue
		}
		k := [2]int{kind, items[i].Index}
		if _, dup := seen[k]; dup {
			return false
		}
		seen[k] = struct{}{}
	}
	return true
}
