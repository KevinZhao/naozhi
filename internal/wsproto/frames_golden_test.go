package wsproto_test

import (
	"encoding/json"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/wsproto"
)

func marshal(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return string(data)
}

// TestFrames_ByteIdenticalToLegacyServerMsg pins every wsproto frame's JSON
// against the legacy node.ServerMsg union carrying the same values.
// Byte identity is not a wire requirement (the browser JSON.parses, the
// relay forwards raw bytes either way) — it is the mechanical proof that the
// ~100 construction-site rewrites of #2535 changed nothing on the wire.
func TestFrames_ByteIdenticalToLegacyServerMsg(t *testing.T) {
	t.Parallel()
	ev := &clievent.EventEntry{Time: 42, Type: "text", Summary: "s"}
	events := []clievent.EventEntry{{Time: 1, Type: "text"}, {Time: 2, Type: "tool_use"}}
	hasMore := true

	cases := []struct {
		name   string
		frame  any
		legacy node.ServerMsg
	}{
		{"auth_ok", wsproto.NewAuthOK(), node.ServerMsg{Type: "auth_ok"}},
		{"auth_fail_rate_limited",
			wsproto.NewAuthFail(wsproto.AuthFail{Error: "rate limited", RetryAfter: 60}),
			node.ServerMsg{Type: "auth_fail", Error: "rate limited", RetryAfter: 60}},
		{"pong", wsproto.NewPong(), node.ServerMsg{Type: "pong"}},
		{"error",
			wsproto.NewError(wsproto.Error{Key: "k", Error: "boom", Node: "n1"}),
			node.ServerMsg{Type: "error", Key: "k", Error: "boom", Node: "n1"}},
		{"subscribed",
			wsproto.NewSubscribed(wsproto.Subscribed{Key: "k", State: "running", Reason: "suspended", Node: "n1"}),
			node.ServerMsg{Type: "subscribed", Key: "k", State: "running", Reason: "suspended", Node: "n1"}},
		{"unsubscribed",
			wsproto.NewUnsubscribed(wsproto.Unsubscribed{Key: "k", Node: "n1"}),
			node.ServerMsg{Type: "unsubscribed", Key: "k", Node: "n1"}},
		{"history_initial",
			wsproto.NewHistory(wsproto.History{Key: "k", Events: events, Node: "n1", HasMore: &hasMore, Initial: true}),
			node.ServerMsg{Type: "history", Key: "k", Events: events, Node: "n1", HasMore: &hasMore, Initial: true}},
		{"history_incremental",
			wsproto.NewHistory(wsproto.History{Key: "k", Events: events}),
			node.ServerMsg{Type: "history", Key: "k", Events: events}},
		{"event",
			wsproto.NewEvent(wsproto.Event{Key: "k", Event: ev, Node: "n1"}),
			node.ServerMsg{Type: "event", Key: "k", Event: ev, Node: "n1"}},
		{"send_ack",
			wsproto.NewSendAck(wsproto.SendAck{Key: "k", ID: "id1", Status: "accepted", Error: "e", Node: "n1"}),
			node.ServerMsg{Type: "send_ack", Key: "k", ID: "id1", Status: "accepted", Error: "e", Node: "n1"}},
		{"send_error",
			wsproto.NewSendError(wsproto.SendError{Key: "k", Error: "boom", Node: "n1"}),
			node.ServerMsg{Type: "send_error", Key: "k", Error: "boom", Node: "n1"}},
		{"interrupt_ack",
			wsproto.NewInterruptAck(wsproto.InterruptAck{Key: "k", ID: "id1", Status: "ok", Error: "e", Node: "n1"}),
			node.ServerMsg{Type: "interrupt_ack", Key: "k", ID: "id1", Status: "ok", Error: "e", Node: "n1"}},
		{"session_state",
			wsproto.NewSessionState(wsproto.SessionState{Key: "k", State: "ready", Reason: "r", Node: "n1"}),
			node.ServerMsg{Type: "session_state", Key: "k", State: "ready", Reason: "r", Node: "n1"}},
		{"sessions_update", wsproto.NewSessionsUpdate(), node.ServerMsg{Type: "sessions_update"}},
		{"agent_event",
			wsproto.NewAgentEvent(wsproto.AgentEvent{Key: "k", Event: ev, TaskID: "t1"}),
			node.ServerMsg{Type: "agent_event", Key: "k", Event: ev, TaskID: "t1"}},
		{"agent_meta",
			wsproto.NewAgentMeta(wsproto.AgentMeta{Key: "k", TaskID: "t1",
				AgentMeta: &wsproto.AgentMetaPatch{LastTool: "Read", LastDetail: "d", ToolUses: 3, DurationMS: 99}}),
			node.ServerMsg{Type: "agent_meta", Key: "k", TaskID: "t1",
				AgentMeta: &node.AgentMetaPatch{LastTool: "Read", LastDetail: "d", ToolUses: 3, DurationMS: 99}}},
		{"agent_done",
			wsproto.NewAgentDone(wsproto.AgentDone{Key: "k", Status: "ok", TaskID: "t1"}),
			node.ServerMsg{Type: "agent_done", Key: "k", Status: "ok", TaskID: "t1"}},
		{"agent_subscribe_rejected",
			wsproto.NewAgentSubscribeRejected(wsproto.AgentSubscribeRejected{Key: "k", Reason: "nope", TaskID: "t1"}),
			node.ServerMsg{Type: "agent_subscribe_rejected", Key: "k", Reason: "nope", TaskID: "t1"}},
	}
	for _, tc := range cases {
		got, want := marshal(t, tc.frame), marshal(t, tc.legacy)
		if got != want {
			t.Errorf("%s:\n frame  %s\n legacy %s", tc.name, got, want)
		}
	}
}

// TestRawFrames_MatchRetiredConstants pins the init-generated raw frames
// against the retired hand-written const strings from internal/server/wshub.go.
func TestRawFrames_MatchRetiredConstants(t *testing.T) {
	t.Parallel()
	cases := []struct{ got, want string }{
		{wsproto.RawAuthOK, `{"type":"auth_ok"}`},
		{wsproto.RawPong, `{"type":"pong"}`},
		{wsproto.RawAuthFailInvalid, `{"type":"auth_fail","error":"invalid token"}`},
		{wsproto.RawErrNotAuth, `{"type":"error","error":"not authenticated"}`},
		{wsproto.RawErrRateLimited, `{"type":"error","error":"rate limited"}`},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("raw frame drifted:\n got  %s\n want %s", tc.got, tc.want)
		}
	}
}

// TestFramesRegistry_TypeStamped asserts every registry exemplar carries its
// own map key as the marshaled "type" — a frame built without its New*
// constructor would land here with an empty type.
func TestFramesRegistry_TypeStamped(t *testing.T) {
	t.Parallel()
	for typ, exemplar := range wsproto.Frames {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(marshal(t, exemplar)), &probe); err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		if probe.Type != string(typ) {
			t.Errorf("registry exemplar for %q marshals type %q", typ, probe.Type)
		}
	}
}
