package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
)

// TestHub_RemoteSendAckCarriesTheNodesStatus: the WS send_ack for a remote
// send is the node's own admission status, so a remote /new rolls the tab's
// optimistic running state back and a busy node is a toast, as for a local
// send. An answer the dashboard does not know reads as accepted.
func TestHub_RemoteSendAckCarriesTheNodesStatus(t *testing.T) {
	cases := []struct {
		name string
		code int
		body string
		want string
	}{
		{"reset", http.StatusOK, `{"key":"test:d:u:general","status":"reset"}`, "reset"},
		{"queued", http.StatusAccepted, `{"key":"test:d:u:general","status":"queued"}`, "queued"},
		{"busy", http.StatusAccepted, `{"key":"test:d:u:general","status":"busy"}`, "busy"},
		{"accepted", http.StatusAccepted, `{"key":"test:d:u:general","status":"accepted"}`, "accepted"},
		{"unknown", http.StatusAccepted, `{"status":"<img src=x>"}`, "accepted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/sessions/send" {
					http.NotFound(w, r)
					return
				}
				w.WriteHeader(tc.code)
				w.Write([]byte(tc.body))
			}))
			defer ts.Close()

			nodes := map[string]node.Conn{"remote": node.NewHTTPClient("remote", ts.URL, "", "Remote")}
			router := session.NewRouter(session.RouterConfig{})
			hub := newHubForTest(t, HubOptions{Router: router, Nodes: newNodeRegistry(nodes)}, sendEngineOpts{})
			defer hub.Shutdown()

			client := newTestWSClient()
			hub.handleSend(client, node.ClientMsg{Type: "send", Key: "test:d:u:general", Text: "/new", Node: "remote", ID: "r1"})

			msg := readClientMsg(t, client, 2*time.Second)
			if msg.Type != "send_ack" || msg.ID != "r1" {
				t.Fatalf("got %s id=%q, want the send_ack for r1", msg.Type, msg.ID)
			}
			if msg.Status != tc.want {
				t.Errorf("status = %q, want %q", msg.Status, tc.want)
			}
			if msg.Error != "" {
				t.Errorf("error = %q on an admitted send, want none", msg.Error)
			}
		})
	}
}

// statusNode is a node.Conn whose Send answers a fixed status, or err.
type statusNode struct {
	fakeCapNode
	status string
	err    error
}

func (n *statusNode) Send(context.Context, node.SendRequest) (node.SendResult, error) {
	return node.SendResult{Status: n.status}, n.err
}

// TestRemoteSend_ABusyNodeIsASendError: the HTTP path answered 202 before
// the RPC, so a node that did not buffer the message is reported to the
// sending tab like a failed send, and a lost answer as "unknown" under the
// send's turn ID; an admitted one is not reported.
func TestRemoteSend_ABusyNodeIsASendError(t *testing.T) {
	const key = "test:d:u:general"
	unknown := fmt.Errorf("%w: timeout", node.ErrSendUnknown)
	for _, tc := range []struct {
		name    string
		status  string
		err     error
		wantErr string // "" = none reported
	}{
		{"busy", "busy", nil, "remote-error remote " + key + "   " + errSendBusy.Error()},
		{"unknown", "", unknown, "remote-error remote " + key + " unknown s1 " + node.ErrSendUnknown.Error()},
		{"accepted", "accepted", nil, ""},
		{"queued", "queued", nil, ""},
		{"reset", "reset", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingNotifier{}
			e := newSendEngine(sendEngineOpts{Notify: rec})
			defer e.drain()

			nc := &statusNode{fakeCapNode: fakeCapNode{id: "remote"}, status: tc.status, err: tc.err}
			if !e.remoteSend(nc, "remote", node.SendRequest{Key: key, Text: "hi", TurnID: "s1"}) {
				t.Fatal("remoteSend refused a send on a live engine")
			}
			e.wg.Wait()

			got := rec.snapshot()
			if tc.wantErr == "" && strings.Contains(got, "error") {
				t.Errorf("notifier calls = %q, want no send error", got)
			}
			if tc.wantErr != "" && !strings.Contains(got, tc.wantErr) {
				t.Errorf("notifier calls = %q, want %q", got, tc.wantErr)
			}
			if !strings.HasSuffix(got, "sessions") {
				t.Errorf("notifier calls = %q, want a sessions update last", got)
			}
		})
	}
}

// refreshCountingNode is a statusNode that counts RefreshSubscription calls.
type refreshCountingNode struct {
	statusNode
	refreshes atomic.Int32
}

func (n *refreshCountingNode) RefreshSubscription(string) { n.refreshes.Add(1) }

