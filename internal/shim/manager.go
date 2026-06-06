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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/envpolicy"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/osutil"
)

// shimReadyMsg carries the result of the shim's ready-line scan back to
// StartShimWithBackend via the readyCh channel.
type shimReadyMsg struct {
	token string
	err   error
}

// validateKeyForShim rejects keys that would leak control bytes into the
// shim argv / socket path. Mirrors session.ValidateSessionKey; we keep a
// local copy here because session → shim is a one-way import and the
// shim package must remain a leaf. Keep this rule set in sync with
// session.ValidateSessionKey — the byte cap below matches
// session.MaxSessionKeyBytes (4*128+3=515), and the rune filter mirrors
// that function verbatim. If either side grows new rune classes, update
// both together.
//
// R237-CR-12 (#719): the "keep in sync" guarantee is no longer comment-only.
// TestValidateKeyForShim_Contract pins this validator's behaviour against
// the same table used by internal/session/router_test.go::TestValidateSessionKey.
// When session's table grows a row, mirror it here in
// internal/shim/manager_validate_key_test.go and the test will fail loudly
// if either side regresses.
func validateKeyForShim(k string) error {
	if k == "" {
		return errors.New("empty key")
	}
	// Matches session.MaxSessionKeyBytes; a divergence here would reject
	// keys that passed every upstream gate.
	const maxKeyBytes = 515
	if len(k) > maxKeyBytes {
		return fmt.Errorf("key exceeds %d-byte limit", maxKeyBytes)
	}
	if !utf8.ValidString(k) {
		return errors.New("key invalid utf-8")
	}
	for _, r := range k {
		if r == 0 || r < 0x20 || (r >= 0x7F && r <= 0x9F) {
			return errors.New("key contains control character")
		}
		switch {
		case r >= 0x200B && r <= 0x200F, // zero-width / LTR-RTL marks
			r >= 0x202A && r <= 0x202E, // bidi embedding / override
			r == 0x2028, r == 0x2029,   // line / paragraph separator
			r == 0xFEFF: // BOM
			return errors.New("key contains invisible control character")
		}
	}
	return nil
}

// ErrMaxShims is returned by StartShim when the configured shim cap is hit.
// Distinct from session.ErrMaxProcs so callers can apply different retry
// policies: max shims means process table is saturated, clears as sessions
// exit; not a configuration problem.
var ErrMaxShims = errors.New("max shims reached")

// ErrStateDirQuotaExceeded is returned by StartShim when the configured
// state-dir quota would be exceeded by spawning another shim. Distinct
// sentinel so callers can surface a more actionable error message
// (operator action: clean ~/.naozhi/shims, raise quota) versus
// ErrMaxShims (transient: another session must exit). RNEW-OPS-415
// (#456) minimal slice; matches the existing osutil scan error API.
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
	// shimEnv is the filtered environment handed to every spawned shim,
	// computed once at Manager construction. The process env does not change
	// at runtime, so recomputing filterShimEnv(os.Environ()) on every spawn
	// would redo the same O(env × prefixes) scan for no benefit.
	//
	// Operational implication: this is a start-time snapshot. Variables
	// injected later via systemctl set-environment or os.Setenv will NOT
	// propagate to newly-spawned shims until naozhi itself is restarted.
	shimEnv []string

	// stateDirQuotaBytes mirrors ManagerConfig.StateDirQuotaBytes; 0
	// disables the gate. RNEW-OPS-415 (#456).
	stateDirQuotaBytes int64

	mu           sync.Mutex
	shims        map[string]*ShimHandle // key → active shim handle
	pendingShims int                    // spawn in progress, not yet in shims map

	// reconnectMu serializes Reconnect calls per key so two concurrent
	// callers cannot each build their own handle, swap one in, and close
	// the other while it's still in use by Router (R51-CONCUR-005). The
	// outer m.mu must NEVER be held while taking a reconnectMu entry —
	// the dial inside connect() takes up to 10 s, and blocking m.mu for
	// that long would stall every other map mutation. See Reconnect for
	// the lock acquisition sequence.
	reconnectMu sync.Mutex
	reconnectKM map[string]*sync.Mutex

	// reaperWG tracks the per-shim cmd.Wait() reaper goroutines spawned
	// by StartShimWithBackend. StopAll waits on this group (bounded by
	// the caller-supplied ctx) so the systemd shutdown path does not
	// return while reaper goroutines may still be running and touching
	// captured locals (today only `keyHash`, but the WaitGroup contract
	// is the structural defense against future captures of Manager state
	// being read mid-shutdown). R216-GO-6 (#565).
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

