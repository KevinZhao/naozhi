package shim

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// maxClientLineBytes limits the size of a single line read from the naozhi client,
// preventing unbounded memory allocation from malformed or malicious input.
const maxClientLineBytes = 16 * 1024 * 1024 // 16MB

// Config holds shim process configuration passed via CLI flags.
type Config struct {
	Key             string
	SocketPath      string
	StateFile       string
	BufferSize      int
	MaxBufBytes     int64
	IdleTimeout     time.Duration
	WatchdogTimeout time.Duration
	CLIPath         string
	CLIArgs         []string
	CWD             string
}

// Run is the main entry point for the shim process.
func Run(cfg Config) error {
	// Signal handling
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)

	// Start CLI subprocess
	cli, err := startCLI(cfg.CLIPath, cfg.CLIArgs, cfg.CWD)
	if err != nil {
		return fmt.Errorf("start CLI: %w", err)
	}

	// Clean stale socket before binding
	_ = CleanStaleSocket(cfg.SocketPath)

	// Create unix socket listener with atomic permissions
	oldUmask := syscall.Umask(0177)
	listener, err := net.Listen("unix", cfg.SocketPath)
	syscall.Umask(oldUmask)
	if err != nil {
		cli.kill()
		return fmt.Errorf("listen %s: %w", cfg.SocketPath, err)
	}
	defer listener.Close()
	defer os.Remove(cfg.SocketPath)

	// Enforce directory permissions (handles pre-existing dirs)
	if dir := socketDir(cfg.SocketPath); dir != "" {
		os.Chmod(dir, 0700) //nolint:errcheck
	}

	// Generate auth token
	tokenRaw, tokenB64, err := GenerateToken()
	if err != nil {
		cli.kill()
		return err
	}

	// Write state file
	state := State{
		ShimPID:   os.Getpid(),
		CLIPID:    cli.pid(),
		Socket:    cfg.SocketPath,
		AuthToken: tokenB64,
		Key:       cfg.Key,
		Workspace: cfg.CWD,
		CLIArgs:   cfg.CLIArgs,
		CLIAlive:  true,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := WriteStateFile(cfg.StateFile, state); err != nil {
		slog.Warn("failed to write state file", "err", err)
	}
	defer RemoveStateFile(cfg.StateFile)

	// Output ready signal to parent, then detach stdio
	fmt.Fprintf(os.Stdout, `{"status":"ready","pid":%d,"token":"%s"}`+"\n", os.Getpid(), tokenB64)
	os.Stdout.Close()
	os.Stdin.Close()

	// Ring buffer
	buf := NewRingBuffer(cfg.BufferSize, cfg.MaxBufBytes)

	// Shim server state
	s := &shimServer{
		cli:       cli,
		listener:  listener,
		buffer:    buf,
		tokenRaw:  tokenRaw,
		stateFile: cfg.StateFile,
		state:     state,
		done:      make(chan struct{}),
	}

	// Watchdog for disconnect periods
	s.watchdog = NewWatchdog(cfg.WatchdogTimeout, func() {
		slog.Warn("watchdog: killing unresponsive CLI")
		cli.kill()
	})

	// Start stdout/stderr readers
	go s.readStdout()
	go s.readStderr()

	// SIGTERM: 30s grace period
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for range sigCh {
			s.mu.Lock()
			hasClient := s.clientConn != nil
			if !hasClient {
				slog.Info("SIGTERM received, starting 30s grace period")
				if s.graceTimer != nil {
					s.graceTimer.Stop()
				}
				s.graceTimer = time.AfterFunc(30*time.Second, func() {
					slog.Info("grace period expired, shutting down")
					s.initiateShutdown()
				})
			}
			s.mu.Unlock()
		}
	}()

	// SIGUSR2: immediate shutdown
	usr2Ch := make(chan os.Signal, 1)
	signal.Notify(usr2Ch, syscall.SIGUSR2)
	go func() {
		<-usr2Ch
		slog.Info("SIGUSR2 received, immediate shutdown")
		s.initiateShutdown()
	}()

	// Accept loop
	idleTimeout := cfg.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = 4 * time.Hour
	}
	s.resetIdleTimer(idleTimeout)

	// Accept loop with bounded concurrency to prevent fd exhaustion
	const maxInflightClients = 16
	clientSem := make(chan struct{}, maxInflightClients)

	acceptCh := make(chan net.Conn, 1)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					slog.Debug("accept error", "err", err)
				}
				return
			}
			select {
			case acceptCh <- conn:
			case <-s.done:
				conn.Close()
				return
			}
		}
	}()

	for {
		select {
		case conn := <-acceptCh:
			select {
			case clientSem <- struct{}{}:
				go func() {
					defer func() { <-clientSem }()
					s.handleClient(conn, idleTimeout)
				}()
			default:
				conn.Close() // shed load
			}

		case <-cli.exited:
			slog.Info("CLI exited", "code", cli.exitCode)
			// Don't broadcastCLIExited here — connected handleClient detects
			// cli.exited in its own select and sends cli_exited to avoid double delivery.
			s.saveStateCLIDead()
			select {
			case conn := <-acceptCh:
				go s.handleClient(conn, idleTimeout)
				select {
				case <-s.done:
				case <-time.After(60 * time.Second):
				}
			case <-s.done:
			case <-time.After(60 * time.Second):
			}
			return nil

		case <-s.idleC():
			s.mu.Lock()
			hasClient := s.clientConn != nil
			s.mu.Unlock()
			if !hasClient {
				slog.Info("idle timeout, shutting down")
				cli.closeStdin()
				cli.waitOrKill(5 * time.Second)
				return nil
			}

		case <-s.watchdog.Fired():
			slog.Warn("watchdog fired, CLI killed")
			s.saveStateCLIDead()
			select {
			case conn := <-acceptCh:
				go s.handleClient(conn, idleTimeout)
				select {
				case <-s.done:
				case <-time.After(60 * time.Second):
				}
			case <-s.done:
			case <-time.After(60 * time.Second):
			}
			return nil

		case <-s.done:
			slog.Info("shutdown initiated")
			cli.closeStdin()
			cli.waitOrKill(5 * time.Second)
			return nil
		}
	}
}

