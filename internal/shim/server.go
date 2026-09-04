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
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
)

// defaultMaxClientLineBytes is the compiled-in per-line size limit.
const defaultMaxClientLineBytes = 16 * 1024 * 1024 // 16MB

// maxClientLineBytesAtomic holds the per-line limit so tests can dial it
// down without racing the runCommandLoop reader; zero means default (#701).
var maxClientLineBytesAtomic atomic.Int64

// maxClientLineBytes returns the active per-line read limit.
func maxClientLineBytes() int {
	v := maxClientLineBytesAtomic.Load()
	if v == 0 {
		return defaultMaxClientLineBytes
	}
	return int(v)
}

// setMaxClientLineBytes overrides the per-line limit for tests; returns the
// previous value, zero resets to the default.
func setMaxClientLineBytes(v int) int {
	return int(maxClientLineBytesAtomic.Swap(int64(v)))
}

// defaultMaxClientSessionBytes is the cumulative post-auth byte budget per
// client connection: the per-line LimitedReader resets every iteration, so
// without it an authenticated client could churn near-16-MB lines
// indefinitely (memory-pressure DoS). Generous on purpose (#541).
const defaultMaxClientSessionBytes int64 = defaultMaxClientLineBytes * 1000

// maxClientSessionBytes is atomic so tests can dial it down without racing
// the recv hot path; zero means default.
var maxClientSessionBytes atomic.Int64

// maxClientSessionBytesValue returns the active cumulative cap. A zero
// stored value resolves to defaultMaxClientSessionBytes.
func maxClientSessionBytesValue() int64 {
	v := maxClientSessionBytes.Load()
	if v == 0 {
		return defaultMaxClientSessionBytes
	}
	return v
}

// setMaxClientSessionBytes overrides the cumulative cap for tests; returns
// the previous value, zero resets to the default.
func setMaxClientSessionBytes(v int64) int64 {
	return maxClientSessionBytes.Swap(v)
}

// defaultMaxWriteLineBytes caps the inner "line" field of a post-auth "write"
// frame before it is piped into CLI stdin. Every byte of msg.Line flows
// through bufio.Scanner on the Claude side (10 MB default buffer); matching
// the naozhi-side producer limit (cli.maxStdinLineBytes = 12 MB) keeps the
// shim a faithful pass-through while refusing anything that would overflow
// Claude's scanner and silently kill its stdout. Held in an atomic so tests
// can dial it down without racing the recv hot path (#701).
const defaultMaxWriteLineBytes int64 = 12 * 1024 * 1024 // 12MB

var maxWriteLineBytes atomic.Int64

// maxWriteLineBytesValue returns the active write-line cap; zero means default.
func maxWriteLineBytesValue() int64 {
	v := maxWriteLineBytes.Load()
	if v == 0 {
		return defaultMaxWriteLineBytes
	}
	return v
}

// setMaxWriteLineBytes overrides the cap for tests; returns the previous
// value, zero resets to the default.
func setMaxWriteLineBytes(v int64) int64 {
	return maxWriteLineBytes.Swap(v)
}

// Shim server timers (semantically independent despite equal values):
//   - shimSocketWatchInterval: stat() poll cadence detecting a deleted AF_UNIX
//     socket file so an orphaned shim can self-shutdown.
//   - shimShutdownGracePeriod: window after SIGTERM/SIGINT for a fresh client
//     Attach; otherwise the shim exits.
//   - shimAuthReadDeadline: wait for a connecting peer's first line.
const (
	shimSocketWatchInterval = 30 * time.Second
	shimShutdownGracePeriod = 30 * time.Second
	shimAuthReadDeadline    = 30 * time.Second
)

// postExitReattachWindow is how long the shim keeps listening for a
// re-attaching naozhi client after the CLI exited (cleanly or via watchdog),
// so the dead-CLI signal can be delivered; reused after a reattach as the
// "re-establish a CLI" window before the shim forfeits its slot. Distinct
// from shimShutdownGracePeriod and freshShimShutdownGuard.
const postExitReattachWindow = 60 * time.Second

