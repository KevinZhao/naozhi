package shim

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

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
