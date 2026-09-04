package server

import (
	"os"
	"path/filepath"
)

// resolveClaudeDir returns the absolute path to the Claude config directory
// (~/.claude). Returns "" when os.UserHomeDir fails. Callers must handle the
// empty return: filepath.Join("", ".claude") would silently yield a relative
// path that escapes the intended root.
func resolveClaudeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// resolveClaudeProjectsDir returns the absolute path to ~/.claude/projects
// (Claude's per-workspace transcripts root). CLAUDE_PROJECTS_DIR overrides
// the default when set. Returns "" when the home probe fails and no override
// is configured.
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