// shimServer holds the shim's runtime state.
//
// Lock ordering: s.mu → buffer.mu (never acquire s.mu while holding buffer.mu).
type shimServer struct {
	cli       *cliProc
	listener  net.Listener
	buffer    *RingBuffer
	tokenRaw  []byte
	stateFile string
	watchdog  *Watchdog

	mu         sync.Mutex
	state      State
	clientConn net.Conn      // current connected client (at most one)
	writeCh    chan []byte   // buffered channel for async writes to client
	clientDone chan struct{} // closed to signal writer goroutine + enqueueWrite to stop
	graceTimer *time.Timer
	idleTimer  *time.Timer
	done       chan struct{} // closed on shutdown
	doneOnce   sync.Once
}

func (s *shimServer) initiateShutdown() {
	s.doneOnce.Do(func() { close(s.done) })
}

func (s *shimServer) idleC() <-chan time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idleTimer == nil {
		return nil
	}
	return s.idleTimer.C
}

func (s *shimServer) resetIdleTimer(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idleTimer != nil {
		s.idleTimer.Stop()
	}
	s.idleTimer = time.NewTimer(d)
}

// setClient atomically replaces the current client and returns a write channel + done channel.
// The old client (if any) is kicked. Must only be called AFTER auth succeeds.
func (s *shimServer) setClient(conn net.Conn) (writeCh chan []byte, clientDone chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Kick old client: close its done channel (signals writer goroutine + enqueueWrite)
	// then close its connection. Never close writeCh — the writer goroutine drains it.
	if s.clientConn != nil {
		if s.clientDone != nil {
			close(s.clientDone)
		}
		s.clientConn.Close()
	}

	s.clientConn = conn
	s.writeCh = make(chan []byte, 256)
	s.clientDone = make(chan struct{})

	// Cancel SIGTERM grace period
	if s.graceTimer != nil {
		s.graceTimer.Stop()
		s.graceTimer = nil
	}

	return s.writeCh, s.clientDone
}