// freshShimShutdownGuard is the window after startup during which an
// *unauthenticated* "shutdown" is ignored while the CLI is alive (protects
// against handshake-glitch shutdowns before client buffers are primed).
// TestShutdownGuard_EvaluationMatrix mirrors the value; bump both together.
const freshShimShutdownGuard = 60 * time.Second

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
	Backend         string // "claude" | "kiro" | ...; stored for reconnect routing
	CLIArgs         []string
	CWD             string
	// SpawnOverlay is recorded verbatim in the state file for the parent's
	// arg-drift comparison (#2494). Nil when the spawning naozhi predates
	// the --spawn-overlay flag; opaque to the shim otherwise.
	SpawnOverlay *SpawnOverlay
}

// Run is the main entry point for the shim process.
//
// The log-file handle is a Run-local atomic.Pointer (not a package var) so
// test harnesses running multiple shimServer instances in one process cannot
// clobber each other's deferred panic handler; atomic so the deferred
// recover() observes the handle even if it races the OpenFile branch (#715).
func Run(cfg Config) error {
	var shimLogFilePtr atomic.Pointer[os.File]
	// Redirect slog to a persistent log file so shim logs survive parent restart.
	logPath := filepath.Join(filepath.Dir(cfg.StateFile), fmt.Sprintf("shim-%d.log", os.Getpid()))
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600); err == nil {
		shimLogFilePtr.Store(f)
		slog.SetDefault(slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})))
		os.Stderr = f
	}
	slog.Info("shim starting", "pid", os.Getpid(), "key", cfg.Key)
	defer func() {
		f := shimLogFilePtr.Load()
		if r := recover(); r != nil {
			slog.Error("shim: Run() panicked", "recover", r, "stack", string(debug.Stack()))
			if f != nil {
				fmt.Fprintf(f, "PANIC: %v\n", r)
			}
		}
		if f != nil {
			fmt.Fprintf(f, "Run() returning at %s\n", time.Now().Format(time.RFC3339))
		}
		slog.Info("shim exiting")
		// Sync+close so the final lines survive power loss.
		if f != nil {
			_ = f.Sync()
			_ = f.Close()
		}
	}()

	ignoreHupPipe()

	cli, err := startCLI(cfg.CLIPath, cfg.CLIArgs, cfg.CWD)
	if err != nil {
		slog.Error("failed to start CLI", "err", err)
		return fmt.Errorf("start CLI: %w", err)
	}

	_ = CleanStaleSocket(cfg.SocketPath)

	// umask 0177 so the socket file is created 0600 atomically.
	oldUmask := setUmask(0177)
	listener, err := net.Listen("unix", cfg.SocketPath)
	setUmask(oldUmask)
	if err != nil {
		cli.kill()
		return fmt.Errorf("listen %s: %w", cfg.SocketPath, err)
	}
	defer listener.Close()
	defer os.Remove(cfg.SocketPath)

	// Re-apply 0700 for pre-existing dirs.
	if dir := socketDir(cfg.SocketPath); dir != "" {
		os.Chmod(dir, 0700) //nolint:errcheck
	}

	tokenRaw, tokenB64, err := GenerateToken()
	if err != nil {
		cli.kill()
		return err
	}

	state := State{
		ShimPID:      os.Getpid(),
		CLIPID:       cli.pid(),
		Socket:       cfg.SocketPath,
		AuthToken:    tokenB64,
		Key:          cfg.Key,
		Workspace:    cfg.CWD,
		Backend:      cfg.Backend,
		CLIArgs:      cfg.CLIArgs,
		SpawnOverlay: cfg.SpawnOverlay,
		CLIAlive:     true,
		StartedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	if err := WriteStateFile(cfg.StateFile, state); err != nil {
		slog.Warn("failed to write state file", "err", err)
	}
	defer RemoveStateFile(cfg.StateFile)

	// Output ready signal to parent, then detach stdio
	fmt.Fprintf(os.Stdout, `{"status":"ready","pid":%d,"token":"%s"}`+"\n", os.Getpid(), tokenB64)
	os.Stdout.Close()
	os.Stdin.Close()

	buf := NewRingBuffer(cfg.BufferSize, cfg.MaxBufBytes)

	s := &shimServer{
		cli:       cli,
		listener:  listener,
		buffer:    buf,
		tokenRaw:  tokenRaw,
		stateFile: cfg.StateFile,
		state:     state,
		startedAt: time.Now(),
		done:      make(chan struct{}),
	}

	s.watchdog = NewWatchdog(cfg.WatchdogTimeout, func() {
		slog.Warn("watchdog: killing unresponsive CLI")
		cli.kill()
	})

	go s.readStdout()
	go s.readStderr()
	// Socket self-watch: if something outside this process unlinks our socket
	// file, the kernel keeps the listener fd alive but no client can ever
	// reach it — an orphan holding a CLI seat. Defense-in-depth behind
	// Manager.Discover's socket check and StartShim's dial-first guard.
	go s.watchSocketFile(cfg.SocketPath, shimSocketWatchInterval)

	// SIGTERM/SIGINT: always arm the grace timer, even with a client attached
	// (otherwise systemctl stop is ignored until SIGKILL). Only a fresh client
	// Attach cancels it (setClient clears graceTimer); a plain Detach does not.
	sigCh := make(chan os.Signal, 1)
	notifyTerminate(sigCh)
	go func() {
		for range sigCh {
			s.mu.Lock()
			hasClient := s.clientConn != nil
			if hasClient {
				slog.Info("SIGTERM received with active client, starting 30s grace period (waiting for detach)")
			} else {
				slog.Info("SIGTERM received, starting 30s grace period")
			}
			// Stop() on a fired timer is safe: initiateShutdown is guarded by
			// doneOnce so duplicate calls are no-ops.
			if s.graceTimer != nil {
				s.graceTimer.Stop()
			}
			s.graceTimer = time.AfterFunc(shimShutdownGracePeriod, func() {
				slog.Info("grace period expired, shutting down")
				s.initiateShutdown()
			})
			s.mu.Unlock()
		}
	}()

	// SIGUSR2: immediate shutdown
	usr2Ch := make(chan os.Signal, 1)
	notifyUSR2(usr2Ch)
	go func() {
		<-usr2Ch
		slog.Info("SIGUSR2 received, immediate shutdown")
		s.initiateShutdown()
	}()

	idleTimeout := cfg.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = 4 * time.Hour
	}
	s.resetIdleTimer(idleTimeout)

	// Accept loop with bounded concurrency to prevent fd exhaustion
	const maxInflightClients = 16
	clientSem := make(chan struct{}, maxInflightClients)

	// spawnClient enforces the clientSem admission gate for every handleClient
	// goroutine (main accept + both post-CLI-death reconnect paths), so a
	// post-kill reconnect storm cannot bypass the cap when fd pressure peaks.
	spawnClient := func(conn net.Conn) {
		select {
		case clientSem <- struct{}{}:
			go func() {
				defer func() { <-clientSem }()
				s.handleClient(conn, idleTimeout)
			}()
		default:
			// Pool full: shed load.
			conn.Close()
		}
	}

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
			spawnClient(conn)

		case <-cli.exited:
			slog.Info("CLI exited", "code", cli.exitCode)
			s.saveStateCLIDead()
			s.waitForReattach(acceptCh, spawnClient, "cli exit")
			return nil

		case <-s.idleC():
			s.mu.Lock()
			hasClient := s.clientConn != nil
			s.mu.Unlock()
			if !hasClient {
				slog.Info("idle timeout, shutting down")
				cli.closeStdin()
				cli.waitOrKill(5 * time.Second)
				slog.Info("exiting: idle timeout")
				return nil
			}

		case <-s.watchdog.Fired():
			slog.Warn("watchdog fired, CLI killed")
			s.saveStateCLIDead()
			s.waitForReattach(acceptCh, spawnClient, "watchdog")
			return nil

		case <-s.done:
			slog.Info("shutdown initiated")
			cli.closeStdin()
			cli.waitOrKill(5 * time.Second)
			slog.Info("exiting: shutdown done")
			return nil
		}
	}
}

