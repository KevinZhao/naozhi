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
)

// maxClientLineBytes limits the size of a single line read from the naozhi client,
// preventing unbounded memory allocation from malformed or malicious input.
const maxClientLineBytes = 16 * 1024 * 1024 // 16MB

// maxWriteLineBytes caps the inner "line" field of a post-auth "write" frame
// before it is piped into CLI stdin. The outer frame cap above accommodates
// an entire NDJSON envelope including 16-MB leeway; but every byte of
// msg.Line flows through bufio.Scanner on the Claude side, whose default
// buffer is 10 MB. Matching cap to the naozhi-side producer limit
// (cli.maxStdinLineBytes = 12 MB) keeps the shim a faithful pass-through
// while refusing anything that would overflow Claude's scanner and silently
// kill its stdout. Defense-in-depth against future drift: naozhi's dispatch
// layer already coalesces user text to a much smaller soft cap, so
// production paths never approach this limit. R67-SEC-5.
//
// R237-CR-4 (#701): held in an atomic.Int64 (instead of a plain `var int`)
// so the handleClient oversize-reject test can dial it down without racing
// the runtime read on the hot recv path. Tests use setMaxWriteLineBytes /
// maxWriteLineBytesValue; production callers read via Load().
const defaultMaxWriteLineBytes int64 = 12 * 1024 * 1024 // 12MB

var maxWriteLineBytes atomic.Int64

// maxWriteLineBytesValue returns the active write-line cap. Wraps the
// atomic load so call sites read like the legacy plain-int access.
func maxWriteLineBytesValue() int64 {
	v := maxWriteLineBytes.Load()
	if v == 0 {
		return defaultMaxWriteLineBytes
	}
	return v
}

// setMaxWriteLineBytes overrides the cap for tests. Returns the previous
// value so callers can restore it via defer. A value of zero resets to the
// compiled-in default.
func setMaxWriteLineBytes(v int64) int64 {
	return maxWriteLineBytes.Swap(v)
}

// Shim server timers. These three durations historically shared the same
// "30s" literal but are semantically independent:
//   - shimSocketWatchInterval: cadence of the defense-in-depth stat() poll
//     that detects a deleted AF_UNIX socket file so an orphaned shim can
//     initiate self-shutdown.
//   - shimShutdownGracePeriod: time after SIGTERM/SIGINT within which a
//     fresh client Attach must arrive; otherwise the shim exits (matches
//     the intended systemctl-stop behaviour).
//   - shimAuthReadDeadline: how long the shim waits for a connecting peer
//     to send its first line ("attach"/"hello") after AF_UNIX dial.
const (
	shimSocketWatchInterval = 30 * time.Second
	shimShutdownGracePeriod = 30 * time.Second
	shimAuthReadDeadline    = 30 * time.Second
)

// postExitReattachWindow is the grace period the shim keeps listening
// for a re-attaching naozhi client after the upstream CLI process has
// exited (cleanly or via watchdog). Within this window the shim
// preserves its socket so the operator's reconnect / fresh_context
// cron / dashboard auto-reattach can deliver the dead-CLI signal to
// the naozhi side; once exceeded the shim exits and naozhi schedules
// a fresh Spawn. Reused after a client has reattached so the second
// timer covers the post-handshake "now you should re-establish a CLI"
// window before the shim forfeits its slot.
//
// Closes R237-CR-7. Previously hard-coded as "60 * time.Second" four
// times in Run(); changing the value here updates every grace path
// uniformly. Do NOT confuse with shimShutdownGracePeriod (post-SIGTERM
// attach grace) or the freshShimShutdownGuard window (boot-handshake
// guard, see line ~975).
const postExitReattachWindow = 60 * time.Second

// freshShimShutdownGuard is the window after shim startup during
// which an *unauthenticated* "shutdown" command is ignored when the
// CLI is still alive — protects against handshake-glitch shutdowns
// before client buffers are primed. See the long comment on the
// refusal path for the full UCCLEP-2026-04-26 rationale. Mirrors the
// `const window = 60 * time.Second` used in
// TestShutdownGuard_EvaluationMatrix; bump both together.
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
}

