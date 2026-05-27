package server

import (
	"os"
	"path/filepath"
)

// resolveClaudeDir returns the absolute path to the Claude config directory
// (~/.claude). Returns "" when os.UserHomeDir fails — typical inside a
// sandboxed test that wipes $HOME. Callers must defensively handle the
// empty-string return; downstream filepath.Join of "" + ".claude" would
// silently produce a relative path that escapes the intended root.
//
// R222-ARCH-9 / #724: previously two sites in internal/server (Server.New
// and NewMemoryHandler) re-implemented the same probe; centralising keeps
// the "how does naozhi find ~/.claude" question single-sourced.
func resolveClaudeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// resolveClaudeProjectsDir returns the absolute path to ~/.claude/projects
// (Claude's per-workspace transcripts root). CLAUDE_PROJECTS_DIR overrides
// the default when set so deployments that pin transcripts under e.g.
// /var/lib/claude can swap the location without recompiling. Returns ""
// when the home probe fails and no override is configured.
//
// R222-ARCH-9 / #724.
func resolveClaudeProjectsDir() string {
	if v := os.Getenv("CLAUDE_PROJECTS_DIR"); v != "" {
		return v
	}
	dir := resolveClaudeDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "projects")
}
