package node

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/limits"
)

// A node's frame past limits.MaxStreamJSONLine must be refused rather than
// buffered whole, as on the reverse and upstream links: the relay hangs up
// mid-frame (the server's write fails) or answers close 1009. Without a limit
// the frame is read in full and the server's read just times out.
func TestWSRelay_RefusesOversizedFrame(t *testing.T) {
	type outcome struct {
		write bool
		err   error
	}
	got := make(chan outcome, 1)
	srv := wsTestServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		if !authHandshake(t, conn) {
			return
		}
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var sub ClientMsg
		if err := conn.ReadJSON(&sub); err != nil {
			return
		}
		big := append([]byte(`{"type":"event","pad":"`), bytes.Repeat([]byte("x"), limits.MaxStreamJSONLine)...)
		big = append(big, `"}`...)
		if err := conn.WriteMessage(websocket.TextMessage, big); err != nil {
			select {
			case got <- outcome{write: true, err: err}:
			default:
			}
			return
		}
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				select {
				case got <- outcome{err: err}:
				default:
				}
				return
			}
		}
	})
	defer srv.Close()

	relay := newWSRelay(newRelayNode(srv))
	defer relay.Close()
	relay.Subscribe(&mockSink{id: 1}, "feishu:group:1", 0, 0)

	select {
	case o := <-got:
		var ce *websocket.CloseError
		if !o.write && (!errors.As(o.err, &ce) || ce.Code != websocket.CloseMessageTooBig) {
			t.Fatalf("server saw %v, want the relay to refuse the frame", o.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server never saw the relay end the connection")
	}
}
