package shim

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/envpolicy"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// shimReadyMsg carries the result of the shim's ready-line scan back to
// StartShimWithBackend via the readyCh channel.
type shimReadyMsg struct {
	token string
	err   error
}

// validateKeyForShim rejects keys that would leak control bytes into the
// shim argv / socket path. Mirrors session.ValidateSessionKey (session →
// shim is a one-way import) via the shared internal/sessionkey rune deny-set
// (#2301); the byte cap below is a local copy of session.MaxSessionKeyBytes,
// and TestValidateKeyForShim_Contract pins both against one table (#719).
func validateKeyForShim(k string) error {
	if k == "" {
		return errors.New("empty key")
	}
	// Mirrors session.MaxSessionKeyBytes (4*128+3).
	const maxKeyBytes = 515
	if len(k) > maxKeyBytes {
		return fmt.Errorf("key exceeds %d-byte limit", maxKeyBytes)
	}
	if !utf8.ValidString(k) {
		return errors.New("key invalid utf-8")
	}
	for _, r := range k {
		switch {
		case sessionkey.IsControlKeyRune(r):
			return errors.New("key contains control character")
		case sessionkey.IsInvisibleKeyRune(r):
			return errors.New("key contains invisible control character")
		}
	}
	return nil
}

// ErrMaxShims is returned by StartShim when the shim cap is hit. Distinct from
// session.ErrMaxProcs: transient (clears as sessions exit), not a config error.
var ErrMaxShims = errors.New("max shims reached")

// ErrStateDirQuotaExceeded is returned by StartShim when spawning another shim
// would exceed the state-dir quota. Operator-actionable (clean ~/.naozhi/shims
// or raise the quota), unlike the transient ErrMaxShims (#456).
var ErrStateDirQuotaExceeded = errors.New("shim state dir quota exceeded")

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
	// shimEnv is the filtered env handed to every spawned shim, snapshotted
	// once at construction: variables injected later (systemctl
	// set-environment, os.Setenv) do not reach new shims until naozhi restarts.
	shimEnv []string

	// stateDirQuotaBytes mirrors ManagerConfig.StateDirQuotaBytes; 0 disables the gate.
	stateDirQuotaBytes int64

	mu           sync.Mutex
	shims        map[string]*ShimHandle // key → active shim handle
	pendingShims int                    // spawn in progress, not yet in shims map

	// reconnectMu guards reconnectKM, the per-key mutexes that serialize
	// Reconnect so two callers cannot each dial, swap, and close the other's
	// in-use handle. Lock order: reconnectKM[key] -> m.mu; NEVER take a
	// reconnectKM entry while holding m.mu (the dial takes up to 10 s).
	reconnectMu sync.Mutex
	reconnectKM map[string]*sync.Mutex

	// reaperWG tracks the per-shim cmd.Wait() reaper goroutines; StopAll waits
	// on it (bounded by ctx) so shutdown does not return while reapers may
	// still touch captured locals (#565).
	reaperWG sync.WaitGroup
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

// ManagerConfig holds configuration for the shim manager. All fields are
// optional; NewManager applies defaults when zero/empty.
type ManagerConfig struct {
	// StateDir holds the per-shim state JSON files (<keyhash>.json). Defaults
	// to ~/.naozhi/shims. Created 0700 because the files embed AuthToken.
	StateDir string
	// CLIPath is the default CLI binary used by StartShim; multi-backend
	// callers pass an explicit cliPath to StartShimWithBackend instead.
	CLIPath string
	// IdleTimeout is how long a shim sits with no client attached before
	// exiting (RAM vs cold-spawn penalty). Defaults to 4h.
	IdleTimeout time.Duration
	// WatchdogTimeout is the per-CLI-turn deadline enforced by the shim's
	// watchdog; a turn exceeding it is force-killed. Defaults to 30m.
	WatchdogTimeout time.Duration
	// BufferSize is the line capacity of the shim's stdout ring buffer.
	// Defaults to defaultRingMaxLines. The ring serves replay on reconnect.
	BufferSize int
	// MaxBufBytes is the byte capacity of the shim's stdout ring buffer.
	// Defaults to defaultRingMaxBytes. Whichever cap (lines or bytes)
	// trips first drives eviction.
	MaxBufBytes int64
	// MaxShims caps concurrent live shim processes. Defaults to 50.
	// StartShim returns ErrMaxShims when at the cap; Reconnect bypasses
	// this gate (it only attaches to already-running processes).
	MaxShims int

	// StateDirQuotaBytes caps the on-disk size of StateDir before StartShim
	// refuses to spawn. Zero (default) disables the gate. Reconnect bypasses
	// it — the quota brakes new growth, not already-running shims (#456).
	StateDirQuotaBytes int64
}

