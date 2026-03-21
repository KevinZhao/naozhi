package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
// If path is empty, defaults to ~/.local/bin/claude.
func NewWrapper(cliPath string, proto Protocol) *Wrapper {
	if cliPath == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			cliPath = filepath.Join(home, ".local", "bin", "claude")
		} else {
			cliPath = "claude"
		}
	}
	cliPath = pathutil.ExpandHome(cliPath)
	return &Wrapper{CLIPath: cliPath, Protocol: proto}
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
