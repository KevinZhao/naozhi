package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/shim"
)

// SpawnOptions configures how a CLI process is spawned.
type SpawnOptions struct {
	Key             string // session key (used for shim naming)
	Model           string
	ResumeID        string   // session ID to resume (empty = new session)
	ExtraArgs       []string // additional CLI args
	WorkingDir      string
	NoOutputTimeout time.Duration // kill process if no output for this long
	TotalTimeout    time.Duration // kill process if total turn exceeds this

	// PermissionMode controls how the Claude CLI handles tool permissions.
	// Zero value (PermissionModeDefault) preserves the legacy behaviour of
	// passing `--dangerously-skip-permissions` to the CLI — required by the
	// stream-json long-lived process model where the user has no opportunity
	// to interactively approve a permission prompt mid-turn. Multi-tenant /
	// untrusted-caller deployments can opt out via PermissionModeStandard,
	// which omits the flag and lets the CLI's own permission prompt fire
	// (which will block the session, by design). R215-SEC-P1-1 / #531.
	//
	// Only ClaudeProtocol consumes this field today; ACP backends ignore it.
	PermissionMode PermissionMode

	// DebugFile, when non-empty, is the path passed to the Claude CLI's
	// `--debug-file` flag (which implicitly enables debug). The CLI writes its
	// raw HTTP request/response + retry diagnostics (Bedrock status codes that
	// otherwise never surface in stream-json or stderr) to that file. Empty
	// (the zero value) keeps debug off — the flag is omitted entirely, so
	// every existing spawn stays bit-identical. The operator opts in per
	// deployment via the NAOZHI_CLI_DEBUG env var, which the session router
	// translates into a per-session path under <dataDir>/cli-debug. Only
	// ClaudeProtocol consumes this; ACP backends ignore it.
	DebugFile string
}

// PermissionMode selects how a Claude-CLI spawn handles tool permissions.
// See SpawnOptions.PermissionMode godoc.
type PermissionMode uint8

const (
	// PermissionModeDefault keeps the legacy --dangerously-skip-permissions
	// flag on the Claude CLI argv. This is the only mode compatible with
	// naozhi's headless `-p` long-lived process model today; switching off
	// the flag stalls the turn on the first permission prompt because the
	// CLI has no interactive surface in that mode. Zero value so existing
	// callers continue to spawn with the flag, no migration required.
	PermissionModeDefault PermissionMode = 0
	// PermissionModeStandard omits --dangerously-skip-permissions, deferring
	// to the Claude CLI's built-in permission prompts. Intended for
	// multi-tenant / untrusted deployments where the operator accepts the
	// stalled-turn cost in exchange for tool-call review. R215-SEC-P1-1.
	PermissionModeStandard PermissionMode = 1
)

// Wrapper manages spawning CLI processes via shim.
//
// R230-ARCH-13 / R231-ARCH-7: ShimManager is currently a public mutable
// pointer to internal/shim, which collapses two abstractions that the
// long-running design wants split — protocol (stream-json vs ACP) and
// transport (shim socket vs in-process / direct exec). Future backends
// that don't run via shim (Gemini SDK / WebSocket peers) will need a
// cli.Transport interface here instead. Until that ADR lands, treat
// ShimManager as the *only* transport: setting it to nil disables
// Spawn, and wrapping it with a custom Manager subclass is unsupported.
// Wrapper is otherwise immutable after NewWrapper — the public field
// access is for legacy wiring in cmd/naozhi only.
type Wrapper struct {
	BackendID   string // "claude" | "kiro" | future backends
	CLIPath     string
	CLIName     string // display name: "claude-code", "kiro"
	CLIVersion  string // semver from --version, e.g. "2.1.92"
	Protocol    Protocol
	ShimManager *shim.Manager

	// History factories are looked up by BackendID via
	// pickHistoryFactory(BackendID) inside NewHistorySource on every call,
	// rather than cached on the Wrapper at construction time. R240-ARCH-28:
	// caching let backend init() registrations that landed after a
	// NewWrapper silently no-op for that wrapper — a real hazard in tests
	// that register replacement factories per-t.Run and any future
	// blank-import / wireup-staged init ordering. The registry RWMutex
	// makes the per-call lookup cheap.
}

