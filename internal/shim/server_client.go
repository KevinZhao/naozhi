package shim

// Client-connection lifecycle: handshake, per-connection read loop, command
// dispatch and the low-level frame writers.

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"time"
)

// performHandshake runs the pre-active-client auth phase: peer-UID check,
// attach-message read under shimAuthReadDeadline + a LimitedReader (caps
// pre-auth memory), constant-time token compare, then clears the read
// deadline so the post-auth loop is not capped. On token failure it sends
// "auth_failed" so the client can surface the reason (#657).
func (s *shimServer) performHandshake(conn net.Conn) (ClientMsg, bool) {
	// Verify connecting peer has same UID (defense-in-depth beyond token auth)
	if !VerifyPeerUID(conn) {
		// Warn (not Debug): a UID mismatch on an owner-private socket is
		// audit-worthy and Debug is silenced in production.
		slog.Warn("shim: rejecting client with UID mismatch",
			"remote", conn.RemoteAddr().String())
		return ClientMsg{}, false
	}

	// If the deadline can't be installed (conn half-closed) the read below
	// could block until keepalive expires — bail instead of leaking.
	if err := conn.SetReadDeadline(time.Now().Add(shimAuthReadDeadline)); err != nil {
		slog.Debug("shim: set auth read deadline failed", "err", err)
		return ClientMsg{}, false
	}

	// Use LimitedReader to prevent pre-auth memory exhaustion
	lr := &io.LimitedReader{R: conn, N: int64(maxClientLineBytes()) + 1}
	reader := bufio.NewReaderSize(lr, 4096)

	attachLine, err := reader.ReadBytes('\n')
	if err != nil || lr.N == 0 {
		slog.Debug("client read attach failed", "err", err)
		return ClientMsg{}, false
	}
	var attachMsg ClientMsg
	if err := json.Unmarshal(bytes.TrimSpace(attachLine), &attachMsg); err != nil || attachMsg.Type != "attach" {
		slog.Debug("client invalid attach message")
		return ClientMsg{}, false
	}

	// Verify token BEFORE setting as active client
	clientToken, err := base64.StdEncoding.DecodeString(attachMsg.Token)
	if err != nil || subtle.ConstantTimeCompare(clientToken, s.tokenRaw) != 1 {
		writeMsg(conn, ServerMsg{Type: "auth_failed", Msg: "invalid token"})
		return ClientMsg{}, false
	}

	// If clearing the deadline fails, a stale one could kick a healthy client
	// later; bail so the client reconnects cleanly.
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		slog.Debug("shim: clear auth read deadline failed", "err", err)
		return ClientMsg{}, false
	}

	return attachMsg, true
}

// handleClient manages one naozhi connection. Runs in its own goroutine.

