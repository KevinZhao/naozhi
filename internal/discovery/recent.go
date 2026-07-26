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
//  1. Workspace resolution: skip directories that can't be mapped back to a real
//     directory on disk (session can't be resumed without the correct CWD).
//  2. Hidden-path: skip workspaces with a dot-prefixed path component, which
//     belong to automated tools (claude-mem observer et al) rather than to a
//     user-visible project — except git worktrees under ".claude/worktrees",
//     which are ordinary user sessions.
//  3. filter.SkipWorkspace: caller-supplied workspace blacklist (e.g. sys-sessions).
//  4. excludeSessionIDs / filter.SkipSessionID: per-session-ID filtering.
//
// filter may be nil; nil is equivalent to passing a no-op filter.
// Sessions in excludeSessionIDs are always skipped (legacy parameter
// kept for source-compat with discovery callers; new callers should
// prefer filter.SkipSessionID for richer semantics).
func RecentSessions(claudeDir string, limit int, maxAge time.Duration, excludeSessionIDs map[string]bool, filter RecentSessionsFilter) []RecentSession {
	return RecentSessionsCtx(context.Background(), claudeDir, limit, maxAge, excludeSessionIDs, filter)
}

// RecentSessionsCtx is RecentSessions with cancellation support. The
// per-directory FS walk (which can stall indefinitely on a slow/hung
// home — NFS, FUSE) checks ctx.Done() before each project directory and
// returns whatever it has scanned so far once the context is cancelled or
// its deadline expires. PERF-009 (#2134): the dashboard history scan runs
// as the singleflight leader, so an unbounded walk blocks every concurrent
// poll goroutine waiting on the flight; a bounded context caps that stall.
//
// On early return the partial result is still sorted/trimmed normally, so
// callers get a best-effort recent slice rather than nil.
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
		// PERF-009 (#2134): bail out of the walk as soon as the context
		// is cancelled / its deadline lapses so a slow FS cannot pin the
		// singleflight leader for the full traversal. Each project dir
		// triggers stat/ReadDir/file reads below, so the gate sits at the
		// top of the per-directory body.
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

		// Layer 1: skip unresolvable workspaces.
		if workspace == "" {
			continue
		}

		// Layer 2: skip tool-owned hidden paths, now that we hold the
		// decoded workspace and can tell ".claude/worktrees/x" (a user's
		// git worktree — keep) from ".claude-mem/..." (an observer's
		// scratch dir — drop). The old pre-decode `strings.Contains(dirName,
		// "--")` heuristic could not, and hid every worktree session (#2370).
		if isHiddenToolWorkspace(workspace) {
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
		// PERF-009 (#2134): extractFirstPrompt opens+reads a JSONL file
		// per result; honour cancellation here too so a slow FS in the
		// prompt-extraction phase cannot pin the leader either.
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
	// Preallocate to len(dirEntries) so the typical projects directory
	// (almost-all .jsonl files, rare hidden subdirs) lands every entry
	// in the initial backing array. Slot waste from filtered-out non-jsonl
	// entries is bounded by the directory entry count, well below the
	// nil→1→2→4→… grow churn this avoids on every cache miss. R247-PERF-19.
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

// firstPromptScanBudget bounds how many bytes of *normal* (sub-cap) lines
// extractFirstPrompt will accumulate before giving up on finding the first
// user prompt. The first real text turn lands within the first few KB on any
// normal transcript, so this only trips on a head full of large non-user
// turns. Oversized lines are drained by readJSONLLine without counting
// against this budget (draining is pure I/O, no unmarshal), so a head of
// inline-image lines is skipped cheaply rather than instantly exhausting it.
const firstPromptScanBudget = 512 * 1024

// extractFirstPrompt reads the first user message from a JSONL file.
// Scans at most firstPromptScanBudget bytes from the head to stay fast, and
// uses readJSONLLine so a leading oversized inline-image line is skipped
// rather than aborting the scan (which is why the sidebar preview used to be
// blank for image-heavy sessions).
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
		// Fast pre-filter: skip lines that can't be user messages. This
		// avoids json.Unmarshal on every line; the Unmarshal below is the
		// authoritative check. Oversized lines carry no first-prompt text.
		if len(line) > 0 && !oversized && bytes.Contains(line, []byte(`"type"`)) {
			// Reuse the package's canonical JSONL line schema (history.go)
			// instead of re-declaring the type/message shape inline. The
			// extra Timestamp/UUID fields are simply ignored here. (#1478)
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

// hasDotComponent reports whether any component of p is dot-prefixed, i.e.
// p is "." something or contains a "<sep>." pair.
//
// Checking the leading position separately matters because workspace paths are
// not guaranteed absolute: resolveWorkspaceWithIndex returns
// sessions-index.json's originalPath verbatim once os.Stat says it is a
// directory, and that field is file content. A relative ".tool/observer" that
// resolves against the process CWD would otherwise pass for non-hidden and
// leak the tool's prompts into the user history panel.
func hasDotComponent(p string) bool {
	return strings.HasPrefix(p, ".") || strings.Contains(p, string(filepath.Separator)+".")
}

// isHiddenToolWorkspace reports whether a resolved workspace path belongs to
// an automated tool rather than a user project. The rule: any dot-prefixed
// path component makes it tool-owned — except the ".claude/worktrees/<name>"
// prefix, which is where Claude Code puts user git worktrees.
//
// Operating on the decoded path (not the encoded directory name) is what lets
// this distinguish the two: both collapse to a "--" in the encoded form.
func isHiddenToolWorkspace(workspace string) bool {
	if !hasDotComponent(workspace) {
		return false
	}
	// Locate ".claude/worktrees/" as a whole component, so the tail of e.g.
	// "my.claude/worktrees/" does not match. It qualifies either at the very
	// start of a relative path or immediately after a separator.
	prefix, rest, ok := cutWorktreesMarker(workspace)
	if !ok {
		return true
	}
	// Everything before the marker must itself be dot-free, otherwise a tool
	// dir that happens to nest a worktree (".cache/x/.claude/worktrees/y")
	// would be whitelisted by association.
	if hasDotComponent(prefix) {
		return true
	}
	// An empty remainder means the path IS ".claude/worktrees" itself — a
	// container directory, not a worktree, so it stays hidden.
	if rest == "" {
		return true
	}
	// The worktree name must not re-introduce a dot component.
	return hasDotComponent(rest)
}

// cutWorktreesMarker splits p around a whole-component ".claude/worktrees/"
// occurrence, returning the part before the marker, the part after it, and
// whether the marker was found. The marker matches at the start of p (relative
// path) or directly after a separator (absolute path).
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
// Claude Code encodes workspace paths by replacing every character outside
// [A-Za-z0-9] with "-" (see ClaudeProjectSlug), so
// "-home-ec2-user-workspace-foo" originated from "/home/ec2-user/workspace/foo".
// The encoding is lossy in two directions: a "-" in the encoded name may have
// been a "/", a literal "-", a ".", a "_", a space, … and directory names may
// themselves contain literal hyphens.
//
// Two passes, cheapest first:
//
//  1. tryResolveParts — splits on "-" and DFS-joins consecutive parts,
//     verifying each candidate with os.Stat. Resolves any path whose only
//     encoded characters were "/" and literal "-", which is every ordinary
//     workspace. ~10-20 stats.
//  2. resolveByDirScan — for names pass 1 cannot explain (a "." / "_" / space
//     in some segment), ReadDir each level and re-encode the real child names
//     to find which one the encoded remainder started with. Deterministic
//     where pass 1 can only guess, at the cost of a ReadDir per level.
//
// Pass 2 is what makes git worktrees visible: a session run in
// "<repo>/.claude/worktrees/<name>" encodes to "…-repo--claude-worktrees-…"
// (the "/." collapsing into "--"), and no amount of hyphen-splitting can
// recover the leading dot of ".claude" (#2370).
//
// dfsPathCache caches successful (non-empty) results. Encoded directory names
// never change, so a resolved mapping is stable and safe to cache for the
// process lifetime.
//
// Negative results ("") are deliberately NOT cached: the empty string is an
// "unresolvable right now" sentinel, not a stable fact. A workspace directory
// may be temporarily absent during the scan (unmounted network/removable
// drive, git worktree mid-rebuild/checkout) and reappear later. Caching the
// negative result would permanently drop that project from the history
// sidebar until the process restarts (#1994). Re-running both bounded passes
// on a cache miss is cheap.
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
	if result == "" {
		// Pass 1 exhausted: some segment holds a character the "-" split
		// cannot recover (".", "_", space). Re-encode real directory names
		// instead of guessing at the decoding.
		budget := dirScanBudget
		result = resolveByDirScan(dirName[1:], "/", &budget)
	}
	if result != "" {
		dfsPathCache.Store(dirName, result)
	}
	return result
}

// dirScanBudget caps how many directory entries resolveByDirScan may re-encode
// across the whole walk. Two encoded children can share a prefix (".claude"
// and "-claude" both encode to "-claude"), so the walk backtracks and is
// worst-case exponential without a ceiling. A real path costs one ReadDir per
// level and a handful of comparisons; the cap only trips on adversarial or
// pathologically wide trees, where returning "" degrades to the pre-fix
// behaviour (project hidden) rather than stalling the history scan.
const dirScanBudget = 10000

// resolveByDirScan walks down from base, consuming the encoded remainder by
// matching it against the *re-encoded* names of base's real subdirectories.
// Returns the absolute path whose ClaudeProjectSlug-encoding is
// "-"+originalRemainder, or "" when no such directory exists.
//
// remainder is the encoded name with the leading "-" already stripped, i.e.
// for "-home-user-x" the initial call passes "home-user-x" with base "/".
//
// The encoding is not injective — ".claude", "-claude" and "_claude" all
// encode to "-claude" — so several real children can match one encoded
// segment. Two properties keep that safe:
//
//   - Every returned path re-encodes to the input by construction (each level
//     is matched against its own re-encoding), so a caller can never receive a
//     path belonging to a *different* encoded name. Ambiguity picks a
//     different valid pre-image, it does not fabricate one.
//   - The choice is deterministic: os.ReadDir sorts by name, and
//     dotPreferredOrder then hoists dot-prefixed candidates so the ".claude"
//     form Claude itself uses wins over an exotic "-claude"/"_claude"
//     sibling. (A path resolvable without a dot is handled by pass 1 and
//     never reaches here at all.)
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
			// A symlink to a directory reports mode Symlink here, not Dir;
			// Stat it so a symlinked workspace component still resolves
			// (matches tryResolveParts, which Stats and thus follows links).
			if e.Type()&os.ModeSymlink == 0 {
				continue
			}
			if info, serr := os.Stat(filepath.Join(base, e.Name())); serr != nil || !info.IsDir() {
				continue
			}
		}
		name := e.Name()
		// "." / ".." would make filepath.Join walk in place or upwards and
		// recurse without consuming the remainder. os.ReadDir does not list
		// them, but guard anyway so a future switch to a different directory
		// reader cannot introduce an unbounded walk.
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
		// The separator "/" encodes to "-" like everything else, so a
		// non-leaf match consumes enc plus exactly one "-".
		if !strings.HasPrefix(remainder, enc) || len(remainder) <= len(enc) || remainder[len(enc)] != '-' {
			continue
		}
		if result := resolveByDirScan(remainder[len(enc)+1:], candidate, budget); result != "" {
			return result
		}
	}
	return ""
}

// dotPreferredOrder returns entries with dot-prefixed names first, preserving
// os.ReadDir's lexical order within each group. Only allocates when the
// directory actually mixes dot and non-dot entries AND a non-dot entry
// precedes a dot one — the common case (no dotfiles, or dotfiles already
// sorted first) returns the input slice untouched.
//
// Rationale: the slug encoding maps ".claude", "-claude" and "_claude" onto
// the same "-claude", and lexical order puts "-claude" first. The ambiguity
// that occurs in practice is Claude's own ".claude/worktrees" layout, so the
// dot form is the better guess.
func dotPreferredOrder(entries []os.DirEntry) []os.DirEntry {
	firstDot := -1
	for i, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			firstDot = i
			break
		}
	}
	// No dot entries, or the very first entry is already a dot entry: the
	// existing order needs no adjustment.
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

// ---------------------------------------------------------------------------
// Naozhi-managed session detection
// ---------------------------------------------------------------------------