// NewWrapper creates a Wrapper with the given CLI path and protocol.
// If path is empty, auto-detects from known install locations and PATH.
//
// R241-ARCH-1: NewWrapper synchronously runs `<cli> --version` with a 5s
// hard timeout (detectVersion → context.Background derived). This makes
// construction blocking-IO instead of pure field assignment. Callers
// that need a true zero-IO constructor (tests / probes from a context
// already plumbed for shutdown) should use NewWrapperLazy + Probe(ctx)
// instead — Probe lets a Background-derived ctx be replaced with the
// caller's stopCtx so SIGTERM during startup does not block for the
// full 5 seconds. cmd/naozhi.main may migrate to the lazy variant once
// it threads ctx into its wrapper construction step (currently every
// wrapper read of CLIVersion happens AFTER construction, so the lazy
// + Probe pattern is wire-compatible).
func NewWrapper(cliPath string, proto Protocol, backend string) *Wrapper {
	w := newWrapperCommon(cliPath, proto, backend)
	// Eager probe — blocks up to 5s. Acceptable on the legacy startup
	// path where main.go expects CLIVersion populated before it logs
	// the "backend X version Y" banner. NewWrapperLazy skips this.
	w.CLIVersion = detectVersion(w.CLIPath)
	return w
}

// NewWrapperLazy is the non-blocking counterpart of NewWrapper:
// constructs the Wrapper without running `<cli> --version`. CLIVersion
// stays "" until the caller invokes Probe(ctx). Suitable for startup
// paths that already hold a stopCtx and want to bound the probe by
// it, and for tests that exercise wrapper construction without
// touching the filesystem. R241-ARCH-1.
func NewWrapperLazy(cliPath string, proto Protocol, backend string) *Wrapper {
	return newWrapperCommon(cliPath, proto, backend)
}

// Manager returns the wrapper's transport (today, *shim.Manager). New
// callers should prefer this accessor over reading the ShimManager field
// directly: when R242-ARCH-3 (#721) lands the cli.Transport interface,
// Wrapper will hold an unexported field of that interface type and
// Manager() will be the only forward-compatible read. Returns nil when
// the wrapper was constructed without a manager (e.g. test fixtures) or
// when the receiver is nil — callers can chain `w.Manager() == nil`
// without a separate nil-Wrapper guard.
func (w *Wrapper) Manager() *shim.Manager {
	if w == nil {
		return nil
	}
	return w.ShimManager
}

// WithManager injects the transport (today *shim.Manager) and returns the
// receiver for fluent construction: `cli.NewWrapperLazy(...).WithManager(m)`.
//
// R214-ARCH-9 (#405): the long-term direction is an unexported transport
// field set only at construction, with the public ShimManager field
// retired. Until the cli.Transport interface (R242-ARCH-3 / #721) lands and
// the cross-package readers in internal/session migrate to Manager(), this
// setter is the forward-compatible write path: new wiring should call
// WithManager instead of assigning the public field directly, so when the
// field finally goes unexported only this method body changes. Nil-safe on
// a nil receiver (returns nil) to compose with the lazy constructors.
func (w *Wrapper) WithManager(m *shim.Manager) *Wrapper {
	if w == nil {
		return nil
	}
	w.ShimManager = m
	return w
}

// Probe runs `<cli> --version` under the caller's context and stores
// the parsed result on the receiver. Safe to call multiple times; each
// call overwrites the cached version. Intended for callers that built
// the Wrapper via NewWrapperLazy and want to populate CLIVersion later
// (typically once during startup, after a stopCtx is available).
//
// The 5s subprocess timeout is still applied internally so a hung
// `<cli> --version` cannot pin the caller longer than the shorter of
// "ctx cancelled" and "5 seconds". Returns the version string for
// convenience; the same value is also written to w.CLIVersion. R241-ARCH-1.
func (w *Wrapper) Probe(ctx context.Context) string {
	if w == nil || w.CLIPath == "" {
		return ""
	}
	v := detectVersionCtx(ctx, w.CLIPath)
	w.CLIVersion = v
	return v
}