// handleClient manages one naozhi connection. Runs in its own goroutine.
func (s *shimServer) handleClient(conn net.Conn, idleTimeout time.Duration) {
	defer conn.Close()

	attachMsg, ok := s.performHandshake(conn)
	if !ok {
		return
	}

	// Switch to bounded reader for the authenticated command loop.
	// LimitedReader prevents a single oversized line from exhausting memory.
	postAuthLR := &io.LimitedReader{R: conn, N: int64(maxClientLineBytes()) + 1}
	reader := bufio.NewReaderSize(postAuthLR, 64*1024)

	// Send hello directly (before becoming the active client, so no live events interleave)
	s.mu.Lock()
	seqStart, seqEnd := s.buffer.SeqRange()
	cliAlive := s.cli.alive()
	sessionID := s.state.SessionID
	s.mu.Unlock()

	writeMsg(conn, ServerMsg{
		Type:            "hello",
		ShimPID:         os.Getpid(),
		CLIPID:          s.cli.pid(),
		CLIAlive:        boolPtr(cliAlive),
		SessionID:       sessionID,
		BufferSeqStart:  seqStart,
		BufferSeqEnd:    seqEnd,
		ProtocolVersion: ProtocolVersion,
	})

	// Replay buffered lines directly (still not the active client, no duplication)
	lines := s.buffer.LinesSince(attachMsg.Seq)
	for _, l := range lines {
		// MarshalReplayLine aliases l.data only across the synchronous marshal
		// and writeRaw writes before the next iteration — no copy needed.
		data, err := MarshalReplayLine(l.seq, l.data)
		if err != nil {
			continue
		}
		writeRaw(conn, data)
	}
	writeMsg(conn, ServerMsg{Type: "replay_done", Count: len(lines)})

	// If CLI already exited, notify and skip the command loop's cli.exited select
	// to avoid sending cli_exited twice (closed channel is always selectable).
	cliWasAlive := cliAlive
	if !cliAlive {
		writeMsg(conn, ServerMsg{Type: "cli_exited", Code: intPtr(s.cli.exitCode)})
	}

	// Reject a new client while the CLI is alive and another client is
	// connected, so an unexpected reconnect cannot kick an active one.
	s.mu.Lock()
	hasActiveClient := s.clientConn != nil
	s.mu.Unlock()
	if hasActiveClient && cliAlive {
		slog.Warn("rejecting new client: active client exists while CLI alive")
		writeMsg(conn, ServerMsg{Type: "error", Msg: "another client is connected"})
		return
	}

	// NOW become the active client (after replay complete, no duplication window)
	writeCh, clientDone := s.setClient(conn)

	// A new client means the shim is needed: stop watchdog, cancel grace timer.
	s.watchdog.Stop()
	s.mu.Lock()
	if s.graceTimer != nil {
		s.graceTimer.Stop()
		s.graceTimer = nil
	}
	s.mu.Unlock()

	// Writer goroutine: drains writeCh to the socket, exits on clientDone.
	// A per-flush write deadline bounds a stuck reader to 10s.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		w := bufio.NewWriter(conn)
		// Without a deadline bufio.Flush can block until keepalive expires and
		// starve the defer that signals clientDone; if the deadline can't be
		// set, skip the Flush. Clearing it afterwards is best-effort.
		flushWithDeadline := func() error {
			if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
				return fmt.Errorf("set write deadline: %w", err)
			}
			err := w.Flush()
			_ = conn.SetWriteDeadline(time.Time{})
			return err
		}
		for {
			select {
			case data, ok := <-writeCh:
				if !ok {
					_ = flushWithDeadline()
					return
				}
				if _, err := w.Write(data); err != nil {
					return
				}
				// Batch flush: drain available buffered messages
				flush := true
				for flush {
					select {
					case more, ok := <-writeCh:
						if !ok {
							_ = flushWithDeadline()
							return
						}
						if _, err := w.Write(more); err != nil {
							return
						}
					default:
						flush = false
					}
				}
				if err := flushWithDeadline(); err != nil {
					return
				}
			case <-clientDone:
				_ = flushWithDeadline()
				return
			}
		}
	}()

	// sendCliExited: when the live CLI died, the terminal cli_exited frame is
	// written synchronously AFTER the async writer has drained and exited so
	// it cannot interleave with buffered output or be lost to conn.Close (#1783).
	var sendCliExited bool
	var cliExitCode int
	defer func() {
		// clearClient closes clientDone so the writer flushes and exits; wait
		// for it before touching conn directly.
		s.clearClient(conn)
		<-writerDone
		// conn is now exclusively ours: deliver cli_exited synchronously.
		if sendCliExited {
			resp := ServerMsg{Type: "cli_exited", Code: intPtr(cliExitCode)}
			if data, err := resp.MarshalLine(); err == nil {
				writeRaw(conn, data)
			}
		}
		conn.Close()
		// Only re-arm watchdog/idle if no new client took over
		s.mu.Lock()
		noNewClient := s.clientConn == nil
		s.mu.Unlock()
		if noNewClient {
			s.watchdog.Start()
			s.resetIdleTimer(idleTimeout)
		}
		s.saveState()
	}()

	s.mu.Lock()
	s.state.LastConnectedAt = time.Now().UTC().Format(time.RFC3339)
	s.mu.Unlock()
	s.saveState()

	sendCliExited, cliExitCode = s.runCommandLoop(reader, postAuthLR, clientDone, cliWasAlive)
}

