package shim

// State-directory concerns: the quota gate every spawn passes and the discovery
// scan that reads the state files back after a restart. Split out of manager.go
// (#2713 B6).

import (
	"context"
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

	// liveMu guards live, the key → shim PID set the last Discover returned
	// (nil before the first), which keeps a steady reconcile tick quiet.
	liveMu sync.Mutex
	live   map[string]int
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

// StateVerdict is how one state file classifies against the live process
// table, in the order Discover checks: corrupt, dead PID, binary mismatch,
// missing socket.
type StateVerdict int

const (
	StateLive StateVerdict = iota
	StateCorrupt
	StateDeadPID
	StateBinaryMismatch
	StateSocketMissing
)

// StateEntry is one classified state file. Err carries the read error of a
// corrupt file or the stat error of a missing socket. IdentityErr is set when
// the binary check could not run; the verdict then comes from the socket check.
type StateEntry struct {
	Path        string
	State       State
	Verdict     StateVerdict
	Err         error
	IdentityErr error
}

// classifyStateFile reads and judges one state file without changing anything.
func (m *Manager) classifyStateFile(path string) StateEntry {
	se := StateEntry{Path: path}
	state, err := ReadStateFile(path)
	if err != nil {
		se.Verdict, se.Err = StateCorrupt, err
		return se
	}
	se.State = state
	if !pidAlive(state.ShimPID) {
		se.Verdict = StateDeadPID
		return se
	}
	// Binary identity catches PID reuse; the linux helper strips "(deleted)"
	// so shims from the previous build are still recognised as ours.
	mismatch, ierr := shimPIDBinaryMismatch(state.ShimPID, m.naozhiBin)
	if ierr != nil {
		se.IdentityErr = ierr
	} else if mismatch {
		se.Verdict = StateBinaryMismatch
		return se
	}
	if _, err := os.Stat(state.Socket); err != nil {
		se.Verdict, se.Err = StateSocketMissing, err
		return se
	}
	se.Verdict = StateLive
	return se
}

// readStateDir lists the state directory; a missing directory is empty.
func (m *Manager) readStateDir() ([]fs.DirEntry, error) {
	entries, err := os.ReadDir(m.stateDir)
	if err != nil && errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return entries, err
}

// Inspect classifies every state file and never removes, renames or signals
// anything, so a diagnostic command run from any binary leaves the directory
// as it found it. The binary verdict is relative to the calling executable.
func (m *Manager) Inspect() ([]StateEntry, error) {
	entries, err := m.readStateDir()
	if err != nil {
		return nil, err
	}
	var out []StateEntry
	for _, e := range entries {
		if e.IsDir() || osutil.IsAtomicTempName(e.Name()) || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		out = append(out, m.classifyStateFile(filepath.Join(m.stateDir, e.Name())))
	}
	return out, nil
}

// Discover scans the state directory for existing shim state files and
// returns the live ones, deleting every other state file and stranded temp
// file and SIGTERMing socketless shims. changed reports whether the live set
// differs from the previous Discover's (always true on the first). Only the
// service should call it: the binary check is against the caller's own
// executable.
func (m *Manager) Discover() (states []State, changed bool, err error) {
	entries, err := m.readStateDir()
	if err != nil {
		return nil, false, err
	}

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
		se := m.classifyStateFile(filepath.Join(m.stateDir, e.Name()))
		path, state := se.Path, se.State
		switch se.Verdict {
		case StateCorrupt:
			slog.Warn("removing corrupt state file", "path", path, "err", se.Err)
			RemoveStateFile(path)
		case StateDeadPID:
			slog.Info("removing stale shim state file", "path", path, "pid", state.ShimPID)
			RemoveStateFile(path)
		case StateBinaryMismatch:
			slog.Info("removing stale shim state file (binary mismatch)", "path", path, "pid", state.ShimPID)
			RemoveStateFile(path)
		case StateSocketMissing:
			// Live PID + missing socket is the zombie signature: the listener fd
			// exists but its path is gone (external rm, /run cleaner, XDG_RUNTIME_DIR
			// rotation), so Reconnect would ENOENT forever. Let it self-terminate
			// via SIGTERM and purge the on-disk record.
			slog.Info("removing shim state: socket missing",
				"path", path, "pid", state.ShimPID,
				"socket", state.Socket, "err", se.Err)
			// Re-check the PID: a shim exiting gracefully unlinks its own socket,
			// and SIGTERM to a dead PID could hit an unrelated process reusing it.
			if pidAlive(state.ShimPID) {
				_ = sendSIGTERM(state.ShimPID)
			}
			RemoveStateFile(path)
		default:
			states = append(states, state)
		}
	}
	return states, m.noteLive(states), nil
}

// noteLive records states as the live set and reports whether it differs from
// the previous one. A shim new to the set, or back under another PID, logs at
// INFO; one already known logs at DEBUG.
func (m *Manager) noteLive(states []State) bool {
	next := make(map[string]int, len(states))
	m.liveMu.Lock()
	defer m.liveMu.Unlock()
	changed := m.live == nil
	for _, s := range states {
		next[s.Key] = s.ShimPID
		level := slog.LevelDebug
		if pid, ok := m.live[s.Key]; !ok || pid != s.ShimPID {
			level, changed = slog.LevelInfo, true
		}
		slog.Log(context.Background(), level, "discovered live shim", "key", s.Key, "pid", s.ShimPID)
	}
	if len(next) != len(m.live) {
		changed = true
	}
	m.live = next
	return changed
}