// clearClient removes the current client if it matches conn.
// Closes clientDone to signal the writer goroutine to exit.
func (s *shimServer) clearClient(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clientConn == conn {
		if s.clientDone != nil {
			close(s.clientDone)
		}
		s.clientConn = nil
		s.writeCh = nil
		s.clientDone = nil
	}
}

// enqueueWrite sends data to the current client's write channel.
// Safe against closed channels: uses clientDone to detect stale state.
// Non-blocking: drops the message if the channel is full.
func (s *shimServer) enqueueWrite(data []byte) {
	s.mu.Lock()
	ch := s.writeCh
	done := s.clientDone
	s.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- data:
	case <-done:
		// Client was replaced or disconnected; don't send
	default:
		slog.Debug("client write channel full, dropping message")
	}
}

// readStdout reads CLI stdout and pushes lines to the ring buffer + client.
func (s *shimServer) readStdout() {
	for s.cli.stdout.Scan() {
		line := s.cli.stdout.Bytes()
		lineCopy := make([]byte, len(line))
		copy(lineCopy, line)

		seq := s.buffer.Push(lineCopy)
		s.watchdog.Reset()

		// Extract session_id from init/result events
		s.tryExtractSessionID(lineCopy)

		// Build message and enqueue (non-blocking, no lock during Flush)
		msg := ServerMsg{Type: "stdout", Seq: seq, Line: string(lineCopy)}
		if data, err := msg.MarshalLine(); err == nil {
			s.enqueueWrite(append(data, '\n'))
		}
	}

	// CLI stdout closed
	s.cli.wait()
	slog.Info("CLI stdout EOF")
}

