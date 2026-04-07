package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/config"
)

// SpawnOptions configures how a CLI process is spawned.
type SpawnOptions struct {
	Model           string
	ResumeID        string   // session ID to resume (empty = new session)
	ExtraArgs       []string // additional CLI args
	WorkingDir      string
	NoOutputTimeout time.Duration // kill process if no output for this long
	TotalTimeout    time.Duration // kill process if total turn exceeds this
}

// Wrapper manages spawning CLI processes.
type Wrapper struct {
	CLIPath    string
	CLIName    string // display name: "claude-code", "kiro"
	CLIVersion string // semver from --version, e.g. "2.1.92"
	Protocol   Protocol
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
	// Output is typically "2.1.92 (Claude Code)\n" or just "2.1.92\n"
	s := strings.TrimSpace(string(out))
	if i := strings.IndexByte(s, ' '); i > 0 {
		s = s[:i]
	}
	// Sanity check: must look like a version (digits and dots), cap length
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

	// Fallback: check PATH
	if p, err := exec.LookPath(name); err == nil {
		return p
	}

	// Last resort: bare name, let exec resolve at spawn time
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

	// Native installer (all platforms)
	paths = append(paths, filepath.Join(home, ".local", "bin", name+ext))

	switch runtime.GOOS {
	case "darwin":
		// Homebrew Apple Silicon
		paths = append(paths, filepath.Join("/opt/homebrew/bin", name))
		// Homebrew Intel
		paths = append(paths, filepath.Join("/usr/local/bin", name))
	case "linux":
		paths = append(paths, filepath.Join("/usr/local/bin", name))
	case "windows":
		// npm global (Windows)
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			paths = append(paths, filepath.Join(appdata, "npm", name+".cmd"))
		}
	}

	return paths
}

// Spawn starts a new long-lived CLI process.
func (w *Wrapper) Spawn(ctx context.Context, opts SpawnOptions) (*Process, error) {
	proto := w.Protocol.Clone()
	args := proto.BuildArgs(opts)

	cwd := opts.WorkingDir
	if cwd == "" {
		cwd = os.TempDir()
	}

	proc, err := newProcess(ctx, w.CLIPath, args, cwd, opts.NoOutputTimeout, opts.TotalTimeout, proto)
	if err != nil {
		return nil, fmt.Errorf("spawn agent: %w", err)
	}

	// Run protocol-specific initialization handshake before starting readLoop
	rw := &JSONRW{
		W: proc.stdin,
		R: &bufioLineReader{scanner: proc.scanner},
	}
	sessionID, err := proto.Init(rw, opts.ResumeID)
	if err != nil {
		proc.Kill()
		return nil, fmt.Errorf("protocol init: %w", err)
	}
	if sessionID != "" {
		proc.SessionID = sessionID
	}

	proc.startReadLoop()
	return proc, nil
}
