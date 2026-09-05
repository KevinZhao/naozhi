package wsproto

import "github.com/naozhi/naozhi/internal/cli/clievent"

// Frames maps every outbound MsgType to an exemplar with every field of that
// frame set non-zero. The schema generator reflects over these to emit
// wsproto.schema.json, and the contract tests marshal them to assert the
// wire shape — one registry, both consumers, so neither can drift from the
// structs.
var Frames = map[MsgType]any{
	TypeAuthOK:   NewAuthOK(),
	TypeAuthFail: NewAuthFail(AuthFail{Error: "e", RetryAfter: 1}),
	TypePong:     NewPong(),
	TypeError:    NewError(Error{Key: "k", Error: "e", Node: "n"}),
	TypeSubscribed: NewSubscribed(Subscribed{
		Key: "k", State: "running", Reason: "r", Node: "n",
	}),
	TypeUnsubscribed: NewUnsubscribed(Unsubscribed{Key: "k", Node: "n"}),
	TypeHistory: NewHistory(History{
		Key:     "k",
		Events:  []clievent.EventEntry{{Time: 1, Type: "text"}},
		Node:    "n",
		HasMore: boolPtr(true),
		Initial: true,
	}),
	TypeEvent: NewEvent(Event{
		Key: "k", Event: &clievent.EventEntry{Time: 1, Type: "text"}, Node: "n",
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
	TypeCronRunStarted: NewCronRunStarted(CronRunStarted{
		JobID: "j", RunID: "r", StartedAt: 1, Trigger: "cron", SessionID: "s", Fresh: true,
	}),
	TypeCronRunEnded: NewCronRunEnded(CronRunEnded{
		JobID: "j", RunID: "r", State: "ok", StartedAt: 1, EndedAt: 2,
		DurationMS: 1, SessionID: "s", ErrorClass: "c", ErrorMsg: "m", Trigger: "cron",
	}),
	TypeDaemonRunStarted: NewDaemonRunStarted(DaemonRunStarted{
		Name: "d", RunID: "r", Trigger: "boot", StartedAt: 1,
	}),
	TypeDaemonRunEnded: NewDaemonRunEnded(DaemonRunEnded{
		Name: "d", RunID: "r", State: "ok", DurationMS: 1, ErrorClass: "c", Trigger: "boot",
	}),
	TypeAgentEvent: NewAgentEvent(AgentEvent{
		Key: "k", Event: &clievent.EventEntry{Time: 1, Type: "text"}, TaskID: "t",
	}),
	TypeAgentMeta: NewAgentMeta(AgentMeta{
		Key: "k", TaskID: "t",
		AgentMeta: &AgentMetaPatch{LastTool: "Read", LastDetail: "d", ToolUses: 1, DurationMS: 1},
	}),
	TypeAgentDone: NewAgentDone(AgentDone{Key: "k", Status: "ok", TaskID: "t"}),
	TypeAgentSubscribeRejected: NewAgentSubscribeRejected(AgentSubscribeRejected{
		Key: "k", Reason: "r", TaskID: "t",
	}),
}

func boolPtr(b bool) *bool { return &b }
