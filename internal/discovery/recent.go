package discovery

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/textutil"
)

// RecentSession represents a past Claude session found on the filesystem.
type RecentSession struct {
	SessionID  string `json:"session_id"`
	Summary    string `json:"summary,omitempty"`
	LastPrompt string `json:"last_prompt,omitempty"`
	LastActive int64  `json:"last_active"` // unix ms (JSONL mtime)
	// RetiredAt is the unix ms instant the session left the live sidebar
	// (Router.Reset / Router.Remove). Filled by SessionHandlers from the
	// RetiredStore when present; zero means "never observed retiring under
	// the current naozhi process generation, fall back to LastActive".
	// The dashboard sorts the history popover by RetiredAt || LastActive
	// so the most recently closed session lands on top regardless of when
	// its JSONL was last appended.
	RetiredAt int64  `json:"retired_at,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Project   string `json:"project,omitempty"` // filled by server
}

// RecentSessionsFilter is the consumer-facing hook RecentSessions calls
// before returning a session.  Both methods are best-effort:  an
// implementation that returns false for everything degrades to the
// pre-filter behaviour, never blocks the scan, and never panics.
//
// Implementations MUST be safe for concurrent reads (RecentSessions may
// be called from multiple goroutines via the dashboard 1Hz poll).
// Construction of the filter (e.g. snapshotting Scheduler.KnownSessionIDs)
// should happen outside the hot loop — RecentSessions calls these
// methods O(N) times per scan.
type RecentSessionsFilter interface {
	// SkipWorkspace reports whether all sessions under the given
	// resolved workspace path should be hidden.  Used to hide
	// naozhi-internal subsystem workdirs (e.g. sys-sessions).
	// workspace is the absolute filesystem path returned by
	// resolveWorkspaceWithIndex / resolveWorkspaceByParts; an empty
	// string is never passed.
	SkipWorkspace(workspace string) bool
	// SkipSessionID reports whether the specific Claude session
	// (identified by its UUID-style sessionID) should be hidden.
	// Used to hide cron-spawned sessions which share their workspace
	// with regular user sessions and so cannot be filtered by path.
	SkipSessionID(sessionID string) bool
}

// noopRecentFilter is the stand-in used when callers pass nil — keeps
// the scan loop branch-free without per-call nil checks.
type noopRecentFilter struct{}

func (noopRecentFilter) SkipWorkspace(string) bool { return false }
func (noopRecentFilter) SkipSessionID(string) bool { return false }

// RecentSessions scans ~/.claude/projects/* for recent sessions,
// returning up to `limit` sessions modified within `maxAge`.
// If limit <= 0, all sessions within the time window are returned.
//
// Filtering layers (in order):
//  1. Directory-level: skip encoded hidden paths ("--" pattern from "/." in original path),
//     which belong to automated tools like claude-mem observer.
//  2. Workspace resolution: skip directories that can't be mapped back to a real
//     directory on disk (session can't be resumed without the correct CWD).
//  3. filter.SkipWorkspace: caller-supplied workspace blacklist (e.g. sys-sessions).
//  4. excludeSessionIDs / filter.SkipSessionID: per-session-ID filtering.
//
// filter may be nil; nil is equivalent to passing a no-op filter.
// Sessions in excludeSessionIDs are always skipped (legacy parameter
// kept for source-compat with discovery callers; new callers should
// prefer filter.SkipSessionID for richer semantics).
func RecentSessions(claudeDir string, limit int, maxAge time.Duration, excludeSessionIDs map[string]bool, filter RecentSessionsFilter) []RecentSession {
	if claudeDir == "" {
		return nil
	}
	if filter == nil {
		filter = noopRecentFilter{}
	}
	projectsDir := filepath.Join(claudeDir, "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil
	}

	cutoff := time.Now().Add(-maxAge).UnixMilli()

	// R247-PERF-16 (#561): preallocate based on directory count. Each
	// project directory typically yields 1-5 sessions in the 7-day window;
	// growing from nil through 1→2→4→8→16→32 doublings on a many-project
	// dev box re-allocs the backing array 5+ times before steady state.
	// The cap hint (one slot per project dir, growable when a project
	// happens to have many sessions in window) eliminates the early
	// doublings without over-committing — a project with zero matches
	// just leaves slots unused, well-bounded by the entry count.
	all := make([]RecentSession, 0, len(entries))
	jsonlPaths := make(map[string]string, len(entries))

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

		// Layer 3: caller-supplied workspace blacklist.  Skip the entire
		// directory (no per-file Stat) — sys-sessions JSONLs would
		// otherwise leak AutoTitler prompt fragments into the user
		// history panel.
		if filter.SkipWorkspace(workspace) {
			continue
		}

		// Try sessions-index.json first (has prompt/summary inline)
		if idx != nil {
			if sessions := recentFromParsedIndex(idx, projDir, workspace, excludeSessionIDs); len(sessions) > 0 {
				for _, rs := range sessions {
					if rs.LastActive < cutoff {
						continue
					}
					if filter.SkipSessionID(rs.SessionID) {
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
			if filter.SkipSessionID(rs.SessionID) {
				continue
			}
			jsonlPaths[rs.SessionID] = filepath.Join(projDir, rs.SessionID+".jsonl")
			all = append(all, rs)
		}
	}

	// Sort by last_active desc (most recent first).
	slices.SortFunc(all, func(a, b RecentSession) int {
		return cmp.Compare(b.LastActive, a.LastActive)
	})

	// Deferred prompt extraction: only read JSONL for sessions that will
	// be returned. Result is bounded by min(limit, len(all)); preallocate
	// to that exact upper bound to skip the post-doubling churn on
	// dashboard polls hitting `limit=50` against a many-session dataset.
	resCap := len(all)
	if limit > 0 && limit < resCap {
		resCap = limit
	}
	result := make([]RecentSession, 0, resCap)
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

// ---------------------------------------------------------------------------
// Directory scan cache
// ---------------------------------------------------------------------------

// jsonlFileInfo holds cached metadata for a single .jsonl file.
type jsonlFileInfo struct {
	sessionID string
	mtime     int64 // unix ms
}

// dirFilesCacheEntry stores cached file metadata for a project directory.
//
// R247-PERF-19: byID is a derived sessionID→mtime map built once at cache
// fill time so recentFromParsedIndex (called per dashboard sidebar refresh)
// no longer rebuilds it on every call. Map and slice share the same dirMtime
// invalidation lifetime; populating both up front trades O(N) extra memory
// (where N = .jsonl count, typically ≤ a few dozen per workspace) for one
// allocation amortised over many sidebar reads.
type dirFilesCacheEntry struct {
	dirMtime int64 // directory mtime in UnixNano (changes on file add/remove)
	files    []jsonlFileInfo
	byID     map[string]int64 // sessionID → mtime; nil iff len(files)==0
}

// dirFilesCache caches per-directory .jsonl file metadata. Cache entries are
// invalidated when the directory mtime changes (i.e. files are added or removed).
// Individual file mtime changes (content appended) do NOT invalidate the cache,
// which is acceptable for the 7-day history sidebar.
var dirFilesCache sync.Map // projDir → *dirFilesCacheEntry

// cachedJSONLFileInfo returns .jsonl file metadata for a project directory,
// using a cache validated by the directory's own mtime.
func cachedJSONLFileInfo(projDir string) []jsonlFileInfo {
	if entry := loadCachedDirEntry(projDir); entry != nil {
		return entry.files
	}
	return nil
}

// cachedJSONLByID returns the sessionID→mtime map for a project directory,
// reusing the same mtime-validated cache as cachedJSONLFileInfo. The returned
// map is read-only; callers must not mutate it. R247-PERF-19.
func cachedJSONLByID(projDir string) map[string]int64 {
	if entry := loadCachedDirEntry(projDir); entry != nil {
		return entry.byID
	}
	return nil
}

// loadCachedDirEntry returns the cached entry for projDir, refilling the cache
// on a miss / stale dirMtime. Centralised so cachedJSONLFileInfo and
// cachedJSONLByID share one scan + one cache slot per directory state.
func loadCachedDirEntry(projDir string) *dirFilesCacheEntry {
	dirInfo, err := os.Stat(projDir)
	if err != nil {
		return nil
	}
	dirMtime := dirInfo.ModTime().UnixNano()

	if v, ok := dirFilesCache.Load(projDir); ok {
		if entry := v.(*dirFilesCacheEntry); entry.dirMtime == dirMtime {
			return entry
		}
	}

	// Cache miss or stale — full scan.
	dirEntries, err := os.ReadDir(projDir)
	if err != nil {
		return nil
	}
	var files []jsonlFileInfo
	for _, de := range dirEntries {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		info, err := de.Info()
		if err != nil || info.Size() == 0 {
			continue
		}
		files = append(files, jsonlFileInfo{
			sessionID: strings.TrimSuffix(name, ".jsonl"),
			mtime:     info.ModTime().UnixMilli(),
		})
	}

	var byID map[string]int64
	if len(files) > 0 {
		byID = make(map[string]int64, len(files))
		for _, f := range files {
			byID[f.sessionID] = f.mtime
		}
	}
	entry := &dirFilesCacheEntry{dirMtime: dirMtime, files: files, byID: byID}
	dirFilesCache.Store(projDir, entry)
	return entry
}

// recentFromJSONLFiles scans a project directory for .jsonl files and collects
// session metadata (ID, mtime, workspace). Prompt extraction is deferred to the
// caller to avoid reading files that won't make the top-N cut.
func recentFromJSONLFiles(projDir, workspace string, exclude map[string]bool) []RecentSession {
	files := cachedJSONLFileInfo(projDir)
	var out []RecentSession
	for _, f := range files {
		if !IsValidSessionID(f.sessionID) || exclude[f.sessionID] {
			continue
		}
		out = append(out, RecentSession{
			SessionID:  f.sessionID,
			LastActive: f.mtime,
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
			return SanitizePromptForTransport(textutil.TruncateRunes(text, 120))
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
// Uses cached file metadata and O(1) map lookups per index entry.
//
// R247-PERF-19: the sessionID→mtime map is now cached inside the directory
// cache entry, so repeated sidebar refreshes (no .jsonl add/remove between
// them) reuse the same map allocation instead of rebuilding it per call.
func recentFromParsedIndex(idx *sessionsIndex, projDir, workspace string, exclude map[string]bool) []RecentSession {
	jsonlMtimes := cachedJSONLByID(projDir)

	var out []RecentSession
	for _, entry := range idx.Entries {
		if entry.SessionID == "" || exclude[entry.SessionID] {
			continue
		}
		mtime, ok := jsonlMtimes[entry.SessionID]
		if !ok {
			continue
		}
		prompt := entry.FirstPrompt
		if prompt == "" {
			prompt = entry.Summary
		}
		out = append(out, RecentSession{
			SessionID:  entry.SessionID,
			Summary:    SanitizePromptForTransport(entry.Summary),
			LastPrompt: SanitizePromptForTransport(textutil.TruncateRunes(prompt, 120)),
			LastActive: mtime,
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
// dfsPathCache permanently caches the result of resolveWorkspaceByParts.
// Encoded directory names never change, so the mapping is stable.
var dfsPathCache sync.Map // encoded dirName → resolved workspace path

func resolveWorkspaceByParts(dirName string) string {
	if v, ok := dfsPathCache.Load(dirName); ok {
		return v.(string)
	}
	if dirName == "" || dirName[0] != '-' {
		return ""
	}
	parts := strings.Split(dirName[1:], "-") // skip leading "-"
	if len(parts) == 0 {
		return ""
	}
	statCount := 0
	result := tryResolveParts(parts, "/", &statCount)
	dfsPathCache.Store(dirName, result)
	return result
}

// tryResolveParts recursively resolves path parts against the filesystem.
// statCount tracks total os.Stat calls to prevent exponential blowup on
// paths with many hyphens (e.g. 20+ segments → 2^19 worst case without limit).
func tryResolveParts(parts []string, base string, statCount *int) string {
	if len(parts) == 0 {
		if info, err := os.Stat(base); err == nil && info.IsDir() {
			return base
		}
		return ""
	}
	for i := 1; i <= len(parts); i++ {
		if *statCount > 200 {
			return ""
		}
		segment := strings.Join(parts[:i], "-")
		if segment == "" || segment == "." || segment == ".." {
			continue
		}
		candidate := filepath.Join(base, segment)
		*statCount++
		info, err := os.Stat(candidate)
		if err != nil || !info.IsDir() {
			continue
		}
		if result := tryResolveParts(parts[i:], candidate, statCount); result != "" {
			return result
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Naozhi-managed session detection
// ---------------------------------------------------------------------------