func (s *shimServer) tryExtractSessionID(line []byte) {
	var ev struct {
		Type      string `json:"type"`
		SubType   string `json:"subtype"`
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	if ev.SessionID == "" {
		return
	}
	s.mu.Lock()
	if ev.Type == "system" && ev.SubType == "init" {
		s.state.SessionID = ev.SessionID
	}
	if ev.Type == "result" && s.state.SessionID == "" {
		s.state.SessionID = ev.SessionID
	}
	s.mu.Unlock()
}

// readStderr reads CLI stderr and forwards to client.
func (s *shimServer) readStderr() {
	scanner := bufio.NewScanner(s.cli.stderrR)
	scanner.Buffer(make([]byte, 4*1024), 64*1024)
	for scanner.Scan() {
		line := scanner.Text()
		slog.Debug("cli stderr", "line", line)

		msg := ServerMsg{Type: "stderr", Line: line}
		if data, err := msg.MarshalLine(); err == nil {
			s.enqueueWrite(append(data, '\n'))
		}
	}
}

// broadcastCLIExited sends cli_exited to the connected client.
func (s *shimServer) broadcastCLIExited() {
	code := s.cli.exitCode
	msg := ServerMsg{Type: "cli_exited", Code: IntPtr(code)}
	if data, err := msg.MarshalLine(); err == nil {
		s.enqueueWrite(append(data, '\n'))
	}
}

// saveStateCLIDead persists the CLI-dead state to the state file.
func (s *shimServer) saveStateCLIDead() {
	s.mu.Lock()
	s.state.CLIAlive = false
	st := s.state // copy under lock
	s.mu.Unlock()
	if err := WriteStateFile(s.stateFile, st); err != nil {
		slog.Warn("failed to write state file", "err", err)
	}
}

func (s *shimServer) saveState() {
	s.mu.Lock()
	st := s.state
	st.BufferCount = s.buffer.Count()
	st.CLIAlive = s.cli.alive()
	s.mu.Unlock()
	if err := WriteStateFile(s.stateFile, st); err != nil {
		slog.Warn("failed to write state file", "err", err)
	}
}

// handleClient manages one naozhi connection. Runs in its own goroutine.
func (s *shimServer) handleClient(conn net.Conn, idleTimeout time.Duration) {
	defer conn.Close()

	// Verify connecting peer has same UID (defense-in-depth beyond token auth)
	if !VerifyPeerUID(conn) {
		slog.Debug("client rejected: UID mismatch")
		return
	}

	// Set read deadline for auth phase (30s to send attach)
	conn.SetReadDeadline(time.Now().Add(30 * time.Second)) //nolint:errcheck

	// Use LimitedReader to prevent pre-auth memory exhaustion
	lr := &io.LimitedReader{R: conn, N: int64(maxClientLineBytes) + 1}
	reader := bufio.NewReaderSize(lr, 4096)

	// Read attach message
	attachLine, err := reader.ReadBytes('\n')
	if err != nil || lr.N == 0 {
		slog.Debug("client read attach failed", "err", err)
		return
	}
	var attachMsg ClientMsg
	if err := json.Unmarshal(bytes.TrimSpace(attachLine), &attachMsg); err != nil || attachMsg.Type != "attach" {
		slog.Debug("client invalid attach message")
		return
	}

	// Verify token BEFORE setting as active client
	clientToken, err := base64.StdEncoding.DecodeString(attachMsg.Token)
	if err != nil || subtle.ConstantTimeCompare(clientToken, s.tokenRaw) != 1 {
		writeMsg(conn, ServerMsg{Type: "auth_failed", Msg: "invalid token"})
		return
	}

	// Clear read deadline after successful auth
	conn.SetReadDeadline(time.Time{}) //nolint:errcheck

	// Switch to bounded reader for the authenticated command loop.
	// LimitedReader prevents a single oversized line from exhausting memory.
	postAuthLR := &io.LimitedReader{R: conn, N: int64(maxClientLineBytes) + 1}
	reader = bufio.NewReaderSize(postAuthLR, 64*1024)

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
		CLIAlive:        BoolPtr(cliAlive),
		SessionID:       sessionID,
		BufferSeqStart:  seqStart,
		BufferSeqEnd:    seqEnd,
		ProtocolVersion: ProtocolVersion,
	})

	// Replay buffered lines directly (still not the active client, no duplication)
	lines := s.buffer.LinesSince(attachMsg.Seq)
	for _, l := range lines {
		writeMsg(conn, ServerMsg{Type: "replay", Seq: l.seq, Line: string(l.data)})
	}
	writeMsg(conn, ServerMsg{Type: "replay_done", Count: len(lines)})

	// If CLI already exited, notify and skip the command loop's cli.exited select
	// to avoid sending cli_exited twice (closed channel is always selectable).
	cliWasAlive := cliAlive
	if !cliAlive {
		writeMsg(conn, ServerMsg{Type: "cli_exited", Code: IntPtr(s.cli.exitCode)})
	}

	// NOW become the active client (after replay complete, no duplication window)
	writeCh, clientDone := s.setClient(conn)

	// Stop disconnect watchdog
	s.watchdog.Stop()

	// Writer goroutine: drains writeCh to the socket, exits on clientDone
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		w := bufio.NewWriter(conn)
		for {
			select {
			case data, ok := <-writeCh:
				if !ok {
					w.Flush() //nolint:errcheck
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
							w.Flush() //nolint:errcheck
							return
						}
						w.Write(more) //nolint:errcheck
					default:
						flush = false
					}
				}
				if err := w.Flush(); err != nil {
					return
				}
			case <-clientDone:
				w.Flush() //nolint:errcheck
				return
			}
		}
	}()

	defer func() {
		s.clearClient(conn)
		conn.Close() // unblock any in-progress write in the writer goroutine
		<-writerDone
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

	// Update state
	s.mu.Lock()
	s.state.LastConnectedAt = time.Now().UTC().Format(time.RFC3339)
	s.mu.Unlock()
	s.saveState()

	// Command loop: reads from client, also watches for CLI exit and shutdown
	lineCh := make(chan []byte, 1)
	go func() {
		defer close(lineCh)
		for {
			postAuthLR.N = int64(maxClientLineBytes) + 1 // reset per-line limit
			line, err := reader.ReadBytes('\n')
			if err != nil {
				if postAuthLR.N == 0 {
					slog.Warn("post-auth line limit exceeded, disconnecting")
				}
				return
			}
			// Enforce line size limit (bufio.NewReaderSize only sets buffer, not max line)
			if len(line) > maxClientLineBytes {
				slog.Warn("client line too large, dropping", "size", len(line))
				continue
			}
			select {
			case lineCh <- line:
			case <-clientDone:
				return // handleClient exited; avoid goroutine leak
			}
		}
	}()

	for {
		select {
		case line, ok := <-lineCh:
			if !ok {
				return // client disconnected
			}
			msg, err := ParseClientMsg(bytes.TrimSpace(line))
			if err != nil {
				continue
			}
			switch msg.Type {
			case "write":
				if s.cli.alive() {
					s.cli.stdin.Write([]byte(msg.Line + "\n")) //nolint:errcheck
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
					CLIAlive: BoolPtr(s.cli.alive()),
					Buffered: s.buffer.Count(),
				}
				if data, err := resp.MarshalLine(); err == nil {
					s.enqueueWrite(append(data, '\n'))
				}
			case "shutdown":
				s.cli.closeStdin()
				s.cli.waitOrKill(5 * time.Second)
				s.initiateShutdown()
				return
			case "detach":
				return // disconnect but keep running
			}

		case <-s.cli.exited:
			if !cliWasAlive {
				// CLI was already dead at connection time; cli_exited sent during replay.
				// Closed channel fires immediately — ignore to avoid double delivery.
				return
			}
			// Send cli_exited (only from here — broadcastCLIExited is for unconnected state)
			code := s.cli.exitCode
			resp := ServerMsg{Type: "cli_exited", Code: IntPtr(code)}
			if data, err := resp.MarshalLine(); err == nil {
				s.enqueueWrite(append(data, '\n'))
			}
			return

		case <-s.done:
			return
		}
	}
}

