package sysession

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/naozhi/naozhi/internal/osutil"
)

// osStat lets unit tests stub the PATH walk in resolveBinPathFromEnv.
var osStat = os.Stat

// Runner is the LLM-call abstraction used by all daemons. Each Run execs
// a fresh "claude -p" subprocess (one transient system session) and
// returns trimmed stdout; nothing is shared between calls (RFC §6.1).
//
// Implementations MUST:
//   - Pipe the prompt through stdin, NOT argv — prompts carry user
//     conversation excerpts and `ps aux` would leak them.
//   - Pass --setting-sources "" so the host's claude hooks don't re-enter
//     and dead-loop the AutoTitler.
//   - Honour ctx (exec.CommandContext).
type Runner interface {
	// Run execs a subprocess for prompt and returns trimmed stdout. Returns
	// ctx.Err() when the exit is attributable to cancellation so callers
	// can errors.Is it; if ctx fires concurrently with an organic non-zero
	// exit, the raw exec error wins so the dashboard sees the real failure.
	Run(ctx context.Context, prompt string) (string, error)
}

// RunnerConfig configures the default exec-based Runner.
type RunnerConfig struct {
	// BinPath is the path to the CLI binary.  Defaults to looking
	// "claude" up via $PATH if empty.
	BinPath string

	// WorkDir is the cwd for subprocesses. MUST be isolated from user
	// workspaces (RFC §6.5): <dataDir>/sys-sessions/ chmod 0700.
	WorkDir string

	// Model overrides --model.  Empty leaves --model off so the binary
	// uses its own default.
	Model string

	// EnvAllowlist names the env vars passed to the subprocess (PATH and
	// HOME always pass). Everything else is stripped: daemons must NOT
	// inherit IM tokens, dashboard secrets or AWS creds.
	EnvAllowlist []string

	// Ledger receives one cost entry per Run, attributed to the daemon run
	// carried on the context (RunInfoFromContext). nil = not recorded.
	Ledger CostLedger
}

// NewRunner returns a process-based Runner; errors when the
// configuration is unusable. Safe for concurrent use: each Run has its
// own subprocess and pipes.
func NewRunner(cfg RunnerConfig) (Runner, error) {
	if cfg.BinPath == "" {
		// Resolved via PATH below; a missing binary surfaces lazily at Run as
		// an upstream error so an operator can install it without restarting.
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
	// Allowlist + parent env are stable post-construction; filter once.
	env := filterEnv(cfg.EnvAllowlist)
	// Pin BinPath to an absolute path using the PATH snapshot inside env.
	// exec.CommandContext would otherwise resolve a relative BinPath via
	// the live os.Getenv("PATH") at call time, so a later os.Setenv could
	// make Run spawn a binary that diverges from what env says. On miss
	// keep the relative name so Run still degrades gracefully.
	if !filepath.IsAbs(cfg.BinPath) && !strings.ContainsRune(cfg.BinPath, filepath.Separator) {
		if abs, ok := resolveBinPathFromEnv(cfg.BinPath, env); ok {
			cfg.BinPath = abs
		}
	} else {
		// Operator-supplied path: skip the PATH walk but keep the
		// regular-file + executable gate so Run can't be pointed at a 0644
		// config file, a directory or a device node. os.Stat (follows
		// symlinks) on purpose: a distro `claude` is commonly a symlink
		// chain and only the terminal target matters.
		if info, err := os.Stat(cfg.BinPath); err != nil {
			return nil, fmt.Errorf("sysession: BinPath %q: %w", cfg.BinPath, err)
		} else if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("sysession: BinPath %q is not a regular file", cfg.BinPath)
		} else if info.Mode()&0o111 == 0 {
			return nil, fmt.Errorf("sysession: BinPath %q is not executable", cfg.BinPath)
		}
	}
	// Fail fast at construction when an absolute BinPath isn't a regular
	// executable (Stat, not Lstat: symlinked installs are the norm). The
	// TOCTOU window to exec matches exec.LookPath's. Relative names that
	// resolveBinPathFromEnv missed fall through unvalidated so a missing
	// CLI doesn't fail startup.
	if filepath.IsAbs(cfg.BinPath) {
		info, err := os.Stat(cfg.BinPath)
		if err != nil {
			return nil, fmt.Errorf("sysession: stat BinPath %q: %w", cfg.BinPath, err)
		}
		mode := info.Mode()
		if !mode.IsRegular() {
			return nil, fmt.Errorf("sysession: BinPath %q is not a regular file (mode=%v)", cfg.BinPath, mode)
		}
		if mode.Perm()&0o111 == 0 {
			return nil, fmt.Errorf("sysession: BinPath %q is not executable (mode=%v)", cfg.BinPath, mode)
		}
	}
	return &runnerImpl{cfg: cfg, env: env}, nil
}

