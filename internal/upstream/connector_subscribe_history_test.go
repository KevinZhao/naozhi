package upstream

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
)

const historyKey = "feishu:direct:alice:general"

// subscribeLink runs a connector against a fake primary that sends sub after
// the handshake and hands every later frame to the returned channel.
func subscribeLink(t *testing.T, r *session.Router, sub node.ReverseMsg) <-chan node.ReverseMsg {
	t.Helper()
	frames := make(chan node.ReverseMsg, 16)
	srv := newFakeServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		handshake(t, conn)
		if err := conn.WriteJSON(sub); err != nil {
			return
		}
		for {
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			var msg node.ReverseMsg
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			frames <- msg
		}
	})
	c := New(&Config{URL: wsURL(srv), NodeID: "n", Token: "t"}, testRouter(r), nil, nil, Discovery{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.runOnce(ctx) //nolint:errcheck
	}()
	t.Cleanup(func() { cancel(); <-done })
	return frames
}

func nextFrame(t *testing.T, frames <-chan node.ReverseMsg, wantType string) node.ReverseMsg {
	t.Helper()
	select {
	case msg := <-frames:
		if msg.Type != wantType {
			t.Fatalf("frame = %+v, want type %q", msg, wantType)
		}
		return msg
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for a %q frame", wantType)
		return node.ReverseMsg{}
	}
}

func frameUUIDs(msg node.ReverseMsg) []string {
	out := make([]string, 0, len(msg.Events))
	for _, e := range msg.Events {
		out = append(out, e.UUID)
	}
	return out
}

func injectHistory(t *testing.T, entries ...clievent.EventEntry) (*session.Router, *session.TestProcess) {
	t.Helper()
	r := makeRouter()
	t.Cleanup(r.Shutdown)
	proc := session.NewTestProcess()
	for _, e := range entries {
		proc.EventLog.Append(e)
	}
	r.InjectSession(historyKey, proc)
	return r, proc
}

// TestHandleConn_WantHistory_InitialPage: an opening want_history subscribe
// is answered with the visible-aware page sized by its limit, flagged initial
// with has_more, and the stream then resumes past it instead of replaying the
// ring.
func TestHandleConn_WantHistory_InitialPage(t *testing.T) {
	const limit = 10
	var entries []clievent.EventEntry
	for i := 1; i <= 4*limit; i++ {
		entries = append(entries, clievent.EventEntry{Time: int64(i * 10), UUID: fmt.Sprintf("e%d", i), Type: "text", Summary: "m"})
	}
	r, proc := injectHistory(t, entries...)

	frames := subscribeLink(t, r, node.ReverseMsg{Type: "subscribe", Key: historyKey, Limit: limit, WantHistory: true})
	nextFrame(t, frames, "subscribed")
	page := nextFrame(t, frames, "events")
	if !page.Initial || page.HasMore == nil || !*page.HasMore {
		t.Fatalf("opening page initial=%v has_more=%v, want an initial page reporting older history", page.Initial, page.HasMore)
	}
	if got := frameUUIDs(page); len(got) != limit || got[len(got)-1] != "e40" {
		t.Fatalf("opening page = %v, want the newest %d entries", got, limit)
	}

	proc.EventLog.Append(clievent.EventEntry{Time: 1000, UUID: "live", Type: "text", Summary: "m"})
	live := nextFrame(t, frames, "events")
	if got := frameUUIDs(live); len(got) != 1 || got[0] != "live" || live.Initial || live.HasMore != nil {
		t.Fatalf("first live frame = %v initial=%v has_more=%v, want only the new entry as a plain batch", got, live.Initial, live.HasMore)
	}
}

// TestHandleConn_WantHistory_CatchUpAfter: a reconnect's want_history
// subscribe gets what followed `after`, that millisecond included (#2432), as
// a non-initial frame; the next Append does not resend any of it.
func TestHandleConn_WantHistory_CatchUpAfter(t *testing.T) {
	r, proc := injectHistory(t,
		clievent.EventEntry{Time: 1000, UUID: "old", Type: "user", Summary: "hi"},
		clievent.EventEntry{Time: 2000, UUID: "a", Type: "thinking", Summary: "..."},
		clievent.EventEntry{Time: 2000, UUID: "b", Type: "text", Summary: "answer"},
	)

	frames := subscribeLink(t, r, node.ReverseMsg{Type: "subscribe", Key: historyKey, After: 2000, WantHistory: true})
	nextFrame(t, frames, "subscribed")
	page := nextFrame(t, frames, "events")
	if got := frameUUIDs(page); len(got) != 2 || got[0] != "a" || got[1] != "b" || page.Initial || page.HasMore != nil {
		t.Fatalf("catch-up = %v initial=%v has_more=%v, want [a b] as a plain batch", got, page.Initial, page.HasMore)
	}

	proc.EventLog.Append(clievent.EventEntry{Time: 3000, UUID: "c", Type: "text", Summary: "next"})
	if got := frameUUIDs(nextFrame(t, frames, "events")); len(got) != 1 || got[0] != "c" {
		t.Fatalf("first live frame = %v, want [c]", got)
	}
}

