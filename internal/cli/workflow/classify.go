package workflow

import "github.com/naozhi/naozhi/internal/cli/clievent"

// frameKind is the role a decoded frame plays for a workflow.
type frameKind uint8

const (
	kindNone frameKind = iota
	kindStarted
	kindProgress
	kindUpdated
	kindNotification
	kindLaunch // user frame carrying the Workflow tool's launch receipt
)

func kindOf(ev *clievent.Event) frameKind {
	switch ev.Type {
	case "system":
		if ev.TaskID == "" {
			return kindNone
		}
		switch ev.SubType {
		case "task_started":
			return kindStarted
		case "task_progress":
			return kindProgress
		case "task_updated":
			return kindUpdated
		case "task_notification":
			return kindNotification
		}
	case "user":
		if ev.WorkflowLaunch != nil && ev.WorkflowLaunch.TaskID != "" {
			return kindLaunch
		}
	}
	return kindNone
}

func taskIDOf(ev *clievent.Event, kind frameKind) string {
	if kind == kindLaunch {
		return ev.WorkflowLaunch.TaskID
	}
	return ev.TaskID
}

// IsWorkflowTask reports whether a system/task_* frame belongs to a
// local_workflow task. summary is no evidence (MCP and agent tasks set it
// too), nor is the task id's shape. Observe stores the verdict in
// ev.WorkflowTask.
func (t *Tracker) IsWorkflowTask(ev *clievent.Event) bool {
	kind := kindOf(ev)
	if kind == kindNone || kind == kindLaunch {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.isWorkflowLocked(ev, kind)
}

// isWorkflowLocked applies RFC §5.7's rules in order: subagent_type rules
// a task out; a local_workflow task_started, a task already judged or
// handed in as known, an existing entry, or a workflow_progress key (even
// one that failed to decode) rules it in; anything else is not a workflow.
func (t *Tracker) isWorkflowLocked(ev *clievent.Event, kind frameKind) bool {
	if ev.SubagentType != "" {
		return false
	}
	if kind == kindStarted && ev.TaskType == clievent.TaskTypeWorkflow {
		return true
	}
	if t.ids[ev.TaskID]&flagWorkflow != 0 || t.builders[ev.TaskID] != nil {
		return true
	}
	return ev.WorkflowProgress != nil || ev.WorkflowDecode == clievent.WorkflowDecodeFailed
}
