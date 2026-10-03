package upstream

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
)

// streamCapture collects every ReverseMsg streamEvents writes.
type streamCapture struct {
	mu   sync.Mutex
	msgs []node.ReverseMsg
}

func (sc *streamCapture) writeJSON(v any) error {
	if msg, ok := v.(node.ReverseMsg); ok {
		sc.mu.Lock()
		sc.msgs = append(sc.msgs, msg)
		sc.mu.Unlock()
	}
	return nil
}

// only returns the single message written, failing the test otherwise.
func (sc *streamCapture) only(t *testing.T) node.ReverseMsg {
	t.Helper()
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if len(sc.msgs) != 1 {
		t.Fatalf("streamEvents wrote %d messages, want exactly 1 terminal session_state: %+v", len(sc.msgs), sc.msgs)
	}
	return sc.msgs[0]
}

func newStreamEventsRouter(t *testing.T, key string) (*session.Router, *Connector) {
	t.Helper()
	r := session.NewRouter(session.RouterConfig{MaxProcs: 1})
	r.RegisterCronStubWithChain(key, "/tmp/stream-events-test", "prompt", nil)
	if r.SessionFor(key) == nil {
		t.Fatal("setup: RegisterCronStub did not install session")
	}
	return r, &Connector{router: testRouter(r)}
}

func assertResetTerminal(t *testing.T, msg node.ReverseMsg, key string) {
	t.Helper()
	want := node.ReverseMsg{Type: "session_state", Key: key, State: "dead", Reason: reasonSessionReset}
	if msg.Type != want.Type || msg.Key != want.Key || msg.State != want.State || msg.Reason != want.Reason {
		t.Errorf("terminal msg = {Type:%q Key:%q State:%q Reason:%q}, want {Type:%q Key:%q State:%q Reason:%q}",
			msg.Type, msg.Key, msg.State, msg.Reason, want.Type, want.Key, want.State, want.Reason)
	}
}

// TestStreamEvents_NotifyClosedAfterReset_EmitsTerminalState pins RNEW-005 for
// the subscribe race (#3041): the handler resolves the session and answers
// "subscribed", then a Reset removes it and closes notify before the pump
// runs. The primary must still get one terminal session_state.
func TestStreamEvents_NotifyClosedAfterReset_EmitsTerminalState(t *testing.T) {
	t.Parallel()
	const key = "cron:stream-events-nil-test"
	r, c := newStreamEventsRouter(t, key)

	sess := c.router.SessionFor(key)
	notify := make(chan struct{})
	r.Reset(key)
	if r.SessionFor(key) != nil {
		t.Fatal("setup: Reset did not drop session")
	}
	close(notify)

	var sc streamCapture
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c.streamEvents(ctx, sc.writeJSON, key, sess, notify, clievent.NewSinceCursor())

	assertResetTerminal(t, sc.only(t), key)
}

// TestStreamEvents_NilSession_EmitsTerminalState pins the entry guard: a nil
// session reports the reset terminal state at once instead of waiting on a
// notify channel nothing will close.
func TestStreamEvents_NilSession_EmitsTerminalState(t *testing.T) {
	t.Parallel()
	const key = "cron:stream-events-nil-session"
	r := session.NewRouter(session.RouterConfig{MaxProcs: 1})
	c := &Connector{router: testRouter(r)}

	var sc streamCapture
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c.streamEvents(ctx, sc.writeJSON, key, nil, make(chan struct{}), clievent.NewSinceCursor())

	if ctx.Err() != nil {
		t.Fatal("streamEvents with a nil session waited for ctx instead of returning")
	}
	assertResetTerminal(t, sc.only(t), key)
}

// TestStreamEvents_NotifyClosedSessionPresent_ReportsSnapshot: when the key
// still resolves at close time (replaced, not removed), the terminal message
// carries that session's own state rather than the reset default.
func TestStreamEvents_NotifyClosedSessionPresent_ReportsSnapshot(t *testing.T) {
	t.Parallel()
	const key = "cron:stream-events-present"
	_, c := newStreamEventsRouter(t, key)

	sess := c.router.SessionFor(key)
	snap := sess.Snapshot()
	if snap.State == "" || snap.State == "dead" {
		t.Fatalf("setup: stub state = %q, want a non-empty state other than dead", snap.State)
	}
	notify := make(chan struct{})
	close(notify)

	var sc streamCapture
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c.streamEvents(ctx, sc.writeJSON, key, sess, notify, clievent.NewSinceCursor())

	msg := sc.only(t)
	if msg.Type != "session_state" || msg.Key != key || msg.State != snap.State || msg.Reason != snap.DeathReason {
		t.Errorf("terminal msg = {Type:%q Key:%q State:%q Reason:%q}, want session_state for %q with snapshot state %q/%q",
			msg.Type, msg.Key, msg.State, msg.Reason, key, snap.State, snap.DeathReason)
	}
}

// TestHandleConn_Subscribe_StreamsTheSubscribedSession pins the handler side:
// the pump it starts streams the session it acked, so an event appended after
// "subscribed" arrives as an events frame rather than a terminal state.
func TestHandleConn_Subscribe_StreamsTheSubscribedSession(t *testing.T) {
	const key = "feishu:p2p:alice:stream-handoff"
	r := session.NewRouter(session.RouterConfig{MaxProcs: 1})
	t.Cleanup(r.Shutdown)
	proc := session.NewTestProcess()
	r.InjectSession(key, proc)

	frames := make(chan node.ReverseMsg, 4)
	srv := newFakeServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		handshake(t, conn)
		if err := conn.WriteJSON(node.ReverseMsg{Type: "subscribe", Key: key}); err != nil {
			return
		}
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		for i := 0; i < 2; i++ {
			var resp node.ReverseMsg
			if err := conn.ReadJSON(&resp); err != nil {
				return
			}
			if resp.Type == "subscribed" {
				proc.EventLog.Append(clievent.EventEntry{Time: 1000, UUID: "after-ack", Type: "text", Summary: "hi"})
			}
			frames <- resp
		}
	})

	cfg := &Config{URL: wsURL(srv), NodeID: "n", Token: "t"}
	c := New(cfg, testRouter(r), nil, nil, Discovery{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.runOnce(ctx) //nolint:errcheck
	}()
	defer func() { cancel(); <-done }()

	for _, want := range []string{"subscribed", "events"} {
		select {
		case got := <-frames:
			if got.Type != want {
				t.Fatalf("frame = %+v, want type %q", got, want)
			}
			if want == "events" && (len(got.Events) != 1 || got.Events[0].UUID != "after-ack") {
				t.Fatalf("events frame = %+v, want the one entry appended after the ack", got.Events)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for a %q frame", want)
		}
	}
}