// newWrapperCommon is the shared constructor body for NewWrapper and
// NewWrapperLazy. Encapsulates the path expansion + backend
// normalisation + display-name lookup that BOTH eager and lazy
// constructors run identically.
func newWrapperCommon(cliPath string, proto Protocol, backend string) *Wrapper {
	if cliPath == "" {
		cliPath = detectCLI(backend)
	}
	cliPath = osutil.ExpandHome(cliPath)
	// R245-SEC-15 (REPEAT-2 with R236 / R241 round): defense-in-depth
	// argv hygiene check. cliPath is fed straight to exec.Command so a
	// relative path or special-file (FIFO / device) cliPath would be a
	// silent argv-injection / environment-confusion risk if config
	// validation upstream ever regresses. We log + carry on rather than
	// fail-fast: the empty-binary-yet-installable case (operator runs
	// naozhi before installing the CLI) and the test-fixture case (a
	// fakeBinary path that doesn't exist yet) both rely on construction
	// succeeding even when the file isn't there. detectVersion below
	// already handles missing files gracefully (returns ""), so the
	// spawn-time error is what surfaces operator-visible.
	validateCLIPath(cliPath)
	id := normalizeBackendID(backend)
	// R225-CR-9: surface unrecognised backend ids early. normalizeBackendID
	// passes through anything that is not empty/"claude" verbatim, so an
	// operator typo like "claud" or a config from an in-progress
	// not-yet-registered backend would silently flow through to the spawn
	// path where the failure is delayed and harder to attribute. The
	// checked set mirrors detect.go's knownBackends — adding a backend
	// requires a single edit to that var, which already gates dashboard
	// detection. Warn-only (not fail-fast) so test fixtures and
	// experimental backends can still construct a Wrapper without
	// pre-registering with knownBackends.
	if !isKnownBackendID(id) {
		slog.Warn("cli: unknown backend id, may fail at spawn",
			"backend", osutil.SanitizeForLog(id, 64),
			"raw", osutil.SanitizeForLog(backend, 64))
	}
	return &Wrapper{
		BackendID: id,
		CLIPath:   cliPath,
		// R228-ARCH-15: feed the canonical id (post-normalize) into
		// backendDisplayName so case variants like "Kiro" / "KIRO" hit
		// the same display branch as "kiro" instead of falling through
		// to the default arm and surfacing the raw operator-typed value.
		CLIName:  backendDisplayName(id),
		Protocol: proto,
	}
	// History factories are resolved at NewHistorySource call time via
	// pickHistoryFactory(BackendID) — see Wrapper struct godoc for
	// rationale (R240-ARCH-28). No binding happens here.
}

// isKnownBackendID reports whether id is a backend the cli package knows
// how to drive. Mirrors the entries in detect.go's knownBackends — the
// single source of truth for "what backends this cli build supports".
// R225-CR-9.
func isKnownBackendID(id string) bool {
	_, ok := lookupBackend(id)
	return ok
}

// backendDisplayName maps a backend config value to its user-facing name.
//
// R239-ARCH-K (#907): the display label is sourced from detect.go's
// knownBackends slice — the single source of truth for "what backends this
// cli build supports" — instead of a parallel hardcoded switch that drifts
// out of sync the moment someone edits one table but not the other. Adding
// a backend (e.g. gemini) is now a single knownBackends edit; this function
// needs no change. (The fully-decoupled backend.Register registry the issue
// proposes is still blocked on the cli/backend import cycle #1034; collapsing
// this in-package mirror is the non-breaking step available today.)
//
// The input is normalized first so case variants ("Kiro"/"KIRO") and the
// empty/legacy alias (""→"claude") resolve to the same canonical entry,
// preserving the R228-ARCH-15 contract. Unknown ids fall back to the raw
// (normalized) value, matching the previous default arm.
func backendDisplayName(backend string) string {
	id := normalizeBackendID(backend)
	if b, ok := lookupBackend(id); ok {
		return b.DisplayName
	}
	return id
}

// normalizeBackendID collapses empty/legacy aliases to the canonical ID.
// Empty strings (from legacy configs omitting cli.backend) map to "claude".
// Case-folds the input so an operator who writes "Claude" or "KIRO" in
// config still hits the canonical key downstream — otherwise the backend
// lookup fails silently and ops sees a "post-normalisation" log line that
// looks correct, masking the misconfiguration. (R227-CR-6)
func normalizeBackendID(backend string) string {
	backend = strings.ToLower(strings.TrimSpace(backend))
	switch backend {
	case "", "claude":
		return "claude"
	default:
		return backend
	}
}

