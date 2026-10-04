package server

import (
	"context"
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

// statusNode is a node.Conn whose Send answers a fixed status.
type statusNode struct {
	fakeCapNode
	status string
}

func (n *statusNode) Send(context.Context, string, string, string) (string, error) {
	return n.status, nil
}

// TestRemoteSend_ABusyNodeIsASendError: the HTTP path answered 202 before
// the RPC, so a node that did not buffer the message is reported to the
// key's subscribers like a failed send; an admitted one is not.
func TestRemoteSend_ABusyNodeIsASendError(t *testing.T) {
	const key = "test:d:u:general"
	for _, tc := range []struct {
		status    string
		wantError bool
	}{
		{"busy", true},
		{"accepted", false},
		{"queued", false},
		{"reset", false},
	} {
		t.Run(tc.status, func(t *testing.T) {
			rec := &recordingNotifier{}
			e := newSendEngine(sendEngineOpts{Notify: rec})
			defer e.drain()

			if !e.remoteSend(&statusNode{fakeCapNode: fakeCapNode{id: "remote"}, status: tc.status}, "remote", key, "hi", "") {
				t.Fatal("remoteSend refused a send on a live engine")
			}
			e.wg.Wait()

			got := rec.snapshot()
			wantErr := "error " + key + " " + errSendBusy.Error()
			if tc.wantError != strings.Contains(got, wantErr) {
				t.Errorf("notifier calls = %q; busy send error reported = %v, want %v", got, !tc.wantError, tc.wantError)
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
// key's subscription after an admitted send, but not after a reset: the key
// has no session on the node until the next send, and a reverse node answers
// that subscribe with an error that drops the key's browsers.
func TestRemoteSend_NoRefreshAfterAReset(t *testing.T) {
	const key = "test:d:u:general"
	for _, tc := range []struct {
		status string
		want   int32
	}{{"reset", 0}, {"accepted", 1}, {"queued", 1}} {
		t.Run("ws/"+tc.status, func(t *testing.T) {
			nc := &refreshCountingNode{statusNode: statusNode{fakeCapNode: fakeCapNode{id: "remote"}, status: tc.status}}
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
			nc := &refreshCountingNode{statusNode: statusNode{fakeCapNode: fakeCapNode{id: "remote"}, status: tc.status}}
			e := newSendEngine(sendEngineOpts{Notify: &recordingNotifier{}})
			defer e.drain()

			if !e.remoteSend(nc, "remote", key, "/new", "") {
				t.Fatal("remoteSend refused a send on a live engine")
			}
			e.wg.Wait()
			if got := nc.refreshes.Load(); got != tc.want {
				t.Errorf("RefreshSubscription calls = %d, want %d", got, tc.want)
			}
		})
	}
}