// TestHandleConn_WantHistory_EmptyCatchUp: a catch-up with nothing after
// `after` is sent, empty, only while the session is running; an idle one sends
// nothing until the next Append.
func TestHandleConn_WantHistory_EmptyCatchUp(t *testing.T) {
	for _, tc := range []struct {
		state     cli.ProcessState
		wantEmpty bool
	}{{cli.StateRunning, true}, {cli.StateReady, false}} {
		t.Run(tc.state.String(), func(t *testing.T) {
			r, proc := injectHistory(t, clievent.EventEntry{Time: 1000, UUID: "old", Type: "user", Summary: "hi"})
			proc.StateVal = tc.state
			// Runs after the link closes; a running session stalls Shutdown.
			t.Cleanup(func() { proc.StateVal = cli.StateReady })

			frames := subscribeLink(t, r, node.ReverseMsg{Type: "subscribe", Key: historyKey, After: 2000, WantHistory: true})
			nextFrame(t, frames, "subscribed")
			if tc.wantEmpty {
				if page := nextFrame(t, frames, "events"); len(page.Events) != 0 || page.Initial || page.HasMore != nil {
					t.Fatalf("catch-up = %+v, want an empty plain batch", page)
				}
			}
			proc.EventLog.Append(clievent.EventEntry{Time: 3000, UUID: "c", Type: "text", Summary: "next"})
			if got := frameUUIDs(nextFrame(t, frames, "events")); len(got) != 1 || got[0] != "c" {
				t.Fatalf("next frame = %v, want the live [c]", got)
			}
		})
	}
}

// TestHandleConn_PlainSubscribe_StartsAtAfter: without want_history the node
// sends no page of its own, and an `after` from the primary seeds the stream
// so the first Append does not replay what the primary already fetched.
func TestHandleConn_PlainSubscribe_StartsAtAfter(t *testing.T) {
	r, proc := injectHistory(t,
		clievent.EventEntry{Time: 1000, UUID: "old", Type: "user", Summary: "hi"},
		clievent.EventEntry{Time: 2000, UUID: "a", Type: "text", Summary: "answer"},
	)

	frames := subscribeLink(t, r, node.ReverseMsg{Type: "subscribe", Key: historyKey, After: 2000})
	nextFrame(t, frames, "subscribed")
	proc.EventLog.Append(clievent.EventEntry{Time: 3000, UUID: "c", Type: "text", Summary: "next"})
	if got := frameUUIDs(nextFrame(t, frames, "events")); len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("first frame = %v, want [a c]: from the after millisecond on, nothing older", got)
	}
}

// TestHandleConn_PlainSubscribe_NoAfterReplaysRing pins the legacy shape an
// older primary relies on: no page of its own, and the first Append delivers
// everything the ring holds.
func TestHandleConn_PlainSubscribe_NoAfterReplaysRing(t *testing.T) {
	r, proc := injectHistory(t, clievent.EventEntry{Time: 1000, UUID: "old", Type: "user", Summary: "hi"})

	frames := subscribeLink(t, r, node.ReverseMsg{Type: "subscribe", Key: historyKey})
	nextFrame(t, frames, "subscribed")
	proc.EventLog.Append(clievent.EventEntry{Time: 3000, UUID: "c", Type: "text", Summary: "next"})
	first := nextFrame(t, frames, "events")
	if got := frameUUIDs(first); len(got) != 2 || got[0] != "old" || got[1] != "c" || first.Initial || first.HasMore != nil {
		t.Fatalf("first frame = %v initial=%v has_more=%v, want [old c] as a plain batch", got, first.Initial, first.HasMore)
	}
}

// TestHandleConn_WantHistory_ProcessLessSession: a session without a process
// still gets its (here empty) initial frame, and it lands before the terminal
// session_state its closed notify channel produces.
func TestHandleConn_WantHistory_ProcessLessSession(t *testing.T) {
	const key = "cron:subscribe-history-stub"
	r := makeRouter()
	t.Cleanup(r.Shutdown)
	r.RegisterCronStubWithChain(key, "/tmp/subscribe-history-test", "prompt", nil)

	frames := subscribeLink(t, r, node.ReverseMsg{Type: "subscribe", Key: key, WantHistory: true})
	nextFrame(t, frames, "subscribed")
	page := nextFrame(t, frames, "events")
	if !page.Initial || page.HasMore == nil || *page.HasMore || len(page.Events) != 0 {
		t.Fatalf("opening page = %+v, want an empty initial frame with has_more=false", page)
	}
	nextFrame(t, frames, "session_state")
}