// NewManager creates a shim manager. Fails if the running binary path cannot
// be resolved: Reconnect's identity check compares /proc/<shimPID>/exe against
// it, and an empty value would reject every reconnect as "binary mismatch".
func NewManager(cfg ManagerConfig) (*Manager, error) {
	if cfg.StateDir == "" {
		home, _ := os.UserHomeDir()
		cfg.StateDir = filepath.Join(home, ".naozhi", "shims")
	}
	if cfg.MaxShims <= 0 {
		cfg.MaxShims = 50
	}
	// Use the ring-buffer constants directly so the manager default and the
	// NewRingBuffer fallback cannot drift.
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = defaultRingMaxLines
	}
	if cfg.MaxBufBytes <= 0 {
		cfg.MaxBufBytes = defaultRingMaxBytes
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 4 * time.Hour
	}
	if cfg.WatchdogTimeout <= 0 {
		cfg.WatchdogTimeout = 30 * time.Minute
	}

	// Own binary path: used to spawn shims and for the reconnect identity check.
	naozhiBin, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve naozhi binary path: %w", err)
	}

	if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
		slog.Warn("failed to create shim state directory", "dir", cfg.StateDir, "err", err)
	}

	return &Manager{
		stateDir:           cfg.StateDir,
		cliPath:            cfg.CLIPath,
		idleTimeout:        cfg.IdleTimeout,
		watchdogTimeout:    cfg.WatchdogTimeout,
		bufferSize:         cfg.BufferSize,
		maxBufBytes:        cfg.MaxBufBytes,
		maxShims:           cfg.MaxShims,
		stateDirQuotaBytes: cfg.StateDirQuotaBytes,
		naozhiBin:          naozhiBin,
		shimEnv:            filterShimEnv(os.Environ()),
		shims:              make(map[string]*ShimHandle),
		reconnectKM:        make(map[string]*sync.Mutex),
	}, nil
}

// checkStateDirQuota returns ErrStateDirQuotaExceeded when StateDirSize(stateDir)
// already exceeds the quota; 0 disables the gate. Scan errors fail open (first
// run, transient I/O). A truncated scan returns a lower bound — if even that
// exceeds the quota it is still a violation, so the size is used regardless.
func (m *Manager) checkStateDirQuota() error {
	if m.stateDirQuotaBytes <= 0 {
		return nil
	}
	size, err := osutil.StateDirSize(m.stateDir)
	if err != nil && !errors.Is(err, osutil.ErrStateDirScanTruncated) {
		// Missing (first run) or unreadable dir: never block spawn on a diagnostic walk.
		return nil
	}
	if size >= m.stateDirQuotaBytes {
		return fmt.Errorf("%w: %d ≥ %d bytes in %s",
			ErrStateDirQuotaExceeded, size, m.stateDirQuotaBytes, m.stateDir)
	}
	return nil
}

// reconnectKey returns (lazily creating) the per-key mutex Reconnect holds
// across read-state + dial + swap. Entries are reclaimed by Remove(key) (#2251).
func (m *Manager) reconnectKey(key string) *sync.Mutex {
	m.reconnectMu.Lock()
	defer m.reconnectMu.Unlock()
	mu, ok := m.reconnectKM[key]
	if !ok {
		mu = &sync.Mutex{}
		m.reconnectKM[key] = mu
	}
	return mu
}

// StartShim spawns a new shim process using the manager's default CLI path;
// a wrapper around StartShimWithBackend for single-backend callers.
func (m *Manager) StartShim(ctx context.Context, key string, cliArgs []string, cwd string) (*ShimHandle, error) {
	return m.StartShimWithBackend(ctx, key, m.cliPath, "", cliArgs, cwd, nil, nil)
}

