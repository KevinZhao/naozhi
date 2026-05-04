package node

import (
	"encoding/json"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

func TestClientMsgJSONRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		msg  ClientMsg
	}{
		{
			name: "auth message",
			msg:  ClientMsg{Type: "auth", Token: "secret"},
		},
		{
			name: "subscribe with after",
			msg:  ClientMsg{Type: "subscribe", Key: "feishu:group:123", After: 1700000000000},
		},
		{
			name: "send with workspace",
			msg: ClientMsg{
				Type:      "send",
				Key:       "feishu:group:123",
				Text:      "hello",
				Workspace: "/home/user/project",
				ID:        "corr-1",
			},
		},
		{
			name: "send with file_ids",
			msg: ClientMsg{
				Type:    "send",
				Key:     "feishu:group:123",
				Text:    "check this",
				FileIDs: []string{"file-a", "file-b"},
			},
		},
		{
			name: "ping",
			msg:  ClientMsg{Type: "ping"},
		},
		{
			name: "with resume_id and node",
			msg: ClientMsg{
				Type:     "send",
				Key:      "feishu:group:123",
				Text:     "hi",
				Node:     "node-2",
				ResumeID: "sess-abc",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.msg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got ClientMsg
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			// Re-marshal both and compare bytes for deep equality.
			want, _ := json.Marshal(tc.msg)
			gotBytes, _ := json.Marshal(got)
			if string(want) != string(gotBytes) {
				t.Fatalf("round-trip mismatch:\nwant %s\ngot  %s", want, gotBytes)
			}
		})
	}
}

func TestServerMsgJSONRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		msg  ServerMsg
	}{
		{name: "auth_ok", msg: ServerMsg{Type: "auth_ok"}},
		{name: "auth_fail with error", msg: ServerMsg{Type: "auth_fail", Error: "bad token"}},
		{
			name: "event",
			msg: ServerMsg{
				Type:  "event",
				Key:   "feishu:group:123",
				Node:  "node-1",
				Event: &cli.EventEntry{Time: 1000, Type: "text", Summary: "hello"},
			},
		},
		{
			name: "history",
			msg: ServerMsg{
				Type: "history",
				Key:  "feishu:group:123",
				Events: []cli.EventEntry{
					{Time: 1000, Type: "text", Summary: "a"},
					{Time: 2000, Type: "tool_use", Summary: "b", Tool: "Bash"},
				},
			},
		},
		{
			name: "send_ack",
			msg:  ServerMsg{Type: "send_ack", ID: "corr-1", Status: "accepted"},
		},
		{
			name: "error with reason",
			msg:  ServerMsg{Type: "error", Key: "k", Error: "session busy", Reason: "busy"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.msg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got ServerMsg
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			want, _ := json.Marshal(tc.msg)
			gotBytes, _ := json.Marshal(got)
			if string(want) != string(gotBytes) {
				t.Fatalf("round-trip mismatch:\nwant %s\ngot  %s", want, gotBytes)
			}
		})
	}
}

func TestReverseMsgJSONRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		msg  ReverseMsg
	}{
		{
			name: "register",
			msg: ReverseMsg{
				Type:        "register",
				NodeID:      "node-1",
				Token:       "tok",
				DisplayName: "Worker 1",
				Hostname:    "worker.internal",
			},
		},
		{
			name: "request with params",
			msg: ReverseMsg{
				Type:   "request",
				ReqID:  "1",
				Method: "fetch_sessions",
				Params: json.RawMessage(`{"key":"val"}`),
			},
		},
		{
			name: "response with result",
			msg: ReverseMsg{
				Type:   "response",
				ReqID:  "1",
				Result: json.RawMessage(`[{"session_id":"abc"}]`),
			},
		},
		{
			name: "response with error",
			msg:  ReverseMsg{Type: "response", ReqID: "1", Error: "not found"},
		},
		{
			name: "event push",
			msg: ReverseMsg{
				Type:  "event",
				Key:   "feishu:group:123",
				Event: &cli.EventEntry{Time: 5000, Type: "text", Summary: "msg"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.msg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got ReverseMsg
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			want, _ := json.Marshal(tc.msg)
			gotBytes, _ := json.Marshal(got)
			if string(want) != string(gotBytes) {
				t.Fatalf("round-trip mismatch:\nwant %s\ngot  %s", want, gotBytes)
			}
		})
	}
}

func TestServerMsgOmitsEmptyFields(t *testing.T) {
	msg := ServerMsg{Type: "auth_ok"}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"key", "event", "events", "id", "status", "state", "reason", "error", "node"} {
		if _, ok := m[field]; ok {
			t.Errorf("field %q should be omitted when empty, but was present in %s", field, data)
		}
	}
}
