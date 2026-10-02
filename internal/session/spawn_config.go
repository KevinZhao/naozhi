package session

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/metrics"
)

// spawnConfig is the spawn facet: the timeouts every CLI process gets, the
// router-owned paths argvSpawnOptions writes into argv, and the hook tests
// use to replace the spawn. Router holds it as the value field spawn, set in
// NewRouter and read-only afterwards; the zero value spawns with the CLI's
// default timeouts and no debug, settings or MCP files.
type spawnConfig struct {
	noOutputTimeout time.Duration
	totalTimeout    time.Duration

	// cliDebugDir, when non-empty, is where each spawned Claude CLI writes its
	// `--debug-file` log. Set only when NAOZHI_CLI_DEBUG opts in at construction;
	// empty keeps every spawn bit-identical. spawn() derives a per-session path.
	cliDebugDir string

	// naozhiSettingsFile mirrors RouterConfig.NaozhiSettingsFile ("" = legacy
	// `--setting-sources user`). Immutable after NewRouter; the shim
	// arg-drift check must pass the SAME value or --settings sessions look drifted.
	naozhiSettingsFile string
	// mcpConfigFile mirrors RouterConfig.MCPConfigFile ("" omits `--mcp-config`).
	// Immutable after NewRouter; the shim arg-drift comparison must mirror the spawn argv exactly.
	mcpConfigFile string

	// hook, when set, replaces the CLI spawn in spawnProcess, so tests
	// can hand back a process of their own at a moment of their choosing.
	// nil in production.
	hook func(ctx context.Context, opts cli.SpawnOptions) (processIface, error)
}

// spawnerFunc is the signature panicSafeSpawnFn executes; tests inject a
// function that panics instead of constructing a real cli.Wrapper. Production
// wraps (*cli.Wrapper).Spawn in a closure at the call site.
type spawnerFunc func(context.Context, cli.SpawnOptions) (*cli.Process, error)

// spawnProcess starts the session's CLI process: hook when a test set
// one, otherwise the backend's runner under panicSafeSpawn.
func (c *spawnConfig) spawnProcess(ctx context.Context, wrapper *cli.Wrapper, opts cli.SpawnOptions, key, backendID string) (processIface, error) {
	if c.hook != nil {
		return c.hook(ctx, opts)
	}
	proc, err := panicSafeSpawn(ctx, wrapper.Runner(), opts, key, backendID)
	if err != nil {
		// Never a typed-nil *cli.Process inside a non-nil interface.
		return nil, err
	}
	return proc, nil
}

// panicSafeSpawn invokes the runner's Spawn inside a deferred recover so a
// panic from the spawn path cannot leave pendingSpawns stranded (which would make every subsequent GetOrCreate fail with
// ErrMaxProcs until restart). The panic becomes a regular error so the
// caller's standard "spawn process: %w" wrap applies.
//
// Takes cli.Runner (the placement seam) rather than *cli.Wrapper so sandbox
// placements reuse the same protection; (*cli.Wrapper)(nil).Runner() returns
// nil, which is checked here.
func panicSafeSpawn(
	ctx context.Context,
	r cli.Runner,
	opts cli.SpawnOptions,
	key, backendID string,
) (*cli.Process, error) {
	if r == nil {
		// Wrap ErrNoCLIWrapper so error classification treats a nil runner
		// identically to the nil-wrapper guard upstream.
		return nil, fmt.Errorf("no runner for backend %q: %w", backendID, ErrNoCLIWrapper)
	}
	return panicSafeSpawnFn(ctx, r.Spawn, opts, key, backendID)
}

// panicSafeSpawnFn is the testable core: tests inject a spawnerFunc that
// panics to verify the recover path without a real wrapper.
func panicSafeSpawnFn(
	ctx context.Context,
	spawn spawnerFunc,
	opts cli.SpawnOptions,
	key, backendID string,
) (proc *cli.Process, err error) {
	defer func() {
		if r := recover(); r != nil {
			// Counter and slog record are paired inside the recover arm so
			// naozhi_spawn_panic_recovered_total counts exactly one per absorbed panic.
			metrics.SpawnPanicRecoveredTotal.Add(1)
			slog.Error("spawnSession: wrapper.Spawn panicked",
				"key", key, "backend", backendID, "panic", r,
				"stack", string(debug.Stack()))
			// Unprefixed: the caller wraps with "spawn process: %w". %w when the
			// panic value is an error preserves the errors.Is/As chain.
			if e, ok := r.(error); ok {
				err = fmt.Errorf("panic: %w", e)
			} else {
				err = fmt.Errorf("panic: %v", r)
			}
		}
	}()
	return spawn(ctx, opts)
}

// cliDebugPathFor returns the per-session debug-file path WITHOUT touching the
// filesystem, or "" when CLI debug capture is off. The file name reuses the
// event-log key-hash stem so a session's debug log lines up with its <stem>.log
// event file. driftCompareArgs uses this (not cliDebugFileFor) so the startup
// drift pass never conjures debug logs for sessions that will not respawn.
func (c *spawnConfig) cliDebugPathFor(key string) string {
	return CLIDebugPath(c.cliDebugDir, key)
}

// cliDebugFileFor returns cliDebugPathFor's path after pre-creating and
// hardening the file, for the spawn path. The file is overwritten on every
// spawn — debug capture is a live-tail diagnostic, not an audit trail.
func (c *spawnConfig) cliDebugFileFor(key string) string {
	path := c.cliDebugPathFor(key)
	if path == "" {
		return ""
	}
	// The claude child creates --debug-file under its own umask, so the log
	// (which may contain API keys) can land world-readable; pre-create at 0600
	// and Chmod to repair a pre-existing file O_CREATE leaves untouched (#2171).
	// No O_EXCL — the file legitimately pre-exists from a prior spawn. Errors
	// are fail-open (warn + still return path): hardening must never block a spawn.
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600); err != nil {
		slog.Warn("cli debug file pre-create failed; continuing without hardening",
			"path", path, "err", err)
	} else {
		_ = f.Close()
		if err := os.Chmod(path, 0o600); err != nil {
			slog.Warn("cli debug file chmod 0600 failed; log may be world-readable",
				"path", path, "err", err)
		}
	}
	return path
}
