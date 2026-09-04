package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/shim"
)

// SpawnOptions configures how a CLI process is spawned.
type SpawnOptions struct {
	Key   string // session key (used for shim naming)
	Model string
	// Effort is the thinking-effort tier to spawn under (kiro: low/medium/high/
	// xhigh/max); empty passes no flag. Only protocols advertising Caps.EffortTier
	// consume it. The tier binds to the PROCESS, not the session: a kiro resume
	// spawned without the flag silently reverts to the global default (verified
	// on 2.16.0), so every spawn path must keep passing it. docs/rfc/kiro-effort-control.md
	Effort          string
	ResumeID        string   // session ID to resume (empty = new session)
	ExtraArgs       []string // additional CLI args
	WorkingDir      string
	NoOutputTimeout time.Duration // kill process if no output for this long
	TotalTimeout    time.Duration // kill process if total turn exceeds this

	// PermissionMode controls Claude CLI tool permissions. The zero value passes
	// `--dangerously-skip-permissions` — required by the headless long-lived
	// process model where nobody can approve a prompt mid-turn; untrusted-caller
	// deployments opt out via PermissionModeStandard (#531). Claude-only.
	PermissionMode PermissionMode

	// DebugFile, when non-empty, is passed as the Claude CLI's `--debug-file`
	// (implicitly enabling debug) so raw HTTP/retry diagnostics — Bedrock status
	// codes that never surface in stream-json — land in that file. Empty omits
	// the flag. Opt-in via NAOZHI_CLI_DEBUG, which the router turns into a
	// per-session path under <dataDir>/cli-debug. Claude-only.
	DebugFile string

	// EnvOverlay is the per-spawn access-profile env overlay (RFC
	// project-access-profile §4): resolved "KEY"→"value" pairs that override the
	// shim's baseline for THIS session only; nil keeps the global baseline. The
	// shim re-gates every entry through filterShimEnv, so this is a value override
	// on already-allowed keys, not a whitelist bypass. Protocol-agnostic.
	EnvOverlay map[string]string

	// SettingsFile, when non-empty, points the Claude CLI at a naozhi-owned
	// settings file (RFC naozhi-owned-settings-v3): BuildArgs switches from
	// `--setting-sources user` to `--setting-sources "" --settings <file>`,
	// isolating naozhi's config from the operator's interactive claude. Must be
	// absolute; non-absolute / leading-dash values are dropped. Claude-only.
	SettingsFile string

	// MCPConfigFile, when non-empty, is passed as `--mcp-config` (RFC
	// cli-mcp-config). Dedicated field because the flag is in deniedExtraFlags
	// (stdio servers = arbitrary command execution); the value comes from
	// config.yaml, never a prompt. Needed because `--setting-sources ""` drops
	// ~/.claude.json's mcpServers and on cc 2.1.235 this flag is the ONLY way in.
	// Must be absolute and not '-'-prefixed (dropped otherwise) and pre-validated
	// (exists, JSON, has `mcpServers`) — cc REFUSES TO START otherwise. Claude-only.
	MCPConfigFile string

	// SpawnOverlay is the per-request layer (agents[].model / .effort /
	// .extra_args, access profile) the router merged into Model/Effort/ExtraArgs.
	// BuildArgs never reads it; it is recorded in the shim state file so the
	// router can re-merge against current config on restart instead of misreading
	// agent overrides as arg-drift (#2494). Non-nil-but-empty means "known and empty".
	SpawnOverlay *shim.SpawnOverlay

	// AppendSystemPrompt, when non-empty, is rendered as `--append-system-prompt`
	// — the ONLY sanctioned channel for naozhi's own prompts (agents[].system_prompt,
	// planner, scratch context); the flag is in deniedExtraFlags so ExtraArgs cannot
	// smuggle it (#2493). Single string: layered prompts are joined with "\n\n", base
	// first (session.JoinSystemPrompts). Must not start with '-', contain NUL, or
	// exceed MaxAppendSystemPromptBytes; a failing value is dropped with a Warn,
	// never truncated — callers truncate at their own layer. Claude-only.
	AppendSystemPrompt string
}

// MaxAppendSystemPromptBytes caps SpawnOptions.AppendSystemPrompt: the worst
// case (32 KiB agent prompt + 24 KiB scratch context or 8 KiB planner) fits
// with ~2x headroom, below the 128 KiB maxExtraArgsBytes budget and ARG_MAX.
const MaxAppendSystemPromptBytes = 64 * 1024

