package sysession

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/naozhi/naozhi/internal/osutil"
)

// osStat is a package-level alias for os.Stat used by
// resolveBinPathFromEnv.  Keeping it as a function var lets unit tests
// stub the filesystem walk without touching disk; production callers
// pay one indirect call per PATH entry which is negligible (PATH walk
// happens once at NewRunner, not per Run).
var osStat = os.Stat

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
	env := filterEnv(cfg.EnvAllowlist)
	// R245-SEC-11: pin BinPath to an absolute path at construction time
	// using the PATH snapshot embedded in env.  Otherwise NewRunner
	// snapshots filtered PATH into env but Run() lets exec.CommandContext
	// resolve a relative BinPath via os.Getenv("PATH") at *call* time —
	// any later os.Setenv("PATH", ...) (tests, plugin loaders, etc.)
	// causes the binary picked up by Run() to diverge from what env says.
	// The race window is dashboard-restart wide: PATH-mutating goroutines
	// with NewRunner+Run pairs can land arbitrary CLI bins.
	//
	// We re-implement a minimal PATH walk because exec.LookPath uses the
	// process's live os.Getenv("PATH"), which is exactly the value we're
	// trying to insulate from. resolveBinPathFromEnv reads the PATH= entry
	// out of env (which is the snapshot we already commit to) and walks
	// it for the first executable file matching cfg.BinPath. On miss we
	// keep the original relative name so Run() still degrades gracefully
	// with an upstream error (matches the godoc above).
	if !filepath.IsAbs(cfg.BinPath) && !strings.ContainsRune(cfg.BinPath, filepath.Separator) {
		if abs, ok := resolveBinPathFromEnv(cfg.BinPath, env); ok {
			cfg.BinPath = abs
		}
	}
	// R247-SEC-19 (REPEAT-2 of R245-SEC-15): if cfg.BinPath is now an
	// absolute path (either operator-supplied or resolved out of the
	// snapshotted PATH above), Stat the eventual target and reject
	// anything that isn't a regular file with at least one executable
	// bit set. Stat (not Lstat) is intentional — a symlinked
	// /usr/local/bin/claude → /opt/.../claude is the dominant
	// installation pattern (Homebrew, pkg managers) and refusing it
	// would break operators on common setups. The TOCTOU window between
	// this check and Run() exec is the same as exec.LookPath's; this
	// guard catches the construction-time class (operator points
	// BinPath at a config dir / dangling link / dir-confused path) so
	// an obviously misconfigured Runner fails fast with a clear error
	// rather than degrading at first Tick.
	//
	// Relative names left in cfg.BinPath (resolveBinPathFromEnv missed)
	// fall through without validation — that path is already documented
	// to "degrade gracefully" via Run's exec.CommandContext and we keep
	// the same behaviour so a missing or mid-install CLI doesn't fail
	// naozhi startup.
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

// resolveBinPathFromEnv walks the PATH= entry inside env (env-slice
// form: "KEY=value" lines) for an executable whose basename equals
// name. Returns ("", false) when no PATH entry exists or no candidate
// is executable; the caller should leave cfg.BinPath as a relative
// name and let Run()'s exec.CommandContext degrade gracefully.
//
// We deliberately do not consult os.Getenv("PATH"): the whole point of
// this function is to insulate from a parent PATH that may be racing
// with another goroutine via os.Setenv. R245-SEC-11.
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
			// POSIX: empty entry is implicit "."; we refuse to honour it
			// because cwd-relative resolution is exactly the
			// cross-tenant attack vector we're trying to close.
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
		// Mode()&0o111 != 0 mirrors exec.LookPath's executability check
		// (any user/group/other +x bit).  Avoids trying to spawn a 0644
		// "claude" config file someone dropped in $PATH.
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

// runnerStderrCapBytes caps the bytes captured from "claude -p" stderr.
// 4 KiB is enough to surface the typical CLI diagnostic prefix
// ("Error: model not found", "auth failed", "context too long" with
// snippet, etc.) while bounding how much stdin-echo can leak into
// the error wrap. The first 256 bytes additionally land in slog.Warn
// (see Run's stderr-head log) — that is a separate, smaller cap and
// is intentional defence-in-depth, not a duplicate.
const runnerStderrCapBytes = 4096