// validateCLIPath emits a Warn for any cliPath that fails defense-in-depth
// argv hygiene (non-absolute path, or non-regular / non-executable when
// the file exists). Empty-path and ENOENT are intentionally NOT warnings:
// the operator may not have installed the CLI yet, and the spawn-time
// error is what surfaces those cases. R245-SEC-15.
//
// Why warn-only: NewWrapper is invoked at process startup before any IM
// channel can deliver a request, and the existing detectVersion path
// already swallows missing-file errors. Failing here would block startup
// for legitimate test fixtures and uninstalled-CLI deployments. The
// security value is in the audit trail, not in fail-fast.
func validateCLIPath(cliPath string) {
	if cliPath == "" {
		return
	}
	if !filepath.IsAbs(cliPath) {
		slog.Warn("cli: cliPath is not absolute; argv hygiene risk",
			"path", osutil.SanitizeForLog(cliPath, 256))
		return
	}
	fi, err := os.Lstat(cliPath)
	if err != nil {
		// ENOENT is fine (operator hasn't installed yet); other errors
		// are real surface to operators — symlink loops, permission
		// denied on the directory, etc.
		if !os.IsNotExist(err) {
			slog.Warn("cli: cliPath Lstat failed",
				"path", osutil.SanitizeForLog(cliPath, 256),
				"err", err)
		}
		return
	}
	mode := fi.Mode()
	// Regular file or symlink (which we'd resolve at exec time).
	// Reject FIFOs, devices, sockets — argv-injection vectors when the
	// kernel re-interprets the file type.
	if !mode.IsRegular() && mode&os.ModeSymlink == 0 {
		slog.Warn("cli: cliPath is not a regular file or symlink",
			"path", osutil.SanitizeForLog(cliPath, 256),
			"mode", mode.String())
		return
	}
	// Executable bit on at least one of user/group/other. Symlinks
	// don't carry the bit reliably, so skip the check for those —
	// exec.Command will surface the error if the target isn't exec.
	if mode.IsRegular() && mode.Perm()&0o111 == 0 {
		slog.Warn("cli: cliPath has no executable bit set",
			"path", osutil.SanitizeForLog(cliPath, 256),
			"mode", mode.String())
	}
}

// enforceCLIPathSafe is the spawn-time companion to validateCLIPath. It
// returns a non-nil error iff cliPath is a known-dangerous file type
// (FIFO, device node, socket, directory). Empty path, ENOENT, and
// non-absolute paths stay nil here: those failures are surfaced by the
// downstream shim spawn (or the construction-time warn). The point of
// this helper is to refuse the file-type-confusion attack vector
// (cli.path = /dev/fuse / /tmp/fifo) at the last hop before exec, even
// when upstream config validation regresses. R20260527122801-SEC-1.
//
// R20260603040203-SEC-2: spawn-time also HARD-REJECTS a non-absolute
// cliPath. By the time we reach here an attacker-controllable IM session
// key has triggered the spawn, and a relative name (e.g. "claude" or
// "../bin/evil") would be re-resolved against the live PATH / CWD by
// exec.Command — a PATH-poisoning argv-injection vector. Construction-time
// validateCLIPath stays warn-only (test fixtures / uninstalled CLI rely on
// construction succeeding); the hard refusal belongs at the last hop. Empty
// path still passes through here — the downstream shim spawn surfaces the
// uninstalled-CLI error.
func enforceCLIPathSafe(cliPath string) error {
	if cliPath == "" {
		return nil
	}
	if !filepath.IsAbs(cliPath) {
		return fmt.Errorf("cliPath must be absolute, got %q (PATH-injection guard)", cliPath)
	}
	fi, err := os.Lstat(cliPath)
	if err != nil {
		// ENOENT / permission-denied: let the downstream shim spawn
		// surface the more diagnostic operator-facing message.
		return nil
	}
	mode := fi.Mode()
	// Regular file is the canonical case. Symlinks pass through —
	// resolved at exec time; if the target is a FIFO/device the kernel
	// itself will fail to exec it with EACCES/ENOEXEC, which is the
	// strongest defense we can layer here without re-implementing the
	// kernel's symlink-walk semantics.
	if mode.IsRegular() || mode&os.ModeSymlink != 0 {
		return nil
	}
	// Reject dangerous file types: FIFO / char-device / block-device /
	// socket / directory. Each of these would cause exec.Command to
	// either fail late (FIFO blocks indefinitely on open) or, worse,
	// silently succeed with kernel-driven file-type-confusion.
	return fmt.Errorf("not a regular file or symlink (mode=%s)", mode.String())
}

// detectVersion runs "<cli> --version" and parses the version string.
// Uses a Background-derived 5s timeout — fine for test / single-backend
// paths. Production startup MUST go through NewWrapperLazy + Probe(ctx)
// (R241-ARCH-1) so SIGTERM during probe never blocks for the full
// timeout: the cmd/naozhi/main_init.go bootstrap already does this, and
// the legacy NewWrapper → detectVersion path now only runs in tests.
//
// R246-ARCH-21 (#803): the per-backend 5s × N startup hang is closed by
// the lazy-Probe migration above; this helper survives only because
// in-package tests construct Wrappers without a shutdown ctx and would
// otherwise force every test fixture to thread a context.Background
// boilerplate. New callers MUST use NewWrapperLazy.
func detectVersion(cliPath string) string {
	return detectVersionCtx(context.Background(), cliPath)
}

