package cli

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// recordingConn is a net.Conn that records what the link does to it.
type recordingConn struct {
	net.Conn
	mu             sync.Mutex
	written        bytes.Buffer
	writeDeadlines []time.Time
	readDeadlines  []time.Time
	closes         int
	refuseDeadline bool
}

func (c *recordingConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.written.Write(b)
}

func (c *recordingConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refuseDeadline {
		return errors.New("deadline refused")
	}
	c.writeDeadlines = append(c.writeDeadlines, t)
	return nil
}

func (c *recordingConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadlines = append(c.readDeadlines, t)
	return nil
}

func (c *recordingConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closes++
	return nil
}

func newRecordingLink(c *recordingConn) *shimLink {
	var l shimLink
	l.init(c, bufio.NewReader(&bytes.Buffer{}), bufio.NewWriter(c), 1, 2)
	return &l
}

// TestShimLink_SendFinalClearsTheDeadline: the last deadline teardown leaves
// on the conn is none, so a later sender cannot inherit it and time out.
func TestShimLink_SendFinalClearsTheDeadline(t *testing.T) {
	c := &recordingConn{}
	l := newRecordingLink(c)
	if err := l.sendFinal(shimClientMsg{Type: "shutdown"}, time.Second, false); err != nil {
		t.Fatalf("sendFinal: %v", err)
	}
	if !bytes.Contains(c.written.Bytes(), []byte(`"shutdown"`)) {
		t.Errorf("wrote %q, want the shutdown frame", c.written.String())
	}
	if n := len(c.writeDeadlines); n == 0 || !c.writeDeadlines[n-1].IsZero() {
		t.Errorf("write deadlines = %v, want the last one cleared", c.writeDeadlines)
	}
	if c.closes != 0 {
		t.Errorf("closed %d times, want 0 without closeAfter", c.closes)
	}
}

// TestShimLink_SendFinalSkipsTheSendWithoutADeadline: a conn that refuses a
// write deadline gets no write (it could block until TCP keepalive), and is
// still closed when teardown asked for it.
func TestShimLink_SendFinalSkipsTheSendWithoutADeadline(t *testing.T) {
	c := &recordingConn{refuseDeadline: true}
	l := newRecordingLink(c)
	if err := l.sendFinal(shimClientMsg{Type: "kill"}, time.Second, true); err == nil {
		t.Error("sendFinal = nil, want the deadline error")
	}
	if c.written.Len() != 0 {
		t.Errorf("wrote %q to a conn without a deadline", c.written.String())
	}
	if c.closes != 1 {
		t.Errorf("closed %d times, want 1", c.closes)
	}
}

// TestShimLink_ClosesOnce: every teardown path closes the link, and the conn
// sees one Close.
func TestShimLink_ClosesOnce(t *testing.T) {
	c := &recordingConn{}
	l := newRecordingLink(c)
	_ = l.sendFinal(shimClientMsg{Type: "detach"}, time.Second, true)
	l.close()
	l.close()
	if c.closes != 1 {
		t.Errorf("conn closed %d times, want 1", c.closes)
	}
}

// TestShimLink_PulseReadDeadline: the pulse makes a parked read return and
// leaves no read deadline behind.
func TestShimLink_PulseReadDeadline(t *testing.T) {
	c := &recordingConn{}
	l := newRecordingLink(c)
	l.pulseReadDeadline()
	if len(c.readDeadlines) != 2 || c.readDeadlines[0].IsZero() || !c.readDeadlines[1].IsZero() {
		t.Errorf("read deadlines = %v, want one set then one cleared", c.readDeadlines)
	}
}
