package shim

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
)

// ErrMaxShims is returned by StartShim when the configured shim cap is hit.
// Distinct from session.ErrMaxProcs so callers can apply different retry
// policies: max shims means process table is saturated, clears as sessions
// exit; not a configuration problem.
var ErrMaxShims = errors.New("max shims reached")

// Manager manages shim process lifecycle: starting, discovering, and reconnecting.
type Manager struct {
	stateDir        string
	cliPath         string
	idleTimeout     time.Duration
	watchdogTimeout time.Duration
	bufferSize      int
	maxBufBytes     int64
	maxShims        int
	naozhiBin       string // path to naozhi binary for spawning shim subprocess
	// shimEnv is the filtered environment handed to every spawned shim,
	// computed once at Manager construction. The process env does not change
	// at runtime, so recomputing filterShimEnv(os.Environ()) on every spawn
	// would redo the same O(env × prefixes) scan for no benefit.
	shimEnv []string

	mu           sync.Mutex
	shims        map[string]*ShimHandle // key → active shim handle
	pendingShims int                    // spawn in progress, not yet in shims map
}

// ShimHandle represents a running shim that naozhi is connected to.
type ShimHandle struct {
	Conn       net.Conn
	Reader     *bufio.Reader
	Writer     *bufio.Writer
	WriteMu    sync.Mutex
	Token      []byte
	State      State
	Hello      ServerMsg
	ClientDone chan struct{} // closed when this handle is invalidated
	closeOnce  sync.Once
}

// ManagerConfig holds configuration for the shim manager.
type ManagerConfig struct {
	StateDir        string
	CLIPath         string
	IdleTimeout     time.Duration
	WatchdogTimeout time.Duration
	BufferSize      int
	MaxBufBytes     int64
	MaxShims        int
}

// NewManager creates a shim manager.
// Returns an error if the running binary path cannot be resolved: the path is
// required for Reconnect's identity check (comparing /proc/<shimPID>/exe), and
// an empty value would cause all reconnects to be rejected as "binary
// mismatch", silently disabling zero-downtime restart.
func NewManager(cfg ManagerConfig) (*Manager, error) {
	if cfg.StateDir == "" {
		home, _ := os.UserHomeDir()
		cfg.StateDir = filepath.Join(home, ".naozhi", "shims")
	}
	if cfg.MaxShims <= 0 {
		cfg.MaxShims = 50
	}
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 10000
	}
	if cfg.MaxBufBytes <= 0 {
		cfg.MaxBufBytes = 50 * 1024 * 1024
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 4 * time.Hour
	}
	if cfg.WatchdogTimeout <= 0 {
		cfg.WatchdogTimeout = 30 * time.Minute
	}

	// Find our own binary path for spawning shim subprocesses and for the
	// reconnect identity check. A missing value would silently break
	// Reconnect — fail fast so operators see the problem at startup.
	naozhiBin, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve naozhi binary path: %w", err)
	}

	if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
		slog.Warn("failed to create shim state directory", "dir", cfg.StateDir, "err", err)
	}

	return &Manager{
		stateDir:        cfg.StateDir,
		cliPath:         cfg.CLIPath,
		idleTimeout:     cfg.IdleTimeout,
		watchdogTimeout: cfg.WatchdogTimeout,
		bufferSize:      cfg.BufferSize,
		maxBufBytes:     cfg.MaxBufBytes,
		maxShims:        cfg.MaxShims,
		naozhiBin:       naozhiBin,
		shimEnv:         filterShimEnv(os.Environ()),
		shims:           make(map[string]*ShimHandle),
	}, nil
}

// StartShim spawns a new shim process using the manager's default CLI path.
// Kept as a wrapper around StartShimWithBackend for callers that don't need
// multi-backend routing.
func (m *Manager) StartShim(ctx context.Context, key string, cliArgs []string, cwd string) (*ShimHandle, error) {
	return m.StartShimWithBackend(ctx, key, m.cliPath, "", cliArgs, cwd)
}

