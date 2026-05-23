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
}

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

	// historyFactory produces backend-specific history.Source instances
	// for sessions whose Backend() matches this wrapper's BackendID.
	// Bound at NewWrapper time via the package-level registry so adding
	// a new backend's history reader is a single new init() call in the
	// backend package — not a session-package edit.
	//
	// nil is fine: NewHistorySource degrades to NoopHistorySource so
	// pre-registration spawns and unknown backends both behave
	// uniformly (the dashboard sees an empty disk tier rather than a
	// nil panic).
	historyFactory HistoryFactoryFn
}

// NewWrapper creates a Wrapper with the given CLI path and protocol.
// If path is empty, auto-detects from known install locations and PATH.
func NewWrapper(cliPath string, proto Protocol, backend string) *Wrapper {
	if cliPath == "" {
		cliPath = detectCLI(backend)
	}
	cliPath = osutil.ExpandHome(cliPath)
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
	w := &Wrapper{
		BackendID: id,
		CLIPath:   cliPath,
		// R228-ARCH-15: feed the canonical id (post-normalize) into
		// backendDisplayName so case variants like "Kiro" / "KIRO" hit
		// the same display branch as "kiro" instead of falling through
		// to the default arm and surfacing the raw operator-typed value.
		CLIName:  backendDisplayName(id),
		Protocol: proto,
	}
	w.CLIVersion = detectVersion(cliPath)
	// Bind the history-source factory for this backend, if one has been
	// registered (history backend packages register from their init()).
	// nil is OK: NewHistorySource handles missing registrations.
	w.historyFactory = pickHistoryFactory(w.BackendID)
	return w
}

// isKnownBackendID reports whether id is a backend the cli package knows
// how to drive. Mirrors the entries in detect.go's knownBackends — the
// single source of truth for "what backends this cli build supports".
// R225-CR-9.
func isKnownBackendID(id string) bool {
	for _, b := range knownBackends {
		if b.ID == id {
			return true
		}
	}
	return false
}

// displayNameByBackend mirrors defaultBinaryByBackend's pattern: a single
// sentinel table keyed by canonical backend ID returning the user-facing
// label rendered in dashboard chips and structured logs. Adding a new
// backend means adding one row here AND a Profile entry in cli/backend
// (whose tests cross-check DisplayName against the sentinel). The empty
// key ("") collapses to claude — matches normalizeBackendID's behaviour
// for legacy configs that omit cli.backend. R231-ARCH-10 anchor.
var displayNameByBackend = map[string]string{
	"":       "claude-code",
	"claude": "claude-code",
	"kiro":   "kiro",
}

// backendDisplayName maps a backend config value to its user-facing name.
//
// Unknown ids are returned verbatim so a typo in cli.backend surfaces in
// the log line / dashboard chip rather than getting silently rewritten —
// startup config validation rejects unknown ids in production paths
// (cmd/naozhi/main.go), so reaching the fallback implies a test fixture
// or operator override.
func backendDisplayName(backend string) string {
	if name, ok := displayNameByBackend[backend]; ok {
		return name
	}
	return backend
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

// detectVersion runs "<cli> --version" and parses the version string.
// Uses a Background-derived 5s timeout — fine for test / single-backend
// paths. Prefer detectVersionCtx when the caller has a shutdown context
// (e.g. naozhi startup) so SIGTERM during probe doesn't block for the
// full timeout.
func detectVersion(cliPath string) string {
	return detectVersionCtx(context.Background(), cliPath)
}

// detectVersionCtx is the context-aware variant of detectVersion.
// The caller's ctx is chained with a hard 5s subprocess timeout so a
// slow --version probe can never exceed the shorter of "caller's
// shutdown signal" and "5 seconds". R55-QUAL-004.
func detectVersionCtx(parent context.Context, cliPath string) string {
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

// defaultBinaryByBackend maps a backend ID to the bare binary name probed
// when `cli.path` is unset and exec.LookPath fails. Kept here (rather than
// reaching into internal/cli/backend.Profile.DefaultBinary) to avoid an
// import cycle: cli/backend imports cli, so cli must stay a leaf w.r.t.
// backend metadata. This is the single sentinel table that detectCLI and
// any future detect-helpers consult; adding a new backend means appending
// one row here AND a Profile entry in cli/backend — the cli/backend test
// suite cross-checks DefaultBinary, so a missed row here surfaces in CI
// rather than silently falling back to "claude". R225-CR-2 anchor.
var defaultBinaryByBackend = map[string]string{
	"claude": "claude",
	"kiro":   "kiro-cli",
}

// detectCLI finds the CLI binary by checking known install paths then PATH.
//
// Returns the bare binary name when neither candidatePaths nor exec.LookPath
// resolve a match, mirroring the historical "best effort" contract: callers
// (detectVersionCtx, Wrapper.Spawn) handle the ENOENT path themselves so
// detectCLI can stay synchronous and side-effect free.
func detectCLI(backend string) string {
	name, ok := defaultBinaryByBackend[backend]
	if !ok {
		// Unknown backend ID: fall back to the claude binary so a
		// stale config token does not strand the user with no CLI at
		// all. The startup config validator rejects unknown IDs in
		// production paths (cmd/naozhi/main.go), so reaching this
		// branch implies a test fixture or operator override.
		name = defaultBinaryByBackend["claude"]
	}

	for _, p := range candidatePaths(name) {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	if p, err := exec.LookPath(name); err == nil {
		return p
	}

	return name
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
		proc.SessionID = sessionID
	}

	// If shim already captured session_id from init event during startup
	if handle.Hello.SessionID != "" && proc.SessionID == "" {
		proc.SessionID = handle.Hello.SessionID
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
		proc.SessionID = handle.Hello.SessionID
	}

	proc.startReadLoop()

	// Detect mid-turn: if the last replayed event is not a turn-complete marker,
	// the CLI is actively processing and state should be Running (not Ready).
	// Also arm reconnectedMidTurn so readLoop's stray-result handler will
	// transition State back to Ready when the CLI finishes without anyone
	// calling Send() on this reattached process.
	if isMidTurn(replays, proto) {
		proc.mu.Lock()
		proc.State = StateRunning
		proc.mu.Unlock()
		proc.reconnectedMidTurn.Store(true)
	}

	return proc, replays, nil
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

func (r *shimLineReader) ReadLine() ([]byte, bool, error) {
	// During Init, we need to read lines that come through the shim stdout wrapper.
	// The shim sends {"type":"stdout","line":"..."} — we need to unwrap.
	for {
		rawLine, err := r.proc.shimR.ReadBytes('\n')
		if err != nil {
			return nil, true, err
		}
		var msg shimMsg
		if json.Unmarshal(rawLine, &msg) != nil {
			continue
		}
		if msg.Type == "stdout" {
			return []byte(msg.Line), false, nil
		}
		if msg.Type == "cli_exited" {
			return nil, true, fmt.Errorf("cli exited during init")
		}
		// Skip other message types (stderr, pong, etc.) during init
	}
}