// waitForReattach keeps the socket open for postExitReattachWindow after the
// CLI died so a reconnecting naozhi client can pick up the dead-CLI signal;
// if one connects it is handed to spawnClient and a second window covers the
// operator handoff. Returns once the window elapses or s.done fires. reason
// keeps the cli_exited and watchdog paths distinguishable in logs (#707).
func (s *shimServer) waitForReattach(acceptCh <-chan net.Conn, spawnClient func(net.Conn), reason string) {
	exitTimer := time.NewTimer(postExitReattachWindow)
	select {
	case conn := <-acceptCh:
		exitTimer.Stop()
		spawnClient(conn)
		reconnectTimer := time.NewTimer(postExitReattachWindow)
		select {
		case <-s.done:
			reconnectTimer.Stop()
			slog.Info("exiting: done after " + reason + " + reconnect")
		case <-reconnectTimer.C:
			slog.Info("exiting: post-exit reattach window expired after "+reason+" + reconnect",
				"window", postExitReattachWindow)
		}
	case <-s.done:
		exitTimer.Stop()
		slog.Info("exiting: done after " + reason)
	case <-exitTimer.C:
		slog.Info("exiting: post-exit reattach window expired after "+reason,
			"window", postExitReattachWindow)
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
	startedAt time.Time

	mu    sync.Mutex
	state State
	// sessionIDKnown mirrors state.SessionID != "" so the stdout hot path
	// skips the per-line scan + Unmarshal without taking s.mu. The ID is
	// assigned once and never cleared, so a one-way latch is safe.
	sessionIDKnown atomic.Bool
	// clientAttached mirrors clientConn != nil so readStdout can skip the
	// per-line frame marshal when no naozhi is attached (#2295). Hint only:
	// a stale true falls through to enqueueWrite, which is nil-safe.
	clientAttached atomic.Bool
	clientConn     net.Conn      // current connected client (at most one)
	writeCh        chan []byte   // buffered channel for async writes to client
	clientDone     chan struct{} // closed to signal writer goroutine + enqueueWrite to stop
	graceTimer     *time.Timer
	idleTimer      *time.Timer
	done           chan struct{} // closed on shutdown
	doneOnce       sync.Once
}

func (s *shimServer) initiateShutdown() {
	s.doneOnce.Do(func() { close(s.done) })
}

// watchSocketFile polls the socket path and initiates shutdown if the file
// disappears (listener fd alive but path gone = unreachable zombie shim).
// interval is parameterised for tests.
func (s *shimServer) watchSocketFile(socketPath string, interval time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("shim watchSocketFile panic recovered",
				"panic", r, "stack", string(debug.Stack()))
		}
	}()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			// Lstat (not Stat): a symlink swap pointing at a still-existing
			// path must not keep this watcher alive past the real socket's removal.
			if _, err := os.Lstat(socketPath); err != nil {
				// Only self-terminate on ENOENT — transient EACCES/ESTALE/EINTR
				// must not take down a healthy shim.
				if !errors.Is(err, os.ErrNotExist) {
					slog.Warn("shim socket stat transient error, staying up",
						"socket", socketPath, "err", err)
					continue
				}
				// Do not recreate the socket here: that would race StartShim's
				// dial-first guard. Exit so naozhi spawns a fresh shim.
				slog.Warn("shim socket file disappeared, shutting down",
					"socket", socketPath, "err", err)
				s.initiateShutdown()
				return
			}
		}
	}
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
		// Drain if Stop returns false so a stale idle event cannot bleed into
		// the next reset cycle (pre-1.23 toolchains; belt-and-suspenders now).
		if !s.idleTimer.Stop() {
			select {
			case <-s.idleTimer.C:
			default:
			}
		}
	}
	s.idleTimer = time.NewTimer(d)
}

