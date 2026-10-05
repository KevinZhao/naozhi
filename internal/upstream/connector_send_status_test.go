package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/node"
)

const sendStatusKey = "dashboard:direct:alice:general"

// sendThroughFakeHub registers c with a primary whose ack carries caps, relays
// one send and returns the node's response frame.
func sendThroughFakeHub(t *testing.T, c *Connector, caps []string) node.ReverseMsg {
	t.Helper()
	params, err := json.Marshal(map[string]string{"key": sendStatusKey, "text": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	answer := make(chan node.ReverseMsg, 1)
	srv := newFakeServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var reg node.ReverseMsg
		if conn.ReadJSON(&reg) != nil {
			return
		}
		if conn.WriteJSON(node.ReverseMsg{Type: "registered", Capabilities: caps}) != nil {
			return
		}
		if conn.WriteJSON(node.ReverseMsg{Type: "request", ReqID: "s1", Method: "send", Params: params}) != nil {
			return
		}
		for {
			var msg node.ReverseMsg
			if conn.ReadJSON(&msg) != nil {
				return
			}
			if msg.Type == "response" && msg.ReqID == "s1" {
				answer <- msg
				return
			}
		}
	})
	c.cfg.URL = wsURL(srv)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if connected, err := c.runOnce(ctx); !connected {
		t.Fatalf("runOnce: not connected (%v)", err)
	}
	select {
	case msg := <-answer:
		return msg
	default:
		t.Fatal("the node never answered the send")
		return node.ReverseMsg{}
	}
}

// A primary that does not advertise send-status discards the send's result, so
// a busy answer would read there as accepted for a message that was dropped:
// the node reports it as the pre-status error instead. The flag follows each
// register, so a reconnect to such a primary loses what the last one said.
func TestRunOnce_BusySendFollowsTheHubsSendStatusCap(t *testing.T) {
	c := New(&Config{NodeID: "n", Token: "t"}, testRouter(makeRouter()), nil, nil, Discovery{}, &fakeSubmitter{status: "busy"})
	steps := []struct {
		name   string
		caps   []string
		status bool
	}{
		{"hub advertises send-status", []string{clievent.SchemaCap, node.CapSendStatus}, true},
		{"reconnect to a hub without it", []string{clievent.SchemaCap}, false},
		{"hub advertises send-status again", []string{node.CapSendStatus, clievent.SchemaCap}, true},
		{"reconnect to a hub sending no caps", nil, false},
	}
	for _, st := range steps {
		msg := sendThroughFakeHub(t, c, st.caps)
		if st.status {
			if msg.Error != "" || string(msg.Result) != `{"status":"busy"}` {
				t.Fatalf("%s: response = (result %s, error %q), want status busy", st.name, msg.Result, msg.Error)
			}
			continue
		}
		if msg.Error != node.ErrSendBusy.Error() || len(msg.Result) != 0 {
			t.Fatalf("%s: response = (result %s, error %q), want error %q", st.name, msg.Result, msg.Error, node.ErrSendBusy)
		}
	}
}

// Against the real hub the busy status reaches the primary's ReverseConn.Send.
func TestReverseLink_BusySendIsAStatus(t *testing.T) {
	r := makeRouter()
	t.Cleanup(r.Shutdown)
	rc := reverseLinkWith(t, r, &fakeSubmitter{status: "busy"})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	status, err := rc.Send(ctx, sendStatusKey, "hi", "")
	if err != nil || status != "busy" {
		t.Fatalf("Send = (%q, %v), want (busy, nil)", status, err)
	}
}

// Only busy loses the message; every other status passes to any primary.
func TestHandleRequest_Send_StatusByHubCap(t *testing.T) {
	for _, status := range []string{"accepted", "queued", "reset", "busy"} {
		for _, hubReads := range []bool{false, true} {
			c := New(&Config{URL: "wss://x", NodeID: "n", Token: "t"}, testRouter(makeRouter()), nil, nil, Discovery{}, &fakeSubmitter{status: status})
			c.hubSendStatus.Store(hubReads)
			result, err := c.handleRequest(context.Background(), context.Background(), sendReq(t, map[string]string{"key": sendStatusKey, "text": "hi"}), &sync.WaitGroup{})
			if status == "busy" && !hubReads {
				if !errors.Is(err, node.ErrSendBusy) || result != nil {
					t.Errorf("busy, hub without send-status: (%s, %v), want ErrSendBusy", result, err)
				}
				continue
			}
			if err != nil || string(result) != `{"status":"`+status+`"}` {
				t.Errorf("%s, hub send-status %v: (%s, %v), want the status", status, hubReads, result, err)
			}
		}
	}
}