// PermissionMode selects how a Claude-CLI spawn handles tool permissions.
type PermissionMode uint8

const (
	// PermissionModeDefault keeps --dangerously-skip-permissions on the argv —
	// the only mode compatible with the headless `-p` process model, where a
	// permission prompt would stall the turn. Zero value: no caller migration.
	PermissionModeDefault PermissionMode = 0
	// PermissionModeStandard omits the flag, deferring to the CLI's prompts;
	// for untrusted deployments that accept the stalled-turn cost.
	PermissionModeStandard PermissionMode = 1
)

// Wrapper manages spawning CLI processes via shim.
//
// ShimManager conflates protocol with transport; until a cli.Transport
// interface splits them, treat it as the *only* transport (nil disables
// Spawn). Wrapper is otherwise immutable after NewWrapper.
type Wrapper struct {
	BackendID   string // "claude" | "kiro" | future backends
	CLIPath     string
	CLIName     string // display name: "claude-code", "kiro"
	CLIVersion  string // semver from --version, e.g. "2.1.92" (spawn-time, immutable)
	Protocol    Protocol
	ShimManager *shim.Manager

	// liveVersion is the CLI version self-reported by a spawned process (system/
	// init claude_code_version). CLIVersion goes stale after a host claude upgrade
	// under a long-lived naozhi; EffectiveVersion prefers this once set. The one
	// mutable field on the Wrapper — lock-free via atomic.Pointer.
	liveVersion atomic.Pointer[string]

	// History factories are looked up by BackendID on every NewHistorySource call,
	// never cached here: caching would let a backend init() registration that
	// lands after NewWrapper silently no-op (tests re-register per-t.Run).
}

// ObserveLiveVersion records a CLI version self-reported by a spawned process.
// The Process side change-gates, so the store is rare. Lock-free.
func (w *Wrapper) ObserveLiveVersion(v string) {
	if v == "" {
		return
	}
	s := v
	w.liveVersion.Store(&s)
}

// EffectiveVersion returns the live version observed from a spawned process,
// falling back to the spawn-time CLIVersion (what the dashboard banner shows).
func (w *Wrapper) EffectiveVersion() string {
	if v := w.liveVersion.Load(); v != nil && *v != "" {
		return *v
	}
	return w.CLIVersion
}

// NewWrapper creates a Wrapper with the given CLI path and protocol (empty path
// auto-detects). It synchronously runs `<cli> --version` with a 5s timeout, so
// construction is blocking IO; callers that want SIGTERM to abort the probe
// should use NewWrapperLazy + Probe(ctx).
func NewWrapper(cliPath string, proto Protocol, backend string) *Wrapper {
	w := newWrapperCommon(cliPath, proto, backend)
	// Eager probe (blocks up to 5s); the legacy startup path logs CLIVersion right after.
	w.CLIVersion = detectVersion(w.CLIPath)
	return w
}

// NewWrapperLazy constructs the Wrapper without running `<cli> --version`;
// CLIVersion stays "" until Probe(ctx).
func NewWrapperLazy(cliPath string, proto Protocol, backend string) *Wrapper {
	return newWrapperCommon(cliPath, proto, backend)
}

// Manager returns the wrapper's transport (today *shim.Manager); prefer it over
// the ShimManager field, which goes unexported once cli.Transport lands (#721).
// Nil-safe on a nil receiver or an unset manager.
func (w *Wrapper) Manager() *shim.Manager {
	if w == nil {
		return nil
	}
	return w.ShimManager
}

// WithManager injects the transport and returns the receiver for fluent
// construction. It is the forward-compatible write path (#405): when the field
// goes unexported behind cli.Transport (#721) only this body changes. Nil-safe.
func (w *Wrapper) WithManager(m *shim.Manager) *Wrapper {
	if w == nil {
		return nil
	}
	w.ShimManager = m
	return w
}

// Probe runs `<cli> --version` under the caller's context (still capped at 5s
// internally) and stores the parsed result in w.CLIVersion, also returning it.
// Safe to call repeatedly; intended for NewWrapperLazy callers once a stopCtx
// is available.
func (w *Wrapper) Probe(ctx context.Context) string {
	if w == nil || w.CLIPath == "" {
		return ""
	}
	v := detectVersionCtx(ctx, w.CLIPath)
	w.CLIVersion = v
	return v
}