// ManagerConfig holds configuration for the shim manager.
//
// All fields are optional; NewManager applies sensible defaults (see the
// constants/literals inside NewManager) when zero/empty so misconfigured
// callers still get a usable Manager rather than a startup error. Per-field
// godoc here documents what each knob actually controls — the historical
// "holds configuration" one-liner forced readers to grep for the field
// usage to discover semantics.
type ManagerConfig struct {
	// StateDir is the directory where per-shim state JSON files live
	// (one file per active shim, named <keyhash>.json). Defaults to
	// ~/.naozhi/shims when empty. Created with mode 0700 because the
	// state files embed AuthToken which grants direct socket attach.
	StateDir string
	// CLIPath is the default CLI binary path used by StartShim.
	// Multi-backend callers should prefer StartShimWithBackend with an
	// explicit cliPath instead of relying on this field.
	CLIPath string
	// IdleTimeout is the duration a shim sits with no client attached
	// before exiting on its own. Defaults to 4h. Set lower for memory-
	// pressure environments; the default trades off RAM against the
	// wall-clock penalty of cold-spawning a fresh CLI subprocess.
	IdleTimeout time.Duration
	// WatchdogTimeout is the per-CLI-turn deadline enforced by the
	// shim's internal watchdog. Defaults to 30m. A turn that exceeds
	// this is force-killed; tune this if your turns legitimately run
	// long (e.g. multi-hour code analyses).
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

	// StateDirQuotaBytes caps the on-disk size of StateDir before
	// StartShim refuses to spawn new shims. Zero (the default) disables
	// the quota gate so legacy callers see no behaviour change.
	//
	// RNEW-OPS-415 (#456) minimal slice: prevents per-shim state files
	// (each ≤4 KiB but 50 active shims × restart loops can multiply)
	// from filling ~/.naozhi when an operator forgets to set ulimit on
	// the data dir. Reconnect still bypasses the gate — quota's job is
	// to brake new growth, not strand already-running shims.
	StateDirQuotaBytes int64
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
	// R237-CR-13: reference the buffer-side constants directly so the
	// "manager default" and "ring builder default" cannot drift.
	// NewRingBuffer also falls back to these when handed maxLines<=0 /
	// maxBytes<=0, but apply them here too so cfg.BufferSize /
	// cfg.MaxBufBytes (read by other manager code) hold the same value.
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
// already exceeds the configured quota. Quota of 0 disables the gate.
// A scan error is treated as "fail open" (no quota enforced) so first-run
// systems with no state dir, or transient i/o issues, do not block spawn.
// The truncation sentinel from osutil returns a lower bound — if even the
// lower bound exceeds quota, that is itself a quota violation, so we use
// the returned size regardless.
//
// Pulled out as a method (not inline) so tests can dial the quota via
// ManagerConfig and exercise the gate without spawning a real shim.
// RNEW-OPS-415 (#456) minimal slice.
func (m *Manager) checkStateDirQuota() error {
	if m.stateDirQuotaBytes <= 0 {
		return nil
	}
	size, err := osutil.StateDirSize(m.stateDir)
	if err != nil && !errors.Is(err, osutil.ErrStateDirScanTruncated) {
		// Either the dir is missing (first run) or unreadable. Either
		// way, do not block shim spawn on a diagnostic walk.
		return nil
	}
	if size >= m.stateDirQuotaBytes {
		return fmt.Errorf("%w: %d ≥ %d bytes in %s",
			ErrStateDirQuotaExceeded, size, m.stateDirQuotaBytes, m.stateDir)
	}
	return nil
}

// reconnectKey returns (and lazily creates) the per-key mutex used by
// Reconnect to serialise concurrent attempts on the same key. The
// returned mutex is shared across all callers for that key and held
// across the dial — see Reconnect for the rationale. Per-key mutexes
// stay in the map for the Manager's lifetime; the entries are tiny
// (sync.Mutex pointer) and the key set is bounded by the lifetime
// session count, so cleanup is unnecessary.
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

// StartShim spawns a new shim process using the manager's default CLI path.
// Kept as a wrapper around StartShimWithBackend for callers that don't need
// multi-backend routing.
func (m *Manager) StartShim(ctx context.Context, key string, cliArgs []string, cwd string) (*ShimHandle, error) {
	return m.StartShimWithBackend(ctx, key, m.cliPath, "", cliArgs, cwd)
}

// buildShimArgs assembles the argv for the shim subprocess. Extracted from
// StartShimWithBackend (R237-CR-11 / #717) so the 200-line spawn function
// body reads as the lifecycle script its godoc describes (validate -> args
// -> exec -> ready -> token -> connect -> cgroup -> map swap) rather than
// 30 lines of strconv-flag-construction wedged between the slot reservation
// and ensureSocketFreeForReuse.
//
// Pure function of its inputs (no Manager mutation, no I/O), so the
// argv-shape regression test can call it directly without spawning a real
// shim.
func (m *Manager) buildShimArgs(key, socketPath, stateFile, cliPath, backend, cwd string, cliArgs []string) []string {
	args := []string{"shim", "run",
		"--key", key,
		"--socket", socketPath,
		"--state-file", stateFile,
		// R246-CR-007: integer→string via strconv avoids the fmt reflect
		// path in StartShimWithBackend (called once per shim spawn).
		// bufferSize is int; maxBufBytes is int64 (see fields above).
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
	return args
}

// awaitReady reads exactly one JSON ready frame from the shim's stdout pipe
// and returns the base64-encoded auth token, or an error if the frame is
// malformed, the shim reported a startup failure, the timeout elapsed, or ctx
// was cancelled.
//
// Extracted from StartShimWithBackend per R246-CR-005 / #740 (P0 subset) so
// the spawn function reads as a lifecycle script (validate → args → exec →
// awaitReady → decode → connect → cgroup → map swap) instead of inlining the
// scanner goroutine + 30s timer + 3-way select. The caller still owns
// lifecycle cleanup (killAndUnblock); awaitReady deliberately does not Kill
// or Close the parent's resources itself so the same outer helper handles
// cleanup regardless of which step (decode token, connect, cgroup move, …)
// fails.
//
// Concurrency: spawns one goroutine that runs a bufio.Scanner.Scan() and
// writes a single shimReadyMsg to a buffered (size-1) channel. The goroutine
// owns Close on the supplied stdout via defer; if the caller's killAndUnblock
// closes stdout to unblock the Scan after a timeout / ctx cancel, the
// goroutine's deferred Close is harmless (double Close returns ErrClosed).
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
				readyCh <- shimReadyMsg{"", fmt.Errorf("shim startup failed: %s", ready.Error)}
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

	// NewTimer + defer Stop so the runtime goroutine backing time.After does
	// not park for the full timeout after a fast-path success or ctx
	// cancellation. Under high start/restart pressure this previously
	// accumulated up to thousands of live timer goroutines between GC cycles.
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
// and backend identifier. The backend is recorded in the shim state file so
// naozhi reconnects post-restart can route back to the matching wrapper.
// Pass cliPath == "" to fall back to the manager's default, and backend ==
// "" when the caller is a legacy single-backend user.
func (m *Manager) StartShimWithBackend(ctx context.Context, key, cliPath, backend string, cliArgs []string, cwd string) (*ShimHandle, error) {
	// Defence-in-depth: the key flows into the shim argv as `--key <key>`.
	// Upstream callers (HTTP / WS / reverse-RPC) already run
	// session.ValidateSessionKey, but the shim manager must not trust
	// that unconditionally — any future call path that forgets the check
	// would silently let control bytes reach exec argv.
	if err := validateKeyForShim(key); err != nil {
		return nil, fmt.Errorf("shim key rejected: %w", err)
	}
	if cliPath == "" {
		cliPath = m.cliPath
	}
	// RNEW-OPS-415 (#456): refuse to spawn when StateDir is already over
	// the configured quota. Performed BEFORE the slot reservation so the
	// caller sees the quota error directly without contending for a
	// pendingShims slot that would only be released. Quota=0 disables
	// the gate (legacy default). The walk reuses StateDirSize's
	// budget/truncation handling — a truncated scan returns a lower
	// bound, which we treat as "fail open" to avoid jamming startup
	// when the dir is unscannable.
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

	args := m.buildShimArgs(key, socketPath, stateFile, cliPath, backend, cwd, cliArgs)

	// Use exec.Command (not CommandContext): shim must outlive naozhi.
	// Context is only used for the startup handshake timeout below.
	cmd := exec.Command(m.naozhiBin, args...)
	setSetsid(cmd)
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
	//
	// R216-GO-6 (#565): tracked via m.reaperWG so StopAll can bound the
	// shutdown path on these goroutines. Today the goroutine only reads
	// the function-local `keyHash` string — Add(1) here and Done() inside
	// the goroutine make the structural contract explicit so future edits
	// that capture Manager state are caught by the WaitGroup-on-shutdown
	// contract rather than by silent races.
	m.reaperWG.Add(1)
	go func() {
		defer m.reaperWG.Done()
		if err := cmd.Wait(); err != nil {
			slog.Warn("shim exited unexpectedly", "key_hash", keyHash, "err", err)
		}
	}()

	// killAndUnblock terminates the shim and closes the caller-side stdout
	// pipe so the scanner goroutine inside awaitReady is not left parked on
	// a Read that won't return until the OS tears down the shim's stdout
	// fd. Closing stdout here raises an error in the scanner's Scan() and
	// lets it deliver to the buffered readyCh + run its own defer
	// stdout.Close() (double Close returns ErrClosed, which is harmless).
	// Without this helper, a shim that ignores SIGTERM keeps the goroutine
	// alive for up to its 4 h idle-timeout — under high-frequency restart
	// pressure this previously accumulated dozens to hundreds of leaked
	// goroutines. R40-CONCUR1 / R42-REL-SHIM-PGKILL.
	killAndUnblock := func() {
		_ = stdout.Close()
		_ = cmd.Process.Kill()
	}

	// awaitReady owns the scanner goroutine + 30s timer + 3-way select so
	// the surrounding 7-step lifecycle script (R246-CR-005 / #740 P0
	// subset) reads at one consistent abstraction level. Cleanup on every
	// failure branch goes through killAndUnblock here, mirroring the
	// downstream decode / connect / cgroup-move failure paths.
	tokenB64, err := awaitReady(ctx, stdout, 30*time.Second)
	if err != nil {
		killAndUnblock()
		return nil, err
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
	//
	// R216-SEC-5 (#546): pass the configured CLI path so the linux helper
	// can verify /proc/<cliPID>/exe matches before adopting the CLI into the
	// privileged cgroup. PPid validation alone (R229-SEC-4) defends against
	// random PIDs but not against a child the shim genuinely spawned that
	// happens not to be the CLI binary; the exe check closes that gap.
	moveToShimsCgroup(ctx, cmd.Process.Pid, handle.Hello.CLIPID, cliPath)

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
// R51-CONCUR-005 fix: per-key mutex (reconnectKM) now serialises
// concurrent Reconnect attempts on the same key. The lock is held
// across the read-state + dial + swap sequence, so two callers see
// the second one wait until the first publishes its handle. The
// second caller then sees the freshly-installed handle in m.shims —
// it does NOT race a second dial, the prior dial's result is what
// gets reused. Outer m.mu is still acquired only for the map mutation
// itself, never across the dial (10 s timeout), so reconcile-time
// fan-out across DIFFERENT keys still proceeds in parallel.
//
// Lock ordering: reconnectKM[key] -> m.mu. NEVER hold m.mu when
// taking a reconnectKM entry (would defeat the cross-key parallelism
// motivation above).
//
// The no-leak semantics of the `oldHandle.Close()` step below are
// contract-tested in manager_reconnect_contract_test.go.
func (m *Manager) Reconnect(ctx context.Context, key string, lastSeq int64) (*ShimHandle, error) {
	// R51-CONCUR-005: per-key serialise. Held across read-state + dial
	// + swap so a second caller never observes a half-installed handle
	// or builds a parallel dial that could close the first caller's
	// in-flight handle. Cross-key Reconnect remains parallel.
	rmu := m.reconnectKey(key)
	rmu.Lock()
	defer rmu.Unlock()

	keyHash := KeyHash(key)
	stateFile := StateFilePath(m.stateDir, keyHash)

	state, err := ReadStateFile(stateFile)
	if err != nil {
		return nil, fmt.Errorf("read state file: %w", err)
	}

	// Validate shim is alive
	if !pidAlive(state.ShimPID) {
		RemoveStateFile(stateFile)
		return nil, fmt.Errorf("shim PID %d not alive", state.ShimPID)
	}

	// Validate shim binary identity. On Linux this reads /proc/PID/exe;
	// on Darwin it falls back to ps -o comm= (a weaker check — no path,
	// just program basename — but still detects PID reuse by an unrelated
	// process). After a rebuild, Linux marks the old binary as "(deleted)"
	// in /proc/PID/exe; the linux helper strips that suffix.
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
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set hello read deadline: %w", err)
	}
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
	// RNEW-ARCH-403 (#427): reject hellos whose ProtocolVersion is outside
	// the [MinSupportedProtocolVersion, ProtocolVersion] band. Older
	// builds of this binary silently kept whatever shape the shim
	// declared, so a forward-rolled shim shipping a wire-incompatible v2
	// frame would survive the attach handshake and only blow up later
	// when readLoop hit an unknown field shape — masking the deploy-skew
	// root cause behind a JSON parse error mid-session. Hello carries
	// ProtocolVersion=0 from pre-versioning shims; treat 0 as v1
	// (matches connect_test.go's existing fixture and the shim sender
	// in server.go which always populates the field today).
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
		_ = sendSIGTERM(state.ShimPID)
	}
}

// isOurShimPID returns true when the process at pid is still running AND
// its binary identity matches the naozhi binary we launched from. The
// underlying check is platform-specific: Linux reads /proc/PID/exe (modulo
// the "(deleted)" suffix added after a rebuild), Darwin falls back to
// ps -o comm=. Mirrors the Discover-time gate so anyone considering
// signalling a PID learned from a state file runs the same safety check.
func (m *Manager) isOurShimPID(pid int) bool {
	if !pidAlive(pid) {
		return false
	}
	mismatch, err := shimPIDBinaryMismatch(pid, m.naozhiBin)
	if err != nil {
		// Unable to confirm identity — err on the side of NOT signalling
		// unknown PIDs. The state-file cleanup alone is enough to exit
		// the ENOENT loop.
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
		if !pidAlive(state.ShimPID) {
			slog.Info("removing stale shim state file", "path", path, "pid", state.ShimPID)
			RemoveStateFile(path)
			continue
		}
		// Validate binary identity to detect PID reuse. Implementation is
		// platform-specific (see shimPIDBinaryMismatch) — Linux reads
		// /proc/PID/exe, Darwin falls back to ps -o comm=. After a rebuild
		// Linux marks the old binary as "(deleted)" in /proc/PID/exe; the
		// linux helper strips that suffix so upgraded shims are still
		// recognized as ours.
		if mismatch, ierr := shimPIDBinaryMismatch(state.ShimPID, m.naozhiBin); ierr == nil && mismatch {
			slog.Info("removing stale shim state file (binary mismatch)", "path", path, "pid", state.ShimPID)
			RemoveStateFile(path)
			continue
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
//
// ctx is honoured as an upper bound on how long the caller is willing
// to block waiting for the per-shim Shutdown goroutines to drain. On
// ctx expiry StopAll returns early and logs the count of in-flight
// shutdowns; the goroutines themselves continue running until their
// respective h.Shutdown() returns (Shutdown is a single SendMsg +
// Close on a TCP handle and is bounded by the OS socket buffer). This
// matches Manager.Stop semantics elsewhere — abandon-the-tail rather
// than block the systemd shutdown watchdog. Closes R237-GO-9.
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
		// Also wait for reaper goroutines (cmd.Wait() per spawned shim)
		// so callers blocking on StopAll see a fully-drained Manager.
		// The reapers themselves are bounded by the shim process lifetime
		// — if the shim survived our Shutdown signal (Setsid: true keeps
		// it under cgroup control), reaperWG would block until the cgroup
		// owner cleans up. The outer ctx still bounds the wall-clock here
		// so a stuck reaper cannot wedge service shutdown indefinitely.
		// R216-GO-6 (#565).
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

// moveToShimsCgroup moves shim and CLI processes to a dedicated lifecycle
// boundary so they survive a naozhi service restart. The implementation is
// platform-specific:
//   - Linux: uses busctl to register a transient systemd scope with
//     KillMode=none, falling back to direct cgroup write if busctl is
//     unavailable. See manager_linux.go.
//   - Darwin: no-op. launchd's default kill semantics only target the
//     plist's main process, so a child started with Setsid: true is
//     automatically reparented to launchd (PID 1) and survives restart
//     without any external lifecycle moves. See manager_darwin.go.
//
// The package-level wrapper here delegates to the platform helper so the
// StartShimWithBackend hot path stays platform-agnostic.

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

	// Claude CLI / Anthropic — explicit list of variables required by the
	// CLI. Avoid the wildcard "ANTHROPIC_"/"CLAUDE_" prefixes (R214-SEC-3 /
	// R219-SEC-3): a future Anthropic-issued variable, an internal company
	// variable that happens to share the namespace, or a Bedrock deployment's
	// stale ANTHROPIC_API_KEY would otherwise leak into the CLI subprocess
	// where the Bash tool can `env | grep ANTHROPIC` to read it. Mirrors the
	// AWS_ / GIT_ / NODE_ / PYTHON* explicit-list pattern below.
	//
	// Variables retained:
	//   - ANTHROPIC_API_KEY      — direct Anthropic auth (non-Bedrock)
	//   - ANTHROPIC_AUTH_TOKEN   — alternate auth for forks / proxies
	//   - ANTHROPIC_MODEL        — pin Claude model selection
	//   - ANTHROPIC_BASE_URL     — proxy / staging endpoint
	//   - ANTHROPIC_BEDROCK_BASE_URL — Bedrock proxy endpoint
	//   - CLAUDE_CODE_USE_BEDROCK    — Bedrock backend selector
	//   - CLAUDE_CODE_SKIP_BEDROCK_AUTH — Bedrock proxy skips local AWS auth
	//   - CLAUDE_BIN             — pin to a specific claude binary path
	//   - CLAUDE_MODEL           — alias for ANTHROPIC_MODEL on some forks
	"ANTHROPIC_API_KEY=", "ANTHROPIC_AUTH_TOKEN=",
	"ANTHROPIC_MODEL=", "ANTHROPIC_BASE_URL=",
	"ANTHROPIC_BEDROCK_BASE_URL=",
	"CLAUDE_CODE_USE_BEDROCK=", "CLAUDE_CODE_SKIP_BEDROCK_AUTH=",
	"CLAUDE_BIN=", "CLAUDE_MODEL=",

	// AWS (Bedrock auth) — explicit list of variables required by the AWS
	// SDK to authenticate Bedrock. Avoid the wildcard "AWS_" prefix because
	// it would forward unrelated AWS_* variables (e.g. AWS_MFA_TOKEN, custom
	// admin profiles, AWS_SHARED_CREDENTIALS_FILE pointing at high-privilege
	// files) into the CLI subprocess where the Bash tool can read them.
	"AWS_REGION=", "AWS_DEFAULT_REGION=",
	"AWS_ACCESS_KEY_ID=", "AWS_SECRET_ACCESS_KEY=", "AWS_SESSION_TOKEN=",
	"AWS_PROFILE=", "AWS_SHARED_CREDENTIALS_FILE=", "AWS_CONFIG_FILE=",
	"AWS_ROLE_ARN=", "AWS_WEB_IDENTITY_TOKEN_FILE=",
	"AWS_ENDPOINT_URL=", "AWS_BEDROCK_ENDPOINT=",

	// Git (SSH, config) — explicit list of variables required for git
	// identity / config lookups. Avoid the wildcard "GIT_" prefix because
	// several GIT_* variables instruct git to execute attacker-controlled
	// commands at network / SSH / commit time. Explicitly excluded:
	//   - GIT_PROXY_COMMAND (runs arbitrary command for every TCP fetch)
	//   - GIT_SSH_COMMAND   (replaces ssh binary git uses for fetch/push)
	//   - GIT_SSH           (older variant of GIT_SSH_COMMAND)
	//   - GIT_EXEC_PATH     (changes where git looks up its sub-binaries)
	//   - GIT_EDITOR / GIT_PAGER / GIT_SEQUENCE_EDITOR (run on commit/rebase)
	//   - GIT_EXTERNAL_DIFF (runs on diff)
	// SSH_AUTH_SOCK= stays — ssh-agent is the standard identity channel
	// for git over SSH, no command injection vector.
	"SSH_AUTH_SOCK=",
	"GIT_AUTHOR_NAME=", "GIT_AUTHOR_EMAIL=",
	"GIT_COMMITTER_NAME=", "GIT_COMMITTER_EMAIL=",
	"GIT_CONFIG_GLOBAL=", "GIT_CONFIG_SYSTEM=",
	"GIT_DIR=", "GIT_WORK_TREE=",

	// Common dev toolchains the CLI's Bash tool may invoke.
	//
	// SECURITY: NODE_* / PYTHON* / CONDA_* / NVM are listed by explicit
	// keys (not bare "NODE_" / "PYTHON" / "CONDA_") because several vars
	// in those namespaces let an attacker load arbitrary code or shadow
	// system paths in any Node.js / Python / conda subprocess the CLI
	// spawns (Claude CLI itself is Node.js). Explicitly excluded:
	//   - NODE_OPTIONS                 (can pass --require /path/to/evil.js)
	//   - NODE_EXTRA_CA_CERTS,
	//     NODE_TLS_REJECT_UNAUTHORIZED (TLS bypass)
	//   - NODE_PATH                    (require() resolution shadowing)
	//   - PYTHONSTARTUP                (runs on every python invocation)
	//   - PYTHONINSPECT                (drops into REPL after script)
	//   - PYTHONPATH                   (R222-SEC-2: any python subprocess
	//                                   loads attacker-writable modules
	//                                   ahead of stdlib; Bash tool reaches
	//                                   this via `python3 -c …`)
	//   - PYTHONHOME                   (R222-SEC-2: redirects sys.prefix
	//                                   to attacker tree, same outcome)
	//   - VIRTUAL_ENV                  (R222-SEC-2: pip / activated venv
	//                                   shims trust this for site-packages
	//                                   resolution; attacker tree behaves
	//                                   like PYTHONPATH on activated venv)
	//   - NVM_DIR                      (R222-SEC-2: node version manager
	//                                   tree exposes a parallel `node`
	//                                   binary that PATH-mismatch attacks
	//                                   can prefer over the system one)
	//   - CONDA_  (bare prefix)        (R222-SEC-2: too wide; only
	//                                   PREFIX/DEFAULT_ENV/SHLVL are
	//                                   benign identity bits — others
	//                                   like CONDA_PYTHON_EXE point at
	//                                   redirectable interpreters)
	"GOPATH=", "GOROOT=", "GOBIN=",
	"CARGO_HOME=", "RUSTUP_HOME=",
	"NODE_ENV=",
	// NPM_CONFIG_* can redirect npm's global-root / prefix / cache,
	// enabling RCE-class module-hijack attacks. Use an explicit allowlist
	// instead of the bare "NPM_" prefix. [R112714-ARCH-2]
	//   NPM_CONFIG_REGISTRY — registry URL redirect, no code execution path.
	//   NPM_TOKEN           — registry authentication token.
	// Explicitly excluded: NPM_CONFIG_PREFIX, NPM_CONFIG_GLOBALCONFIG,
	// NPM_CONFIG_CACHE, NPM_CONFIG_TMP — all redirect writable paths that
	// npm uses to resolve packages or run lifecycle scripts.
	"NPM_CONFIG_REGISTRY=", "NPM_TOKEN=",
	"PYTHONDONTWRITEBYTECODE=", "PYTHONUNBUFFERED=",
	"CONDA_PREFIX=", "CONDA_DEFAULT_ENV=", "CONDA_SHLVL=",
	"JAVA_HOME=",
}

// maxShimEnvEntryBytes caps the byte length of any single forwarded env
// variable value. Legitimate KEY=value pairs in the allowlist (PATH,
// HOME, NVM_DIR, JAVA_HOME, language locale, etc.) are well under 4 KiB
// in practice. A pathologically large value (e.g. a misconfigured
// PYTHONPATH or an attacker-poisoned shell rc) inflates the forked
// process's environment and slog attrs without contributing to CLI
// behavior; reject and log instead.
const maxShimEnvEntryBytes = 4 * 1024

// maxShimEnvOversizeWarnings caps how many "oversized entry rejected" warnings
// are emitted per process lifetime. [R20260603-SEC-5] A single sync.Once
// previously let the first benign oversized entry exhaust the log budget,
// masking a subsequent attacker-injected oversized entry (e.g. an
// ANTHROPIC_BASE_URL pointing at the IMDS endpoint) that would then be dropped
// silently. A small counter lets the first few of each batch surface while
// still bounding log volume from a pathological environment.
const maxShimEnvOversizeWarnings = 5

// filterShimEnvOversizeWarnings counts how many oversized-entry warnings have
// been emitted. Oversized entries are always rejected (the continue fires);
// only the *logging* is capped at maxShimEnvOversizeWarnings. [R20260603-SEC-5]
var filterShimEnvOversizeWarnings atomic.Int64

// filterShimEnv returns a copy of environ keeping only variables whose key
// matches one of the allowed prefixes. This is defense-in-depth: the CLI
// with --skip-permissions can still run `env` via Bash, but at least secrets
// not needed by the CLI are not exposed by default.
//
// Oversized entries (len > maxShimEnvEntryBytes) are rejected and logged
// (key prefix only, never the value) as documented by the maxShimEnvEntryBytes
// godoc. [R112714-ARCH-3]
func filterShimEnv(environ []string) []string {
	filtered := make([]string, 0, len(environ)/2)
	for _, kv := range environ {
		if len(kv) > maxShimEnvEntryBytes {
			// Log key prefix only — never log the value in case it is a
			// secret (e.g. a misconfigured PYTHONPATH that happens to
			// contain credential data). A counter caps the log output at
			// maxShimEnvOversizeWarnings per process lifetime so a
			// pathological environment (dozens of multi-MB exports) does
			// not flood the log, while still surfacing the first few —
			// previously a single sync.Once let one benign oversized entry
			// mask later attacker-injected ones. [R112714-ARCH-3]
			// [R20260603-SEC-5]
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
				// R20260602-SEC-1 (#1576, shim sibling): the endpoint vars
				// below steer where the CLI subprocess (which has Bash + raw
				// network access) sends API traffic. A poisoned shell rc / a
				// tampered profile that exports one of these at an attacker
				// host or the IMDS endpoint over plain http would silently
				// redirect / harvest. Require https for non-loopback hosts,
				// mirroring filterClaudeEnv's guard. Other allowlisted vars
				// are not URLs and pass through unchanged.
				if shimEndpointEnvDropped(kv) {
					break
				}
				// R20260603-SEC-1: AWS_PROFILE / AWS_DEFAULT_PROFILE select a
				// named profile from the AWS config. A profile may declare a
				// `credential_process` that the SDK executes as a shell command.
				// A poisoned shell rc / tampered profile that exports a profile
				// name containing path separators or shell metacharacters could
				// redirect that lookup to an attacker-controlled profile. Reject
				// values that don't match ^[A-Za-z0-9_-]{1,64}$. Mirrors
				// sysession/env.go isSafeProfileValue. Log key only, never value.
				if shimProfileEnvDropped(kv) {
					break
				}
				// R20260603-SEC-2: AWS_SHARED_CREDENTIALS_FILE / AWS_CONFIG_FILE /
				// AWS_WEB_IDENTITY_TOKEN_FILE name files the AWS SDK opens inside
				// the CLI subprocess. A poisoned shell rc / systemctl
				// set-environment that points one of these at /proc/self/environ,
				// /etc/shadow, or a relative-traversal path would make the SDK
				// read arbitrary host files and ship their contents to STS as a
				// credential / OIDC token (or leak the path content in SDK error
				// logs). Require an absolute, traversal-free, null-free path.
				// Log key only, never value.
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

// shimProfileEnvKeys is the set of allowlisted env keys whose value is an AWS
// profile *name*. A malformed value can redirect the SDK's credential_process
// lookup to an attacker-controlled profile, so the value is validated before
// pass-through. R20260603-SEC-1.
var shimProfileEnvKeys = map[string]bool{
	"AWS_PROFILE":         true,
	"AWS_DEFAULT_PROFILE": true,
}

// shimProfileEnvDropped reports whether kv (a "KEY=value" env entry) is an AWS
// profile-name var that must be dropped because its value contains characters
// outside ^[A-Za-z0-9_-]{1,64}$. Non-profile keys return false. The value is
// never logged. R20260603-SEC-1.
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

// shimCredPathEnvKeys is the set of allowlisted env keys whose value is a
// filesystem path the AWS SDK opens inside the CLI subprocess. A malicious
// value (e.g. /proc/self/environ, /etc/shadow, or a relative ../ traversal)
// makes the SDK read an arbitrary host file and treat it as a credential /
// OIDC token, so the value is validated before pass-through. R20260603-SEC-2.
var shimCredPathEnvKeys = map[string]bool{
	"AWS_SHARED_CREDENTIALS_FILE": true,
	"AWS_CONFIG_FILE":             true,
	"AWS_WEB_IDENTITY_TOKEN_FILE": true,
}

// shimCredPathEnvDropped reports whether kv (a "KEY=value" env entry) is an AWS
// credential-file path var that must be dropped because its value is not a
// safe path: it must be absolute, contain no ".." traversal segment, and carry
// no embedded null byte. Non-path keys and safe values return false. The value
// is never logged. R20260603-SEC-2.
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
// non-empty, no embedded null byte, absolute, and with no "." or ".." path
// segment after cleaning (so values like /a/../../etc/shadow are rejected even
// though they begin with a slash). R20260603-SEC-2.
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
// endpoint URL forwarded to the CLI subprocess. shimEndpointEnvDropped applies
// an SSRF/redirect guard to each. R20260602-SEC-1 (#1576, shim sibling).
var shimEndpointEnvKeys = map[string]bool{
	"ANTHROPIC_BASE_URL":         true,
	"ANTHROPIC_BEDROCK_BASE_URL": true,
	"AWS_ENDPOINT_URL":           true,
	"AWS_BEDROCK_ENDPOINT":       true,
}

// shimEndpointEnvDropped reports whether kv (a "KEY=value" env entry) is an
// endpoint URL var that must be dropped because its value targets a plain-http
// non-loopback host. Non-endpoint keys and safe URLs return false. The value
// is never logged. R20260602-SEC-1 (#1576, shim sibling).
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
// (localhost / 127.0.0.0/8 / ::1), for which plain http is allowed so local
// mock gateways still work. Mirrors filterClaudeEnv's validateClaudeBaseURLEnv.
//
// R20260603150052-SEC-7 (#1713): even an https:// target must not point at a
// literal internal IP (loopback excepted for local mocks). Without this, an
// operator rc that exports ANTHROPIC_BASE_URL=https://169.254.169.254/... would
// steer the CLI's Anthropic client — API key in hand — at the EC2 IMDS or an
// internal admin port. We only inspect literal IPs here (no DNS resolution at
// env-filter time); hostname-based rebinding is out of scope for this guard.
func validateShimEndpointURL(v string) error {
	u, err := url.Parse(v)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		host := u.Hostname()
		if ip := net.ParseIP(host); ip != nil && shimEndpointInternalIP(ip) && !ip.IsLoopback() {
			return fmt.Errorf("https:// to internal IP %q rejected (SSRF/IMDS guard)", host)
		}
		return nil
	case "http":
		host := u.Hostname()
		if strings.EqualFold(host, "localhost") {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("plain http:// to non-loopback host %q rejected (SSRF/redirect guard); use https://", host)
	}
	return fmt.Errorf("scheme %q not allowed; use https://", u.Scheme)
}

// shimEndpointInternalIP reports whether ip falls in the SSRF deny-set:
// loopback, RFC1918/ULA private, link-local (incl. 169.254.0.0/16 IMDS), or the
// unspecified address. Mirrors weixin's rejectInternalIP deny-set.
func shimEndpointInternalIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified()
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
