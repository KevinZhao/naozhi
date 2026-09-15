package subagent

// Filesystem side: where the CLI keeps subagent transcripts, how their meta
// first lines are read, and the history seed that recovers links after a
// restart. Split out of link.go (#2714 G-follow).

import (
	"bufio"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// SeedFromHistory pre-populates the cache from persisted EventEntry records
// (Process.InjectHistory after AppendBatch) so reconnect/respawn keeps the
// task_id → jsonl mapping. Entries missing InternalAgentID or JSONLPath are
// skipped. Paths outside ~/.claude/projects are refused: a mutated
// sessions/*.jsonl must not redirect agent_events streaming to an arbitrary
// readable file.
func (l *Linker) SeedFromHistory(entries []clievent.EventEntry) {
	if len(entries) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	claudeRoot := claudeProjectsRoot()
	for _, e := range entries {
		if e.TaskID == "" || e.InternalAgentID == "" || e.JSONLPath == "" {
			continue
		}
		// claudeRoot (not l.projectDir) so entries persisted under a previous
		// cwd in the same session are still accepted.
		clean := filepath.Clean(e.JSONLPath)
		if !strings.HasPrefix(clean, claudeRoot+string(filepath.Separator)) {
			slog.Warn("agent_link: SeedFromHistory rejected jsonl path outside claude projects root",
				"task_id", e.TaskID, "path", e.JSONLPath)
			continue
		}
		// A live Resolve outranks historical data.
		if _, ok := l.byTaskID[e.TaskID]; ok {
			continue
		}
		info := LinkInfo{
			InternalAgentID: e.InternalAgentID,
			JSONLPath:       clean,
			Name:            e.Subagent,
			FirstPromptID:   e.FirstPromptID,
			Resolved:        true,
			FromHistory:     true,
		}
		l.byTaskID[e.TaskID] = info
		if e.ToolUseID != "" {
			if _, ok := l.byToolUseID[e.ToolUseID]; !ok {
				l.byToolUseID[e.ToolUseID] = info
			}
		}
		if e.Subagent != "" {
			l.appendNamedLink(e.Subagent, info)
		}
	}
}

// claudeProjectsRoot returns ~/.claude/projects; shared by ProjectDir
// and SeedFromHistory's prefix check so the two cannot drift.
func claudeProjectsRoot() string {
	return claudefs.ProjectsRoot(claudefs.DefaultDir())
}

// scanMetaFiles reads subagentDir and parses each .meta.json into
// (hex, agentType) pairs, TTL-cached (default 200ms) so concurrent Resolves
// in one turn share a scan. Cache hits return the cached slice by reference
// (callers must not mutate) under RLock so they run concurrently.
func (l *Linker) scanMetaFiles(dir string) []metaEntry {
	now := time.Now()
	l.mu.RLock()
	if !l.dirCache.at.IsZero() && now.Sub(l.dirCache.at) < l.cacheTTL {
		entries := l.dirCache.entries
		l.mu.RUnlock()
		return entries
	}
	l.mu.RUnlock()

	// Scan WITHOUT l.mu (ReadDir + ReadFile per meta is blocking IO that
	// would stall every concurrent fast-path RLock), then publish under the
	// write lock. scannedAt is captured BEFORE the scan so a later-started
	// scan wins the "freshest snapshot" comparison (#1595).
	scannedAt := time.Now()
	if l.scanHook != nil {
		l.scanHook()
	}
	entries := rawScanSubagentsDir(dir)

	l.mu.Lock()
	defer l.mu.Unlock()
	// Prefer a fresher cache published while we scanned unlocked.
	if !l.dirCache.at.IsZero() && l.dirCache.at.After(scannedAt) {
		return l.dirCache.entries
	}
	l.dirCache.at = scannedAt
	l.dirCache.entries = entries
	return entries
}

func rawScanSubagentsDir(dir string) []metaEntry {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]metaEntry, 0, len(ents))
	for _, ent := range ents {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if !strings.HasPrefix(name, "agent-") || !strings.HasSuffix(name, ".meta.json") {
			continue
		}
		hex := strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".meta.json")
		if !agentHexRe.MatchString(hex) {
			continue
		}
		metaPath := filepath.Join(dir, name)
		// Real meta files are well under 8 KiB; cap so a stray multi-MB file
		// cannot inflate scan latency.
		const maxMetaBytes = 8 * 1024
		if info, err := ent.Info(); err != nil || info.Size() > maxMetaBytes {
			continue
		}
		data, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}
		var m struct {
			AgentType string `json:"agentType"`
		}
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		if m.AgentType == "" {
			continue
		}
		out = append(out, metaEntry{
			hex:       hex,
			metaPath:  metaPath,
			jsonlPath: filepath.Join(dir, "agent-"+hex+".jsonl"),
			agentType: m.AgentType,
		})
	}
	return out
}

func readFirstLineMeta(path string) (firstLineMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return firstLineMeta{}, err
	}
	defer f.Close()
	reader := bufio.NewReaderSize(f, 32*1024)
	line, err := reader.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return firstLineMeta{}, errFirstLineTooLong
		}
		if len(line) == 0 {
			return firstLineMeta{}, err
		}
	}
	var raw struct {
		SessionID string `json:"sessionId"`
		PromptID  string `json:"promptId"`
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return firstLineMeta{}, err
	}
	out := firstLineMeta{SessionID: raw.SessionID, PromptID: raw.PromptID}
	if raw.Timestamp != "" {
		if ts, ok := claudefs.ParseTimestamp(raw.Timestamp); ok {
			out.Timestamp = ts
		}
	}
	return out, nil
}

// ProjectDir is the ~/.claude/projects/<encoded-cwd> directory for cwd. Empty
// input → "" (Resolve bails). The encoding is lossy ("/tmp/a.b" and "/tmp/a_b"
// collide), which the first-line sessionId cross-check in Resolve defends
// against.
//
// This used to hand-roll the encoding — a SEVENTH copy of the slug rule, and a
// wrong one: it wrote one '-' per RUNE, while the CLI substitutes per UTF-16 code
// unit, so a non-BMP rune (an emoji in the workspace path) produced one dash here
// and two in the real directory name. The transcript was then looked for in a
// directory the CLI never wrote to, and subagent linking silently found nothing.
// claudefs.ProjectSlug is the copy that was verified against CLI 2.1.219,
// including that case (#2649 G1-f).
func ProjectDir(cwd string) string {
	if cwd == "" {
		return ""
	}
	return claudefs.ProjectDir(claudefs.DefaultDir(), cwd)
}
