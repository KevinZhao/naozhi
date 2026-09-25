package shim

// State-directory concerns: the quota gate every spawn passes and the discovery
// scan that reads the state files back after a restart. Split out of manager.go
// (#2713 B6).

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
)

// StateDir is the resolved shim state directory — the ManagerConfig value with
// NewManager's ~/.naozhi/shims default already applied. Callers that need to
// operate on that directory (the datadir.Sweeper retention pass) MUST read it
// here rather than re-deriving it from config: an empty cfg.Session.Shim.StateDir
// is the common case, and a second copy of the default silently disagrees with
// this one the moment either changes.
func (m *Manager) StateDir() string { return m.stateDir }

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

// strandedTempAge separates a temp file stranded by a crash from one a live
// shim is writing right now: WriteStateFile holds its temp file for a single
// write, and a running shim rewrites its state while Discover scans.
const strandedTempAge = 10 * time.Minute

// strandedStateTemp reports whether a temp file is old enough to have been
// left by a write that never finished. A successful write renames its temp
// file into place, so one that outlives strandedTempAge carries no state.
func strandedStateTemp(e fs.DirEntry, now time.Time) bool {
	info, err := e.Info()
	if err != nil {
		return false
	}
	return now.Sub(info.ModTime()) > strandedTempAge
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
		if osutil.IsAtomicTempName(e.Name()) {
			if strandedStateTemp(e, time.Now()) {
				_ = os.Remove(filepath.Join(m.stateDir, e.Name()))
			}
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