// TestRemoteSend_NoRefreshAfterAReset: both remote send paths refresh the
// key's subscription after an admitted send, and after one whose answer was
// lost (the node may be running it), but not after a reset: the key has no
// session on the node until the next send, and a reverse node answers that
// subscribe with an error that drops the key's browsers.
func TestRemoteSend_NoRefreshAfterAReset(t *testing.T) {
	const key = "test:d:u:general"
	unknown := fmt.Errorf("%w: timeout", node.ErrSendUnknown)
	for _, tc := range []struct {
		status string
		err    error
		want   int32
	}{{"reset", nil, 0}, {"accepted", nil, 1}, {"queued", nil, 1}, {"unknown", unknown, 1}} {
		t.Run("ws/"+tc.status, func(t *testing.T) {
			nc := &refreshCountingNode{statusNode: statusNode{fakeCapNode: fakeCapNode{id: "remote"}, status: tc.status, err: tc.err}}
			router := session.NewRouter(session.RouterConfig{})
			hub := newHubForTest(t, HubOptions{Router: router, Nodes: newNodeRegistry(map[string]node.Conn{"remote": nc})}, sendEngineOpts{})
			defer hub.Shutdown()

			client := newTestWSClient()
			hub.handleSend(client, node.ClientMsg{Type: "send", Key: key, Text: "/new", Node: "remote", ID: "r1"})
			readClientMsg(t, client, 2*time.Second)
			hub.engine.wg.Wait()
			if got := nc.refreshes.Load(); got != tc.want {
				t.Errorf("RefreshSubscription calls = %d, want %d", got, tc.want)
			}
		})
		t.Run("http/"+tc.status, func(t *testing.T) {
			nc := &refreshCountingNode{statusNode: statusNode{fakeCapNode: fakeCapNode{id: "remote"}, status: tc.status, err: tc.err}}
			e := newSendEngine(sendEngineOpts{Notify: &recordingNotifier{}})
			defer e.drain()

			if !e.remoteSend(nc, "remote", node.SendRequest{Key: key, Text: "/new", TurnID: "s1"}) {
				t.Fatal("remoteSend refused a send on a live engine")
			}
			e.wg.Wait()
			if got := nc.refreshes.Load(); got != tc.want {
				t.Errorf("RefreshSubscription calls = %d, want %d", got, tc.want)
			}
		})
	}
}

// deadlineNode is a node.Conn whose Send records how long its ctx allows.
type deadlineNode struct {
	fakeCapNode
	budget chan time.Duration
}

func (n *deadlineNode) Send(ctx context.Context, _ node.SendRequest) (node.SendResult, error) {
	dl, _ := ctx.Deadline()
	n.budget <- time.Until(dl)
	return node.SendResult{Status: "accepted"}, nil
}

// TestRemoteSend_WaitsOutASpawn: a node answers a send once the session is
// up, and an ACP backend takes ~12s to start, so neither path gives up on the
// first send to a new remote session before remoteSendTimeout.
func TestRemoteSend_WaitsOutASpawn(t *testing.T) {
	const key = "test:d:u:general"
	const atLeast = remoteSendTimeout - 5*time.Second
	t.Run("ws", func(t *testing.T) {
		nc := &deadlineNode{fakeCapNode: fakeCapNode{id: "remote"}, budget: make(chan time.Duration, 1)}
		router := session.NewRouter(session.RouterConfig{})
		hub := newHubForTest(t, HubOptions{Router: router, Nodes: newNodeRegistry(map[string]node.Conn{"remote": nc})}, sendEngineOpts{})
		defer hub.Shutdown()

		hub.handleSend(newTestWSClient(), node.ClientMsg{Type: "send", Key: key, Text: "hi", Node: "remote", ID: "r1"})
		if got := <-nc.budget; got < atLeast {
			t.Errorf("WS remote send RPC budget = %v, want about %v", got, remoteSendTimeout)
		}
	})
	t.Run("http", func(t *testing.T) {
		nc := &deadlineNode{fakeCapNode: fakeCapNode{id: "remote"}, budget: make(chan time.Duration, 1)}
		e := newSendEngine(sendEngineOpts{Notify: &recordingNotifier{}})
		defer e.drain()

		if !e.remoteSend(nc, "remote", node.SendRequest{Key: key, Text: "hi"}) {
			t.Fatal("remoteSend refused a send on a live engine")
		}
		if got := <-nc.budget; got < atLeast {
			t.Errorf("HTTP remote send RPC budget = %v, want about %v", got, remoteSendTimeout)
		}
	})
}
