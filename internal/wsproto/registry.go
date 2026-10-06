package wsproto

import (
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/cli/workflow"
)

// Frames maps every outbound MsgType to an exemplar with every field of that
// frame set non-zero. The schema generator reflects over these to emit
// wsproto.schema.json, and the contract tests marshal them to assert the
// wire shape — one registry, both consumers, so neither can drift from the
// structs.
var Frames = map[MsgType]any{
	TypeAuthOK:   NewAuthOK(AuthOK{AssetVersion: "v"}),
	TypeAuthFail: NewAuthFail(AuthFail{Error: "e", RetryAfter: 1}),
	TypePong:     NewPong(),
	TypeError:    NewError(Error{Key: "k", Error: "e", Node: "n"}),
	TypeSubscribed: NewSubscribed(Subscribed{
		Key: "k", State: "running", Reason: "r", Node: "n",
	}),
	TypeUnsubscribed: NewUnsubscribed(Unsubscribed{Key: "k", Node: "n"}),
	TypeHistory: NewHistory(History{
		Key:     "k",
		Events:  []clievent.EventEntry{{Time: 1, Type: clievent.KindText}},
		Node:    "n",
		HasMore: boolPtr(true),
		Initial: true,
	}),
	TypeEvent: NewEvent(Event{
		Key: "k", Event: &clievent.EventEntry{Time: 1, Type: clievent.KindText}, Node: "n",
	}),
	TypeSendAck: NewSendAck(SendAck{
		Key: "k", ID: "i", Status: "accepted", Error: "e", Node: "n",
	}),
	TypeSendError: NewSendError(SendError{Key: "k", Error: "e", Node: "n"}),
	TypeInterruptAck: NewInterruptAck(InterruptAck{
		Key: "k", ID: "i", Status: "ok", Error: "e", Node: "n",
	}),
	TypeSessionState: NewSessionState(SessionState{
		Key: "k", State: "ready", Reason: "r", Node: "n",
	}),
	TypeSessionsUpdate: NewSessionsUpdate(),
	TypeRunStarted: NewRunStarted(RunStarted{
		Subsystem: "cron", OwnerID: "o", RunID: "r", StartedAt: 1,
		Trigger: "cron", SessionID: "s", Fresh: true,
	}),
	TypeRunEnded: NewRunEnded(RunEnded{
		Subsystem: "cron", OwnerID: "o", RunID: "r", State: "ok", StartedAt: 1,
		EndedAt: 2, DurationMS: 1, SessionID: "s", ErrorClass: "c", ErrorMsg: "m", Trigger: "cron",
	}),
	TypeAgentEvent: NewAgentEvent(AgentEvent{
		Key: "k", Event: &clievent.EventEntry{Time: 1, Type: clievent.KindText}, TaskID: "t",
	}),
	TypeAgentMeta: NewAgentMeta(AgentMeta{
		Key: "k", TaskID: "t",
		AgentMeta: &AgentMetaPatch{LastTool: "Read", LastDetail: "d", ToolUses: 1, DurationMS: 1},
	}),
	TypeAgentDone: NewAgentDone(AgentDone{Key: "k", Status: "ok", TaskID: "t"}),
	TypeAgentSubscribeRejected: NewAgentSubscribeRejected(AgentSubscribeRejected{
		Key: "k", Reason: "r", TaskID: "t",
	}),
	TypeWorkflowState: NewWorkflowState(WorkflowState{
		Key: "k", Node: "n", TaskID: "t", Epoch: "e", Version: 2, BaseVersion: 1, Full: true,
		ServerNow: 1, RowsOmitted: 1, Workflow: exemplarWorkflow(),
	}),
	TypeWorkflowSet: NewWorkflowSet(WorkflowSet{
		Key: "k", Node: "n", Epoch: "e", TaskIDs: []string{"t"}, ServerNow: 1,
	}),
}

// exemplarWorkflow is a WireView with every field set.
func exemplarWorkflow() workflow.WireView {
	counts := workflow.Counts{Total: 1, Queued: 1, Running: 1, Done: 1, Failed: 1, Skipped: 1, Stopped: 1}
	return workflow.WireView{
		TaskID: "t", RunID: "r", Name: "n", Description: "d", Current: "c",
		Status: workflow.StatusRunning, RawStatus: "r", StartedAt: 1, EndedAt: 1, LastObservedAt: 1,
		Tokens: 1, ToolCalls: 1, DurationMS: 1, Counts: counts,
		Phases: []workflow.Phase{{Index: 1, Title: "p", Counts: counts}},
		Agents: []workflow.Agent{{
			Index: 1, PhaseIndex: 1, Label: "l", AgentID: "a", PrevAgentIDs: []string{"a0"}, Model: "m",
			State: workflow.AgentRunning, RawState: "r", Blocked: true, Attempt: 1, Cached: true,
			QueuedAt: 1, StartedAt: 1, LastProgressAt: 1, DurationMS: 1, Tokens: 1, ToolCalls: 1,
			LastTool: "t", LastToolSummary: "s", Error: "e", Rev: 1,
		}},
		AgentsCapped: true, NotifySummary: "s", Source: workflow.SourceStream, Degraded: "d", Version: 2,
	}
}

func boolPtr(b bool) *bool { return &b }
