package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SpawnOptions configures how a claude CLI process is spawned.
type SpawnOptions struct {
	Model           string
	ResumeID        string   // session ID to resume (empty = new session)
	ExtraArgs       []string // additional CLI args
	WorkingDir      string
	NoOutputTimeout time.Duration // kill process if no output for this long
	TotalTimeout    time.Duration // kill process if total turn exceeds this
}

// Wrapper manages spawning claude CLI processes.
type Wrapper struct {
	CLIPath string
}

// NewWrapper creates a Wrapper with the given CLI path.
// If path is empty, defaults to ~/.local/bin/claude.
func NewWrapper(cliPath string) *Wrapper {
	if cliPath == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			cliPath = filepath.Join(home, ".local", "bin", "claude")
		} else {
			cliPath = "claude" // fallback to PATH lookup
		}
	}
	// Expand ~ prefix
	if strings.HasPrefix(cliPath, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			cliPath = filepath.Join(home, cliPath[2:])
		}
	}
	return &Wrapper{CLIPath: cliPath}
}

// Spawn starts a new long-lived claude CLI process.
func (w *Wrapper) Spawn(ctx context.Context, opts SpawnOptions) (*Process, error) {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--setting-sources", "",
		"--dangerously-skip-permissions",
	}

	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}

	if opts.ResumeID != "" {
		args = append(args, "--resume", opts.ResumeID)
	}

	args = append(args, opts.ExtraArgs...)

	cwd := opts.WorkingDir
	if cwd == "" {
		cwd = os.TempDir()
	}

	proc, err := newProcess(ctx, w.CLIPath, args, cwd, opts.NoOutputTimeout, opts.TotalTimeout)
	if err != nil {
		return nil, fmt.Errorf("spawn claude: %w", err)
	}

	return proc, nil
}