// runCommandLoop is the post-auth, post-replay client dispatch loop; any
// return unwinds the calling handleClient's defers.
//
//   - reader / postAuthLR: bounded line reader; postAuthLR.N is reset per line.
//   - clientDone: closed by setClient teardown; the producer goroutine watches
//     it to avoid leaking.
//   - cliWasAlive: cli.alive() at attach time; drives cli_exited dedup.
//
// Returns sendCliExited=true (plus exit code) when the live CLI died, so
// handleClient delivers cli_exited synchronously after the writer drains (#1783).

// runCommandLoop is the post-auth, post-replay client dispatch loop; any
// return unwinds the calling handleClient's defers.
//
//   - reader / postAuthLR: bounded line reader; postAuthLR.N is reset per line.
//   - clientDone: closed by setClient teardown; the producer goroutine watches
//     it to avoid leaking.
//   - cliWasAlive: cli.alive() at attach time; drives cli_exited dedup.
//
// Returns sendCliExited=true (plus exit code) when the live CLI died, so
// handleClient delivers cli_exited synchronously after the writer drains (#1783).
func (s *shimServer) runCommandLoop(
	reader *bufio.Reader,
	postAuthLR *io.LimitedReader,
	clientDone <-chan struct{},
	cliWasAlive bool,
) (sendCliExited bool, exitCode int) {
	lineCh := make(chan []byte, 1)
	go func() {
		defer close(lineCh)
		// Cumulative byte tally: the per-line LimitedReader alone would let an
		// authenticated client churn near-max lines indefinitely (#541).
		var sessionBytes int64
		for {
			postAuthLR.N = int64(maxClientLineBytes()) + 1 // reset per-line limit
			line, err := reader.ReadBytes('\n')
			if err != nil {
				// postAuthLR.N reaching 0 surfaces as an error, not a clean close:
				// distinguish oversize from EOF/disconnect.
				if postAuthLR.N <= 0 {
					slog.Warn("client line exceeded per-line byte limit, disconnecting",
						"limit", maxClientLineBytes())
				}
				return
			}
			// bufio.NewReaderSize sets buffer, not max line. Disconnect rather
			// than spin: a flooding client would burn CPU while holding a slot.
			if len(line) > maxClientLineBytes() {
				slog.Warn("client line too large, disconnecting", "size", len(line))
				return
			}
			// Cumulative cap (#541).
			sessionBytes += int64(len(line))
			if cap := maxClientSessionBytesValue(); sessionBytes > cap {
				slog.Warn("client session byte cap exceeded, disconnecting",
					"session_bytes", sessionBytes, "cap", cap)
				return
			}
			select {
			case lineCh <- line:
			case <-clientDone:
				return // handleClient exited; avoid goroutine leak
			}
		}
	}()

	// nil when the CLI was already dead at attach time: cli_exited was emitted
	// during replay and a nil channel is never selectable, so the loop won't
	// busy-return on the perpetually-closed s.cli.exited.
	cliExited := s.cli.exited
	if !cliWasAlive {
		cliExited = nil
	}

	for {
		select {
		case line, ok := <-lineCh:
			if !ok {
				return false, 0 // client disconnected
			}
			msg, err := ParseClientMsg(bytes.TrimSpace(line))
			if err != nil {
				continue
			}
			if disconnect := s.handleClientCommand(msg); disconnect {
				return false, 0
			}

		case <-cliExited:
			// Reachable only when cliWasAlive (see nil-ing above). Do NOT
			// enqueue cli_exited on writeCh: returning closes clientDone and
			// conn, racing the async writer's flush (#1783). handleClient
			// delivers it synchronously after the writer has drained.
			return true, s.cli.exitCode

		case <-s.done:
			return false, 0
		}
	}
}

