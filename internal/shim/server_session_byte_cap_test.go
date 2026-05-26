package shim

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestHandleClient_SessionByteCap_Disconnects pins R216-SEC-3 (#541): an
// authenticated client may not exceed the cumulative byte budget across
// the post-auth command stream. Without this cap the per-line
// LimitedReader resets on every iteration, so a token-holding peer can
// drive arbitrary memory churn through the shim by repeatedly sending
// near-max-size lines.
//
// Override drives both knobs down for the test: the cumulative cap to a
// few KiB and the write-line cap to 1 KiB so each "write" frame uses up
// a measurable slice of the budget. After enough such writes the
// cumulative total clears the cap; the reader goroutine logs + returns
// and the outer handleClient defer chain closes the connection.
func TestHandleClient_SessionByteCap_Disconnects(t *testing.T) {
	origLine := setMaxWriteLineBytes(1024) // 1 KiB per line
	defer setMaxWriteLineBytes(origLine)
	// 4 KiB cumulative — small enough that ~8 sub-1 KiB lines exceed it.
	origSession := setMaxClientSessionBytes(4 * 1024)
	defer setMaxClientSessionBytes(origSession)

	_, client, cleanup := setupShimServerWithClient(t)
	defer cleanup()

	// Each write is ~512 B of payload, plus a small JSON envelope. After 8
	// of them we are well past the 4 KiB cumulative cap. Send 32 to be
	// safely above the threshold even if individual sends race the
	// disconnect.
	body := strings.Repeat("A", 512)
	msg := ClientMsg{Type: "write", Line: body}
	data, _ := json.Marshal(msg)
	data = append(data, '\n')
	for i := 0; i < 32; i++ {
		client.conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond)) //nolint:errcheck
		if _, err := client.conn.Write(data); err != nil {
			break // shim already closed the conn after the cap fired
		}
	}
	client.conn.SetWriteDeadline(time.Time{}) //nolint:errcheck

	// Connection must EOF within a short window — handleClient's reader
	// goroutine returns once sessionBytes > cap, and the outer defer
	// closes the conn. If the cap path was never taken we would either
	// echo back stdout (cat) forever or block reading.
	client.conn.SetReadDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	buf := make([]byte, 4096)
	closed := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, err := client.conn.Read(buf)
		if err != nil {
			closed = true
			break
		}
	}
	if !closed {
		t.Fatalf("session byte cap did not disconnect client")
	}
}