// detectVersionCtx is the context-aware variant of detectVersion.
// The caller's ctx is chained with a hard 5s subprocess timeout so a
// slow --version probe can never exceed the shorter of "caller's
// shutdown signal" and "5 seconds". R55-QUAL-004.
func detectVersionCtx(parent context.Context, cliPath string) string {
	// Short-circuit if the caller's context is already cancelled — avoids
	// forking a subprocess that will be SIGKILL'd immediately. R249-GO-11.
	if err := parent.Err(); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cliPath, "--version")
	// Anchor the subprocess CWD to "/" so relative binary names (the
	// detectCLI fallback when PATH lookup fails) cannot accidentally
	// resolve to a file in naozhi's working directory.
	cmd.Dir = "/"
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return parseVersionOutput(string(out))
}

// detectCLI finds the CLI binary by checking known install paths then PATH.
//
// The backend → binary mapping is sourced from the knownBackends table (see
// detect.go) rather than an inline switch so adding a future backend (e.g.
// gemini-cli) is a single registry edit. Unknown backend ids fall back to
// "claude" for the historical default-launcher behaviour. R225-CR-2.
//
// R249-SEC-7 (#920): when neither the well-known candidate paths nor
// exec.LookPath resolve the binary, return "" instead of the bare basename
// (e.g. "claude"). The bare-name path was reached at exec.Command time
// which re-resolves through the live PATH; a PATH-poisoning vector (admin
// misconfig prepending an attacker-writable directory, or a local
// privilege-escalation chain that mutates the env between detect and
// exec) would then run a malicious binary inside the shim spawn path.
// Returning "" forces the caller through the empty-path branch — Probe
// short-circuits, validateCLIPath stays silent, and exec.Command("")
// surfaces a clear error at spawn time instead of silently launching
// whatever happens to be on PATH at that moment.
func detectCLI(backend string) string {
	name, ok := knownBackendBinary(backend)
	if !ok || name == "" {
		name = "claude"
	}

	for _, p := range candidatePaths(name) {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	if p, err := exec.LookPath(name); err == nil {
		return p
	}

	return ""
}

// candidatePaths returns OS-specific install locations to probe.
func candidatePaths(name string) []string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return nil
	}

	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}

	var paths []string
	paths = append(paths, filepath.Join(home, ".local", "bin", name+ext))

	switch runtime.GOOS {
	case "darwin":
		paths = append(paths, filepath.Join("/opt/homebrew/bin", name))
		paths = append(paths, filepath.Join("/usr/local/bin", name))
	case "linux":
		paths = append(paths, filepath.Join("/usr/local/bin", name))
	case "windows":
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			paths = append(paths, filepath.Join(appdata, "npm", name+".cmd"))
		}
	}

	return paths
}