// Run is the main entry point for the shim process.
//
// R237-CR-8 (#715): the log-file handle is a Run-local atomic.Pointer
// instead of a package-level var so test harnesses that spin up multiple
// shimServer instances in the same process (no production caller does
// this — the OS-level shim is one binary one Run — but the Run-as-library
// exec path under -race shares the address space) cannot clobber each
// other's deferred panic handler. atomic.Pointer is still required so the
// deferred recover() observes a memory-barrier-reachable handle even if
// the signal handler / panic recover races against the os.OpenFile branch
// initialization (R216-GO-2 invariant preserved).
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
			if f != nil {
				fmt.Fprintf(f, "PANIC: %v\n", r)
			}
		}
		if f != nil {
			fmt.Fprintf(f, "Run() returning at %s\n", time.Now().Format(time.RFC3339))
		}
		slog.Info("shim exiting")
		// Flush + close the log file so the final "returning"/"exiting"
		// lines survive sudden power loss or aggressive fs flush delays.
		if f != nil {
			_ = f.Sync()
			_ = f.Close()
		}
	}()

	// Signal handling
	ignoreHupPipe()

	// Start CLI subprocess
	cli, err := startCLI(cfg.CLIPath, cfg.CLIArgs, cfg.CWD)
	if err != nil {
		slog.Error("failed to start CLI", "err", err)
		return fmt.Errorf("start CLI: %w", err)
	}

	// Clean stale socket before binding
	_ = CleanStaleSocket(cfg.SocketPath)

	// Create unix socket listener with atomic permissions
	oldUmask := setUmask(0177)
	listener, err := net.Listen("unix", cfg.SocketPath)
	setUmask(oldUmask)
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
		Backend:   cfg.Backend,
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
		startedAt: time.Now(),
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
	// Socket self-watch: if something outside this process unlinks our socket
	// file (external cleaner, stray rm -rf, another process's StartShim that
	// bypassed the dial-first guard), the kernel keeps the AF_UNIX listener fd
	// alive but no new connection can ever reach it — we become an orphan
	// consuming a CLI seat and piling up ring-buffer events no one drains.
	// Poll stat() every 30s; on ENOENT, initiate the same shutdown path as
	// SIGTERM. This is a defense-in-depth layer behind Manager.Discover's
	// socket check (F4) and StartShim's dial-first guard (F3); under normal
	// operation it never fires.
	go s.watchSocketFile(cfg.SocketPath, shimSocketWatchInterval)

	// SIGTERM/SIGINT: always start a 30s grace period regardless of whether a
	// client is currently connected. Previously the grace timer was skipped when
	// clientConn != nil, causing `systemctl stop naozhi-shim-*` to be silently
	// ignored until systemd sent SIGKILL. Now we always arm the timer; the only
	// way to cancel it is a fresh client Attach (setClient clears graceTimer),
	// so a re-attached naozhi cancels shutdown. A plain Detach (clearClient)
	// does NOT cancel — if no new attach arrives within 30s, the shim exits,
	// which is the intended systemctl-stop behavior.
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

	// Accept loop
	idleTimeout := cfg.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = 4 * time.Hour
	}
	s.resetIdleTimer(idleTimeout)

	// Accept loop with bounded concurrency to prevent fd exhaustion
	const maxInflightClients = 16
	clientSem := make(chan struct{}, maxInflightClients)

	// spawnClient enforces the clientSem admission gate so every handleClient
	// goroutine (normal accept + post-cli_exited reconnect + post-watchdog
	// reconnect) pays the same semaphore slot. Previously only the main
	// accept branch was gated; the two reconnect branches did a raw `go
	// s.handleClient(...)` which let a post-kill reconnect storm bypass the
	// 16-slot cap exactly when fd pressure peaks. R71-GO-H1.
	spawnClient := func(conn net.Conn) {
		select {
		case clientSem <- struct{}{}:
			go func() {
				defer func() { <-clientSem }()
				s.handleClient(conn, idleTimeout)
			}()
		default:
			// Pool full → shed load identically to the main accept branch.
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

// waitForReattach implements the post-CLI-death grace window. After the
// CLI process has exited (cleanly via cli.exited or forcibly via
// watchdog.Fired) the shim keeps its socket open for postExitReattachWindow
// so a reconnecting naozhi client can pick up the dead-CLI signal. If a
// client connects, the helper hands it to spawnClient and waits a second
// postExitReattachWindow for the operator handoff to complete (or s.done
// to fire). Either way the helper returns once the window has elapsed or
// shutdown was requested.
//
// reason is woven into the log messages so cli_exited and watchdog paths
// remain individually traceable in operator logs even though the timer
// machinery is shared. Closes R237-CR-5 (#707).
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

// watchSocketFile polls the socket path and initiates shutdown if the file
// disappears. See startup comment next to `go s.watchSocketFile(...)` for
// the motivation — this handles the "zombie shim" failure mode where the
// listener fd is alive but the filesystem path is gone, making the shim
// unreachable to any new client.
//
// Stat() is used (not Lstat) so a symlink replacement points us at whatever
// the operator intended; if that target is missing we still fire. interval
// is parameterised so tests can exercise the trigger without a 30s wait.
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
			// Lstat (not Stat) so a symlink swap pointing at a still-existing
			// path cannot keep this watcher alive past the real socket's
			// removal — Stat follows symlinks and would happily report success
			// on the swap target. R218-SEC-12.
			if _, err := os.Lstat(socketPath); err != nil {
				// Only self-terminate on confirmed ENOENT — transient errors
				// like EACCES (SELinux relabel mid-deploy), ESTALE (NFS), or
				// EINTR would otherwise take down a healthy shim on the next
				// 30s tick. R65-GO-M-1.
				if !errors.Is(err, os.ErrNotExist) {
					slog.Warn("shim socket stat transient error, staying up",
						"socket", socketPath, "err", err)
					continue
				}
				// Do not retry — if the path is gone, it is gone; recreating
				// the socket from here would race StartShim's dial-first
				// guard and reintroduce the clobber bug we just fixed. Just
				// exit cleanly so naozhi can spawn a fresh shim on the next
				// message. Grace timer already armed by the SIGTERM handler
				// when no client attached; initiateShutdown shortcuts that.
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
		// Stop returns false if the timer already fired but the C channel
		// hasn't been drained yet. Without the non-blocking drain, a stale
		// idle event could outlive the Stop and bleed into the next reset
		// cycle on Go toolchains older than 1.23 (R224-GO-8). Modern
		// toolchains auto-drain on Reset; the select is belt-and-suspenders.
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

		// Extract session_id from init/result events
		s.tryExtractSessionID(line)

		// Build message and enqueue (non-blocking, no lock during Flush).
		//
		// R67-PERF-3: route the stdout hot path through MarshalStdoutLine,
		// which aliases `line` directly into the ServerMsg via unsafe.String
		// so json.Marshal walks the scanner's existing buffer instead of an
		// intermediate `string(line)` copy. enqueueWrite hands the result to
		// a client channel before the next Scan() runs, so the alias never
		// outlives this iteration. Wire output is byte-for-byte identical to
		// the prior MarshalLine path (round-tripped in protocol_test.go).
		if data, err := MarshalStdoutLine(seq, line); err == nil {
			s.enqueueWrite(data)
		}
	}

	// CLI stdout closed
	s.cli.wait()
	slog.Info("CLI stdout EOF")
}

func (s *shimServer) tryExtractSessionID(line []byte) {
	// Fast gate: fires on every CLI stdout line (5-50/s during active turns).
	// Only init / result / ACP session/new events carry session_id; the vast
	// majority of lines are assistant_delta / tool_use and contain neither
	// token. Two bytes.Contains scans cover both the claude (snake_case
	// "session_id") and ACP/kiro (camelCase "sessionId") frame conventions.
	// R65-PERF-H-3 design preserved: still no full decoder-state alloc when
	// the line lacks both tokens.
	hasSnake := bytes.Contains(line, []byte(`"session_id"`))
	hasCamel := bytes.Contains(line, []byte(`"sessionId"`))
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
			s.mu.Unlock()
			return
		}
	}

	if hasCamel {
		// ACP / kiro: session/new response carries the canonical session ID
		// in result.sessionId; subsequent session/update notifications also
		// echo it. The first sighting wins so a later notification can't
		// overwrite a freshly-assigned ID with a stale one.
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
		s.mu.Unlock()
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
		slog.Debug("cli stderr", "line", line)

		msg := ServerMsg{Type: "stderr", Line: line}
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

// performHandshake runs the pre-active-client auth phase: peer-UID
// verification, attach-message read under a deadline + LimitedReader, and
// constant-time token compare against s.tokenRaw. On success it clears the
// auth read deadline (so the post-auth command loop is not capped at
// shimAuthReadDeadline) and returns the parsed attach message + ok=true.
//
// Carved out of handleClient (R219-CR-8 / #657) so the auth gate is
// independently testable and the parent function reads as a flat:
//
//	handshake → hello → replay → become-active → loop.
//
// Pure semantic-equivalent extraction; ALL the legacy guard-rails are
// preserved in place:
//   - VerifyPeerUID Warn-level audit log on UID mismatch.
//   - SetReadDeadline failure short-circuits BEFORE the read loop starts
//     (prevents goroutine leak if the conn is half-closed).
//   - LimitedReader caps pre-auth memory at maxClientLineBytes+1 so a
//     pre-auth peer cannot OOM the shim with a very long attach line.
//   - subtle.ConstantTimeCompare guards the token check.
//   - On token failure we send "auth_failed" so the client can surface
//     the reason (vs. silent close which would look like a network blip).
//   - Clearing the read deadline post-auth is itself fail-safe: if the
//     clear fails (rare; conn dying) we bail rather than leave a stale
//     deadline armed against the post-auth command loop.
func (s *shimServer) performHandshake(conn net.Conn) (ClientMsg, bool) {
	// Verify connecting peer has same UID (defense-in-depth beyond token auth)
	if !VerifyPeerUID(conn) {
		// Elevated to Warn: a UID mismatch on a unix socket that is supposed
		// to be owner-private is an audit-worthy event. Debug-level output
		// is silenced in production deployments and would hide an in-progress
		// same-host privilege escalation attempt.
		slog.Warn("shim: rejecting client with UID mismatch",
			"remote", conn.RemoteAddr().String())
		return ClientMsg{}, false
	}

	// Set read deadline for auth phase (shimAuthReadDeadline to send attach).
	// If the deadline can't be installed (conn already half-closed), the
	// read loop below would block until TCP keepalive expires — leak the
	// goroutine. Bail out immediately instead.
	if err := conn.SetReadDeadline(time.Now().Add(shimAuthReadDeadline)); err != nil {
		slog.Debug("shim: set auth read deadline failed", "err", err)
		return ClientMsg{}, false
	}

	// Use LimitedReader to prevent pre-auth memory exhaustion
	lr := &io.LimitedReader{R: conn, N: int64(maxClientLineBytes) + 1}
	reader := bufio.NewReaderSize(lr, 4096)

	// Read attach message
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

	// Clear read deadline after successful auth. If clearing fails, an old
	// deadline could fire and prematurely kick a healthy client; bail out so
	// the client reconnects cleanly.
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
	postAuthLR := &io.LimitedReader{R: conn, N: int64(maxClientLineBytes) + 1}
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
		writeMsg(conn, ServerMsg{Type: "replay", Seq: l.seq, Line: string(l.data)})
	}
	writeMsg(conn, ServerMsg{Type: "replay_done", Count: len(lines)})

	// If CLI already exited, notify and skip the command loop's cli.exited select
	// to avoid sending cli_exited twice (closed channel is always selectable).
	cliWasAlive := cliAlive
	if !cliAlive {
		writeMsg(conn, ServerMsg{Type: "cli_exited", Code: intPtr(s.cli.exitCode)})
	}

	// Reject new client if CLI is alive and another client is already connected.
	// This prevents an unexpected reconnect from kicking out a client that is
	// actively communicating with the CLI (race during session creation).
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

	// Stop disconnect watchdog and cancel SIGTERM grace timer (if active).
	// A new client connecting means the shim is needed — don't shut down.
	s.watchdog.Stop()
	s.mu.Lock()
	if s.graceTimer != nil {
		s.graceTimer.Stop()
		s.graceTimer = nil
	}
	s.mu.Unlock()

	// Writer goroutine: drains writeCh to the socket, exits on clientDone.
	// A per-flush write deadline bounds slow/stuck reader scenarios so
	// Flush cannot wedge the goroutine beyond 10s even if the outer
	// conn.Close() in the defer is delayed.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		w := bufio.NewWriter(conn)
		// If SetWriteDeadline fails (conn already closed / half-closed), skip the
		// Flush entirely. Without a deadline, bufio.Flush can block until TCP
		// keepalive expires (minutes), wedging this writer goroutine and
		// starving the defer that signals clientDone to readers. Clearing the
		// deadline after a successful Flush is best-effort — if it fails the
		// conn is dying and the outer close will tear everything down.
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

	s.runCommandLoop(reader, postAuthLR, clientDone, cliWasAlive)
}