// resolveBinPathFromEnv walks the PATH= entry of env ("KEY=value" lines)
// for an executable named name; ("", false) when none, so the caller
// keeps a relative BinPath. It deliberately ignores os.Getenv("PATH"):
// insulating from a racing parent PATH is the whole point.
func resolveBinPathFromEnv(name string, env []string) (string, bool) {
	const pathPrefix = "PATH="
	var path string
	for _, kv := range env {
		if strings.HasPrefix(kv, pathPrefix) {
			path = kv[len(pathPrefix):]
			break
		}
	}
	if path == "" {
		return "", false
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			// POSIX treats an empty entry as "."; refuse it — cwd-relative
			// resolution is the attack vector being closed.
			continue
		}
		candidate := filepath.Join(dir, name)
		info, err := osStat(candidate)
		if err != nil {
			continue
		}
		if info.IsDir() {
			continue
		}
		// Mirrors exec.LookPath's +x check (any user/group/other bit).
		if info.Mode()&0o111 == 0 {
			continue
		}
		return candidate, true
	}
	return "", false
}

type runnerImpl struct {
	cfg RunnerConfig
	env []string
}

// runnerStderrCapBytes caps captured stderr: enough for the CLI's
// diagnostic prefix while bounding how much stdin-echo (prompt content)
// can leak into the error wrap. The 256-byte slog head in Run is a
// separate, tighter cap on purpose.
const runnerStderrCapBytes = 4096

// runnerStdoutCapBytes bounds stdout so a runaway CLI can't OOM naozhi. The
// json envelope adds a few KiB (usage, modelUsage, permission_denials) around
// a reply that is itself far below 64 KiB; an output that hits the cap is
// truncated JSON and Run reports it as an error. limitedWriter claims
// n=len(p) so exec.Cmd's pump doesn't spin past the cap.
const runnerStdoutCapBytes = 256 * 1024

// runnerImplBaseArgs is the fixed argv prefix for every daemon "claude -p"
// call (one-shot; unrelated to the stream-json argv protocol_claude.go
// BuildArgs builds for long-lived sessions). Pinned by
// TestRunnerImplBaseArgs_Contract:
//   - "-p": one-shot mode (no stream-json).
//   - "--output-format json": Run decodes the single result envelope and
//     hands the daemon its `result` string; the envelope also carries the
//     cost receipt booked to the ledger. text/stream-json break every daemon.
//   - `--setting-sources ""`: disables host hooks so naozhi's own hooks
//     don't re-enter and dead-loop the daemon's CLI call (DESIGN.md §6.5).
var runnerImplBaseArgs = []string{"-p", "--output-format", "json", "--setting-sources", ""}

