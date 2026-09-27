package cli

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// shimLink is a process's connection to its shim: the socket, the buffered
// reader the read loop owns (the Init handshake reads through it first), the
// write side serialised by wMu, the heartbeat's pong signal, the replay
// cursor, and the two PIDs the shim's hello reported.
//
// Lock ordering: shimWriter.mu → wMu. Code inside withWriteLock writes
// through sendLocked, never through stdin (which would take wMu again).
type shimLink struct {
	conn      net.Conn
	closeOnce sync.Once
	r         *bufio.Reader
	w         *bufio.Writer
	wMu       sync.Mutex
	stdin     *shimWriter

	cliPID  int // CLI PID reported by the shim's hello
	shimPID int // shim PID, for Kill's SIGUSR2 fallback

	lastSeq atomic.Int64 // last received shim seq, for reconnect
	// pongRecv is signalled by the read loop on each pong; buffered so a
	// heartbeat scheduler stall cannot drop pongs and miscount a healthy shim.
	pongRecv chan struct{}
}

func (l *shimLink) init(conn net.Conn, r *bufio.Reader, w *bufio.Writer, cliPID, shimPID int) {
	l.conn, l.r, l.w = conn, r, w
	l.cliPID, l.shimPID = cliPID, shimPID
	l.pongRecv = make(chan struct{}, 4)
	l.stdin = &shimWriter{link: l}
}

// stdinWriter is the io.Writer protocols write CLI stdin lines through.
func (l *shimLink) stdinWriter() io.Writer { return l.stdin }

// withWriteLock runs fn holding the write lock, for a caller that must order
// its own state with the bytes it writes (fn writes through sendLocked).
func (l *shimLink) withWriteLock(fn func() error) error {
	l.wMu.Lock()
	defer l.wMu.Unlock()
	return fn()
}

// preempt sets a write deadline without the write lock, to unblock a writer
// pinned inside a write on a full socket buffer — that writer holds wMu, so
// teardown could otherwise not take it until TCP keepalive (minutes).
func (l *shimLink) preempt(d time.Duration) {
	if err := l.conn.SetWriteDeadline(time.Now().Add(d)); err != nil {
		// A conn that refuses a deadline is already closed or broken, so any
		// write blocked in it has failed already; there is nothing to preempt.
		slog.Debug("teardown: write-deadline preempt failed", "err", err)
	}
}

// sendFinal is teardown's last word to the shim: it preempts a writer pinned
// on a full socket, then sends msg under a write deadline of d and, when
// closeAfter, closes the socket in the same write-lock hold — bufio.Writer is
// not safe against a concurrent Close + Flush from the heartbeat or an
// interrupt. The deadline is cleared before the lock is released so a later
// sender cannot inherit it and hit a spurious i/o timeout. A conn that
// refuses the deadline skips the send: without one the write could block
// until TCP keepalive (minutes).
func (l *shimLink) sendFinal(msg shimClientMsg, d time.Duration, closeAfter bool) error {
	l.preempt(d)
	l.wMu.Lock()
	defer l.wMu.Unlock()
	if closeAfter {
		defer l.close()
	}
	if err := l.conn.SetWriteDeadline(time.Now().Add(d)); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}
	err := l.sendLocked(msg)
	_ = l.conn.SetWriteDeadline(time.Time{})
	return err
}

// close closes the socket once.
func (l *shimLink) close() {
	l.closeOnce.Do(func() {
		if err := l.conn.Close(); err != nil {
			slog.Debug("shimConn close failed", "err", err)
		}
	})
}

// pulseReadDeadline makes a read parked on the socket return (i/o timeout)
// and clears the deadline again, so later reads are not cancelled.
func (l *shimLink) pulseReadDeadline() {
	if l.conn == nil {
		return
	}
	_ = l.conn.SetReadDeadline(time.Now())
	_ = l.conn.SetReadDeadline(time.Time{})
}