// runCommandLoop is the post-auth, post-replay client message dispatch loop.
// Carved out of handleClient (#697 / R237-CR-3) so the 327-line parent
// stays comprehensible to AI-review windows. Pure semantic-equivalent
// extraction: same goroutine ownership, same return semantics (any return
// here unwinds the calling handleClient defers), same channel topology.
//
//   - reader / postAuthLR: bounded post-auth line reader; postAuthLR.N is
//     reset per-line so the LimitedReader gate fires on oversize input.
//   - clientDone: closed by setClient teardown; the producer goroutine
//     watches it to avoid leaking when handleClient returns.
//   - cliWasAlive: snapshot of cli.alive() taken at attach time. Drives
//     the cli.exited dedup so a dead-CLI replay path doesn't re-emit
//     cli_exited (closed channel is always selectable).
func (s *shimServer) runCommandLoop(
	reader *bufio.Reader,
	postAuthLR *io.LimitedReader,
	clientDone <-chan struct{},
	cliWasAlive bool,
) {
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
			// Enforce line size limit (bufio.NewReaderSize only sets buffer, not max line).
			// Disconnect on oversize lines: a misbehaving/malicious client that flood-sends
			// large lines would otherwise burn CPU in a tight loop while holding the
			// single-client semaphore slot — better to sever and let them reconnect.
			if len(line) > maxClientLineBytes {
				slog.Warn("client line too large, disconnecting", "size", len(line))
				return
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
			if disconnect := s.handleClientCommand(msg); disconnect {
				return
			}

		case <-s.cli.exited:
			if !cliWasAlive {
				// CLI was already dead at connection time; cli_exited sent during replay.
				// Closed channel fires immediately — ignore to avoid double delivery.
				return
			}
			// Send cli_exited to the connected client.
			code := s.cli.exitCode
			resp := ServerMsg{Type: "cli_exited", Code: intPtr(code)}
			if data, err := resp.MarshalLine(); err == nil {
				s.enqueueWrite(data)
			}
			return

		case <-s.done:
			return
		}
	}
}