// Spawn starts a new CLI process via shim and returns a connected Process.
func (w *Wrapper) Spawn(ctx context.Context, opts SpawnOptions) (*Process, error) {
	if w.ShimManager == nil {
		return nil, fmt.Errorf("shim manager not configured")
	}

	// R164029-SEC-6: reject cwd containing NUL bytes. The OS silently
	// truncates at the NUL, which can redirect the working directory.
	if strings.ContainsRune(opts.WorkingDir, 0) {
		return nil, fmt.Errorf("cwd contains NUL byte")
	}

	// R20260527122801-SEC-1: enforce argv hygiene at spawn time. The
	// construction-time validateCLIPath is warn-only by design (test
	// fixtures + uninstalled-CLI deployments rely on construction
	// succeeding). But by the time we reach Spawn an IM message is in
	// flight, so we MUST refuse to feed a FIFO / device / socket / dir
	// into exec.Command via the shim — these are the real argv-injection
	// / file-type-confusion vectors that warn-only validation cannot
	// neutralise. ENOENT and empty-path stay warn-only here too: the
	// downstream shim spawn produces the operator-facing error in those
	// cases, and over-eager rejection would mask the more diagnostic
	// "shim could not exec <path>" message.
	if err := enforceCLIPathSafe(w.CLIPath); err != nil {
		return nil, fmt.Errorf("cli path unsafe: %w", err)
	}

	proto := w.Protocol.Clone()
	cliArgs := proto.BuildArgs(opts)

	cwd := opts.WorkingDir
	if cwd == "" {
		cwd = os.TempDir()
	}

	// Start shim → connect → auth → get hello. Use the wrapper-owned CLI
	// path + backend ID so multi-backend deployments launch each shim
	// against the correct binary and record its backend in state for
	// post-restart reconnect routing.
	handle, err := w.ShimManager.StartShimWithBackend(ctx, opts.Key, w.CLIPath, w.BackendID, cliArgs, cwd)
	if err != nil {
		return nil, fmt.Errorf("start shim: %w", err)
	}

	// Drain replay messages (for fresh shim this is empty)
	_, err = handle.DrainReplay()
	if err != nil {
		handle.Close()
		return nil, fmt.Errorf("drain replay: %w", err)
	}

	// R181-ARCH-P1-19: log a warning when the shim's protocol_version
	// differs from the compiled-in constant. Hot-upgrade paths may mix
	// an older shim binary with a newer naozhi; a silent mismatch here
	// manifested earlier as cryptic framing bugs. We intentionally do
	// NOT hard-fail yet — the field was added in v1 and older shims
	// emit 0, so a refusal would break every forward migration — but
	// operator visibility via slog is the cheapest safety net.
	if hv := handle.Hello.ProtocolVersion; hv != shim.ProtocolVersion {
		slog.Warn("shim protocol_version mismatch",
			"shim", hv,
			"naozhi", shim.ProtocolVersion,
			"key", opts.Key,
		)
		// R230B-ARCH-22 / RNEW-ARCH-403: when the shim reports a version
		// outside [MinSupportedProtocolVersion, ProtocolVersion], log at
		// Error so a rolling-deploy mismatch is loud in journalctl. We
		// still don't hard-fail because v0 (= "field absent") is the
		// historical bootstrap shape and refusing it would brick
		// upgrades from pre-v1 shims; once that path is gone the
		// branch can promote to a refused-attach error.
		if hv > 0 && (hv < shim.MinSupportedProtocolVersion || hv > shim.ProtocolVersion) {
			slog.Error("shim protocol_version outside supported range; reattach may misframe",
				"shim", hv,
				"min_supported", shim.MinSupportedProtocolVersion,
				"max_supported", shim.ProtocolVersion,
				"key", opts.Key,
			)
		}
	}

	cliPID := 0
	if handle.Hello.CLIPID > 0 {
		cliPID = handle.Hello.CLIPID
	}
	shimPID := 0
	if handle.Hello.ShimPID > 0 {
		shimPID = handle.Hello.ShimPID
	}

	proc := newShimProcess(
		handle.Conn, handle.Reader, handle.Writer,
		proto, cliPID, shimPID,
		opts.NoOutputTimeout, opts.TotalTimeout,
	)
	proc.SetSlogKey(opts.Key)
	proc.InitLinker(cwd)
	// UI Round 5 R5-3: stamp the spawn-time model so SessionView can show
	// "claude-opus-4.7" / "auto" in the dashboard header. opts.Model is
	// resolved at SpawnOptions assembly (router.go), pulling from
	// cli.backends[].model first, falling back to top-level cli.model.
	// "" means the operator left it unconfigured.
	proc.setModel(opts.Model)

	// Protocol init handshake (stream-json: no-op; ACP: initialize + session/new)
	rw := &JSONRW{
		W: proc.shimStdinWriter(),
		R: &shimLineReader{proc: proc},
	}
	sessionID, err := proto.Init(rw, opts.ResumeID, opts.WorkingDir)
	if err != nil {
		// proc.Kill() signals the shim to exit but does NOT close the net
		// connection owned by handle (Process.startReadLoop hasn't run, so
		// nothing is going to notice the close of killCh). Explicitly close
		// the handle so the Unix socket fd is freed immediately instead of
		// waiting for the shim's idle timeout (default 4h).
		proc.Kill()
		handle.Close()
		return nil, fmt.Errorf("protocol init: %w", err)
	}
	if sessionID != "" {
		proc.sessionID = sessionID
	}

	// If shim already captured session_id from init event during startup
	if handle.Hello.SessionID != "" && proc.sessionID == "" {
		proc.sessionID = handle.Hello.SessionID
	}

	proc.startReadLoop()
	// OBS2: counts successful fresh CLI spawns. SpawnReconnect is not counted
	// here — reconnect does not fork a new CLI child, it only re-attaches to
	// an already-running shim's socket. Incrementing only on Spawn keeps the
	// metric aligned with "new CLI process births".
	//
	// Multi-Backend RFC §10 (Sprint 6a): RecordCLISpawn double-writes the
	// legacy unlabeled CLISpawnTotal AND the new per-backend vector
	// CLISpawnTotalByBackend. The legacy counter stays for a 4-week
	// migration so existing pprof.md jq queries keep working.
	metrics.RecordCLISpawn(w.BackendID)
	return proc, nil
}

