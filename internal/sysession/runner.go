package sysession

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/naozhi/naozhi/internal/osutil"
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
	// stdout.  Prefers ctx.Err() when the subprocess exit is
	// attributable to context cancellation (i.e. cmd's error chain
	// contains context.Canceled or context.DeadlineExceeded), so
	// callers can errors.Is(err, context.DeadlineExceeded) without
	// reaching for exit codes.  In the rare race where ctx fires
	// concurrently with an organic non-zero exit, the raw exec error
	// is returned instead so the dashboard sees the real failure.
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
	// EnvAllowlist + parent env are stable post-construction, so the
	// filtered "KEY=value" slice is computed once. Avoids an os.Environ()
	// syscall + O(N) scan on every Run() (AutoTitler call rate). R230-PERF-3.
	return &runnerImpl{cfg: cfg, env: filterEnv(cfg.EnvAllowlist)}, nil
}

type runnerImpl struct {
	cfg RunnerConfig
	env []string
}

func (r *runnerImpl) Run(ctx context.Context, prompt string) (string, error) {
	args := []string{"-p", "--output-format", "text", "--setting-sources", ""}
	if r.cfg.Model != "" {
		args = append(args, "--model", r.cfg.Model)
	}

	cmd := exec.CommandContext(ctx, r.cfg.BinPath, args...)
	cmd.Dir = r.cfg.WorkDir
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = r.env

	// Capture stderr separately so panic-debug isn't lost when stdout is
	// empty (e.g. binary error before output).  We only return stderr
	// in the error wrap — never in the success path.
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, max: 4096}

	// Cap stdout so a runaway "claude -p" can't OOM the parent process.
	// AutoTitler validates ≤16 Chinese chars; even with reasoning
	// prefixes the upstream output is well below 64 KiB. Use the same
	// limitedWriter pattern as stderr — it lies about n=len(p) so
	// exec.Cmd's pump doesn't spin re-trying past the cap.
	var stdout bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &stdout, max: 64 * 1024}

	err := cmd.Run()
	if err != nil {
		// Prefer ctx.Err() ONLY when err is the context cancellation
		// surfacing through exec — otherwise an exec.ExitError that
		// happened to coincide with ctx cancellation gets clobbered
		// and the dashboard loses the real failure detail.
		if ctxErr := ctx.Err(); ctxErr != nil &&
			(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			return "", ctxErr
		}
		// Sec-LOW-2:  stderr from claude -p can echo back portions of
		// stdin (= the prompt = user conversation excerpts) when the
		// CLI errors — e.g. "context too long" diagnostics.  We log a
		// truncated head at Warn so a tripping breaker is debuggable
		// from journalctl without operators having to flip slog level.
		// 256 bytes is enough to see the CLI's diagnostic prefix
		// ("Error: model not found", "auth failed", etc.) while
		// limiting how much prompt content can leak into log
		// aggregators.  ErrorMsg in the breaker log line is still
		// sanitized (only "exit status N").
		if stderr.Len() > 0 {
			// SanitizeForLog handles both byte-level truncation and
			// rune-boundary safety, so a multi-byte CJK character at the
			// 256-byte cap doesn't leak invalid UTF-8 into structured log
			// sinks. It also scrubs C0/C1/bidi bytes the prompt fragment
			// (echoed back by the CLI on error) might carry.
			// 不在此处预截：byte-slice 截断会把多字节 rune 切到中段，而
			// SanitizeForLog 的 walk-back rune 边界修正只在 mapped 长度
			// 超过 maxLen 时触发；slow-path strings.Map 把非法 rune 替换
			// 为 '_'（1 字节），mapped 长度 ≤ 输入长度，于是 walk-back
			// 不会跑，最终输出残留 mid-rune 字节。
			head := osutil.SanitizeForLog(stderr.String(), 256)
			slog.Warn("sysession: runner stderr",
				"binary", filepath.Base(r.cfg.BinPath),
				"stderr_head", head)
		}
		return "", fmt.Errorf("sysession: %s -p failed: %w",
			filepath.Base(r.cfg.BinPath), err)
	}
	// bytes.TrimSpace operates on []byte directly; the final string()
	// conversion is the only allocation, vs strings.TrimSpace(string(out))
	// which copies twice.
	return string(bytes.TrimSpace(stdout.Bytes())), nil
}

// Compile-time guarantee that runnerImpl satisfies the Runner contract
// described above (stdin pipe / --setting-sources / ctx honour). Adding
// an unrelated method to the interface fails the build in this file.
var _ Runner = (*runnerImpl)(nil)

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
	// Always claim we consumed the whole input — io.Writer's contract
	// says n must equal len(p) when err is nil, and exec.Cmd's stderr
	// pump treats n<len(p) as a partial write and re-tries indefinitely.
	// Anything past max is silently discarded.
	remaining := lw.max - lw.n
	if remaining <= 0 {
		return len(p), nil
	}
	chunk := p
	if len(chunk) > remaining {
		chunk = chunk[:remaining]
	}
	written, err := lw.w.Write(chunk)
	lw.n += written
	// io.Writer contract: when err != nil, n MUST be < len(p). Surfacing
	// (len(p), err) violates that and confuses callers (and exec.Cmd's
	// stderr pump). On the discard path we already swallow overflow
	// without an error; do the same on writer error so the pump treats
	// the chunk as fully accepted and keeps draining.
	_ = err
	return len(p), nil
}