// handleClientCommand dispatches a single ClientMsg into the appropriate
// shim action. Returns true when the caller must disconnect the client
// (oversize write, stdin EPIPE, shutdown, detach, refused-shutdown
// guard); returns false to keep the loop running. Carved out of
// runCommandLoop's switch (#697 / R237-CR-3) so each verb's policy
// stays readable in isolation.
func (s *shimServer) handleClientCommand(msg ClientMsg) (disconnect bool) {
	switch msg.Type {
	case "write":
		// Reject payloads that would overflow Claude's 10 MB bufio.Scanner
		// buffer and deadlock stdout. naozhi's own dispatch layer caps
		// coalesced user text well below this ceiling; a hostile or buggy
		// client reaching this path is treated as a protocol violation
		// and disconnected so the slot frees for healthy clients.
		// R67-SEC-5.
		if limit := maxWriteLineBytesValue(); int64(len(msg.Line)) > limit {
			slog.Warn("client write too large, disconnecting",
				"size", len(msg.Line), "limit", limit)
			return true
		}
		if s.cli.alive() {
			// R190-ERR-M1: previously the write error was silently
			// dropped. If the CLI process dies between alive() and
			// Write (EPIPE), the client's message is lost without
			// notification — the client keeps waiting for a reply
			// that will never arrive until its next ping times out.
			// Log the failure and disconnect the client so it can
			// reconnect to a fresh shim; cli.exited will fire on
			// the next loop iteration and take the normal exit path.
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
		// Only refuse an "early shutdown" when it comes from a path
		// with no authenticated client. An authenticated client
		// (naozhi) issuing shutdown within the 60s window means the
		// client has made the deliberate choice to tear this shim
		// down — fresh_context cron, explicit Router.Reset, config
		// drift handling, etc. Blocking those would keep the shim's
		// socket listening for 30+ seconds and cause the "refusing
		// to clobber" regression on fast restart (UCCLEP-2026-04-26).
		// The 60s window was originally added to protect against
		// handshake glitches where a half-ready shim receives an
		// errant shutdown before buffers are primed; that's only
		// meaningful when no client is actively driving the
		// lifecycle. We're inside the per-client message loop here,
		// so clientConn normally equals conn — the defensive check
		// below stays in case a future refactor drifts that.
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

// writeMsg writes a ServerMsg directly to a connection (used during auth/replay
// before the client becomes the active client with async writes).
// Enforces a 10s write deadline so a malicious or stalled client cannot pin
// the single-client semaphore slot indefinitely by refusing to read.
func writeMsg(conn net.Conn, msg ServerMsg) {
	data, err := msg.MarshalLine()
	if err != nil {
		return
	}
	// If SetWriteDeadline fails (conn already closed by defer teardown), skip
	// the write. Without a deadline, a stalled client could pin the single-
	// client semaphore slot indefinitely — the point of the deadline is to
	// guarantee this helper never blocks beyond 10s.
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
	// Use time.NewTimer + defer Stop instead of time.After: the fast-path
	// (c.exited fires first) would otherwise leave a parked timer goroutine
	// until the full timeout elapses. Called up to 3 times per shutdown.
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

// ensureSocketFreeForReuse is the StartShim-side pre-bind check: if someone
// is actively listening on the target socket, refuse to clobber — removing
// a live listener's filesystem entry silently turns the peer into a zombie
// (its listener fd is still held by the kernel, but no new client can dial
// it). That is exactly the UCCLEP-2026-04-25 regression.
//
// 500ms is generous — Unix domain socket connect is microseconds under
// normal load; anything slower is already diagnostic of a hosed peer and
// we would rather block StartShim briefly than race into corruption. The
// split from CleanStaleSocket exists because CleanStaleSocket is also used
// by the shim-side bind path, where a different error surface is expected
// (the shim owns the socket and just wants to rm leftovers).
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
// Used by callers that just asked a shim to shut down and intend to spawn
// a fresh one on the same key — observing socket unlink before StartShim
// avoids the dial-first guard ("refusing to clobber") and the 30s zombie
// window described in server.go's shutdown path.
//
// The socket file is unlinked in the shim's Run defer chain after its
// listener closes, so this normally returns in a single 20ms tick once
// the shim exits. We poll by stat rather than dial because a dial would
// re-establish connection state with any remaining accept goroutine and
// muddies the semantics; stat maps directly to "is the filesystem entry
// still there".
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