// SpawnReconnect creates a Process by reconnecting to an existing shim.
// Used after naozhi restart to resume an active session.
func (w *Wrapper) SpawnReconnect(ctx context.Context, key string, lastSeq int64, proto Protocol, noOutputTimeout, totalTimeout time.Duration) (*Process, []shim.ServerMsg, error) {
	if w.ShimManager == nil {
		return nil, nil, fmt.Errorf("shim manager not configured")
	}

	handle, err := w.ShimManager.Reconnect(ctx, key, lastSeq)
	if err != nil {
		return nil, nil, fmt.Errorf("reconnect shim: %w", err)
	}

	// Drain replay
	replays, err := handle.DrainReplay()
	if err != nil {
		handle.Close()
		return nil, nil, fmt.Errorf("drain replay: %w", err)
	}

	cliPID := 0
	if handle.Hello.CLIPID > 0 {
		cliPID = handle.Hello.CLIPID
	}
	shimPID := 0
	if handle.Hello.ShimPID > 0 {
		shimPID = handle.Hello.ShimPID
	}

	proc := newShimProcess(
		handle.Conn, handle.Reader, handle.Writer,
		proto.Clone(), cliPID, shimPID,
		noOutputTimeout, totalTimeout,
	)
	proc.SetSlogKey(key)
	// cwd unavailable on the reconnect path — shim owns it in its state file.
	// Caller (SessionRouter) supplies it via Process.SetCwdForLinker once the
	// session record is reloaded; until then SubagentLinker.Resolve bails out
	// with projectDir=="" and the dashboard falls back to the tombstone UX.
	proc.InitLinker("")

	if handle.Hello.SessionID != "" {
		proc.sessionID = handle.Hello.SessionID
	}

	// Detect mid-turn: if the last replayed event is not a turn-complete marker,
	// the CLI is actively processing and state should be Running (not Ready).
	// Also arm reconnectedMidTurn so readLoop's stray-result handler will
	// transition State back to Ready when the CLI finishes without anyone
	// calling Send() on this reattached process.
	//
	// Arm BEFORE startReadLoop (#1778): isMidTurn only inspects the already-
	// drained `replays`, so it is safe to evaluate before the loop starts.
	// Doing the State=Running + reconnectedMidTurn.Store ahead of startReadLoop
	// closes the window where readLoop is already consuming the live socket and
	// could process the turn's terminating result before the flag is armed —
	// which would leave the stray-result handler disabled and the session stuck
	// in StateRunning forever. startReadLoop preserves this pre-armed
	// StateRunning instead of forcing StateReady (see its godoc).
	if isMidTurn(replays, proto) {
		proc.mu.Lock()
		proc.state = StateRunning
		proc.mu.Unlock()
		proc.reconnectedMidTurn.Store(true)
	}

	proc.startReadLoop()

	return proc, replays, nil
}

// WaitSocketGoneForKey blocks until the shim socket associated with the
// given session key disappears from the filesystem, or maxWait elapses.
// Returns true when the socket is gone, false on timeout. Empty key is
// treated as "nothing to wait for" and returns true immediately.
//
// R222-ARCH-2 (#711): this absorbs the shim.SocketPath / shim.KeyHash /
// shim.WaitSocketGone trio behind a single cli-level helper so the
// session package no longer needs to import internal/shim just to
// compute a socket path. Lifecycle-side callers (Reset / ResetAndRecreate)
// use this to ensure the previous shim has released its UNIX socket
// before a fresh StartShim attempts to bind the same path; without the
// wait the dial-first guard ("refusing to clobber") in shim/server.go
// rejects the new bind and the user-visible reset stalls.
//
// Stateless: depends only on the global shim socket-naming convention
// (XDG_RUNTIME_DIR + KeyHash), so a *Wrapper receiver is unnecessary —
// callers reach this without plumbing a wrapper through Reset paths.
func WaitSocketGoneForKey(key string, maxWait time.Duration) bool {
	if key == "" {
		return true
	}
	socketPath := shim.SocketPath(shim.KeyHash(key))
	return shim.WaitSocketGone(socketPath, maxWait)
}

// isMidTurn checks replay events to determine if the CLI was mid-turn at
// reconnection time. Returns true if the last meaningful event is not a
// turn-complete result.
func isMidTurn(replays []shim.ServerMsg, proto Protocol) bool {
	lastType := ""
	for i := len(replays) - 1; i >= 0; i-- {
		if replays[i].Type != "replay" {
			continue
		}
		events, _, err := proto.ReadEvent(replays[i].Line)
		if err != nil || len(events) == 0 {
			continue
		}
		// Walk the slice in reverse so the last semantic event in the wire
		// frame wins (ACP turn-end emits assistant+result; only the result
		// settles the mid-turn question).
		picked := ""
		for j := len(events) - 1; j >= 0; j-- {
			if events[j].Type != "" {
				picked = events[j].Type
				break
			}
		}
		if picked == "" {
			continue
		}
		lastType = picked
		break
	}
	// "result" marks turn complete; anything else means mid-turn
	return lastType != "" && lastType != "result"
}