// setClient atomically replaces the current client and returns a write channel + done channel.
// The old client (if any) is kicked. Must only be called AFTER auth succeeds.
func (s *shimServer) setClient(conn net.Conn) (writeCh chan []byte, clientDone chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Kick old client: close its done channel, then its conn. Never close
	// writeCh — the writer goroutine drains it.
	if s.clientConn != nil {
		if s.clientDone != nil {
			close(s.clientDone)
		}
		s.clientConn.Close()
	}

	s.clientConn = conn
	s.writeCh = make(chan []byte, 256)
	s.clientDone = make(chan struct{})
	s.clientAttached.Store(true)

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
		s.clientAttached.Store(false)
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
	defer func() {
		if r := recover(); r != nil {
			slog.Error("shim readStdout panic recovered",
				"panic", r, "stack", string(debug.Stack()))
		}
	}()
	for s.cli.stdout.Scan() {
		line := s.cli.stdout.Bytes() // valid until next Scan()

		seq := s.buffer.Push(line) // Push makes its own copy for replay storage
		s.watchdog.Reset()

		s.tryExtractSessionID(line)

		// Skip the per-line frame marshal when no naozhi is attached (#2295);
		// Push/extract/Reset above still run. A client attaching mid-line
		// misses one frame, which replay backfills on attach.
		if !s.clientAttached.Load() {
			continue
		}

		// MarshalStdoutLine aliases `line` via unsafe.String; enqueueWrite hands
		// the result off before the next Scan(), so the alias never outlives
		// this iteration.
		if data, err := MarshalStdoutLine(seq, line); err == nil {
			s.enqueueWrite(data)
		}
	}

	s.cli.wait()
	slog.Info("CLI stdout EOF")
}

