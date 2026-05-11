package server

import (
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/node"
)

// These tests isolate the WS handler policy — not the full upgrade/readPump
// path. handleAgentSubscribe and handleAgentUnsubscribe take a *wsClient +
// node.ClientMsg; we can exercise them with the fakeWSClient harness from
// agent_tailer_test.go.

func newHubForAgentTest(t *testing.T) *Hub {
	t.Helper()
	hub, _ := newTestHub("test-token")
	t.Cleanup(hub.Shutdown)
	return hub
}

// drainMsgs pulls node.ServerMsg up to `n` messages or until timeout.
func drainMsgs(out <-chan node.ServerMsg, n int, timeout time.Duration) []node.ServerMsg {
	got := make([]node.ServerMsg, 0, n)
	deadline := time.After(timeout)
	for len(got) < n {
		select {
		case msg := <-out:
			got = append(got, msg)
		case <-deadline:
			return got
		}
	}
	return got
}

func TestHandleAgentSubscribe_InvalidKey(t *testing.T) {
	t.Parallel()
	hub := newHubForAgentTest(t)
	c, out := newCapturedClient(t, hub)
	hub.handleAgentSubscribe(c, node.ClientMsg{
		Type:   "agent_subscribe",
		Key:    "", // empty — ValidateSessionKey rejects
		TaskID: "t1",
	})
	msgs := drainMsgs(out, 1, 200*time.Millisecond)
	if len(msgs) != 1 || msgs[0].Type != "error" {
		t.Errorf("expected error, got %+v", msgs)
	}
}

func TestHandleAgentSubscribe_InvalidTaskID(t *testing.T) {
	t.Parallel()
	hub := newHubForAgentTest(t)
	c, out := newCapturedClient(t, hub)
	hub.handleAgentSubscribe(c, node.ClientMsg{
		Type:   "agent_subscribe",
		Key:    "dashboard:direct:test:general",
		TaskID: "has/slash", // regex rejects
	})
	msgs := drainMsgs(out, 1, 200*time.Millisecond)
	if len(msgs) != 1 || msgs[0].Type != "error" {
		t.Errorf("expected error, got %+v", msgs)
	}
}

func TestHandleAgentSubscribe_UnknownSession_Rejected(t *testing.T) {
	t.Parallel()
	hub := newHubForAgentTest(t)
	c, out := newCapturedClient(t, hub)
	hub.handleAgentSubscribe(c, node.ClientMsg{
		Type:   "agent_subscribe",
		Key:    "dashboard:direct:nosuch:general",
		TaskID: "t1",
	})
	msgs := drainMsgs(out, 1, 200*time.Millisecond)
	if len(msgs) != 1 {
		t.Fatalf("want 1 msg, got %d", len(msgs))
	}
	if msgs[0].Type != "agent_subscribe_rejected" {
		t.Errorf("type=%q, want agent_subscribe_rejected", msgs[0].Type)
	}
	if msgs[0].Reason != "session_not_found" {
		t.Errorf("reason=%q", msgs[0].Reason)
	}
}