// newWrapperCommon is the shared constructor body for NewWrapper and NewWrapperLazy.
func newWrapperCommon(cliPath string, proto Protocol, backend string) *Wrapper {
	if cliPath == "" {
		cliPath = detectCLI(backend)
	}
	cliPath = osutil.ExpandHome(cliPath)
	// Defense-in-depth argv hygiene (cliPath goes straight to exec.Command).
	// Warn-only: construction must succeed with a not-yet-installed CLI or a
	// test-fixture path; enforceCLIPathSafe at spawn is the hard gate.
	validateCLIPath(cliPath)
	id := normalizeBackendID(backend)
	// Warn on unrecognised ids (a typo like "claud" would otherwise only fail at
	// spawn); warn-only so test fixtures / experimental backends still construct.
	if !isKnownBackendID(id) {
		slog.Warn("cli: unknown backend id, may fail at spawn",
			"backend", osutil.SanitizeForLog(id, 64),
			"raw", osutil.SanitizeForLog(backend, 64))
	}
	return &Wrapper{
		BackendID: id,
		CLIPath:   cliPath,
		// Canonical id so "Kiro" / "KIRO" resolve to the same display name.
		CLIName:  backendDisplayName(id),
		Protocol: proto,
	}
}

// isKnownBackendID reports whether id is in detect.go's knownBackends table.
func isKnownBackendID(id string) bool {
	_, ok := lookupBackend(id)
	return ok
}

// backendDisplayName maps a backend config value to its user-facing name via
// detect.go's knownBackends, not a parallel switch that drifts (#907). Input is
// normalized first; unknown ids fall back to the normalized value.
func backendDisplayName(backend string) string {
	id := normalizeBackendID(backend)
	if b, ok := lookupBackend(id); ok {
		return b.DisplayName
	}
	return id
}

// normalizeBackendID collapses empty/legacy aliases to "claude" and case-folds
// so "Claude" / "KIRO" hit the canonical key; otherwise the backend lookup fails
// silently behind a post-normalisation log line that looks correct.
func normalizeBackendID(backend string) string {
	backend = strings.ToLower(strings.TrimSpace(backend))
	switch backend {
	case "", "claude":
		return "claude"
	default:
		return backend
	}
}

// validateCLIPath Warns when cliPath fails argv hygiene (non-absolute, or
// non-regular / non-executable when the file exists). Empty path and ENOENT are
// NOT warnings: the CLI may not be installed yet and the spawn-time error covers
// that. Warn-only because NewWrapper runs at startup before any IM request can
// arrive and failing would block test fixtures / uninstalled deployments.
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
		// ENOENT is fine (not installed yet); other errors — symlink loops,
		// permission denied — are real surface to operators.
		if !os.IsNotExist(err) {
			slog.Warn("cli: cliPath Lstat failed",
				"path", osutil.SanitizeForLog(cliPath, 256),
				"err", err)
		}
		return
	}
	mode := fi.Mode()
	// Regular file or symlink (resolved at exec time). FIFOs, devices, sockets
	// are argv-injection vectors when the kernel re-interprets the file type.
	if !mode.IsRegular() && mode&os.ModeSymlink == 0 {
		slog.Warn("cli: cliPath is not a regular file or symlink",
			"path", osutil.SanitizeForLog(cliPath, 256),
			"mode", mode.String())
		return
	}
	// Executable bit on user/group/other. Symlinks don't carry it reliably, so
	// skip them — exec.Command surfaces a non-exec target.
	if mode.IsRegular() && mode.Perm()&0o111 == 0 {
		slog.Warn("cli: cliPath has no executable bit set",
			"path", osutil.SanitizeForLog(cliPath, 256),
			"mode", mode.String())
	}
}

// enforceCLIPathSafe is the spawn-time hard gate behind validateCLIPath: it
// errors iff cliPath is non-absolute or a dangerous file type (FIFO, device,
// socket, directory). By now an attacker-controllable IM session key has
// triggered the spawn, and a relative name would be re-resolved against the
// live PATH / CWD by exec.Command — a PATH-poisoning vector. Empty path and
// ENOENT pass through; the downstream shim spawn surfaces those.
func enforceCLIPathSafe(cliPath string) error {
	if cliPath == "" {
		return nil
	}
	if !filepath.IsAbs(cliPath) {
		return fmt.Errorf("cliPath must be absolute, got %q (PATH-injection guard)", cliPath)
	}
	fi, err := os.Lstat(cliPath)
	if err != nil {
		// ENOENT / EACCES: let the shim spawn surface the diagnostic message.
		return nil
	}
	mode := fi.Mode()
	// Regular file is canonical. Symlinks pass through — resolved at exec time,
	// and if the target is a FIFO/device the kernel refuses to exec it, which is
	// the strongest defense short of re-implementing symlink-walk semantics.
	if mode.IsRegular() || mode&os.ModeSymlink != 0 {
		return nil
	}
	// FIFO / char / block / socket / directory would make exec.Command fail late
	// (FIFO blocks on open) or silently succeed with file-type confusion.
	return fmt.Errorf("not a regular file or symlink (mode=%s)", mode.String())
}