// tryExtractSessionID search keys, hoisted to avoid a per-line []byte alloc.
// snake = claude stream-json frames, camel = ACP/kiro frames.
var (
	sessionIDSnakeKey = []byte(`"session_id"`)
	sessionIDCamelKey = []byte(`"sessionId"`)
)

func (s *shimServer) tryExtractSessionID(line []byte) {
	// The ID is assigned once and never cleared; the latch skips the scan and
	// s.mu on the common already-known case.
	if s.sessionIDKnown.Load() {
		return
	}

	// Only init / result / ACP session/new frames carry the ID; two Contains
	// scans avoid a decoder alloc for the vast majority of lines.
	hasSnake := bytes.Contains(line, sessionIDSnakeKey)
	hasCamel := bytes.Contains(line, sessionIDCamelKey)
	if !hasSnake && !hasCamel {
		return
	}

	if hasSnake {
		var ev struct {
			Type      string `json:"type"`
			SubType   string `json:"subtype"`
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(line, &ev) == nil && ev.SessionID != "" {
			s.mu.Lock()
			if ev.Type == "system" && ev.SubType == "init" {
				s.state.SessionID = ev.SessionID
			}
			if ev.Type == "result" && s.state.SessionID == "" {
				s.state.SessionID = ev.SessionID
			}
			known := s.state.SessionID != ""
			s.mu.Unlock()
			if known {
				s.sessionIDKnown.Store(true)
			}
			return
		}
	}

	if hasCamel {
		// ACP / kiro: session/new response carries result.sessionId; later
		// session/update notifications echo it in params. First sighting wins.
		var ev struct {
			Result struct {
				SessionID string `json:"sessionId"`
			} `json:"result"`
			Params struct {
				SessionID string `json:"sessionId"`
			} `json:"params"`
		}
		if json.Unmarshal(line, &ev) != nil {
			return
		}
		sid := ev.Result.SessionID
		if sid == "" {
			sid = ev.Params.SessionID
		}
		if sid == "" {
			return
		}
		s.mu.Lock()
		if s.state.SessionID == "" {
			s.state.SessionID = sid
		}
		known := s.state.SessionID != ""
		s.mu.Unlock()
		if known {
			s.sessionIDKnown.Store(true)
		}
	}
}

// readStderr reads CLI stderr and forwards to client.
func (s *shimServer) readStderr() {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("shim readStderr panic recovered",
				"panic", r, "stack", string(debug.Stack()))
		}
	}()
	scanner := bufio.NewScanner(s.cli.stderrR)
	scanner.Buffer(make([]byte, 4*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		safeLine := osutil.SanitizeForLog(line, 512)
		slog.Debug("cli stderr", "line", safeLine)

		wsLine := line
		if len(line) > 64*1024 {
			wsLine = line[:64*1024] + "...[truncated]"
		}
		msg := ServerMsg{Type: "stderr", Line: wsLine}
		if data, err := msg.MarshalLine(); err == nil {
			s.enqueueWrite(data)
		}
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
func writeRaw(conn net.Conn, data []byte) {
	// Deadline set failed (conn already closed): skip the write.
	if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return
	}
	defer func() { _ = conn.SetWriteDeadline(time.Time{}) }()
	conn.Write(data) //nolint:errcheck
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
	setSetsid(cmd)
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
		_ = sendProcGroupSIGINT(c.cmd.Process.Pid)
	}
}