// buildShimArgs assembles the argv for the shim subprocess. Pure function of
// its inputs so the argv-shape test can call it without spawning (#717).
//
// spawnOverlay rides along as one JSON-encoded `--spawn-overlay` token after
// the --cli-arg run (empty omits it): a new overlay field is a struct change,
// not a wire change, and MAX_ARG_STRLEN bounds it like any --cli-arg (#2494).
func (m *Manager) buildShimArgs(key, socketPath, stateFile, cliPath, backend, cwd string, cliArgs []string, spawnOverlay string) []string {
	args := []string{"shim", "run",
		"--key", key,
		"--socket", socketPath,
		"--state-file", stateFile,
		"--buffer-size", strconv.Itoa(m.bufferSize),
		"--max-buffer-bytes", strconv.FormatInt(m.maxBufBytes, 10),
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
	if spawnOverlay != "" {
		args = append(args, "--spawn-overlay", spawnOverlay)
	}
	return args
}

// awaitReady reads exactly one JSON ready frame from the shim's stdout pipe and
// returns the base64 auth token, or an error if the frame is malformed, the
// shim reported a startup failure, the timeout elapsed, or ctx was cancelled.
//
// It never Kills or Closes the parent's resources; the caller's killAndUnblock
// owns cleanup for every failure step. The scanner goroutine owns Close on
// stdout via defer — if killAndUnblock closes stdout first to unblock Scan,
// the deferred double Close is harmless (ErrClosed).
func awaitReady(ctx context.Context, stdout io.ReadCloser, timeout time.Duration) (string, error) {
	readyCh := make(chan shimReadyMsg, 1)
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
				readyCh <- shimReadyMsg{"", fmt.Errorf("parse ready: %w", err)}
				return
			}
			if ready.Status == "error" {
				readyCh <- shimReadyMsg{"", fmt.Errorf("shim startup failed: %s", osutil.SanitizeForLog(ready.Error, 256))}
				return
			}
			if ready.Status != "ready" {
				readyCh <- shimReadyMsg{"", fmt.Errorf("unexpected status: %s", ready.Status)}
				return
			}
			readyCh <- shimReadyMsg{ready.Token, nil}
		} else {
			readyCh <- shimReadyMsg{"", fmt.Errorf("shim exited before ready")}
		}
	}()

	// NewTimer + deferred Stop: time.After would park a timer goroutine for the
	// full timeout after a fast success or ctx cancel.
	readyTimer := time.NewTimer(timeout)
	defer readyTimer.Stop()

	select {
	case result := <-readyCh:
		if result.err != nil {
			return "", result.err
		}
		return result.token, nil
	case <-readyTimer.C:
		return "", fmt.Errorf("shim ready timeout")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// StartShimWithBackend spawns a new shim process with an explicit CLI binary
// and backend identifier; the backend is recorded in the shim state file so
// post-restart reconnects route back to the matching wrapper. cliPath == ""
// falls back to the manager default; backend == "" means legacy single-backend.
//
// envOverlay (RFC project-access-profile §4) overrides m.shimEnv for this shim
// only and is re-gated through filterShimEnv by mergeShimEnv — never a bypass.
// spawnOverlay (#2494) is recorded verbatim in the state file so the parent can
// re-merge it against current config on the next restart; nil records nothing.
func (m *Manager) StartShimWithBackend(ctx context.Context, key, cliPath, backend string, cliArgs []string, cwd string, envOverlay map[string]string, spawnOverlay *SpawnOverlay) (*ShimHandle, error) {
	// Defence-in-depth: key flows into exec argv as `--key <key>`; do not
	// trust that upstream callers already ran session.ValidateSessionKey.
	if err := validateKeyForShim(key); err != nil {
		return nil, fmt.Errorf("shim key rejected: %w", err)
	}
	if cliPath == "" {
		cliPath = m.cliPath
	}
	// Quota gate runs BEFORE the slot reservation so a quota failure never
	// contends for a pendingShims slot it would only release (#456).
	if err := m.checkStateDirQuota(); err != nil {
		return nil, err
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

	overlayJSON, err := EncodeSpawnOverlay(spawnOverlay)
	if err != nil {
		// json.Marshal on a plain struct: a failure is a programming error, so
		// fail the spawn rather than start a shim whose state reads as legacy.
		return nil, err
	}
	args := m.buildShimArgs(key, socketPath, stateFile, cliPath, backend, cwd, cliArgs, overlayJSON)

	// Use exec.Command (not CommandContext): shim must outlive naozhi.
	// Context is only used for the startup handshake timeout below.
	cmd := exec.Command(m.naozhiBin, args...)
	setSetsid(cmd)
	// Per-spawn overlay re-gated by filterShimEnv (see mergeShimEnv).
	cmd.Env = mergeShimEnv(m.shimEnv, envOverlay)

	// Remove a stale socket left by a previous shim — but only after verifying
	// nothing is listening: unlinking a live socket turns the peer shim into an
	// unreachable zombie (listener fd with no filesystem entry). Fail loud.
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
	// Reap asynchronously: the shim outlives naozhi (Setsid) but cmd.Wait()
	// must still collect its status when it exits on its own. reaperHandle is
	// published under m.mu in the same critical section as the map insert; the
	// reaper uses it for an identity-checked delete (removeShimIfCurrent) so
	// m.shims stops growing by one entry per distinct key. nil = spawn failed.
	var reaperHandle atomic.Pointer[ShimHandle]

	// Tracked via m.reaperWG so StopAll can bound shutdown. The closure must
	// capture only function locals (keyHash, key, reaperHandle) plus the
	// allowlisted m.reaperWG / m.removeShimIfCurrent — pinned by
	// TestReaperGoroutine_OnlyCapturesLocalKeyHash (#565).
	m.reaperWG.Add(1)
	go func() {
		defer m.reaperWG.Done()
		err := cmd.Wait()
		// Identity-checked: a concurrent StartShim/Reconnect that already
		// replaced this key's handle must keep its live entry. nil = the spawn
		// never reached the map insert.
		if h := reaperHandle.Load(); h != nil {
			m.removeShimIfCurrent(key, h)
		}
		if err != nil {
			slog.Warn("shim exited unexpectedly", "key_hash", keyHash, "err", err)
		}
	}()

	// killAndUnblock kills the shim AND closes our end of stdout so the scanner
	// goroutine inside awaitReady is not parked on Read until the shim's fd is
	// torn down (a shim ignoring SIGTERM would hold it for the 4 h idle-timeout).
	killAndUnblock := func() {
		_ = stdout.Close()
		_ = cmd.Process.Kill()
	}

	tokenB64, err := awaitReady(ctx, stdout, 30*time.Second)
	if err != nil {
		killAndUnblock()
		return nil, err
	}

	tokenRaw, err := base64.StdEncoding.DecodeString(tokenB64)
	if err != nil {
		// The scanner already delivered the ready frame; this is just reaping,
		// but the shared helper keeps the failure branches symmetric.
		killAndUnblock()
		return nil, fmt.Errorf("decode shim token: %w", err)
	}

	handle, err := m.connect(socketPath, tokenRaw, 0)
	if err != nil {
		killAndUnblock()
		return nil, fmt.Errorf("connect to new shim: %w", err)
	}

	// Move shim + CLI to an independent systemd scope so they survive service
	// restarts. Must run after connect (CLI PID comes from hello). ctx lets
	// SIGTERM during a spawn storm cancel the busctl subprocess. cliPath lets
	// the linux helper verify /proc/<cliPID>/exe before adopting the CLI into
	// the privileged cgroup — PPid alone does not prove it is the CLI (#546).
	moveToShimsCgroup(ctx, cmd.Process.Pid, handle.Hello.CLIPID, cliPath)

	m.mu.Lock()
	// A concurrent StartShim/Reconnect may already have installed a handle for
	// this key; close it (outside the lock — Close does network I/O) rather
	// than leak its socket fd and buffers.
	oldHandle := m.shims[key]
	m.shims[key] = handle
	m.pendingShims-- // slot fulfilled: transfer from pending to active
	slotReleased = true
	// Publish to the reaper INSIDE the lock, in the same critical section as
	// the map insert: a fast-dying shim's reaper Loads the instant m.mu is
	// released, and a Store after Unlock would let it observe nil, skip the
	// delete, and leak the entry forever ("max shims reached"). Pinned by
	// TestReaperHandleStore_HappensUnderLock.
	reaperHandle.Store(handle)
	m.mu.Unlock()
	if oldHandle != nil {
		oldHandle.Close()
	}

	// Counts fresh shim births only; Reconnect reattaches to an existing
	// process and is not counted.
	metrics.ShimRestartTotal.Add(1)
	return handle, nil
}

// Reconnect connects to an existing shim identified by its state file; lastSeq
// is the last received sequence number for replay positioning.
//
// Not gated by pendingShims/maxShims: callers reattach sequentially to shims
// already on disk, so a gate would only fail a cold start with >maxShims files.
//
// reconnectKM[key] is held across read-state + dial + swap so two callers on
// the same key never build parallel handles and close one Router already uses.
// The old handle is captured and swapped under m.mu and closed outside m.mu
// (Close does network I/O). Lock order: reconnectKM[key] -> m.mu, never reversed.
func (m *Manager) Reconnect(ctx context.Context, key string, lastSeq int64) (*ShimHandle, error) {
	// Per-key serialise across read-state + dial + swap; cross-key stays parallel.
	rmu := m.reconnectKey(key)
	rmu.Lock()
	defer rmu.Unlock()

	keyHash := KeyHash(key)
	stateFile := StateFilePath(m.stateDir, keyHash)

	state, err := ReadStateFile(stateFile)
	if err != nil {
		return nil, fmt.Errorf("read state file: %w", err)
	}

	if !pidAlive(state.ShimPID) {
		RemoveStateFile(stateFile)
		return nil, fmt.Errorf("shim PID %d not alive", state.ShimPID)
	}

	// Binary identity: Linux reads /proc/PID/exe (strips "(deleted)" after a
	// rebuild); Darwin falls back to ps -o comm= — weaker, but still catches
	// PID reuse by an unrelated process.
	if mismatch, err := shimPIDBinaryMismatch(state.ShimPID, m.naozhiBin); err == nil && mismatch {
		sendSIGUSR2(state.ShimPID) //nolint:errcheck
		RemoveStateFile(stateFile)
		return nil, fmt.Errorf("shim PID %d binary mismatch", state.ShimPID)
	} else if err != nil {
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
	// Same invariant as StartShim: close a raced-in prior handle, never leak it.
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
		// Include the socket path so operators can check it straight from the log.
		return nil, fmt.Errorf("dial shim at %s: %w", socketPath, err)
	}

	reader := bufio.NewReaderSize(conn, 256*1024) // 256KB buffer (bufio grows as needed for large lines)
	writer := bufio.NewWriter(conn)

	attach := ClientMsg{
		Type:  "attach",
		Token: base64.StdEncoding.EncodeToString(token),
		Seq:   lastSeq,
	}
	data, _ := json.Marshal(attach)
	// If SetWriteDeadline fails (peer closed between Dial and here) bail with the
	// real cause rather than letting Flush block without a deadline.
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

	// Read hello byte-by-byte through the same bufio (so later reads share its
	// state) with a 64 KB hard cap: bufio.ReadBytes has no upper bound and a
	// malicious shim could force unbounded buffering before authentication.
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set hello read deadline: %w", err)
	}
	const maxHelloBytes = 64 * 1024
	// 1 KB initial cap fits a realistic hello and keeps the loop O(n).
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
		return nil, fmt.Errorf("shim auth failed: %s", osutil.SanitizeForLog(hello.Msg, 128))
	}
	if hello.Type != "hello" {
		conn.Close()
		return nil, fmt.Errorf("unexpected message type: %s", osutil.SanitizeForLog(hello.Type, 64))
	}
	// Reject hellos outside [MinSupportedProtocolVersion, ProtocolVersion] so
	// deploy skew fails at attach instead of as a JSON parse error mid-session.
	// Pre-versioning shims send ProtocolVersion=0; treat 0 as v1 (#427).
	helloVer := hello.ProtocolVersion
	if helloVer == 0 {
		helloVer = 1
	}
	if helloVer < MinSupportedProtocolVersion || helloVer > ProtocolVersion {
		conn.Close()
		return nil, fmt.Errorf("shim protocol_version %d outside supported [%d,%d]; check naozhi/shim binary skew",
			helloVer, MinSupportedProtocolVersion, ProtocolVersion)
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
// its state file and best-effort SIGTERMs the process. Used by the router on
// repeated socket ENOENT instead of waiting up to 30s for the next Discover
// tick. PID 0 or empty key are no-ops.
//
// The PID's binary identity is re-validated before signalling (PID reuse
// between Reconnect's check and this call); a mismatch skips the kill but
// still removes the state file.
func (m *Manager) ForceCleanupZombie(state State) {
	// Remove the state file BEFORE SIGTERM so a concurrent reconnectShims tick
	// cannot see the file + a still-alive PID and attach to a dying shim.
	// Discover reads the filesystem, not the map.
	keyHash := KeyHash(state.Key)
	RemoveStateFile(StateFilePath(m.stateDir, keyHash))
	m.mu.Lock()
	delete(m.shims, state.Key)
	m.mu.Unlock()
	if state.ShimPID > 0 && m.isOurShimPID(state.ShimPID) {
		_ = sendSIGTERM(state.ShimPID)
	}
}

// isOurShimPID reports whether pid is alive AND its binary matches the naozhi
// binary we launched from (same gate as Discover). Run it before signalling
// any PID learned from a state file.
func (m *Manager) isOurShimPID(pid int) bool {
	if !pidAlive(pid) {
		return false
	}
	mismatch, err := shimPIDBinaryMismatch(pid, m.naozhiBin)
	if err != nil {
		// Cannot confirm identity: do not signal unknown PIDs.
		return false
	}
	return !mismatch
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
		// Leftover os.CreateTemp files from a crashed WriteStateFile never carry
		// usable state; a successful write would have renamed them into place.
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
		if !pidAlive(state.ShimPID) {
			slog.Info("removing stale shim state file", "path", path, "pid", state.ShimPID)
			RemoveStateFile(path)
			continue
		}
		// Binary identity catches PID reuse; the linux helper strips "(deleted)"
		// so shims from the previous build are still recognised as ours.
		if mismatch, ierr := shimPIDBinaryMismatch(state.ShimPID, m.naozhiBin); ierr == nil && mismatch {
			slog.Info("removing stale shim state file (binary mismatch)", "path", path, "pid", state.ShimPID)
			RemoveStateFile(path)
			continue
		}
		// Live PID + missing socket is the zombie signature: the listener fd
		// exists but its path is gone (external rm, /run cleaner, XDG_RUNTIME_DIR
		// rotation), so Reconnect would ENOENT forever. Skip it, let it
		// self-terminate via SIGTERM, and purge the on-disk record.
		if _, err := os.Stat(state.Socket); err != nil {
			slog.Info("removing shim state: socket missing",
				"path", path, "pid", state.ShimPID,
				"socket", state.Socket, "err", err)
			// Re-check the PID: a shim exiting gracefully unlinks its own socket,
			// and SIGTERM to a dead PID could hit an unrelated process reusing it.
			if pidAlive(state.ShimPID) {
				_ = sendSIGTERM(state.ShimPID)
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
//
// Close() does not take WriteMu, so guard on ClientDone before and after
// acquiring it: SendMsg after Close is then a deterministic net.ErrClosed
// instead of a Flush onto a closed fd (#1969).
func (h *ShimHandle) SendMsg(msg ClientMsg) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	select {
	case <-h.ClientDone:
		return net.ErrClosed
	default:
	}
	h.WriteMu.Lock()
	defer h.WriteMu.Unlock()
	// Re-check under the lock: ClientDone is closed before Conn.Close(), so
	// seeing it here means the fd may already be gone.
	select {
	case <-h.ClientDone:
		return net.ErrClosed
	default:
	}
	h.Writer.Write(data)     //nolint:errcheck
	h.Writer.WriteByte('\n') //nolint:errcheck
	return h.Writer.Flush()
}

// maxServerLineBytes caps a single server→client line so a runaway shim cannot
// exhaust naozhi's heap; aligned with the server-side maxClientLineBytes.
const maxServerLineBytes = 16 * 1024 * 1024

// ReadMsg reads the next ServerMsg from the handle's connection.
func (h *ShimHandle) ReadMsg() (ServerMsg, error) {
	// Accumulate ReadSlice chunks and bail past maxServerLineBytes;
	// bufio.ReadBytes would grow unbounded on a line that never ends.
	var buf []byte
	for {
		chunk, err := h.Reader.ReadSlice('\n')
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			// errors.Is: a wrapped ErrBufferFull must keep accumulating, not
			// close the connection. A partial chunk on a terminal error is abandoned.
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

// drainReplayTimeout caps the total wait for a shim's replay so one wedged
// shim cannot stall ReconnectShims (serial across all persisted sessions).
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

// StopAll sends shutdown to all known shims concurrently. ctx bounds how long
// the caller blocks for the drain; on expiry StopAll returns early and logs
// the in-flight count while the goroutines finish on their own (abandon the
// tail rather than block the systemd shutdown watchdog).
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

	done := make(chan struct{})
	go func() {
		wg.Wait()
		// Also drain the reaper goroutines so callers see a fully-drained
		// Manager; they are bounded by shim lifetime, and the outer ctx still
		// bounds wall-clock if one is stuck (#565).
		m.reaperWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		slog.Warn("shim.Manager.StopAll: ctx expired before drain",
			"err", ctx.Err(),
			"pending_shims", len(handles))
	}
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

// moveToShimsCgroup (manager_linux.go / manager_darwin.go) moves shim and CLI
// to a lifecycle boundary that survives a naozhi restart: Linux registers a
// transient systemd scope with KillMode=none (direct cgroup write fallback);
// Darwin is a no-op because launchd only kills the plist's main process and a
// Setsid child is reparented to PID 1.

// Remove removes a shim handle from the manager's tracking.
func (m *Manager) Remove(key string) {
	m.mu.Lock()
	delete(m.shims, key)
	m.mu.Unlock()

	// Also drop the per-key reconnect mutex so reconnectKM does not grow with
	// the lifetime count of keys (#2251). Take reconnectMu separately, NEVER
	// while holding m.mu (lock order). A racing Reconnect simply re-creates
	// the entry, which is correct.
	m.reconnectMu.Lock()
	delete(m.reconnectKM, key)
	m.reconnectMu.Unlock()
}

// removeShimIfCurrent deletes key from m.shims only when the stored handle is
// still `want`. It is the reaper's map-cleanup hook: when a spawned shim exits,
// its entry is dropped so the admission count (len(m.shims)+pendingShims vs
// maxShims) reflects live shims rather than lifetime distinct keys.
//
// The identity check is load-bearing: a concurrent StartShim/Reconnect for the
// same key may have swapped in a fresh live handle, and an unconditional delete
// from the old process's reaper would strand it (uncounted, unfindable). A
// superseded reaper is a no-op. reconnectKM is deliberately untouched — a live
// replacement still needs its per-key mutex; only Manager.Remove reclaims it.
func (m *Manager) removeShimIfCurrent(key string, want *ShimHandle) {
	m.mu.Lock()
	if m.shims[key] == want {
		delete(m.shims, key)
	}
	m.mu.Unlock()
}

// CLIPath returns the configured CLI binary path.
func (m *Manager) CLIPath() string {
	return m.cliPath
}

// shimEnvAllowedPrefixes lists env variable prefixes passed to shim/CLI
// subprocesses; everything else is dropped so unrelated secrets do not reach
// a CLI process with Bash tool access.
//
// CONTRACT — do NOT grow this list to make a settings.json value "take
// effect": the spawned claude reads ~/.claude/settings.json itself (see
// protocol_claude.go BuildArgs) and a settings.json `env` value WINS over the
// inherited process env, so forwarding functional knobs here is redundant and
// re-widens the leak surface. Only system/toolchain plumbing and raw Bedrock
// credentials that settings.json does NOT carry belong here.
var shimEnvAllowedPrefixes = []string{
	// System essentials
	"HOME=", "USER=", "LOGNAME=", "PATH=", "SHELL=",
	"TERM=", "TMPDIR=", "TMP=", "TEMP=",
	"LANG=", "LC_", "TZ=",
	"XDG_",

	// Claude CLI / Anthropic — explicit keys, not the "ANTHROPIC_"/"CLAUDE_"
	// prefixes: a future or namespace-sharing variable (e.g. a Bedrock
	// deployment's stale ANTHROPIC_API_KEY) would otherwise be readable via
	// the Bash tool. CLAUDE_CODE_OAUTH_TOKEN is a credential the CLI cannot
	// obtain headlessly, not a settings.json knob, so it belongs here.
	"ANTHROPIC_API_KEY=", "ANTHROPIC_AUTH_TOKEN=",
	"CLAUDE_CODE_OAUTH_TOKEN=",
	"ANTHROPIC_MODEL=", "ANTHROPIC_BASE_URL=",
	"ANTHROPIC_BEDROCK_BASE_URL=",
	"CLAUDE_CODE_USE_BEDROCK=", "CLAUDE_CODE_SKIP_BEDROCK_AUTH=",
	"CLAUDE_BIN=", "CLAUDE_MODEL=",

	// AWS (Bedrock auth) — explicit keys, not "AWS_": the wildcard would forward
	// AWS_MFA_TOKEN, admin profiles, high-privilege credential files, etc.
	"AWS_REGION=", "AWS_DEFAULT_REGION=",
	"AWS_ACCESS_KEY_ID=", "AWS_SECRET_ACCESS_KEY=", "AWS_SESSION_TOKEN=",
	"AWS_PROFILE=", "AWS_SHARED_CREDENTIALS_FILE=", "AWS_CONFIG_FILE=",
	"AWS_ROLE_ARN=", "AWS_WEB_IDENTITY_TOKEN_FILE=",
	"AWS_ENDPOINT_URL=", "AWS_BEDROCK_ENDPOINT=",

	// Git — explicit keys, not "GIT_": GIT_PROXY_COMMAND / GIT_SSH_COMMAND /
	// GIT_SSH / GIT_EXEC_PATH / GIT_EDITOR / GIT_PAGER / GIT_SEQUENCE_EDITOR /
	// GIT_EXTERNAL_DIFF make git execute attacker-controlled commands.
	// SSH_AUTH_SOCK is the standard ssh-agent identity channel, no injection.
	"SSH_AUTH_SOCK=",
	"GIT_AUTHOR_NAME=", "GIT_AUTHOR_EMAIL=",
	"GIT_COMMITTER_NAME=", "GIT_COMMITTER_EMAIL=",
	"GIT_CONFIG_GLOBAL=", "GIT_CONFIG_SYSTEM=",
	"GIT_DIR=", "GIT_WORK_TREE=",

	// Dev toolchains — explicit keys, not "NODE_"/"PYTHON"/"CONDA_"/NVM_DIR:
	// NODE_OPTIONS (--require), NODE_PATH, NODE_EXTRA_CA_CERTS /
	// NODE_TLS_REJECT_UNAUTHORIZED, PYTHONSTARTUP/INSPECT/PATH/HOME,
	// VIRTUAL_ENV, NVM_DIR and CONDA_PYTHON_EXE all let an attacker load code
	// or shadow interpreters in subprocesses the CLI spawns (CLI itself is Node).
	"GOPATH=", "GOROOT=", "GOBIN=",
	"CARGO_HOME=", "RUSTUP_HOME=",
	"NODE_ENV=",
	// NPM_CONFIG_* (PREFIX, GLOBALCONFIG, CACHE, TMP) redirect paths npm uses to
	// resolve packages / run lifecycle scripts (RCE-class); allow only the
	// registry URL and token.
	"NPM_CONFIG_REGISTRY=", "NPM_TOKEN=",
	"PYTHONDONTWRITEBYTECODE=", "PYTHONUNBUFFERED=",
	"CONDA_PREFIX=", "CONDA_DEFAULT_ENV=", "CONDA_SHLVL=",
	"JAVA_HOME=",
}

// maxShimEnvEntryBytes caps a single forwarded env entry. Legitimate allowlisted
// values are well under 4 KiB; a pathological one only inflates the child env
// and slog attrs, so reject and log instead.
const maxShimEnvEntryBytes = 4 * 1024

// maxShimEnvOversizeWarnings caps oversized-entry warnings per process
// lifetime. A counter (not sync.Once) so one benign oversized entry cannot
// mask a later attacker-injected one while log volume stays bounded.
const maxShimEnvOversizeWarnings = 5

// filterShimEnvOversizeWarnings counts emitted oversized-entry warnings;
// entries are always rejected, only the logging is capped.
var filterShimEnvOversizeWarnings atomic.Int64

// filterShimEnv returns a copy of environ keeping only variables whose key
// matches an allowed prefix (defense-in-depth against `env` via the Bash
// tool). Oversized entries are rejected and logged by key prefix only.
func filterShimEnv(environ []string) []string {
	filtered := make([]string, 0, len(environ)/2)
	for _, kv := range environ {
		if len(kv) > maxShimEnvEntryBytes {
			// Key prefix only — never the value (may be a secret). Logging is
			// capped at maxShimEnvOversizeWarnings; rejection is not.
			if n := filterShimEnvOversizeWarnings.Add(1); n <= maxShimEnvOversizeWarnings {
				msg := "shim env: oversized entry rejected"
				if n == maxShimEnvOversizeWarnings {
					msg = "shim env: oversized entry rejected (further oversized warnings suppressed)"
				}
				slog.Warn(msg,
					"key_prefix", kvKeyPrefix(kv),
					"len", len(kv),
					"max", maxShimEnvEntryBytes)
			}
			continue
		}
		for _, prefix := range shimEnvAllowedPrefixes {
			if strings.HasPrefix(kv, prefix) {
				// Endpoint vars steer where the CLI (Bash + raw network) sends API
				// traffic; a poisoned rc pointing one at an attacker host or IMDS over
				// plain http would silently redirect/harvest. https for non-loopback (#1576).
				if shimEndpointEnvDropped(kv) {
					break
				}
				// AWS_PROFILE / AWS_DEFAULT_PROFILE select a profile that may declare a
				// credential_process the SDK executes; restrict to ^[A-Za-z0-9_-]{1,64}$
				// (mirrors sysession/env.go isSafeProfileValue). Key logged, never value.
				if shimProfileEnvDropped(kv) {
					break
				}
				// AWS_*_FILE vars name files the SDK opens in the CLI subprocess; a
				// value like /proc/self/environ or ../ traversal would ship arbitrary
				// host files to STS. Require an absolute, traversal-free, null-free path.
				if shimCredPathEnvDropped(kv) {
					break
				}
				filtered = append(filtered, kv)
				break
			}
		}
	}
	return filtered
}

// mergeShimEnv layers a per-spawn env overlay (the materialised access profile,
// RFC project-access-profile §4: resolved "KEY=value" pairs) onto the process
// baseline and returns the effective env for one CLI subprocess.
//
// INVARIANT — the overlay is NOT a whitelist bypass: the merged slice is re-run
// through filterShimEnv, so every overlay entry faces the same allowlist +
// SSRF/profile/cred-path guards. It may only override the VALUE of an already
// allowlisted key, per spawn. nil/empty overlay returns baseline unchanged.
// Overlay wins on conflict; ordering is baseline order then sorted extras so
// the argv-shape tests stay stable.
func mergeShimEnv(baseline []string, overlay map[string]string) []string {
	if len(overlay) == 0 {
		return baseline
	}
	merged := make([]string, 0, len(baseline)+len(overlay))
	usedOverlay := make(map[string]bool, len(overlay))
	for _, kv := range baseline {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if v, ok := overlay[key]; ok {
			merged = append(merged, key+"="+v)
			usedOverlay[key] = true
			continue
		}
		merged = append(merged, kv)
	}
	// Append overlay keys that had no baseline counterpart, in sorted order for
	// determinism. These still face filterShimEnv below.
	extra := make([]string, 0, len(overlay))
	for k := range overlay {
		if !usedOverlay[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		merged = append(merged, k+"="+overlay[k])
	}
	// Re-gate: overlay values face the identical allowlist + guards. Zero bypass.
	return filterShimEnv(merged)
}

// shimProfileEnvKeys is the set of allowlisted env keys whose value is an AWS
// profile *name*; a malformed value can redirect the SDK's credential_process
// lookup to an attacker-controlled profile, so it is validated first.
var shimProfileEnvKeys = map[string]bool{
	"AWS_PROFILE":         true,
	"AWS_DEFAULT_PROFILE": true,
}

// shimProfileEnvDropped reports whether kv ("KEY=value") is an AWS profile-name
// var whose value falls outside ^[A-Za-z0-9_-]{1,64}$ and must be dropped.
// Non-profile keys return false. The value is never logged.
func shimProfileEnvDropped(kv string) bool {
	i := strings.IndexByte(kv, '=')
	if i < 0 {
		return false
	}
	key, val := kv[:i], kv[i+1:]
	if !shimProfileEnvKeys[key] {
		return false
	}
	if !envpolicy.IsSafeProfileValue(val) {
		slog.Warn("shim env: rejecting unsafe AWS profile value (credential_process injection guard)", "key", key)
		return true
	}
	return false
}

// shimCredPathEnvKeys is the set of allowlisted env keys whose value is a file
// path the AWS SDK opens inside the CLI subprocess; a malicious value (e.g.
// /proc/self/environ, ../ traversal) would make the SDK read an arbitrary host
// file as a credential / OIDC token, so it is validated first.
var shimCredPathEnvKeys = map[string]bool{
	"AWS_SHARED_CREDENTIALS_FILE": true,
	"AWS_CONFIG_FILE":             true,
	"AWS_WEB_IDENTITY_TOKEN_FILE": true,
}

// shimCredPathEnvDropped reports whether kv ("KEY=value") is an AWS
// credential-file path var whose value is not absolute, traversal-free and
// null-free, and must be dropped. Non-path keys and safe values return false.
// The value is never logged.
func shimCredPathEnvDropped(kv string) bool {
	i := strings.IndexByte(kv, '=')
	if i < 0 {
		return false
	}
	key, val := kv[:i], kv[i+1:]
	if !shimCredPathEnvKeys[key] {
		return false
	}
	if !isSafeShimCredPath(val) {
		slog.Warn("shim env: rejecting unsafe AWS credential file path (path traversal guard)", "key", key)
		return true
	}
	return false
}

// isSafeShimCredPath reports whether v is a safe absolute credential file path:
// non-empty, no embedded null byte, absolute, and no ".." segment (so
// /a/../../etc/shadow is rejected even though it begins with a slash).
func isSafeShimCredPath(v string) bool {
	if v == "" {
		return false
	}
	if strings.IndexByte(v, 0) >= 0 {
		return false
	}
	if !filepath.IsAbs(v) {
		return false
	}
	// Reject any path containing a ".." segment outright (even if it would
	// clean away) so a tampered value can never escape its intended root.
	for _, seg := range strings.Split(filepath.ToSlash(v), "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// shimEndpointEnvKeys is the set of allowlisted env keys whose value is an API
// endpoint URL forwarded to the CLI; shimEndpointEnvDropped applies an
// SSRF/redirect guard to each (#1576).
var shimEndpointEnvKeys = map[string]bool{
	"ANTHROPIC_BASE_URL":         true,
	"ANTHROPIC_BEDROCK_BASE_URL": true,
	"AWS_ENDPOINT_URL":           true,
	"AWS_BEDROCK_ENDPOINT":       true,
}

// shimEndpointEnvDropped reports whether kv ("KEY=value") is an endpoint URL
// var whose value targets a plain-http non-loopback host (or an internal IP)
// and must be dropped. Non-endpoint keys and safe URLs return false. The value
// is never logged (#1576).
func shimEndpointEnvDropped(kv string) bool {
	i := strings.IndexByte(kv, '=')
	if i < 0 {
		return false
	}
	key, val := kv[:i], kv[i+1:]
	if !shimEndpointEnvKeys[key] {
		return false
	}
	if val == "" {
		return false
	}
	if err := validateShimEndpointURL(val); err != nil {
		slog.Warn("shim env: rejecting unsafe endpoint base_url", "key", key, "err", err)
		return true
	}
	return false
}

// validateShimEndpointURL enforces https:// unless the host is loopback
// (localhost / 127.0.0.0/8 / ::1), where plain http is allowed for local mocks.
// Even https:// must not target a literal internal IP (loopback excepted):
// ANTHROPIC_BASE_URL=https://169.254.169.254/... would steer the CLI's client,
// API key in hand, at IMDS or an internal admin port (#1713). Only literal IPs
// are inspected — no DNS resolution here; hostname rebinding is out of scope.
//
// Shares envpolicy.ClassifyHost with envpolicy.ValidateBaseURLValue (#2300) so
// the range classification cannot drift, but is deliberately stricter: no
// NAOZHI_ALLOW_PRIVATE_BASE_URL escape hatch, and 0.0.0.0 / :: are rejected.
func validateShimEndpointURL(v string) error {
	u, err := url.Parse(v)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		host := u.Hostname()
		// Deny-set = every internal class except loopback (local https
		// mocks). The classes are disjoint (pinned in envpolicy tests), so
		// clearing the loopback bit never un-denies another range.
		if k, ok := envpolicy.ClassifyHost(host); ok && k&^envpolicy.IPLoopback != 0 {
			return fmt.Errorf("https:// to internal IP %q rejected (SSRF/IMDS guard)", host)
		}
		return nil
	case "http":
		host := u.Hostname()
		if strings.EqualFold(host, "localhost") {
			return nil
		}
		if k, ok := envpolicy.ClassifyHost(host); ok && k.Has(envpolicy.IPLoopback) {
			return nil
		}
		return fmt.Errorf("plain http:// to non-loopback host %q rejected (SSRF/redirect guard); use https://", host)
	}
	return fmt.Errorf("scheme %q not allowed; use https://", u.Scheme)
}

// kvKeyPrefix returns the key part (before '=') of a KEY=value env string,
// capped at 64 bytes to bound log line length even for pathologically long
// key names. Never returns the value.
func kvKeyPrefix(kv string) string {
	if i := strings.IndexByte(kv, '='); i >= 0 {
		k := kv[:i]
		if len(k) > 64 {
			k = k[:64]
		}
		return k
	}
	// Malformed (no '='): return a safe prefix.
	if len(kv) > 64 {
		return kv[:64]
	}
	return kv
}
