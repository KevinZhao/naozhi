package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/config"
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
type Wrapper struct {
	CLIPath     string
	CLIName     string // display name: "claude-code", "kiro"
	CLIVersion  string // semver from --version, e.g. "2.1.92"
	Protocol    Protocol
	ShimManager *shim.Manager
}

// NewWrapper creates a Wrapper with the given CLI path and protocol.
// If path is empty, auto-detects from known install locations and PATH.
func NewWrapper(cliPath string, proto Protocol, backend string) *Wrapper {
	if cliPath == "" {
		cliPath = detectCLI(backend)
	}
	cliPath = config.ExpandHome(cliPath)
	w := &Wrapper{
		CLIPath:  cliPath,
		CLIName:  backendDisplayName(backend),
		Protocol: proto,
	}
	w.CLIVersion = detectVersion(cliPath)
	return w
}

// backendDisplayName maps a backend config value to its user-facing name.
func backendDisplayName(backend string) string {
	switch backend {
	case "kiro":
		return "kiro"
	case "", "claude":
		return "claude-code"
	default:
		return backend
	}
}

// detectVersion runs "<cli> --version" and parses the version string.
func detectVersion(cliPath string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, cliPath, "--version").Output()
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(out))
	if i := strings.IndexByte(s, ' '); i > 0 {
		s = s[:i]
	}
	if len(s) > 32 {
		s = s[:32]
	}
	if len(s) > 0 && s[0] >= '0' && s[0] <= '9' {
		return s
	}
	return ""
}

// detectCLI finds the CLI binary by checking known install paths then PATH.
func detectCLI(backend string) string {
	name := "claude"
	if backend == "kiro" {
		name = "kiro-cli"
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
		return nil, fmt.Errorf("ShimManager not configured")
	}

	proto := w.Protocol.Clone()
	cliArgs := proto.BuildArgs(opts)

	cwd := opts.WorkingDir
	if cwd == "" {
		cwd = os.TempDir()
	}

	// Start shim → connect → auth → get hello
	handle, err := w.ShimManager.StartShim(ctx, opts.Key, cliArgs, cwd)
	if err != nil {
		return nil, fmt.Errorf("start shim: %w", err)
	}

	// Drain replay messages (for fresh shim this is empty)
	_, err = handle.DrainReplay()
	if err != nil {
		handle.Close()
		return nil, fmt.Errorf("drain replay: %w", err)
	}

	cliPID := 0
	if handle.Hello.CLIPID > 0 {
		cliPID = handle.Hello.CLIPID
	}

	proc := newShimProcess(
		handle.Conn, handle.Reader, handle.Writer,
		proto, cliPID,
		opts.NoOutputTimeout, opts.TotalTimeout,
	)

	// Protocol init handshake (stream-json: no-op; ACP: initialize + session/new)
	rw := &JSONRW{
		W: proc.shimStdinWriter(),
		R: &shimLineReader{proc: proc},
	}
	sessionID, err := proto.Init(rw, opts.ResumeID)
	if err != nil {
		proc.Kill()
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
	return proc, nil
}

// SpawnReconnect creates a Process by reconnecting to an existing shim.
// Used after naozhi restart to resume an active session.
func (w *Wrapper) SpawnReconnect(ctx context.Context, key string, lastSeq int64, proto Protocol) (*Process, []shim.ServerMsg, error) {
	if w.ShimManager == nil {
		return nil, nil, fmt.Errorf("ShimManager not configured")
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

	proc := newShimProcess(
		handle.Conn, handle.Reader, handle.Writer,
		proto.Clone(), cliPID,
		0, 0, // timeouts will be set by router
	)

	if handle.Hello.SessionID != "" {
		proc.SessionID = handle.Hello.SessionID
	}

	proc.startReadLoop()

	// Detect mid-turn: if the last replayed event is not a turn-complete marker,
	// the CLI is actively processing and state should be Running (not Ready).
	if isMidTurn(replays, proto) {
		proc.mu.Lock()
		proc.State = StateRunning
		proc.mu.Unlock()
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
		ev, _, err := proto.ReadEvent([]byte(replays[i].Line))
		if err != nil || ev.Type == "" {
			continue
		}
		lastType = ev.Type
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
			return nil, true, fmt.Errorf("CLI exited during init")
		}
		// Skip other message types (stderr, pong, etc.) during init
	}
}