func (r *runnerImpl) Run(ctx context.Context, prompt string) (string, error) {
	// Copy the shared prefix so the per-call --model append can't race
	// another Run on the same backing array.
	args := append([]string(nil), runnerImplBaseArgs...)
	if r.cfg.Model != "" {
		args = append(args, "--model", r.cfg.Model)
	}

	cmd := exec.CommandContext(ctx, r.cfg.BinPath, args...)
	cmd.Dir = r.cfg.WorkDir
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = r.env

	// stderr is only surfaced in the error wrap, never on success.
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, max: runnerStderrCapBytes}

	// Cap stdout; see runnerStdoutCapBytes.
	var stdout bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &stdout, max: runnerStdoutCapBytes}

	err := cmd.Run()
	if err != nil {
		// On ctx cancel cmd.Run returns *exec.ExitError (killed by signal),
		// not the ctx error, so check ctx.Err() directly.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		// stderr can echo stdin (= user conversation excerpts) on CLI
		// errors. Log a 256-byte sanitized head at Warn so a tripping
		// breaker is debuggable from journalctl while limiting prompt leakage,
		// and fold the same head into the returned error so the dashboard's
		// last_error is more than "exit status N" (#804).
		var stderrHead string
		if stderr.Len() > 0 {
			// SanitizeForLog does byte truncation, rune-boundary walk-back and
			// C0/C1/bidi scrubbing in one pass. 不在此处预截：byte-slice 截断会把
			// 多字节 rune 切到中段，而 SanitizeForLog 的 walk-back 只在 mapped
			// 长度超过 maxLen 时触发，最终会残留 mid-rune 字节。
			stderrHead = osutil.SanitizeForLog(stderr.String(), 256)
			slog.Warn("sysession: runner stderr",
				"binary", filepath.Base(r.cfg.BinPath),
				"stderr_head", stderrHead)
		}
		if stderrHead != "" {
			return "", fmt.Errorf("sysession: %s -p failed: %w (stderr: %s)",
				filepath.Base(r.cfg.BinPath), err, stderrHead)
		}
		return "", fmt.Errorf("sysession: %s -p failed: %w",
			filepath.Base(r.cfg.BinPath), err)
	}
	env, err := parseResultEnvelope(stdout.Bytes())
	// Book before judging the outcome: a run that failed after several
	// model calls still spent the money (unparsable output books nothing).
	ri, ok := RunInfoFromContext(ctx)
	if !ok {
		ri = RunInfo{Daemon: "unmanaged"}
	}
	r.bookRunCost(env, ri)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(env.Result), nil
}

// Compile-time check that runnerImpl satisfies Runner.
var _ Runner = (*runnerImpl)(nil)

// limitedWriter caps an io.Writer so a runaway subprocess can't fill
// memory; bytes past max are discarded with no "[truncated]" marker.
// Pointer receiver is required: cmd.Stderr holds an interface value and
// a value receiver would see a fresh n=0 every call.
//
// io.Writer CONTRACT VIOLATION: Write always returns (len(p), nil), on
// the discard path AND on inner-writer error. exec.Cmd's pump (and
// io.Copy) treats short writes as "retry forever", which would loop on
// the cap or cascade a sink fault. Callers never see write errors; do
// NOT expose this beyond the sysession one-shot Run path.
type limitedWriter struct {
	w      io.Writer
	max    int
	n      int
	failed bool // set once the inner Write errors (#794).
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	// Always claim the whole input consumed (see type godoc); past max
	// is silently discarded.
	remaining := lw.max - lw.n
	if remaining <= 0 {
		return len(p), nil
	}
	// Once the inner writer has errored, never call it again: lw.n would
	// never grow, the cap fast-path would never engage, and every chunk
	// would burn a call on a known-broken sink (#794).
	if lw.failed {
		return len(p), nil
	}
	chunk := p
	if len(chunk) > remaining {
		chunk = chunk[:remaining]
	}
	written, err := lw.w.Write(chunk)
	lw.n += written
	// Never propagate err (pump-safety invariant, see type godoc), but
	// Warn once on the failed transition so a dead sink / ENOSPC is
	// diagnosable (#2253).
	if err != nil && !lw.failed {
		lw.failed = true
		slog.Warn("sysession: limitedWriter inner sink failed; discarding remaining output",
			"written", lw.n, "err", osutil.SanitizeForLog(err.Error(), 256))
	}
	return len(p), nil
}