// writeMsg writes a ServerMsg directly to a connection (used during auth/replay
// before the client becomes the active client with async writes).
func writeMsg(conn net.Conn, msg ServerMsg) {
	data, err := msg.MarshalLine()
	if err != nil {
		return
	}
	conn.Write(append(data, '\n')) //nolint:errcheck
}

func socketDir(socketPath string) string {
	dir := filepath.Dir(socketPath)
	if dir == "." || dir == "/" {
		return ""
	}
	return dir
}

// --- CLI process management ---

type cliProc struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   *bufio.Scanner
	stderrR  io.ReadCloser
	exited   chan struct{}
	exitCode int
	exitOnce sync.Once
	killOnce sync.Once
}

func startCLI(cliPath string, args []string, cwd string) (*cliProc, error) {
	cmd := exec.Command(cliPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if cwd != "" {
		cmd.Dir = cwd
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		stdin.Close()
		stdout.Close()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		return nil, fmt.Errorf("start: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)

	return &cliProc{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  scanner,
		stderrR: stderrPipe,
		exited:  make(chan struct{}),
	}, nil
}

func (c *cliProc) pid() int {
	if c.cmd.Process != nil {
		return c.cmd.Process.Pid
	}
	return 0
}

func (c *cliProc) alive() bool {
	select {
	case <-c.exited:
		return false
	default:
		return true
	}
}

func (c *cliProc) wait() {
	c.exitOnce.Do(func() {
		_ = c.cmd.Wait()
		if c.cmd.ProcessState != nil {
			c.exitCode = c.cmd.ProcessState.ExitCode()
		}
		close(c.exited)
	})
}

func (c *cliProc) interrupt() {
	if !c.alive() {
		return
	}
	if c.cmd.Process != nil {
		_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGINT)
	}
}

func (c *cliProc) kill() {
	c.killOnce.Do(func() {
		_ = c.stdin.Close()
		if c.cmd.Process != nil {
			_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
		}
	})
	c.wait()
}

func (c *cliProc) closeStdin() {
	_ = c.stdin.Close()
}

func (c *cliProc) waitOrKill(timeout time.Duration) {
	c.closeStdin()
	select {
	case <-c.exited:
	case <-time.After(timeout):
		c.kill()
	}
}

// CleanStaleSocket removes a socket file if no shim is listening on it.
func CleanStaleSocket(path string) error {
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err == nil {
		conn.Close()
		return fmt.Errorf("socket %s is alive, not removing", path)
	}
	return os.Remove(path)
}
