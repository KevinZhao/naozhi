package upstream

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// frameSink is a browser stand-in on the primary: it records every frame the
// ReverseConn fans out to it.
type frameSink struct {
	mu     sync.Mutex
	frames [][]byte
}

func (s *frameSink) SendJSON(any) {}

func (s *frameSink) SendRaw(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, bytes.Clone(data))
}

func (s *frameSink) count(substr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, f := range s.frames {
		if strings.Contains(string(f), substr) {
			n++
		}
	}
	return n
}

func (s *frameSink) saw(substr string) bool { return s.count(substr) > 0 }

// reverseLink connects a real Connector serving r to a real ReverseServer
// and returns the primary's ReverseConn for it.
func reverseLink(t *testing.T, r *session.Router) *node.ReverseConn {
	t.Helper()
	rs := node.NewReverseServer(map[string]node.ReverseNodeAuth{"n": {Token: "t"}}, false)
	registered := make(chan *node.ReverseConn, 1)
	rs.OnRegister = func(_ string, rc *node.ReverseConn) { registered <- rc }
	srv := httptest.NewServer(http.HandlerFunc(rs.ServeHTTP))

	c := New(&Config{URL: wsURL(srv), NodeID: "n", Token: "t"}, testRouter(r), nil, nil, Discovery{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.runOnce(ctx) //nolint:errcheck
	}()
	var rc *node.ReverseConn
	select {
	case rc = <-registered:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("connector never registered")
	}
	t.Cleanup(func() {
		cancel()
		rc.Close()
		<-done
		srv.Close()
	})
	return rc
}

// TestRelayReset_LiveSinkSurvivesRefreshOfAResetKey: a primary that refreshes
// a key right after the node reset it (every primary did on a relayed /new)
// must leave the browser on that key, so the conversation the next send starts
// still reaches it.
func TestRelayReset_LiveSinkSurvivesRefreshOfAResetKey(t *testing.T) {
	const key = "dashboard:direct:alice:general"
	r := makeRouter()
	t.Cleanup(r.Shutdown)
	before := session.NewTestProcess()
	r.InjectSession(key, before)
	rc := reverseLink(t, r)

	sink := &frameSink{}
	rc.Subscribe(sink, key, 0, 50)
	testhelper.Eventually(t, func() bool { return sink.saw(`"subscribed"`) }, 3*time.Second, "subscribe never acked")
	before.EventLog.Append(clievent.EventEntry{Time: 1000, UUID: "u1", Type: "text", Summary: "BEFORE-RESET"})
	testhelper.Eventually(t, func() bool { return sink.saw("BEFORE-RESET") }, 3*time.Second, "live event never delivered")

	r.Reset(key)
	rc.RefreshSubscription(key)
	// The connector handles a subscribe before it reads the next request, and
	// the primary reads frames in order: once this RPC answers, whatever the
	// node said about the refresh has reached the sink.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := rc.FetchSessions(ctx); err != nil {
		t.Fatalf("fetch_sessions barrier: %v", err)
	}

	after := session.NewTestProcess()
	r.InjectSession(key, after)
	rc.RefreshSubscription(key)
	// A stream replays its ring on the first Append after it starts, so the
	// event goes in once the node has answered.
	testhelper.Eventually(t, func() bool { return sink.count(`"subscribed"`) == 2 }, 3*time.Second,
		"the browser on the reset key never saw the new session's subscribe answered")
	after.EventLog.Append(clievent.EventEntry{Time: 2000, UUID: "u2", Type: "text", Summary: "AFTER-NEW"})
	testhelper.Eventually(t, func() bool { return sink.saw("AFTER-NEW") }, 3*time.Second,
		"the new conversation never reached the browser that was on the key when it was reset")
}

// scriptedPrimary runs a connector against a fake primary that writes what
// the test sends on the returned channel and reports the node's answers to
// subscribe and unsubscribe; stream frames are dropped.
func scriptedPrimary(t *testing.T, r *session.Router) (chan<- node.ReverseMsg, <-chan node.ReverseMsg) {
	t.Helper()
	out := make(chan node.ReverseMsg, 8)
	answers := make(chan node.ReverseMsg, 16)
	srv := newFakeServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		handshake(t, conn)
		go func() {
			for msg := range out {
				if conn.WriteJSON(msg) != nil {
					return
				}
			}
		}()
		for {
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			var msg node.ReverseMsg
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			switch msg.Type {
			case "subscribed", "subscribe_error", "unsubscribed":
				answers <- msg
			}
		}
	})
	c := New(&Config{URL: wsURL(srv), NodeID: "n", Token: "t"}, testRouter(r), nil, nil, Discovery{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.runOnce(ctx) //nolint:errcheck
	}()
	t.Cleanup(func() { cancel(); <-done; close(out) })
	return out, answers
}

// TestHandleConn_ResetServedKey_NoSubscribeError: a subscribe for a key the
// primary already holds here gets no subscribe_error once its session is gone,
// since a primary drops the key's browsers on one; after an unsubscribe the
// key is unknown again and the error comes back.
func TestHandleConn_ResetServedKey_NoSubscribeError(t *testing.T) {
	const key, missing = "dashboard:direct:alice:general", "dashboard:direct:bob:general"
	r := makeRouter()
	t.Cleanup(r.Shutdown)
	r.InjectSession(key, session.NewTestProcess())
	out, answers := scriptedPrimary(t, r)

	out <- node.ReverseMsg{Type: "subscribe", Key: key}
	nextFrame(t, answers, "subscribed")

	r.Reset(key)
	out <- node.ReverseMsg{Type: "subscribe", Key: key}
	// Answers keep request order: the first one here is about the reset key
	// unless the node said nothing about it.
	out <- node.ReverseMsg{Type: "subscribe", Key: missing}
	if got := nextFrame(t, answers, "subscribe_error"); got.Key != missing {
		t.Fatalf("subscribe_error for %q, want none for the served key %q", got.Key, key)
	}

	out <- node.ReverseMsg{Type: "unsubscribe", Key: key}
	nextFrame(t, answers, "unsubscribed")
	out <- node.ReverseMsg{Type: "subscribe", Key: key}
	if got := nextFrame(t, answers, "subscribe_error"); got.Key != key {
		t.Fatalf("subscribe_error for %q, want %q: an unsubscribed key is unknown again", got.Key, key)
	}
}
