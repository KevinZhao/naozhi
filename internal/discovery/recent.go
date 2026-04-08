package discovery

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
// returning up to `limit` sessions modified within `maxAge`.
// If limit <= 0, all sessions within the time window are returned.
//
// Two layers of filtering:
//  1. Directory-level: skip encoded hidden paths ("--" pattern from "/." in original path),
//     which belong to automated tools like claude-mem observer.
//  2. Workspace resolution: skip directories that can't be mapped back to a real
//     directory on disk (session can't be resumed without the correct CWD).
//
// Sessions in excludeSessionIDs are always skipped.
func RecentSessions(claudeDir string, limit int, maxAge time.Duration, excludeSessionIDs map[string]bool) []RecentSession {
	if claudeDir == "" {
		return nil
	}
	projectsDir := filepath.Join(claudeDir, "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil
	}

	cutoff := time.Now().Add(-maxAge).UnixMilli()

	var all []RecentSession
	// jsonlPaths maps sessionID → JSONL file path for deferred prompt extraction.
	jsonlPaths := make(map[string]string)

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dirName := e.Name()

		// Layer 1: skip encoded hidden paths.
		if strings.Contains(dirName, "--") {
			continue
		}

		projDir := filepath.Join(projectsDir, dirName)
		workspace, idx := resolveWorkspaceWithIndex(projDir, dirName)

		// Layer 2: skip unresolvable workspaces.
		if workspace == "" {
			continue
		}

		// Try sessions-index.json first (has prompt/summary inline)
		if idx != nil {
			if sessions := recentFromParsedIndex(idx, projDir, workspace, excludeSessionIDs); len(sessions) > 0 {
				for _, rs := range sessions {
					if rs.LastActive < cutoff {
						continue
					}
					jsonlPaths[rs.SessionID] = filepath.Join(projDir, rs.SessionID+".jsonl")
					all = append(all, rs)
				}
				continue
			}
		}

		// Fallback: collect metadata only (no file reads for prompt yet)
		for _, rs := range recentFromJSONLFiles(projDir, workspace, excludeSessionIDs) {
			if rs.LastActive < cutoff {
				continue
			}
			jsonlPaths[rs.SessionID] = filepath.Join(projDir, rs.SessionID+".jsonl")
			all = append(all, rs)
		}
	}

	// Sort by last_active desc (most recent first)
	sort.Slice(all, func(i, j int) bool {
		return all[i].LastActive > all[j].LastActive
	})

	// Deferred prompt extraction: only read JSONL for sessions that will be returned.
	var result []RecentSession
	for i := range all {
		if limit > 0 && len(result) >= limit {
			break
		}
		path := jsonlPaths[all[i].SessionID]
		if all[i].LastPrompt == "" && all[i].Summary == "" && path != "" {
			all[i].LastPrompt = extractFirstPrompt(path)
		}
		result = append(result, all[i])
	}
	return result
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

// ---------------------------------------------------------------------------
// Workspace resolution
// ---------------------------------------------------------------------------

// resolveWorkspaceWithIndex determines the real filesystem path for a Claude
// project directory and optionally returns the parsed sessions index (if present).
// Reading the index once avoids double I/O for directories that have both
// originalPath and session entries.
func resolveWorkspaceWithIndex(projDir, dirName string) (string, *sessionsIndex) {
	data, err := os.ReadFile(filepath.Join(projDir, "sessions-index.json"))
	if err == nil {
		var idx sessionsIndex
		if json.Unmarshal(data, &idx) == nil {
			if idx.OriginalPath != "" {
				if info, err := os.Stat(idx.OriginalPath); err == nil && info.IsDir() {
					return idx.OriginalPath, &idx
				}
			}
			// Index exists but originalPath missing or stale — still use entries,
			// fall through to DFS for workspace.
			if ws := resolveWorkspaceByParts(dirName); ws != "" {
				return ws, &idx
			}
			return "", &idx
		}
	}
	return resolveWorkspaceByParts(dirName), nil
}

// recentFromParsedIndex extracts sessions from an already-parsed sessions index.
func recentFromParsedIndex(idx *sessionsIndex, projDir, workspace string, exclude map[string]bool) []RecentSession {
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

// resolveWorkspaceByParts reconstructs a workspace path from an encoded project
// directory name by testing segments against the filesystem.
//
// Claude Code encodes workspace paths by replacing "/" with "-", so
// "-home-ec2-user-workspace-foo" originated from "/home/ec2-user/workspace/foo".
// Since the encoding is lossy (directory names may contain literal hyphens), a
// simple reverse replacement fails for paths like "/home/ec2-user/..." where
// "ec2-user" contains a hyphen.
//
// The algorithm splits the encoded name by "-" and uses DFS: at each filesystem
// level, it tries consuming 1, 2, 3, ... consecutive parts as a single directory
// name, verifying each candidate with os.Stat. Invalid branches are pruned
// immediately, keeping the total stat calls manageable (~10-20 for typical paths).
func resolveWorkspaceByParts(dirName string) string {
	if dirName == "" || dirName[0] != '-' {
		return ""
	}
	parts := strings.Split(dirName[1:], "-") // skip leading "-"
	if len(parts) == 0 {
		return ""
	}
	return tryResolveParts(parts, "/")
}

// tryResolveParts recursively resolves path parts against the filesystem.
func tryResolveParts(parts []string, base string) string {
	if len(parts) == 0 {
		if info, err := os.Stat(base); err == nil && info.IsDir() {
			return base
		}
		return ""
	}
	for i := 1; i <= len(parts); i++ {
		segment := strings.Join(parts[:i], "-")
		if segment == "" || segment == "." || segment == ".." {
			continue
		}
		candidate := filepath.Join(base, segment)
		info, err := os.Stat(candidate)
		if err != nil || !info.IsDir() {
			continue
		}
		if result := tryResolveParts(parts[i:], candidate); result != "" {
			return result
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Naozhi-managed session detection
// ---------------------------------------------------------------------------

// isNaozhiManaged checks if a JSONL file belongs to a naozhi-managed session
// by reading its first line for a "queue-operation" type marker. Naozhi writes
// this envelope as the first line of every session it manages, which is distinct
// from user-initiated Claude Code sessions (whose first line is typically
// "file-history-snapshot", "permission-mode", or "user").
func isNaozhiManaged(jsonlPath string) bool {
	f, err := os.Open(jsonlPath)
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 4*1024), 16*1024)
	if !scanner.Scan() {
		return false
	}
	line := scanner.Bytes()
	// Fast pre-filter before JSON parse.
	if !bytes.Contains(line, []byte(`"queue-operation"`)) {
		return false
	}
	var envelope struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(line, &envelope) == nil && envelope.Type == "queue-operation"
}