// runnerStdoutCapBytes caps "claude -p" stdout. AutoTitler validates
// ≤16 Chinese characters; even with reasoning prefixes legitimate
// upstream output is well below this 64 KiB cap. The cap exists so a
// runaway CLI (infinite-loop reasoning, base64-blob hallucination)
// cannot OOM the parent naozhi process. limitedWriter lies about
// n=len(p) so exec.Cmd's stdout pump does not spin re-trying past
// the cap (see limitedWriter godoc).
const runnerStdoutCapBytes = 64 * 1024

// runnerImplBaseArgs is the fixed argv prefix for every "claude -p"
// invocation issued by sysession daemons (AutoTitler etc.).
//
// Closes R236-QA-17. These flags MUST stay in sync with the host
// session protocol contract that internal/cli/wrapper.go assumes:
//
//   - "-p"                  one-shot prompt mode (no stream-json).
//   - "--output-format text" parsed by Run() as a plain UTF-8 string;
//     switching to json/stream-json silently breaks every daemon.
//   - "--setting-sources \"\"" disables host hooks so naozhi's own
//     learning hooks do not re-enter on the daemon's CLI invocation
//     (would dead-loop the AutoTitler with the host's own hooks —
//     see Runner godoc and DESIGN.md §6.5).
//
// Any change here is a contract change for sysession + auto_titler.
// Verify against internal/cli/wrapper.go's spawn argv before editing.
//
// NEEDS-DESIGN (R241-ARCH-14): this slice is conceptually duplicated
// by internal/cli/protocol_claude.go:BuildArgs (which constructs the
// same -p / --output-format / --setting-sources triplet for
// ManagedSession one-shots). Today we keep them aligned by hand and
// rely on the inline reminder above. Plan: extend backend.Profile
// with a OneshotArgs() method so a new backend can express its
// streaming-vs-oneshot argv contract once; runnerImplBaseArgs and
// BuildArgs both consume that shared output. Deferred until a
// second backend (gemini-cli) needs the split, since premature
// abstraction here would commit cli + sysession to a Profile shape
// that doesn't yet have a non-Claude consumer to constrain it.
var runnerImplBaseArgs = []string{"-p", "--output-format", "text", "--setting-sources", ""}

func (r *runnerImpl) Run(ctx context.Context, prompt string) (string, error) {
	// Copy the package-level prefix so per-call --model append cannot
	// race with another concurrent Run mutating the shared backing
	// array. Cap the slice exactly at len(runnerImplBaseArgs) so the
	// first append always allocates fresh storage (defence-in-depth
	// against a future len/cap drift).
	args := append([]string(nil), runnerImplBaseArgs...)
	if r.cfg.Model != "" {
		args = append(args, "--model", r.cfg.Model)
	}

	cmd := exec.CommandContext(ctx, r.cfg.BinPath, args...)
	cmd.Dir = r.cfg.WorkDir
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = r.env

	// Capture stderr separately so panic-debug isn't lost when stdout is
	// empty (e.g. binary error before output).  We only return stderr
	// in the error wrap — never in the success path. See
	// runnerStderrCapBytes for the cap rationale.
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, max: runnerStderrCapBytes}

	// Cap stdout so a runaway "claude -p" can't OOM the parent process.
	// See runnerStdoutCapBytes for sizing rationale. limitedWriter lies
	// about n=len(p) so exec.Cmd's pump doesn't spin re-trying past the
	// cap.
	var stdout bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &stdout, max: runnerStdoutCapBytes}

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
//
// io.Writer CONTRACT VIOLATION (R232-GO-4): Write always returns
// (len(p), nil) — including the discard-after-cap path AND the inner
// writer error path. Returning (n<len(p), err) is what io.Writer
// formally requires, but exec.Cmd's stderr pump (and io.Copy in
// general) treats short writes as "retry forever", which would either
// loop on the cap or cascade an inner-writer fault into a stderr
// pump that never finishes. Callers of limitedWriter MUST be aware
// they will never see write errors and MUST NOT chain it into pipes
// that demand the standard contract. Currently only used as
// cmd.Stderr / cmd.Stdout for the sysession one-shot Run path; do
// NOT expose it beyond the package without revisiting this trade-off.
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