func (c *cliProc) kill() {
	c.killOnce.Do(func() {
		_ = c.stdin.Close()
		if c.cmd.Process != nil {
			_ = sendProcGroupSIGKILL(c.cmd.Process.Pid)
		}
	})
	c.wait()
}

func (c *cliProc) closeStdin() {
	_ = c.stdin.Close()
}

func (c *cliProc) waitOrKill(timeout time.Duration) {
	c.closeStdin()
	// NewTimer + Stop rather than time.After so the fast path does not leave a
	// parked timer for the full timeout.
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-c.exited:
	case <-t.C:
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

// ensureSocketFreeForReuse is the StartShim-side pre-bind check: refuse to
// clobber a live listener, since removing its filesystem entry turns the peer
// into an unreachable zombie (fd still held by the kernel). 500ms is generous
// for a unix connect; slower is already diagnostic. Separate from
// CleanStaleSocket, whose shim-side bind path expects a different error surface.
func ensureSocketFreeForReuse(socketPath string) error {
	if conn, err := net.DialTimeout("unix", socketPath, 500*time.Millisecond); err == nil {
		_ = conn.Close()
		return fmt.Errorf("shim already listening on %s: refusing to clobber", socketPath)
	}
	_ = os.Remove(socketPath)
	return nil
}

// WaitSocketGone polls the socket path until it disappears or maxWait
// elapses. Returns true when the socket is gone; false on timeout.
//
// Used by callers that just asked a shim to shut down and will spawn a fresh
// one on the same key: observing the unlink avoids the dial-first "refusing
// to clobber" guard. Polls by stat (not dial) so no connection state is
// re-established with a lingering accept goroutine.
func WaitSocketGone(socketPath string, maxWait time.Duration) bool {
	if socketPath == "" {
		return true
	}
	deadline := time.Now().Add(maxWait)
	// Fast path: already gone.
	if _, err := os.Stat(socketPath); errors.Is(err, fs.ErrNotExist) {
		return true
	}
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for {
		<-t.C
		if _, err := os.Stat(socketPath); errors.Is(err, fs.ErrNotExist) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}