// StartShimWithBackend spawns a new shim process with an explicit CLI binary
// and backend identifier. The backend is recorded in the shim state file so
// naozhi reconnects post-restart can route back to the matching wrapper.
// Pass cliPath == "" to fall back to the manager's default, and backend ==
// "" when the caller is a legacy single-backend user.
func (m *Manager) StartShimWithBackend(ctx context.Context, key, cliPath, backend string, cliArgs []string, cwd string) (*ShimHandle, error) {
	if cliPath == "" {
		cliPath = m.cliPath
	}
	// Reserve a slot atomically to prevent TOCTOU race with concurrent callers
	m.mu.Lock()
	if len(m.shims)+m.pendingShims >= m.maxShims {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w (%d)", ErrMaxShims, m.maxShims)
	}
	m.pendingShims++
	m.mu.Unlock()

	// Release the reserved slot on any failure path
	slotReleased := false
	defer func() {
		if !slotReleased {
			m.mu.Lock()
			m.pendingShims--
			m.mu.Unlock()
		}
	}()

	keyHash := KeyHash(key)
	socketPath := SocketPath(keyHash)
	stateFile := StateFilePath(m.stateDir, keyHash)

	// Build shim subprocess args
	args := []string{"shim", "run",
		"--key", key,
		"--socket", socketPath,
		"--state-file", stateFile,
		"--buffer-size", fmt.Sprintf("%d", m.bufferSize),
		"--max-buffer-bytes", fmt.Sprintf("%d", m.maxBufBytes),
		"--idle-timeout", m.idleTimeout.String(),
		"--watchdog-timeout", m.watchdogTimeout.String(),
		"--cli-path", cliPath,
		"--cwd", cwd,
	}
	if backend != "" {
		args = append(args, "--backend", backend)
	}
	for _, a := range cliArgs {
		args = append(args, "--cli-arg", a)
	}

	// Use exec.Command (not CommandContext): shim must outlive naozhi.
	// Context is only used for the startup handshake timeout below.
	cmd := exec.Command(m.naozhiBin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = m.shimEnv

	// Remove stale socket from a previous shim that didn't clean up
	// (e.g., killed during post-CLI-exit wait period). Before we rm, verify
	// nothing is actively listening: a live listener means discover/reconcile
	// missed this shim (racing concurrent paths, or state file got lost).
	// Destroying a live socket turns the peer shim into a zombie whose
	// listener fd has no filesystem entry, unreachable until it dies — this
	// is exactly the regression that caused UCCLEP's "session cannot be
	// reopened" bug in 2026-04-25. Fail loud instead of corrupting state.
	if err := ensureSocketFreeForReuse(socketPath); err != nil {
		return nil, err
	}

	// Capture stdout for the ready message
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("shim stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start shim: %w", err)
	}
	// Reap the shim process asynchronously to prevent zombie accumulation.
	// The shim is designed to outlive naozhi (Setsid: true), but when it exits
	// on its own (idle timeout, CLI exit), cmd.Wait() collects its status.
	//
	// R187-RELY-L1: log non-nil Wait errors so an OOM-killed / exec-permission
	// shim doesn't silently vanish. Normal termination (idle-timeout exit 0)
	// returns nil and stays quiet; any other exit surfaces in journald with
	// the keyHash so operators can correlate with the next dial failure.
	go func() {
		if err := cmd.Wait(); err != nil {
			slog.Warn("shim exited unexpectedly", "key_hash", keyHash, "err", err)
		}
	}()

	// Read ready message (with timeout)
	readyCh := make(chan struct {
		token string
		err   error
	}, 1)
	go func() {
		defer stdout.Close()
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			var ready struct {
				Status string `json:"status"`
				PID    int    `json:"pid"`
				Token  string `json:"token"`
				Error  string `json:"error"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &ready); err != nil {
				readyCh <- struct {
					token string
					err   error
				}{"", fmt.Errorf("parse ready: %w", err)}
				return
			}
			if ready.Status == "error" {
				readyCh <- struct {
					token string
					err   error
				}{"", fmt.Errorf("shim startup failed: %s", ready.Error)}
				return
			}
			if ready.Status != "ready" {
				readyCh <- struct {
					token string
					err   error
				}{"", fmt.Errorf("unexpected status: %s", ready.Status)}
				return
			}
			readyCh <- struct {
				token string
				err   error
			}{ready.Token, nil}
		} else {
			readyCh <- struct {
				token string
				err   error
			}{"", fmt.Errorf("shim exited before ready")}
		}
	}()

	// Use NewTimer + defer Stop so the goroutine backing time.After does not
	// park for 30s after a fast-path success or ctx cancellation. Under high
	// start/restart pressure this previously accumulated up to thousands of
	// live timer goroutines between GC cycles.
	readyTimer := time.NewTimer(30 * time.Second)
	defer readyTimer.Stop()

	// killAndUnblock terminates the shim and closes the caller-side stdout
	// pipe so the scanner goroutine spawned above is not left parked on a
	// Read that won't return until the OS tears down the shim's stdout fd.
	// Closing stdout here raises an error in the goroutine's Scan() and lets
	// it deliver to the buffered readyCh + run its own defer stdout.Close()
	// (double Close returns ErrClosed, which is harmless). Without this
	// helper, a shim that ignores SIGTERM keeps the goroutine alive for up
	// to its 4 h idle-timeout — under high-frequency restart pressure this
	// previously accumulated dozens to hundreds of leaked goroutines.
	// R40-CONCUR1 / R42-REL-SHIM-PGKILL.
	killAndUnblock := func() {
		_ = stdout.Close()
		_ = cmd.Process.Kill()
	}

	var tokenB64 string
	select {
	case result := <-readyCh:
		if result.err != nil {
			killAndUnblock()
			return nil, result.err
		}
		tokenB64 = result.token
	case <-readyTimer.C:
		killAndUnblock()
		return nil, fmt.Errorf("shim ready timeout")
	case <-ctx.Done():
		killAndUnblock()
		return nil, ctx.Err()
	}

	tokenRaw, err := base64.StdEncoding.DecodeString(tokenB64)
	if err != nil {
		// Kill the shim and close stdout alongside: the scanner goroutine
		// already received the successful ready frame and is parked on its
		// defer-only path, so this is just about reaping the process — no
		// unblock needed — but keeping the shared helper keeps the 4
		// failure branches symmetric. R40-CONCUR1.
		killAndUnblock()
		return nil, fmt.Errorf("decode shim token: %w", err)
	}

	// Connect to shim socket
	handle, err := m.connect(socketPath, tokenRaw, 0)
	if err != nil {
		killAndUnblock()
		return nil, fmt.Errorf("connect to new shim: %w", err)
	}

	// Move shim (and CLI) to an independent systemd scope so they survive
	// service restarts. Must happen after connect so we have the CLI PID from hello.
	// Thread the caller's ctx so SIGTERM during a spawn storm cancels the
	// busctl subprocess instead of letting dozens run in parallel for their
	// full 3 s budget past shutdown.
	moveToShimsCgroup(ctx, cmd.Process.Pid, handle.Hello.CLIPID)

	m.mu.Lock()
	// Guard against a concurrent StartShim/Reconnect having already installed
	// a handle for this key — overwriting without closing leaks the previous
	// Unix-domain socket fd and bufio buffers. Close the old handle outside
	// the lock to avoid holding it across network I/O.
	oldHandle := m.shims[key]
	m.shims[key] = handle
	m.pendingShims-- // slot fulfilled: transfer from pending to active
	slotReleased = true
	m.mu.Unlock()
	if oldHandle != nil {
		oldHandle.Close()
	}

	// OBS2: count every successful fresh shim birth. Reconnect (which reattaches
	// to an existing shim socket) is NOT counted — this metric answers "how many
	// shim processes forked" rather than "how many shim handshakes happened".
	metrics.ShimRestartTotal.Add(1)
	return handle, nil
}

// Reconnect connects to an existing shim identified by its state file.
// lastSeq is the last received sequence number for replay positioning.
//
// Unlike StartShim this path deliberately does not participate in the
// pendingShims admission counter: Reconnect is driven exclusively by
// Discover at router startup and by ReconnectShimsCtx during reconcile,
// both of which already loop sequentially over shims they found on disk.
// The admission gate protects concurrent StartShim spawners from exceeding
// maxShims; a batch that reattaches to already-running processes cannot
// create new shims, so gating it would only manufacture spurious failures
// on a cold start with more than maxShims persisted state files. The
// startup ordering is owned by the caller (single goroutine), not the
// manager, and changing that would require re-auditing the Router's
// reconnectShims lock order. R40-REL1.
//
// RACE CONTRACT (R49-REL-SHIM-MANAGER-RECONNECT-CONCUR): when two
// callers race Reconnect on the same key (net.DialTimeout happens
// outside m.mu, so both can build their own handle before the winning
// branch takes m.mu), the late winner's `m.shims[key] = handle`
// overwrites the early winner's entry. The late branch also closes the
// prior handle to prevent an fd leak — BUT that handle may already
// have been delivered to the caller (Router's reconnectShims attaches
// it to a Process). Closing a handle under active use causes the
// Process's readLoop to observe EOF and mark the session Dead.
//
// Today the scenario is not observed in production because:
//   - Router's reconcile ticker runs at 30 s intervals and each per-key
//     Reconnect finishes well within that window.
//   - StartShim and Reconnect cannot race on the same key either, because
//     Router only calls Reconnect for suspended-but-shim-alive sessions.
//
// If you add a second driver that calls Reconnect (e.g. a UI-triggered
// "reattach now" action) or shorten the reconcile interval, you MUST
// introduce per-key serialisation here (e.g. a singleflight keyed on
// `key`, or a per-key mutex pool) — otherwise the above invariant is
// one race edge away from breaking and the user sees spurious session
// deaths on reconcile. The no-leak semantics of the `oldHandle.Close()`
// step below are contract-tested in manager_reconnect_contract_test.go.
func (m *Manager) Reconnect(ctx context.Context, key string, lastSeq int64) (*ShimHandle, error) {
	keyHash := KeyHash(key)
	stateFile := StateFilePath(m.stateDir, keyHash)

	state, err := ReadStateFile(stateFile)
	if err != nil {
		return nil, fmt.Errorf("read state file: %w", err)
	}

	// Validate shim is alive
	if err := syscall.Kill(state.ShimPID, 0); err != nil {
		RemoveStateFile(stateFile)
		return nil, fmt.Errorf("shim PID %d not alive: %w", state.ShimPID, err)
	}

	// Validate shim binary identity via /proc/pid/exe (Linux only).
	// After a rebuild, the old binary shows "(deleted)" suffix — strip it.
	if exePath, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", state.ShimPID)); err == nil {
		cleanPath := strings.TrimSuffix(exePath, " (deleted)")
		if cleanPath != m.naozhiBin {
			syscall.Kill(state.ShimPID, syscall.SIGUSR2) //nolint:errcheck
			RemoveStateFile(stateFile)
			return nil, fmt.Errorf("shim PID %d binary mismatch: got %s, want %s", state.ShimPID, exePath, m.naozhiBin)
		}
	} else {
		slog.Warn("binary identity check skipped", "pid", state.ShimPID, "err", err)
	}

	// Validate socket path matches expected path exactly (prevents path injection)
	expectedSocket := SocketPath(keyHash)
	if state.Socket != expectedSocket {
		return nil, fmt.Errorf("socket path mismatch: got %s, expected %s", state.Socket, expectedSocket)
	}

	tokenRaw, err := base64.StdEncoding.DecodeString(state.AuthToken)
	if err != nil {
		return nil, fmt.Errorf("decode token: %w", err)
	}

	handle, err := m.connect(state.Socket, tokenRaw, lastSeq)
	if err != nil {
		return nil, err
	}
	handle.State = state

	m.mu.Lock()
	// Same invariant as StartShim: do not silently leak a previously stored
	// handle if Reconnect races with itself or with StartShim for the same key.
	oldHandle := m.shims[key]
	m.shims[key] = handle
	m.mu.Unlock()
	if oldHandle != nil {
		oldHandle.Close()
	}

	return handle, nil
}

// connect establishes an authenticated connection to a shim socket.
func (m *Manager) connect(socketPath string, token []byte, lastSeq int64) (*ShimHandle, error) {
	conn, err := net.DialTimeout("unix", socketPath, 10*time.Second)
	if err != nil {
		// Include the socket path so operators can check permissions /
		// existence directly from the log line instead of reverse-engineering
		// it from the shim-state key.
		return nil, fmt.Errorf("dial shim at %s: %w", socketPath, err)
	}

	reader := bufio.NewReaderSize(conn, 256*1024) // 256KB buffer (bufio grows as needed for large lines)
	writer := bufio.NewWriter(conn)

	// Send attach with token
	attach := ClientMsg{
		Type:  "attach",
		Token: base64.StdEncoding.EncodeToString(token),
		Seq:   lastSeq,
	}
	data, _ := json.Marshal(attach)
	// If SetWriteDeadline fails (conn closed by peer between Dial and here),
	// bail early with the real cause rather than letting the bufio Flush block
	// on a deadline-less write until TCP keepalive eventually surfaces.
	if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set attach write deadline: %w", err)
	}
	writer.Write(data)         //nolint:errcheck
	writer.Write([]byte{'\n'}) //nolint:errcheck
	if err := writer.Flush(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("write attach: %w", err)
	}
	_ = conn.SetWriteDeadline(time.Time{})

	// Read hello or auth_failed. The hello envelope is a few hundred bytes
	// of JSON; a 64 KB ceiling here prevents a malicious or buggy shim from
	// forcing us to buffer unbounded bytes before we've even authenticated.
	// Read byte-by-byte through the existing bufio so subsequent reads
	// continue to use the same buffered state — we cannot use bufio.ReadBytes
	// because it has no hard upper bound and would grow the buffer beyond
	// our 64 KB policy before we could check.
	conn.SetReadDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck
	const maxHelloBytes = 64 * 1024
	// Pre-allocated cap keeps the inner loop O(n) rather than O(n²). A 1 KB
	// initial cap fits the realistic hello payload and only grows by powers
	// of two until the 64 KB ceiling — a handful of grows in the worst case.
	helloLine := make([]byte, 0, 1024)
	for len(helloLine) < maxHelloBytes {
		b, err := reader.ReadByte()
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("read hello: %w", err)
		}
		helloLine = append(helloLine, b)
		if b == '\n' {
			break
		}
	}
	if len(helloLine) == 0 || helloLine[len(helloLine)-1] != '\n' {
		conn.Close()
		return nil, fmt.Errorf("hello exceeds %d-byte cap without newline", maxHelloBytes)
	}
	conn.SetReadDeadline(time.Time{}) //nolint:errcheck

	var hello ServerMsg
	if err := json.Unmarshal(helloLine, &hello); err != nil {
		conn.Close()
		return nil, fmt.Errorf("parse hello: %w", err)
	}
	if hello.Type == "auth_failed" {
		conn.Close()
		return nil, fmt.Errorf("shim auth failed: %s", hello.Msg)
	}
	if hello.Type != "hello" {
		conn.Close()
		return nil, fmt.Errorf("unexpected message type: %s", hello.Type)
	}

	return &ShimHandle{
		Conn:       conn,
		Reader:     reader,
		Writer:     writer,
		Token:      token,
		Hello:      hello,
		ClientDone: make(chan struct{}),
	}, nil
}

// ForceCleanupZombie purges a shim whose reconnect is irrecoverable: removes
// its state file and best-effort-signals SIGTERM to the process. Used by the
// router when it gets repeated ENOENT on the socket path — the next Discover
// tick would handle it via the F4 socket-stat check, but waiting 30s while
// reconnect spams WARN logs (and, worse, while the owning dashboard tab
// retries) is a poor UX. Caller passes the stale state it obtained from a
// failed Reconnect so we identify the exact target; PID 0 or empty key are
// treated as no-ops.
//
// Re-validates the PID's binary identity before signalling. Without this
// guard we are susceptible to PID reuse: between Reconnect's identity
// check and this call, the original shim could have exited and a non-shim
// process inherited the same PID. The same check runs in Discover, so
// duplicating it here keeps the SIGTERM target honest. A miss (binary
// mismatch) skips the kill but still cleans the state file.
func (m *Manager) ForceCleanupZombie(state State) {
	// Remove the state file BEFORE sending SIGTERM so a concurrent
	// reconnectShims tick cannot observe the still-present file, see the
	// PID alive (signal hasn't landed yet), and install a half-initialized
	// ShimHandle against a dying shim. The in-memory map is also purged
	// below; Discover reads from the filesystem, not the map. R65-GO-L-1.
	keyHash := KeyHash(state.Key)
	RemoveStateFile(StateFilePath(m.stateDir, keyHash))
	m.mu.Lock()
	delete(m.shims, state.Key)
	m.mu.Unlock()
	if state.ShimPID > 0 && m.isOurShimPID(state.ShimPID) {
		_ = syscall.Kill(state.ShimPID, syscall.SIGTERM)
	}
}

// isOurShimPID returns true when the process at pid is still running AND its
// /proc/PID/exe points at the naozhi binary we launched from (modulo the
// "(deleted)" suffix Linux adds after a rebuild). Mirrors the Discover-time
// binary-identity check so anyone considering signalling a PID learned
// from a state file runs the same safety gate.
func (m *Manager) isOurShimPID(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	exePath, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		// On platforms without /proc (not currently supported, but keep
		// the call safe) we cannot confirm identity. Err on the side of
		// NOT signalling unknown PIDs — the state-file cleanup alone is
		// enough to exit the ENOENT loop.
		return false
	}
	return strings.TrimSuffix(exePath, " (deleted)") == m.naozhiBin
}

// Discover scans the state directory for existing shim state files.
// Returns states for shims whose PIDs are still alive.
func (m *Manager) Discover() ([]State, error) {
	entries, err := os.ReadDir(m.stateDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var states []State
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// Clean up leftover temp files from a crashed WriteStateFile. The
		// `.shim-state-*.tmp` naming comes from os.CreateTemp, so these never
		// carry usable state — a successful write would have renamed them
		// into place. Leaving them lying around accumulates across restarts.
		if strings.HasPrefix(e.Name(), ".shim-state-") && strings.HasSuffix(e.Name(), ".tmp") {
			_ = os.Remove(filepath.Join(m.stateDir, e.Name()))
			continue
		}
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(m.stateDir, e.Name())
		state, err := ReadStateFile(path)
		if err != nil {
			slog.Warn("removing corrupt state file", "path", path, "err", err)
			RemoveStateFile(path)
			continue
		}
		// Check if shim is alive
		if err := syscall.Kill(state.ShimPID, 0); err != nil {
			slog.Info("removing stale shim state file", "path", path, "pid", state.ShimPID, "err", err)
			RemoveStateFile(path)
			continue
		}
		// Validate binary identity to detect PID reuse.
		// After a rebuild, Linux marks the old binary as "(deleted)" in /proc/pid/exe
		// (e.g. "/path/to/naozhi (deleted)"). Strip the suffix so that upgraded shims
		// are still recognized as ours.
		if exePath, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", state.ShimPID)); err == nil {
			cleanPath := strings.TrimSuffix(exePath, " (deleted)")
			if cleanPath != m.naozhiBin {
				slog.Info("removing stale shim state file (binary mismatch)", "path", path, "pid", state.ShimPID, "exe", exePath)
				RemoveStateFile(path)
				continue
			}
		}
		// PID alive + binary matches, but is the socket still reachable?
		// "Live shim + missing socket" is the zombie signature: the process
		// holds a listener fd that kernel never lost, but its filesystem
		// path is gone (external rm, /run cleaner, XDG_RUNTIME_DIR rotation,
		// or a pre-fix StartShim that clobbered it). Any naozhi Reconnect
		// would ENOENT forever, so skip it and let the shim self-terminate
		// via SIGTERM grace. RemoveStateFile here also purges the stale
		// on-disk record so restart discovery doesn't re-find the same
		// zombie.
		if _, err := os.Stat(state.Socket); err != nil {
			slog.Info("removing shim state: socket missing",
				"path", path, "pid", state.ShimPID,
				"socket", state.Socket, "err", err)
			// Re-check the PID before signalling. When the shim exits on
			// its own during graceful shutdown, it unlinks the socket itself
			// — the os.Stat above succeeds at detecting the missing socket,
			// but the process is already gone. Sending SIGTERM to a dead PID
			// either silently no-ops (race-lost) or terminates an unrelated
			// PID reusing the number. Probing with Kill(pid, 0) first removes
			// the noisy "caught SIGTERM during shutdown" log line from the
			// shim's crash path and the small but real wrong-PID risk.
			// R65-GO-L-2.
			if err := syscall.Kill(state.ShimPID, 0); err == nil {
				_ = syscall.Kill(state.ShimPID, syscall.SIGTERM)
			}
			RemoveStateFile(path)
			continue
		}
		slog.Info("discovered live shim", "key", state.Key, "pid", state.ShimPID)
		states = append(states, state)
	}
	return states, nil
}

// SendMsg sends a ClientMsg over the handle's connection.
func (h *ShimHandle) SendMsg(msg ClientMsg) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	h.WriteMu.Lock()
	defer h.WriteMu.Unlock()
	h.Writer.Write(data)     //nolint:errcheck
	h.Writer.WriteByte('\n') //nolint:errcheck
	return h.Writer.Flush()
}

// maxServerLineBytes caps the size of a single server→client line so a
// runaway or malicious shim cannot exhaust naozhi's heap. bufio.ReadBytes
// would otherwise grow its internal buffer without bound; we enforce a
// hard cap aligned with the server-side limit (`maxClientLineBytes`).
const maxServerLineBytes = 16 * 1024 * 1024

// ReadMsg reads the next ServerMsg from the handle's connection.
func (h *ShimHandle) ReadMsg() (ServerMsg, error) {
	// bufio.Reader.ReadBytes grows unbounded; a malicious/buggy shim that
	// never emits '\n' could drive OOM. Track running length after each
	// buffered read and bail once we exceed maxServerLineBytes.
	var buf []byte
	for {
		chunk, err := h.Reader.ReadSlice('\n')
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			// R188-ERR-H1: use errors.Is to match cli/process.go convention;
			// a future bufio wrapper that wraps ErrBufferFull would otherwise
			// be treated as terminal and close the connection on every
			// oversized message instead of continuing to accumulate chunks.
			// Any partial chunk on a terminal error is abandoned; we cannot
			// parse a half line and the bufio reader is about to be closed.
			return ServerMsg{}, err
		}
		if len(buf)+len(chunk) > maxServerLineBytes {
			return ServerMsg{}, fmt.Errorf("server msg exceeds %d bytes", maxServerLineBytes)
		}
		buf = append(buf, chunk...)
		if err == nil {
			break // terminator found
		}
		// ErrBufferFull: keep reading until newline or cap
	}
	var msg ServerMsg
	if err := json.Unmarshal(buf, &msg); err != nil {
		return ServerMsg{}, fmt.Errorf("parse server msg: %w", err)
	}
	return msg, nil
}

// drainReplayTimeout caps the total time we wait for a shim to finish replaying
// buffered messages. A wedged shim must not block ReconnectShims (which is
// serial across all persisted sessions) — without this cap, one unresponsive
// shim could stall the entire naozhi startup.
const drainReplayTimeout = 20 * time.Second

// DrainReplay reads and returns all replay messages until replay_done.
// Must be called immediately after connect, before starting the live read loop.
// Applies a total deadline to the conn so a wedged shim cannot block forever;
// the deadline is cleared before returning on success.
func (h *ShimHandle) DrainReplay() ([]ServerMsg, error) {
	_ = h.Conn.SetReadDeadline(time.Now().Add(drainReplayTimeout))
	defer func() { _ = h.Conn.SetReadDeadline(time.Time{}) }()

	var replays []ServerMsg
	for {
		msg, err := h.ReadMsg()
		if err != nil {
			return replays, fmt.Errorf("drain replay: %w", err)
		}
		switch msg.Type {
		case "replay":
			replays = append(replays, msg)
		case "replay_done":
			return replays, nil
		case "cli_exited":
			// CLI already exited before we connected
			replays = append(replays, msg)
			return replays, nil
		default:
			slog.Debug("unexpected message during replay", "type", msg.Type)
		}
	}
}

// Close closes the shim connection and signals done.
func (h *ShimHandle) Close() {
	h.closeOnce.Do(func() { close(h.ClientDone) })
	h.Conn.Close()
}

// Detach sends a detach message and closes the connection.
func (h *ShimHandle) Detach() {
	h.SendMsg(ClientMsg{Type: "detach"}) //nolint:errcheck
	h.Close()
}

// Shutdown sends a shutdown message and closes the connection.
func (h *ShimHandle) Shutdown() {
	h.SendMsg(ClientMsg{Type: "shutdown"}) //nolint:errcheck
	h.Close()
}

// StopAll sends shutdown to all known shims concurrently.
func (m *Manager) StopAll(ctx context.Context) {
	m.mu.Lock()
	handles := make(map[string]*ShimHandle, len(m.shims))
	for k, v := range m.shims {
		handles[k] = v
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for key, h := range handles {
		wg.Add(1)
		go func(k string, h *ShimHandle) {
			defer wg.Done()
			slog.Info("shutting down shim", "key", k)
			h.Shutdown()
		}(key, h)
	}
	wg.Wait()
}

// DetachAll sends detach to all known shims concurrently (used during graceful shutdown).
func (m *Manager) DetachAll() {
	m.mu.Lock()
	handles := make(map[string]*ShimHandle, len(m.shims))
	for k, v := range m.shims {
		handles[k] = v
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, h := range handles {
		wg.Add(1)
		go func(h *ShimHandle) {
			defer wg.Done()
			h.Detach()
		}(h)
	}
	wg.Wait()
}

// cgroupProcsPath is the fixed fallback cgroup file naozhi writes to via
// `sudo tee` when busctl is unavailable. Exposed as a package-level const
// so the sudoers policy contract test can assert the exact string and
// deploy/naozhi-sudoers.example stays synced.
const cgroupProcsPath = "/sys/fs/cgroup/naozhi-shims/cgroup.procs"

// buildBusctlArgs constructs the argv tail passed to `sudo` for the
// StartTransientUnit D-Bus call that adopts shim/CLI PIDs into an
// independent systemd scope. Split out from moveToShimsCgroup so the
// exact argv shape can be pinned by a unit test — the
// deploy/naozhi-sudoers.example policy depends on these literals not
// drifting (see docs/ops/sudoers-hardening.md). The returned slice
// starts with the "-n" non-interactive flag and the "busctl" command
// name; moveToShimsCgroup prepends "sudo" via exec.CommandContext.
//
// scopeName must already be the final "naozhi-shim-<PID>.scope" form.
// pids is expected to be len 1 (shim only) or 2 (shim + cli). Other
// lengths are permitted but are not covered by the shipped sudoers
// policy — callers that change the expected range must update both
// this function's contract test and the Cmnd_Alias set in the policy.
func buildBusctlArgs(scopeName string, pids []int) []string {
	args := []string{"-n", "busctl", "call",
		"org.freedesktop.systemd1",
		"/org/freedesktop/systemd1",
		"org.freedesktop.systemd1.Manager",
		"StartTransientUnit",
		"ssa(sv)a(sa(sv))",
		scopeName, "fail", "2",
		"PIDs", "au", strconv.Itoa(len(pids)),
	}
	for _, p := range pids {
		args = append(args, strconv.Itoa(p))
	}
	args = append(args, "KillMode", "s", "none", "0")
	return args
}

// moveToShimsCgroup moves shim and CLI processes to an independent systemd
// scope so they survive service restarts. Uses busctl to call StartTransientUnit
// directly with KillMode=none, making the processes invisible to the
// naozhi.service lifecycle. Falls back to direct cgroup move if
// busctl is not available.
func moveToShimsCgroup(parentCtx context.Context, shimPID, cliPID int) {
	scopeName := fmt.Sprintf("naozhi-shim-%d.scope", shimPID)

	// Build PID list for the scope
	pids := []int{shimPID}
	if cliPID > 0 {
		pids = append(pids, cliPID)
	}

	// Use busctl to create a transient scope adopting the shim PIDs.
	// This registers them as an independent systemd unit.
	args := buildBusctlArgs(scopeName, pids)

	ctx, cancel := context.WithTimeout(parentCtx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sudo", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Truncate busctl's combined stdout+stderr so repeated failures
		// can't flood the journal with D-Bus diagnostic payloads.
		if len(out) > 512 {
			out = append(out[:512:512], []byte("...(truncated)")...)
		}
		slog.Warn("moveToShimsCgroup: systemd scope failed, trying direct cgroup — zero-downtime restart may not survive service restart",
			"pid", shimPID, "err", err, "output", string(out))
		moveToShimsCgroupDirect(parentCtx, shimPID)
		if cliPID > 0 {
			moveToShimsCgroupDirect(parentCtx, cliPID)
		}
		return
	}
	slog.Info("moved shim to independent systemd scope", "scope", scopeName, "pids", pids)
}

// moveToShimsCgroupDirect is the fallback: move a process to a root-level
// cgroup directly. Less reliable than systemd scope (systemd may still
// clean it up during restart).
func moveToShimsCgroupDirect(parentCtx context.Context, pid int) {
	// The procs path is pinned as a package-level const so the sudoers
	// policy contract test can diff it against the shipped
	// deploy/naozhi-sudoers.example literal; drifting one without the
	// other would silently start rejecting the fallback tee at runtime.
	ctx, cancel := context.WithTimeout(parentCtx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sudo", "-n", "tee", cgroupProcsPath)
	cmd.Stdin = strings.NewReader(strconv.Itoa(pid) + "\n")
	cmd.Stdout = nil // tee copies to stdout; inherit parent (journal) is fine
	if err := cmd.Run(); err != nil {
		slog.Warn("moveToShimsCgroupDirect: failed — shim may not survive service restart", "pid", pid, "err", err)
		return
	}
	slog.Info("moved shim to independent cgroup (direct)", "pid", pid)
}

// Remove removes a shim handle from the manager's tracking.
func (m *Manager) Remove(key string) {
	m.mu.Lock()
	delete(m.shims, key)
	m.mu.Unlock()
}

// CLIPath returns the configured CLI binary path.
func (m *Manager) CLIPath() string {
	return m.cliPath
}

// shimEnvAllowedPrefixes lists environment variable prefixes passed to shim/CLI
// subprocesses. Variables not matching any prefix are filtered out to reduce
// the risk of leaking unrelated secrets (database passwords, third-party tokens)
// to the Claude CLI process which has Bash tool access.
var shimEnvAllowedPrefixes = []string{
	// System essentials
	"HOME=", "USER=", "LOGNAME=", "PATH=", "SHELL=",
	"TERM=", "TMPDIR=", "TMP=", "TEMP=",
	"LANG=", "LC_", "TZ=",
	"XDG_",

	// Claude CLI / Anthropic
	"ANTHROPIC_", "CLAUDE_",

	// AWS (Bedrock auth)
	"AWS_",

	// Git (SSH, config)
	"SSH_AUTH_SOCK=", "GIT_",

	// Common dev toolchains the CLI's Bash tool may invoke
	"GOPATH=", "GOROOT=", "GOBIN=",
	"CARGO_HOME=", "RUSTUP_HOME=",
	"NVM_DIR=", "NODE_", "NPM_",
	"PYTHON", "VIRTUAL_ENV=", "CONDA_",
	"JAVA_HOME=",
}

// filterShimEnv returns a copy of environ keeping only variables whose key
// matches one of the allowed prefixes. This is defense-in-depth: the CLI
// with --skip-permissions can still run `env` via Bash, but at least secrets
// not needed by the CLI are not exposed by default.
func filterShimEnv(environ []string) []string {
	filtered := make([]string, 0, len(environ)/2)
	for _, kv := range environ {
		for _, prefix := range shimEnvAllowedPrefixes {
			if strings.HasPrefix(kv, prefix) {
				filtered = append(filtered, kv)
				break
			}
		}
	}
	return filtered
}
