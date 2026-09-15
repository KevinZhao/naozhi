package shim

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/envpolicy"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/spawndiag"
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
		shimEnv:            baselineShimEnv(),
		shims:              make(map[string]*ShimHandle),
		reconnectKM:        make(map[string]*sync.Mutex),
	}, nil
}

// baselineShimEnv is the process env as the shim column allows it, reporting
// what the gate refused. Entries an operator exported expecting effect (an
// unsafe AWS_PROFILE, an http endpoint, an oversized value) reach metrics and
// `naozhi config check` this way instead of a bare log line.
func baselineShimEnv() []string {
	env := os.Environ()
	emitEnvDrops(shimEnvScope, envpolicy.ShimEnvDrops(env))
	return envpolicy.FilterShimEnv(env)
}

// shimEnvScope groups the process-baseline env drops for dedup. Per-spawn
// overlay drops use the session key instead.
const shimEnvScope = "shim-env"

// emitEnvDrops reports env-filter rejections as spawn diags.
func emitEnvDrops(scope string, drops []envpolicy.Drop) {
	if len(drops) == 0 {
		return
	}
	diags := make([]spawndiag.Diag, 0, len(drops))
	for _, d := range drops {
		diags = append(diags, spawndiag.Diag{
			Layer: "env-filter", Key: d.Key, Action: "dropped", Reason: d.Reason,
		})
	}
	spawndiag.Emit(scope, diags)
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
// only and is re-gated through envpolicy.FilterShimEnv by MergeShimEnv — never a bypass.
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
	// Per-spawn overlay re-gated by envpolicy.FilterShimEnv (see MergeShimEnv).
	// An overlay entry the gate refuses is an access profile that did not take
	// effect, so it is reported under this session's scope alongside the argv
	// gate's diags rather than dropped in silence.
	cmd.Env = envpolicy.MergeShimEnv(m.shimEnv, envOverlay)
	emitEnvDrops(key, envpolicy.MergeShimEnvDrops(m.shimEnv, envOverlay))

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

// moveToShimsCgroup (manager_linux.go / manager_darwin.go) moves shim and CLI
// to a lifecycle boundary that survives a naozhi restart: Linux registers a
// transient systemd scope with KillMode=none (direct cgroup write fallback);
// Darwin is a no-op because launchd only kills the plist's main process and a
// Setsid child is reparented to PID 1.

// CLIPath returns the configured CLI binary path.
func (m *Manager) CLIPath() string {
	return m.cliPath
}
