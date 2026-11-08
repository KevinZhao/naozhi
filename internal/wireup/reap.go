package wireup

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/apierr"
	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/planner/reap"
	"github.com/naozhi/naozhi/internal/session"
)

// maxTranscriptLine caps one transcript line read for its cwd; a longer
// line (a huge tool result) ends the scan with what was found.
const maxTranscriptLine = 16 << 20

// ReapSources are the router's signals and actions planner/reap uses.
type ReapSources struct {
	Router *session.Router
	// Health is the health snapshot (a failed unit holds a session).
	Health func() health.Snapshot
	// ClaudeDir is the Claude CLI's directory, where the transcripts are.
	ClaudeDir string
}

// Session is reap.Config.Session: the same signals custody's Settled reads,
// kept apart so the hold says which one holds.
func (c ReapSources) Session(key string) (reap.Session, bool) {
	s := c.Router.SessionFor(key)
	if s == nil {
		return reap.Session{}, false
	}
	snap := s.Snapshot()
	out := reap.Session{
		Running:    snap.State == "running",
		Asking:     askPending(s.EventLastN(askScan)),
		Background: runningWorkflow(snap.Workflows) != "",
	}
	if at := s.ProcessAttachedAt(); !out.Background && !at.IsZero() && snap.State != "dead" {
		if t, _ := openTask(s.EventLastN(taskScan), at, true); t != nil {
			out.Background = true
		}
	}
	if apierr.IsEnvelope(snap.LastResponse) {
		out.Failed = snap.LastResponse
	}
	out.LastActive, _ = c.LastActive(key)
	if c.Health != nil {
		for _, u := range c.Health().Units {
			if u.ID == key && u.State == health.Failed {
				out.Failed = u.Evidence
				if out.Failed == "" {
					out.Failed = string(health.Failed)
				}
			}
		}
	}
	return out, true
}

// LastActive is reap.Config.LastActive: key's last activity (zero when it
// has none), false when key is not in the router.
func (c ReapSources) LastActive(key string) (time.Time, bool) {
	s := c.Router.SessionFor(key)
	if s == nil {
		return time.Time{}, false
	}
	if la := s.LastActive(); la.UnixNano() > 0 {
		return la, true
	}
	return time.Time{}, true
}

// Archive is reap.Config.Archive.
func (c ReapSources) Archive(key string) (reap.Archived, bool) {
	a, ok := c.Router.Archive(key)
	return reap.Archived{SessionID: a.SessionID, Workspace: a.Workspace, LastPrompt: a.LastPrompt, Chain: a.ChainIDs}, ok
}

// Restore is reap.Config.Restore: the session back under key, dead, so the
// next message resumes sessionID with the task's options (TaskOpts.ForKey).
func (c ReapSources) Restore(key, sessionID, workspace, lastPrompt string) error {
	if got := c.Router.RegisterForResume(key, sessionID, workspace, lastPrompt); got != key {
		return errors.New("这个会话已在 " + got + " 下打开")
	}
	return nil
}

// InUse is reap.Config.InUse: a session other than key whose workspace is
// dir or under it.
func (c ReapSources) InUse(dir, key string) bool {
	for _, snap := range c.Router.ListSessions() {
		if snap.Key != key && snap.Workspace != "" && within(snap.Workspace, dir) {
			return true
		}
	}
	return false
}

// Dirs is reap.Config.Dirs: the working directories sessionIDs' transcripts
// recorded (the CLI writes its cwd on each line, so a cd into a worktree
// shows), in the order first seen.
func (c ReapSources) Dirs(workspace string, sessionIDs []string) []string {
	if c.ClaudeDir == "" || workspace == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, id := range sessionIDs {
		p := claudefs.LocateSessionJSONL(c.ClaudeDir, workspace, id)
		if p == "" {
			continue
		}
		for _, d := range transcriptCwds(p) {
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	return out
}

// transcriptCwds are the distinct cwd fields of the transcript at path.
func transcriptCwds(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxTranscriptLine)
	key := []byte(`"cwd":`)
	var out []string
	seen := map[string]bool{}
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, key) {
			continue
		}
		var v struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(line, &v) != nil || v.Cwd == "" || seen[v.Cwd] || !filepath.IsAbs(v.Cwd) {
			continue
		}
		seen[v.Cwd] = true
		out = append(out, v.Cwd)
	}
	return out
}

// within reports whether path is dir or under it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
