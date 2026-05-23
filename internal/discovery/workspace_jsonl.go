package discovery

import (
	"path/filepath"
)

// WorkspaceJSONL is one .jsonl file under ~/.claude/projects/<slug>/.
// Re-exports the package-internal jsonlFileInfo as a stable public type
// so callers in internal/session can consume the (id, mtime) pair without
// reaching into discovery internals.
type WorkspaceJSONL struct {
	SessionID string
	Mtime     int64
}

// ListWorkspaceJSONL enumerates .jsonl files for a workspace's Claude
// project directory (`<claudeDir>/projects/<slug>/`). Backed by the
// existing dirFilesCache so repeated calls during startup or per-spawn
// cost approximately one Stat each after the first ReadDir.
//
// Returns nil for empty claudeDir / workspace, when the project
// directory does not exist, or when no eligible JSONL is present.
// Filters: must end in .jsonl, size > 0, IsValidSessionID(name).
//
// Used by the auto-workspace-chain feature (docs/rfc/auto-workspace-chain.md
// §4.2) so the scan + cache logic stays in this package; session has no
// business reaching into ReadDir + dirFilesCache directly.
func ListWorkspaceJSONL(claudeDir, workspace string) []WorkspaceJSONL {
	if claudeDir == "" || workspace == "" {
		return nil
	}
	projDir := filepath.Join(claudeDir, "projects", projDirName(workspace))
	files := cachedJSONLFileInfo(projDir)
	if len(files) == 0 {
		return nil
	}
	out := make([]WorkspaceJSONL, 0, len(files))
	for _, f := range files {
		if !IsValidSessionID(f.sessionID) {
			continue
		}
		out = append(out, WorkspaceJSONL{
			SessionID: f.sessionID,
			Mtime:     f.mtime,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