// shimLineReader adapts Process shim connection to the LineReader interface.
// Used during protocol Init handshake before readLoop starts.
type shimLineReader struct {
	proc *Process
}

// shimLineReaderMaxSkips caps the number of non-stdout/non-cli_exited shim
// frames the Init handshake will silently swallow before bailing with an
// error. Defends against a buggy or hostile shim that streams stderr / pong
// frames forever during proto.Init: the surrounding LineReader interface
// has no ctx parameter (R237-GO-6 / #633), so this is the structural
// timeout. 4096 is generous enough to absorb the realistic burst of
// transient ping / stderr lines a freshly-spawned CLI might emit before
// its first stdout JSON event, while still bounding the DoS surface.
const shimLineReaderMaxSkips = 4096

// ReadLine returns the next CLI stdout line received over the shim
// transport, blocking until either a stdout frame arrives or the shim
// signals CLI exit.
//
// Protocol contract (R240-CR-13):
//
//   - Each newline-terminated frame on the shim socket is a JSON
//     `shimMsg` envelope (see `internal/shim/protocol.go`). The shim
//     never forwards raw CLI bytes — every byte the CLI writes to its
//     stdout reaches us re-wrapped under `{"type":"stdout","line":...}`.
//   - `stdout` frames carry one logical CLI line in `line`; the
//     trailing `\n` is stripped by the shim, so callers downstream
//     (Protocol.Init) treat the returned []byte as a complete event
//     payload without further splitting.
//   - `cli_exited` is a terminal control frame. Returning a non-nil
//     error here triggers Init failure paths in the caller; we use a
//     descriptive message so an EOF mid-handshake is distinguishable
//     from a transport-level read error in logs.
//   - Anything else (`stderr`, `pong`, future control types) is
//     swallowed silently — Init only consumes stdout, and other types
//     are routed via different shim mechanisms once readLoop owns the
//     stream. The loop continues until the next stdout/cli_exited
//     frame, mirroring readLoop's discrimination.
//   - JSON parse failure on a frame is non-fatal: corrupt or partial
//     control frames from a misbehaving shim are skipped rather than
//     killing handshake. Read errors (transport-level) are returned
//     with eof=true so Init can retry or surface the failure.
//
// The (data, eof, err) signature matches the LineReader interface
// consumed by Protocol.Init; eof==true means "no more lines will ever
// arrive on this connection" and is mutually exclusive with a non-nil
// data return.
func (r *shimLineReader) ReadLine() ([]byte, bool, error) {
	// During Init, we need to read lines that come through the shim stdout wrapper.
	// The shim sends {"type":"stdout","line":"..."} — we need to unwrap.
	//
	// R237-GO-6 (#633): bounded skip counter so a misbehaving shim that
	// streams non-stdout/non-cli_exited frames forever cannot wedge the
	// handshake. The LineReader interface has no ctx parameter (its only
	// caller is Protocol.Init across both stream-json and ACP; widening
	// the signature is breaking across both backends), so we enforce the
	// upper bound locally here. On overflow we return eof=true + a
	// descriptive error so Spawn's error path tears down the shim
	// connection (Init failure goes through proc.Kill / handle.Close).
	skipped := 0
	for {
		rawLine, err := r.proc.shimR.ReadBytes('\n')
		if err != nil {
			return nil, true, err
		}
		var msg shimMsg
		if json.Unmarshal(rawLine, &msg) != nil {
			skipped++
			if skipped > shimLineReaderMaxSkips {
				return nil, true, fmt.Errorf("shim sent %d unparseable frames during init without stdout", skipped)
			}
			continue
		}
		if msg.Type == "stdout" {
			return []byte(msg.Line), false, nil
		}
		if msg.Type == "cli_exited" {
			return nil, true, fmt.Errorf("cli exited during init")
		}
		// Skip other message types (stderr, pong, etc.) during init,
		// but bound the loop so a forever-pinging shim cannot stall Init.
		skipped++
		if skipped > shimLineReaderMaxSkips {
			return nil, true, fmt.Errorf("shim sent %d non-stdout frames during init without stdout (last type=%q)", skipped, msg.Type)
		}
	}
}
