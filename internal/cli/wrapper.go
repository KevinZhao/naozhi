package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/naozhi/naozhi/internal/pathutil"
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
	CLIPath  string
	Protocol Protocol
}

// NewWrapper creates a Wrapper with the given CLI path and protocol.
// If path is empty, auto-detects from known install locations and PATH.
func NewWrapper(cliPath string, proto Protocol) *Wrapper {
	if cliPath == "" {
		cliPath = detectCLI(proto.Name())
	}
	cliPath = pathutil.ExpandHome(cliPath)
	return &Wrapper{CLIPath: cliPath, Protocol: proto}
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
	args := w.Protocol.BuildArgs(opts)

	cwd := opts.WorkingDir
	if cwd == "" {
		cwd = os.TempDir()
	}

	proc, err := newProcess(ctx, w.CLIPath, args, cwd, opts.NoOutputTimeout, opts.TotalTimeout, w.Protocol)
	if err != nil {
		return nil, fmt.Errorf("spawn agent: %w", err)
	}

	// Run protocol-specific initialization handshake before starting readLoop
	rw := &JSONRW{
		W: proc.stdin,
		R: &bufioLineReader{scanner: proc.scanner},
	}
	sessionID, err := w.Protocol.Init(rw, opts.ResumeID)
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