// detectVersion runs "<cli> --version" with a Background-derived 5s timeout.
// Production MUST use NewWrapperLazy + Probe(ctx) so SIGTERM aborts the probe;
// this survives only for in-package tests without a shutdown ctx (#803).
func detectVersion(cliPath string) string {
	return detectVersionCtx(context.Background(), cliPath)
}

// detectVersionCtx is the context-aware detectVersion: the caller's ctx is
// chained with a hard 5s subprocess timeout, whichever fires first.
func detectVersionCtx(parent context.Context, cliPath string) string {
	// Already-cancelled ctx: don't fork a subprocess just to SIGKILL it.
	if err := parent.Err(); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cliPath, "--version")
	// Anchor CWD to "/" so a relative binary name cannot resolve to a file in
	// naozhi's working directory.
	cmd.Dir = "/"
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return parseVersionOutput(string(out))
}

// detectCLI finds the CLI binary via known install paths then PATH (binary
// name from knownBackends; unknown ids fall back to "claude"). When nothing
// resolves it returns "" rather than the bare basename: a bare name would be
// re-resolved through the live PATH at exec.Command time — a PATH-poisoning
// vector (#920) — whereas "" makes exec.Command fail clearly at spawn.
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

	// npm-global backends (codex; gemini-cli) live in node version-manager bin
	// dirs that a bare systemd unit's $PATH often lacks, so exec.LookPath misses
	// them; probe those dirs directly (each os.Stat-guarded by the caller).
	for _, dir := range npmGlobalBinDirs(home) {
		paths = append(paths, filepath.Join(dir, name+ext))
	}

	return paths
}

// npmGlobalBinDirs returns dirs where an npm-global CLI launcher may live
// (may not exist; caller stats each). Env-derived dirs first, then static
// fallbacks; without NVM_BIN we glob every ~/.nvm/versions/node/*/bin.
func npmGlobalBinDirs(home string) []string {
	var dirs []string
	seen := map[string]bool{}
	add := func(d string) {
		if d == "" || seen[d] {
			return
		}
		seen[d] = true
		dirs = append(dirs, d)
	}

	// Exact, from the environment naozhi inherited.
	add(os.Getenv("NVM_BIN")) // nvm exports the active node's bin dir verbatim
	if p := os.Getenv("NPM_CONFIG_PREFIX"); p != "" {
		add(filepath.Join(p, "bin")) // user-set npm prefix (e.g. ~/.npm-global)
	}
	if p := os.Getenv("N_PREFIX"); p != "" { // tj/n version manager
		add(filepath.Join(p, "bin"))
	}
	if p := os.Getenv("VOLTA_HOME"); p != "" {
		add(filepath.Join(p, "bin"))
	}

	if home != "" {
		// Common user-level npm prefixes that don't export an env var.
		add(filepath.Join(home, ".npm-global", "bin"))
		add(filepath.Join(home, ".npm-packages", "bin"))
		add(filepath.Join(home, ".volta", "bin"))
		// nvm without NVM_BIN: every installed node version's bin (Glob errors → empty).
		if matches, _ := filepath.Glob(filepath.Join(home, ".nvm", "versions", "node", "*", "bin")); len(matches) > 0 {
			for _, m := range matches {
				add(m)
			}
		}
	}

	return dirs
}