// handleClientCommand dispatches one ClientMsg. Returns true when the caller
// must disconnect the client (oversize write, stdin EPIPE, shutdown, detach,
// refused-shutdown guard).

// handleClientCommand dispatches one ClientMsg. Returns true when the caller
// must disconnect the client (oversize write, stdin EPIPE, shutdown, detach,
// refused-shutdown guard).
func (s *shimServer) handleClientCommand(msg ClientMsg) (disconnect bool) {
	switch msg.Type {
	case "write":
		// Reject payloads that would overflow Claude's 10 MB bufio.Scanner and
		// deadlock stdout; treated as a protocol violation → disconnect.
		if limit := maxWriteLineBytesValue(); int64(len(msg.Line)) > limit {
			slog.Warn("client write too large, disconnecting",
				"size", len(msg.Line), "limit", limit)
			return true
		}
		if s.cli.alive() {
			// EPIPE between alive() and Write would silently lose the message;
			// disconnect so the client reconnects, and cli.exited takes the
			// normal exit path next iteration.
			if _, err := s.cli.stdin.Write([]byte(msg.Line + "\n")); err != nil {
				slog.Warn("shim: cli stdin write failed, disconnecting client", "err", err)
				return true
			}
		}
	case "interrupt":
		s.cli.interrupt()
	case "close_stdin":
		s.cli.closeStdin()
	case "kill":
		s.cli.kill()
	case "ping":
		resp := ServerMsg{
			Type:     "pong",
			CLIAlive: boolPtr(s.cli.alive()),
			Buffered: s.buffer.Count(),
		}
		if data, err := resp.MarshalLine(); err == nil {
			s.enqueueWrite(data)
		}
	case "shutdown":
		// Only refuse an early shutdown when no authenticated client is
		// attached: an authed naozhi issuing shutdown within the guard window
		// is deliberate (fresh_context cron, Router.Reset, config drift), and
		// blocking it caused "refusing to clobber" on fast restart. Inside this
		// loop clientConn normally equals conn; the check is defensive.
		s.mu.Lock()
		hasClient := s.clientConn != nil
		s.mu.Unlock()
		if !hasClient && s.cli.alive() && time.Since(s.startedAt) < freshShimShutdownGuard {
			slog.Warn("ignoring shutdown: CLI alive, shim recently started, no authed client",
				"age", time.Since(s.startedAt).Round(time.Millisecond))
			return true
		}
		s.cli.closeStdin()
		s.cli.waitOrKill(5 * time.Second)
		s.initiateShutdown()
		return true
	case "detach":
		return true // disconnect but keep running
	}
	return false
}

// writeMsg writes a ServerMsg directly to conn (auth/replay phase, before the
// async writer exists) under writeRaw's 10s write deadline.

// writeMsg writes a ServerMsg directly to conn (auth/replay phase, before the
// async writer exists) under writeRaw's 10s write deadline.
func writeMsg(conn net.Conn, msg ServerMsg) {
	data, err := msg.MarshalLine()
	if err != nil {
		return
	}
	writeRaw(conn, data)
}

// writeRaw writes a pre-marshaled NDJSON frame under a 10s write deadline so a
// stalled client cannot pin a semaphore slot indefinitely. Split out so the
// replay loop can feed MarshalReplayLine's zero-copy output.

// writeRaw writes a pre-marshaled NDJSON frame under a 10s write deadline so a
// stalled client cannot pin a semaphore slot indefinitely. Split out so the
// replay loop can feed MarshalReplayLine's zero-copy output.
func writeRaw(conn net.Conn, data []byte) {
	// Deadline set failed (conn already closed): skip the write.
	if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return
	}
	defer func() { _ = conn.SetWriteDeadline(time.Time{}) }()
	conn.Write(data) //nolint:errcheck
}
