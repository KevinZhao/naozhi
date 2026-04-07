package discovery

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/naozhi/naozhi/internal/cli"
)

// RecentSession represents a past Claude session found on the filesystem.
type RecentSession struct {
	SessionID  string `json:"session_id"`
	Summary    string `json:"summary,omitempty"`
	LastPrompt string `json:"last_prompt,omitempty"`
	LastActive int64  `json:"last_active"` // unix ms (JSONL mtime)
	Workspace  string `json:"workspace,omitempty"`
	Project    string `json:"project,omitempty"` // filled by server
}

// RecentSessions scans ~/.claude/projects/* for recent sessions,
// returning the top `limit` by modification time. Sessions in excludeSessionIDs are skipped.
// It first tries sessions-index.json; if absent, falls back to scanning .jsonl files directly.
//
// For the JSONL fallback path, prompt extraction is deferred until after the
// global sort+truncate so we only open files for sessions that will be returned.
func RecentSessions(claudeDir string, limit int, excludeSessionIDs map[string]bool) []RecentSession {
	if claudeDir == "" || limit <= 0 {
		return nil
	}
	projectsDir := filepath.Join(claudeDir, "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil
	}

	var all []RecentSession
	// jsonlPaths maps sessionID → JSONL file path for fallback entries,
	// so deferred prompt extraction can skip the full directory search.
	jsonlPaths := make(map[string]string)

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dirName := e.Name()
		projDir := filepath.Join(projectsDir, dirName)
		workspace := reverseProjDir(dirName)

		// Try sessions-index.json first (has prompt/summary inline)
		if sessions := recentFromIndex(projDir, workspace, excludeSessionIDs); len(sessions) > 0 {
			all = append(all, sessions...)
			continue
		}

		// Fallback: collect metadata only (no file reads for prompt yet)
		for _, rs := range recentFromJSONLFiles(projDir, workspace, excludeSessionIDs) {
			jsonlPaths[rs.SessionID] = filepath.Join(projDir, rs.SessionID+".jsonl")
			all = append(all, rs)
		}
	}

	// Sort by last_active desc (most recent first)
	sort.Slice(all, func(i, j int) bool {
		return all[i].LastActive > all[j].LastActive
	})

	if len(all) > limit {
		all = all[:limit]
	}

	// Deferred prompt extraction: only read JSONL heads for the top-N results
	// that came from the fallback path (no index → no prompt yet).
	for i := range all {
		if all[i].LastPrompt == "" && all[i].Summary == "" {
			if path := jsonlPaths[all[i].SessionID]; path != "" {
				all[i].LastPrompt = extractFirstPrompt(path)
			}
		}
	}

	return all
}

// recentFromIndex reads sessions-index.json and returns sessions found there.
func recentFromIndex(projDir, workspace string, exclude map[string]bool) []RecentSession {
	data, err := os.ReadFile(filepath.Join(projDir, "sessions-index.json"))
	if err != nil {
		return nil
	}
	var idx sessionsIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil
	}

	var out []RecentSession
	for _, entry := range idx.Entries {
		if entry.SessionID == "" || exclude[entry.SessionID] {
			continue
		}
		info, err := os.Stat(filepath.Join(projDir, entry.SessionID+".jsonl"))
		if err != nil {
			continue
		}
		prompt := entry.FirstPrompt
		if prompt == "" {
			prompt = entry.Summary
		}
		out = append(out, RecentSession{
			SessionID:  entry.SessionID,
			Summary:    entry.Summary,
			LastPrompt: cli.TruncateRunes(prompt, 120),
			LastActive: info.ModTime().UnixMilli(),
			Workspace:  workspace,
		})
	}
	return out
}

// recentFromJSONLFiles scans a project directory for .jsonl files and collects
// session metadata (ID, mtime, workspace). Prompt extraction is deferred to the
// caller to avoid reading files that won't make the top-N cut.
func recentFromJSONLFiles(projDir, workspace string, exclude map[string]bool) []RecentSession {
	dirEntries, err := os.ReadDir(projDir)
	if err != nil {
		return nil
	}

	var out []RecentSession
	for _, de := range dirEntries {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		sessionID := strings.TrimSuffix(name, ".jsonl")
		if !IsValidSessionID(sessionID) || exclude[sessionID] {
			continue
		}
		info, err := de.Info()
		if err != nil || info.Size() == 0 {
			continue
		}
		out = append(out, RecentSession{
			SessionID:  sessionID,
			LastActive: info.ModTime().UnixMilli(),
			Workspace:  workspace,
		})
	}
	return out
}

// extractFirstPrompt reads the first user message from a JSONL file.
// Only reads up to 64KB from the head to stay fast.
func extractFirstPrompt(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 16*1024), 64*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		// Fast pre-filter: skip lines that can't be user messages.
		// This avoids json.Unmarshal on every line. The subsequent Unmarshal
		// is the authoritative check; this just eliminates obvious non-matches.
		if len(line) == 0 || !bytes.Contains(line, []byte(`"type"`)) {
			continue
		}
		var hl struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
		}
		if json.Unmarshal(line, &hl) != nil || hl.Type != "user" {
			continue
		}
		text := extractUserText(hl.Message)
		if text != "" {
			return cli.TruncateRunes(text, 120)
		}
	}
	return ""
}

// reverseProjDir attempts to convert a project directory name back to a workspace path.
// e.g., "-home-user-workspace-foo" → "/home/user/workspace/foo"
//
// The encoding (projDirName) replaces "/" with "-", which is lossy when the original
// path contains hyphens. We stat the candidate to verify it is a real directory.
// Returns "" if the candidate path does not exist, preventing bogus workspace paths
// from being passed to the CLI or validateWorkspace.
func reverseProjDir(dirName string) string {
	candidate := strings.ReplaceAll(dirName, "-", "/")
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return candidate
	}
	return ""
}
