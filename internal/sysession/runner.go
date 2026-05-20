package sysession

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
)

// Runner is the LLM-call abstraction used by all daemons.  Each Run()
// invocation execs a fresh "claude -p" subprocess (= one transient
// system session) and returns the trimmed stdout.  The subprocess
// terminates the moment exec.Cmd.Output returns; there is no shared
// long-lived state between calls (RFC §6.1 — the SharedCLI route was
// rejected as self-contradictory).
//
// Implementations MUST:
//   - Pipe prompt through stdin (NOT argv) — prompts can contain user
//     conversation excerpts and `ps aux` would leak them otherwise.
//   - Set --setting-sources "" so naozhi-host claude hooks don't
//     re-enter.  Project rule: never inherit hooks across embedded
//     CLI invocations (would dead-loop the AutoTitler with the host's
//     own learning hooks).
//   - Honour ctx — exec.CommandContext is the standard mechanism.
type Runner interface {
	// Run execs a subprocess to evaluate prompt.  Returns trimmed
	// stdout.  Returns ctx.Err() when ctx is cancelled (so callers
	// can errors.Is(err, context.DeadlineExceeded) without reaching
	// for exit codes).
	Run(ctx context.Context, prompt string) (string, error)
}

// RunnerConfig configures the default exec-based Runner.
type RunnerConfig struct {
	// BinPath is the path to the CLI binary.  Defaults to looking
	// "claude" up via $PATH if empty.
	BinPath string

	// WorkDir is the cwd for spawned subprocesses.  Daemons MUST keep
	// this isolated from user workspaces (RFC §6.5):  use
	// <dataDir>/sys-sessions/ chmodded 0700.
	WorkDir string

	// Model overrides --model.  Empty leaves --model off so the binary
	// uses its own default.
	Model string

	// EnvAllowlist is a list of environment variable names that are
	// passed through to the subprocess (in addition to PATH and HOME
	// which are always passed).  Everything else is stripped — daemons
	// must NOT inherit IM tokens, dashboard secrets, or AWS creds.
	EnvAllowlist []string
}

// NewRunner returns a process-based Runner.  Returns an error if the
// configuration is unusable (e.g. WorkDir doesn't exist).
//
// The returned Runner is safe for concurrent use across goroutines —
// each Run() exec'd a separate subprocess with its own pipes, so there
// is no shared mutable state to race on.
func NewRunner(cfg RunnerConfig) (Runner, error) {
	if cfg.BinPath == "" {
		// Resolve via PATH.  We don't fail here; LookPath happens lazily
		// inside Run so a missing binary surfaces as an upstream error
		// (not a startup error).  This matches naozhi's default policy
		// of degrading gracefully when an optional CLI isn't installed
		// yet — operator can fix it without restarting.
		cfg.BinPath = "claude"
	}
	if cfg.WorkDir == "" {
		return nil, fmt.Errorf("sysession: Runner needs a WorkDir")
	}
	abs, err := filepath.Abs(cfg.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("sysession: resolve WorkDir: %w", err)
	}
	cfg.WorkDir = abs
	return &runnerImpl{cfg: cfg}, nil
}

type runnerImpl struct {
	cfg RunnerConfig
}

func (r *runnerImpl) Run(ctx context.Context, prompt string) (string, error) {
	args := []string{"-p", "--output-format", "text", "--setting-sources", ""}
	if r.cfg.Model != "" {
		args = append(args, "--model", r.cfg.Model)
	}

	cmd := exec.CommandContext(ctx, r.cfg.BinPath, args...)
	cmd.Dir = r.cfg.WorkDir
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = filterEnv(r.cfg.EnvAllowlist)

	// Capture stderr separately so panic-debug isn't lost when stdout is
	// empty (e.g. binary error before output).  We only return stderr
	// in the error wrap — never in the success path.
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, max: 4096}

	out, err := cmd.Output()
	if err != nil {
		// Prefer ctx.Err() so callers can errors.Is on
		// context.DeadlineExceeded without parsing exec.ExitError.
		// CommandContext sets process state to killed when ctx fires;
		// we still want a clean DeadlineExceeded return value.
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("sysession: %s -p failed: %w (stderr: %q)",
			filepath.Base(r.cfg.BinPath), err, stderr.String())
	}
	return strings.TrimSpace(string(out)), nil
}

// limitedWriter caps an io.Writer so a runaway subprocess can't fill
// memory with stderr.  Discards everything past max.  We don't emit a
// "[truncated]" marker because the stderr is only used in error
// messages — readability beats fidelity here.
//
// Pointer-receiver Write is intentional:  cmd.Stderr stores an
// io.Writer interface value, and a value-receiver Write would let
// every call see a fresh n=0 copy of the struct, defeating the cap.
type limitedWriter struct {
	w   io.Writer
	max int
	n   int
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	remaining := lw.max - lw.n
	if remaining <= 0 {
		return len(p), nil
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	written, err := lw.w.Write(p)
	lw.n += written
	return written, err
}