// Spawn starts a new CLI process via shim and returns a connected Process.
func (w *Wrapper) Spawn(ctx context.Context, opts SpawnOptions) (*Process, error) {
	if w.ShimManager == nil {
		return nil, fmt.Errorf("shim manager not configured")
	}

	// NUL in cwd: the OS truncates at it, redirecting the working directory.
	if strings.ContainsRune(opts.WorkingDir, 0) {
		return nil, fmt.Errorf("cwd contains NUL byte")
	}

	// Spawn-time argv hygiene: validateCLIPath is warn-only, but an IM message is
	// now in flight, so refuse FIFO / device / socket / dir here. ENOENT and empty
	// path still pass — the shim spawn produces the more diagnostic error.
	if err := enforceCLIPathSafe(w.CLIPath); err != nil {
		return nil, fmt.Errorf("cli path unsafe: %w", err)
	}

	proto := w.Protocol.Clone()
	cliArgs := proto.BuildArgs(opts)

	cwd := opts.WorkingDir
	if cwd == "" {
		cwd = os.TempDir()
	}

	// Start shim → connect → auth → hello, with the wrapper-owned CLI path and
	// backend ID so the shim records its backend for post-restart reconnect routing.
	handle, err := w.ShimManager.StartShimWithBackend(ctx, opts.Key, w.CLIPath, w.BackendID, cliArgs, cwd, opts.EnvOverlay, opts.SpawnOverlay)
	if err != nil {
		return nil, fmt.Errorf("start shim: %w", err)
	}

	// Drain replay messages (for fresh shim this is empty)
	_, err = handle.DrainReplay()
	if err != nil {
		handle.Close()
		return nil, fmt.Errorf("drain replay: %w", err)
	}

	// Warn on protocol_version mismatch (hot-upgrade may mix an older shim with a
	// newer naozhi; silent mismatch shows up as cryptic framing bugs). Not fatal:
	// older shims emit 0, so refusing would break every forward migration.
	if hv := handle.Hello.ProtocolVersion; hv != shim.ProtocolVersion {
		slog.Warn("shim protocol_version mismatch",
			"shim", hv,
			"naozhi", shim.ProtocolVersion,
			"key", opts.Key,
		)
		// Error (still non-fatal) when outside [MinSupportedProtocolVersion,
		// ProtocolVersion] so a rolling-deploy mismatch is loud in journalctl; v0
		// ("field absent") is the pre-v1 bootstrap shape and must keep attaching.
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
	// Spawn-time model for the dashboard header; opts.Model is resolved at
	// SpawnOptions assembly (cli.backends[].model, then cli.model), "" = unset.
	proc.setModel(opts.Model)
	// `--effort` is a spawn pin, so argv is the truth for the process. Seeded
	// before readLoop so a kiro metadata report can only overwrite, never race.
	proc.seedEffort(opts.Effort)
	// Push the self-reported binary version (init frame) up for the global banner.
	proc.SetOnLiveVersion(w.ObserveLiveVersion)

	// Protocol init handshake (stream-json: no-op; ACP: initialize + session/new)
	rw := &JSONRW{
		W: proc.shimStdinWriter(),
		R: &shimLineReader{proc: proc},
	}
	sessionID, err := proto.Init(rw, opts.ResumeID, opts.WorkingDir)
	if err != nil {
		// proc.Kill() signals the shim but does NOT close the net connection owned by
		// handle (readLoop hasn't started, so nobody notices killCh). Close it so the
		// socket fd is freed now instead of at the shim's idle timeout (default 4h).
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
	// Counts fresh CLI spawns only (SpawnReconnect forks no CLI child).
	// RecordCLISpawn double-writes the legacy unlabeled counter and the
	// per-backend vector so existing pprof.md jq queries keep working (RFC §10).
	metrics.RecordCLISpawn(w.BackendID)
	return proc, nil
}

// SpawnReconnect creates a Process by reconnecting to an existing shim after a naozhi restart.
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
	// cwd is unavailable on reconnect (the shim owns it); SessionRouter supplies it
	// via SetCwdForLinker. Until then Resolve bails out and the dashboard shows a tombstone.
	proc.InitLinker("")
	// Mirror the fresh-spawn wiring so a re-attached process also feeds its live version.
	proc.SetOnLiveVersion(w.ObserveLiveVersion)

	if handle.Hello.SessionID != "" {
		proc.sessionID = handle.Hello.SessionID
	}

	// Mid-turn detection: a last replayed event that is not a result means the CLI
	// is still processing → StateRunning, with reconnectedMidTurn letting readLoop's
	// stray-result handler flip back to Ready. Arm BEFORE startReadLoop, else the
	// result could be consumed first and park the session in StateRunning (#1778).
	if isMidTurn(replays, proto) {
		proc.mu.Lock()
		proc.state = StateRunning
		proc.mu.Unlock()
		proc.reconnectedMidTurn.Store(true)
	}

	proc.startReadLoop()

	return proc, replays, nil
}

// WaitSocketGoneForKey blocks until the shim socket for the session key
// disappears or maxWait elapses; true when gone, false on timeout, true
// immediately for an empty key. Wraps shim.SocketPath / KeyHash /
// WaitSocketGone so session need not import internal/shim (#711). Reset paths
// use it so the previous shim has released its socket before a fresh
// StartShim binds the same path; otherwise shim/server.go's dial-first
// "refusing to clobber" guard rejects the bind and the reset stalls.
func WaitSocketGoneForKey(key string, maxWait time.Duration) bool {
	if key == "" {
		return true
	}
	socketPath := shim.SocketPath(shim.KeyHash(key))
	return shim.WaitSocketGone(socketPath, maxWait)
}

// isMidTurn reports whether the CLI was mid-turn at reconnection time: the
// last meaningful replayed event is not a turn-complete result.
func isMidTurn(replays []shim.ServerMsg, proto Protocol) bool {
	lastType := ""
	for i := len(replays) - 1; i >= 0; i-- {
		if replays[i].Type != "replay" {
			continue
		}
		// done is intentionally discarded: turn-end is read off the emitted
		// events' Type below, not the advisory bool (#2303).
		events, _, err := proto.ReadEvent(replays[i].Line)
		if err != nil || len(events) == 0 {
			continue
		}
		// Reverse walk so the last semantic event in the frame wins (ACP turn-end
		// emits assistant+result; only the result settles the question).
		picked := ""
		for j := len(events) - 1; j >= 0; j-- {
			if events[j].Type != "" && !isTurnNeutralEventType(events[j].Type) {
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
	return lastType != "" && lastType != "result"
}

// isTurnNeutralEventType reports whether an Event type carries no turn state
// and must be skipped by isMidTurn's reverse walk. control_ack (receipt for a
// naozhi-originated control RPC, see ModelSetter) is emitted with or without a
// turn in flight — an idle session that switched models leaves it as the LAST
// buffered shim line — so counting it would arm reconnectedMidTurn with no
// result coming and park the session in StateRunning forever after a restart.
func isTurnNeutralEventType(t string) bool {
	return t == "control_ack"
}

// shimLineReader adapts the shim connection to LineReader for the Init handshake.
type shimLineReader struct {
	proc *Process
}

// shimLineReaderMaxSkips caps the non-stdout frames the Init handshake swallows
// before bailing, so a buggy/hostile shim streaming stderr / pong forever cannot
// wedge proto.Init — LineReader has no ctx (#633), so this is the structural
// timeout. 4096 absorbs a realistic startup burst while bounding DoS surface.
const shimLineReaderMaxSkips = 4096

// ReadLine returns the next CLI stdout line from the shim transport, blocking
// until a stdout frame arrives or the shim signals CLI exit. Every frame is a
// JSON shimMsg envelope; `stdout` carries one CLI line (trailing \n stripped),
// `cli_exited` returns a descriptive terminal error, and other / unparseable
// frames are skipped, bounded by shimLineReaderMaxSkips. eof==true means no
// more lines will arrive and excludes non-nil data (LineReader contract).
func (r *shimLineReader) ReadLine() ([]byte, bool, error) {
	// LineReader has no ctx (widening it is breaking across both backends), so
	// the skip counter bounds the loop; overflow returns eof=true + error (#633).
	skipped := 0
	for {
		// Bound the per-line read: bufio.Reader.ReadBytes grows without limit, so a
		// giant newline-less line from a hostile shim could OOM the handshake.
		// Accumulate ReadSlice chunks under maxScannerBufBytes (#2183).
		var rawLine []byte
		for {
			chunk, err := r.proc.shimR.ReadSlice('\n')
			if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
				// A partial chunk on a terminal error is useless; the connection is going away.
				return nil, true, err
			}
			if len(rawLine)+len(chunk) > maxScannerBufBytes {
				return nil, true, fmt.Errorf("shim init line exceeds %d bytes", maxScannerBufBytes)
			}
			rawLine = append(rawLine, chunk...)
			if err == nil {
				break // terminator found
			}
			// ErrBufferFull: keep reading until newline or cap.
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
		// Skip other frame types (stderr, pong, ...) but bound the loop.
		skipped++
		if skipped > shimLineReaderMaxSkips {
			return nil, true, fmt.Errorf("shim sent %d non-stdout frames during init without stdout (last type=%q)", skipped, msg.Type)
		}
	}
}
