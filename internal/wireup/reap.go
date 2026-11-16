package wireup

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/apierr"
	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/reap"
	"github.com/naozhi/naozhi/internal/session"
)

// ReapSources are the router's signals and actions planner/reap uses.
type ReapSources struct {
	Router *session.Router
	// Health is the health snapshot (a failed unit holds a session).
	Health func() health.Snapshot
	// ClaudeDir is the Claude CLI's directory, where the transcripts are.
	ClaudeDir string
	// Transcripts keeps what was read of each transcript; nil reads each
	// one whole every time.
	Transcripts *TranscriptDirs
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
		Background: background(s, snap).Open(),
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

// InUse is reap.Config.InUse: a session other than key, still in the
// router (not put away), whose workspace is dir or under it, or whose
// transcripts or its sub-agents' went into dir.
func (c ReapSources) InUse(dir, key string) bool {
	for _, snap := range c.Router.ListSessions() {
		if snap.Key == key {
			continue
		}
		if snap.Workspace != "" && within(snap.Workspace, dir) {
			return true
		}
		ids := []string{snap.SessionID}
		if s := c.Router.SessionFor(snap.Key); s != nil {
			ids = s.SnapshotChainIDs()
		}
		if slices.ContainsFunc(c.sessionDirs(snap.Workspace, ids), func(d string) bool { return within(d, dir) }) {
			return true
		}
	}
	return false
}

// Dirs is reap.Config.Dirs: the working directories sessionIDs' transcripts
// went into, in the order first seen. The CLI writes its cwd on each line of
// the session's own transcript, so a cd there shows; a sub-agent's shell is
// reset to the workspace before each command, so its transcript's cwd stays
// put and the directories its commands cd into are read instead (a
// workflow's agents each working in their own worktree).
func (c ReapSources) Dirs(workspace string, sessionIDs []string) []string {
	return c.sessionDirs(workspace, sessionIDs)
}

func (c ReapSources) sessionDirs(workspace string, sessionIDs []string) []string {
	if c.ClaudeDir == "" || workspace == "" {
		return nil
	}
	projectDirs := claudefs.WorkspaceProjectDirs(claudefs.ProjectsRoot(c.ClaudeDir), workspace)
	var out []string
	seen := map[string]bool{}
	add := func(dirs []string) {
		for _, d := range dirs {
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	for _, id := range sessionIDs {
		if id == "" {
			continue
		}
		if p := claudefs.LocateSessionJSONL(c.ClaudeDir, workspace, id); p != "" {
			add(c.Transcripts.read(p, false))
		}
		for _, pd := range projectDirs {
			for _, p := range claudefs.SubagentTranscripts(pd, id) {
				add(c.Transcripts.read(p, true))
			}
		}
	}
	return out
}

// within reports whether path is dir or under it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// InboxWaiting is reap.Config.Waiting: an item of inbox's still open from
// source(key), the "等你" a session's need_user report opened.
func InboxWaiting(inbox *notify.Notifier, source func(key string) string) func(key string) bool {
	return func(key string) bool {
		if inbox == nil {
			return false
		}
		src := source(key)
		open, _ := inbox.List(0)
		return slices.ContainsFunc(open, func(it notify.Item) bool { return it.Source == src })
	}
}
