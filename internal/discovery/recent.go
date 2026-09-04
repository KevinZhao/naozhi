package discovery

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"log/slog"
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
	// (Router.Reset / Router.Remove), filled from the RetiredStore; zero means
	// "fall back to LastActive". The dashboard sorts the history popover by
	// RetiredAt || LastActive so the most recently closed session lands on top.
	RetiredAt int64  `json:"retired_at,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Project   string `json:"project,omitempty"` // filled by server
}

// RecentSessionsFilter is the consumer-facing hook RecentSessions calls before
// returning a session. Both methods are best-effort (false = unfiltered),
// MUST be safe for concurrent reads (the dashboard 1Hz poll calls
// RecentSessions from multiple goroutines) and should snapshot state at
// construction — they run O(N) times per scan.
type RecentSessionsFilter interface {
	// SkipWorkspace reports whether all sessions under the given resolved
	// (absolute, non-empty) workspace path should be hidden, e.g.
	// naozhi-internal sys-session workdirs.
	SkipWorkspace(workspace string) bool
	// SkipSessionID reports whether the specific Claude session should be
	// hidden, e.g. cron-spawned sessions that share their workspace with
	// regular user sessions and so cannot be filtered by path.
	SkipSessionID(sessionID string) bool
}

// noopRecentFilter stands in for a nil filter so the scan loop needs no nil checks.
type noopRecentFilter struct{}

func (noopRecentFilter) SkipWorkspace(string) bool { return false }
func (noopRecentFilter) SkipSessionID(string) bool { return false }

// RecentSessions scans ~/.claude/projects/* for recent sessions, returning up
// to `limit` sessions modified within `maxAge` (limit <= 0 returns all).
// Filtering, in order: (1) directories that cannot be mapped back to a real
// workspace on disk are skipped; (2) workspaces with a dot-prefixed component
// (automated tools) are skipped, except git worktrees under ".claude/worktrees";
// (3) filter.SkipWorkspace, then excludeSessionIDs / filter.SkipSessionID.
// filter may be nil; excludeSessionIDs is always honoured.
func RecentSessions(claudeDir string, limit int, maxAge time.Duration, excludeSessionIDs map[string]bool, filter RecentSessionsFilter) []RecentSession {
	return RecentSessionsCtx(context.Background(), claudeDir, limit, maxAge, excludeSessionIDs, filter)
}

// RecentSessionsCtx is RecentSessions with cancellation support: the walk
// checks ctx before each project directory and returns the (still sorted and
// trimmed) partial result once cancelled, so a hung NFS/FUSE home cannot
// block every poll waiting on the singleflight leader (#2134).
func RecentSessionsCtx(ctx context.Context, claudeDir string, limit int, maxAge time.Duration, excludeSessionIDs map[string]bool, filter RecentSessionsFilter) []RecentSession {
	if claudeDir == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
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

	all := make([]RecentSession, 0, len(entries))
	jsonlPaths := make(map[string]string, len(entries))

	for _, e := range entries {
		// Bail out as soon as ctx is cancelled so a slow FS cannot pin the
		// singleflight leader for the full traversal (#2134).
		if err := ctx.Err(); err != nil {
			slog.Warn("recent sessions scan cancelled mid-walk; returning partial result",
				"scanned", len(all), "err", err)
			break
		}
		if !e.IsDir() {
			continue
		}
		dirName := e.Name()

		projDir := filepath.Join(projectsDir, dirName)
		workspace, idx := resolveWorkspaceWithIndex(projDir, dirName)

		if workspace == "" {
			continue
		}

		// Layer 2: skip tool-owned hidden paths. Operating on the decoded
		// workspace tells ".claude/worktrees/x" (a user's git worktree — keep)
		// from ".claude-mem/..." (an observer's scratch dir — drop) (#2370).
		if isHiddenToolWorkspace(workspace) {
			continue
		}

		// Layer 3: caller-supplied workspace blacklist, so sys-session JSONLs
		// cannot leak AutoTitler prompt fragments into the user history panel.
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

	slices.SortFunc(all, func(a, b RecentSession) int {
		return cmp.Compare(b.LastActive, a.LastActive)
	})

	// Deferred prompt extraction: only read JSONL for sessions that will be returned.
	resCap := len(all)
	if limit > 0 && limit < resCap {
		resCap = limit
	}
	result := make([]RecentSession, 0, resCap)
	for i := range all {
		if limit > 0 && len(result) >= limit {
			break
		}
		// extractFirstPrompt opens+reads a JSONL per result; honour
		// cancellation here too so a slow FS cannot pin the leader (#2134).
		if err := ctx.Err(); err != nil {
			slog.Warn("recent sessions prompt extraction cancelled; returning partial result",
				"extracted", len(result), "err", err)
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

// jsonlFileInfo holds cached metadata for a single .jsonl file.
type jsonlFileInfo struct {
	sessionID string
	mtime     int64 // unix ms
}

// dirFilesCacheEntry stores cached .jsonl metadata for a project directory;
// byID is derived once at fill time so recentFromParsedIndex (called per
// sidebar refresh) does not rebuild it. Both share the dirMtime lifetime.
type dirFilesCacheEntry struct {
	dirMtime int64 // directory mtime in UnixNano (changes on file add/remove)
	files    []jsonlFileInfo
	byID     map[string]int64 // sessionID → mtime; nil iff len(files)==0
}

// dirFilesCache caches per-directory .jsonl metadata, invalidated when the
// directory mtime changes (file add/remove). Appends to individual files do
// NOT invalidate it, which is acceptable for the 7-day history sidebar.
var dirFilesCache sync.Map // projDir → *dirFilesCacheEntry

// cachedJSONLFileInfo returns .jsonl file metadata for a project directory,
// using a cache validated by the directory's own mtime.
func cachedJSONLFileInfo(projDir string) []jsonlFileInfo {
	if entry := loadCachedDirEntry(projDir); entry != nil {
		return entry.files
	}
	return nil
}

// cachedJSONLByID returns the sessionID→mtime map for a project directory from
// the same mtime-validated cache as cachedJSONLFileInfo. Read-only for callers.
func cachedJSONLByID(projDir string) map[string]int64 {
	if entry := loadCachedDirEntry(projDir); entry != nil {
		return entry.byID
	}
	return nil
}

// loadCachedDirEntry returns the cached entry for projDir, refilling on a
// miss / stale dirMtime so both accessors share one scan per directory state.
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

	dirEntries, err := os.ReadDir(projDir)
	if err != nil {
		return nil
	}
	files := make([]jsonlFileInfo, 0, len(dirEntries))
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

// firstPromptScanBudget bounds how many bytes of normal (sub-cap) lines
// extractFirstPrompt accumulates before giving up; the first text turn lands
// within a few KB on any normal transcript. Oversized lines are drained by
// readJSONLLine without counting, so an inline-image head is skipped cheaply.
const firstPromptScanBudget = 512 * 1024

// extractFirstPrompt reads the first user message from a JSONL file, scanning
// at most firstPromptScanBudget bytes and skipping (not aborting on) a leading
// oversized inline-image line via readJSONLLine.
func extractFirstPrompt(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 16*1024)
	var scanned int
	for {
		line, oversized, rerr := readJSONLLine(r)
		scanned += len(line)
		// Cheap pre-filter before json.Unmarshal; the Unmarshal below is the
		// authoritative check. Oversized lines carry no first-prompt text.
		if len(line) > 0 && !oversized && bytes.Contains(line, []byte(`"type"`)) {
			var hl historyLine
			if json.Unmarshal(line, &hl) == nil && hl.Type == "user" {
				if text := extractUserText(hl.Message); text != "" {
					return SanitizePromptForTransport(textutil.TruncateRunes(text, 120))
				}
			}
		}
		if rerr != nil || scanned > firstPromptScanBudget {
			return ""
		}
	}
}

// worktreesMarker is the path fragment Claude Code uses for git worktrees it
// creates on a user's behalf. Sessions there are ordinary user work and must
// stay visible in the history panel even though the path is dot-hidden.
const worktreesMarker = ".claude" + string(filepath.Separator) + "worktrees" + string(filepath.Separator)

// hasDotComponent reports whether any component of p is dot-prefixed. The
// leading position is checked separately because workspace paths are not
// guaranteed absolute (sessions-index.json's originalPath is used verbatim),
// and a relative ".tool/observer" must not pass as non-hidden.
func hasDotComponent(p string) bool {
	return strings.HasPrefix(p, ".") || strings.Contains(p, string(filepath.Separator)+".")
}

// isHiddenToolWorkspace reports whether a resolved workspace path belongs to
// an automated tool rather than a user project: any dot-prefixed component
// makes it tool-owned, except Claude Code's ".claude/worktrees/<name>" git
// worktrees. It works on the decoded path; both encode to "--".
func isHiddenToolWorkspace(workspace string) bool {
	if !hasDotComponent(workspace) {
		return false
	}
	// Match ".claude/worktrees/" as a whole component so "my.claude/worktrees/"
	// does not qualify.
	prefix, rest, ok := cutWorktreesMarker(workspace)
	if !ok {
		return true
	}
	// The prefix must itself be dot-free, else a tool dir nesting a worktree
	// (".cache/x/.claude/worktrees/y") would be whitelisted by association.
	if hasDotComponent(prefix) {
		return true
	}
	// An empty remainder is the ".claude/worktrees" container itself, not a worktree.
	if rest == "" {
		return true
	}
	// The worktree name must not re-introduce a dot component.
	return hasDotComponent(rest)
}

// cutWorktreesMarker splits p around a whole-component ".claude/worktrees/"
// occurrence (at the start of a relative path or directly after a separator),
// returning the parts before and after it and whether it was found.
func cutWorktreesMarker(p string) (prefix, rest string, ok bool) {
	if strings.HasPrefix(p, worktreesMarker) {
		return "", p[len(worktreesMarker):], true
	}
	sep := string(filepath.Separator)
	if i := strings.Index(p, sep+worktreesMarker); i >= 0 {
		return p[:i], p[i+len(sep)+len(worktreesMarker):], true
	}
	return "", "", false
}

// resolveWorkspaceWithIndex determines the real filesystem path for a Claude
// project directory and returns the parsed sessions index if present, reading
// the index once for both purposes.
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
			// Index exists but originalPath missing or stale — keep entries, DFS for workspace.
			if ws := resolveWorkspaceByParts(dirName); ws != "" {
				return ws, &idx
			}
			return "", &idx
		}
	}
	return resolveWorkspaceByParts(dirName), nil
}

// recentFromParsedIndex extracts sessions from an already-parsed sessions
// index using the cached sessionID→mtime map (O(1) per index entry).
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

// dfsPathCache caches successful resolveWorkspaceByParts results; encoded
// names never change, so a mapping is stable for the process lifetime.
// Negative results ("") are NOT cached: a workspace may be temporarily absent
// (unmounted drive, worktree mid-rebuild) and caching "" would hide the
// project until restart (#1994). Re-running the bounded passes is cheap.
var dfsPathCache sync.Map // encoded dirName → resolved workspace path

// resolveWorkspaceByParts reconstructs a workspace path from an encoded
// project directory name, where every non-[A-Za-z0-9] character became "-"
// (see ClaudeProjectSlug). Pass 1, tryResolveParts, splits on "-" and
// DFS-joins parts with os.Stat, resolving any ordinary workspace; pass 2,
// resolveByDirScan, re-encodes real child names per level for segments that
// held ".", "_" or spaces — what makes ".claude/worktrees" sessions visible (#2370).
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
	if result == "" {
		budget := dirScanBudget
		result = resolveByDirScan(dirName[1:], "/", &budget)
	}
	if result != "" {
		dfsPathCache.Store(dirName, result)
	}
	return result
}

// dirScanBudget caps how many directory entries resolveByDirScan may re-encode
// per walk: encoded children can share a prefix (".claude" and "-claude" both
// give "-claude"), so backtracking is worst-case exponential. It only trips on
// adversarial trees, where "" (project hidden) beats stalling the scan.
const dirScanBudget = 10000

// resolveByDirScan walks down from base, consuming the encoded remainder
// (leading "-" stripped) by matching it against the re-encoded names of
// base's real subdirectories; returns the path whose encoding is "-"+remainder
// or "". The encoding is not injective, but every returned path re-encodes to
// the input by construction, and the choice is deterministic: os.ReadDir sorts
// by name and dotPreferredOrder hoists dot-prefixed candidates.
func resolveByDirScan(remainder, base string, budget *int) string {
	if remainder == "" {
		return ""
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	entries = dotPreferredOrder(entries)
	for _, e := range entries {
		if *budget <= 0 {
			return ""
		}
		*budget--
		if !e.IsDir() {
			// Symlinks to directories report mode Symlink, not Dir; Stat so a
			// symlinked workspace component still resolves (as tryResolveParts does).
			if e.Type()&os.ModeSymlink == 0 {
				continue
			}
			if info, serr := os.Stat(filepath.Join(base, e.Name())); serr != nil || !info.IsDir() {
				continue
			}
		}
		name := e.Name()
		// "." / ".." would recurse without consuming the remainder. os.ReadDir
		// omits them, but guard so a different directory reader cannot loop.
		if name == "." || name == ".." {
			continue
		}
		enc := substituteNonAlnum(name)
		if enc == "" {
			continue
		}
		candidate := filepath.Join(base, name)
		if remainder == enc {
			return candidate
		}
		// "/" encodes to "-" too, so a non-leaf match consumes enc plus one "-".
		if !strings.HasPrefix(remainder, enc) || len(remainder) <= len(enc) || remainder[len(enc)] != '-' {
			continue
		}
		if result := resolveByDirScan(remainder[len(enc)+1:], candidate, budget); result != "" {
			return result
		}
	}
	return ""
}

// dotPreferredOrder returns dot-prefixed entries first, preserving os.ReadDir's
// lexical order within each group, and only allocates when a non-dot entry
// precedes a dot one. ".claude" is the pre-image seen in practice for "-claude".
func dotPreferredOrder(entries []os.DirEntry) []os.DirEntry {
	firstDot := -1
	for i, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			firstDot = i
			break
		}
	}
	if firstDot <= 0 {
		return entries
	}
	out := make([]os.DirEntry, 0, len(entries))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			out = append(out, e)
		}
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e)
		}
	}
	return out
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
