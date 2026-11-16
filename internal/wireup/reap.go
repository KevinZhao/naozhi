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
	// Now is the clock InUse reads; nil is time.Now.
	Now func() time.Time
}

// cmdInUseFor is how long a directory a session's command cd'd into counts
// as that session's in InUse. A cwd counts while the session is in the
// router, but a command may only have looked (a reviewer's git -C W diff),
// and a session such as the user's own stays in the router for weeks; one
// still working there names it again with each command.
const cmdInUseFor = 24 * time.Hour

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
// router (not put away), whose workspace is dir or under it, whose
// transcripts' cwd went into dir, or whose commands or its sub-agents' cd'd
// into it within cmdInUseFor.
func (c ReapSources) InUse(dir, key string) bool {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	since := now().Add(-cmdInUseFor)
	using := func(d dirSeen) bool { return (d.cwd || d.cmdAt.After(since)) && within(d.dir, dir) }
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
		if slices.ContainsFunc(c.sessionDirs(snap.Workspace, ids), using) {
			return true
		}
	}
	return false
}

// Dirs is reap.Config.Dirs: the working directories sessionIDs' transcripts
// and their sub-agents' went into, in the order first seen. The CLI writes
// its cwd on each line, but the shell is reset to the workspace after a cd
// out of the project (a worktree under ~/workspace/naozhiwt/) and, in a
// sub-agent, before each command; so the directories Bash commands cd into
// are read too (a workflow's agents each working in their own worktree).
func (c ReapSources) Dirs(workspace string, sessionIDs []string) []string {
	var out []string
	for _, d := range c.sessionDirs(workspace, sessionIDs) {
		out = append(out, d.dir)
	}
	return out
}

func (c ReapSources) sessionDirs(workspace string, sessionIDs []string) []dirSeen {
	if c.ClaudeDir == "" || workspace == "" {
		return nil
	}
	projectDirs := claudefs.WorkspaceProjectDirs(claudefs.ProjectsRoot(c.ClaudeDir), workspace)
	var out []dirSeen
	idx := map[string]int{}
	add := func(dirs []dirSeen) {
		for _, d := range dirs {
			if i, ok := idx[d.dir]; ok {
				out[i].merge(d)
				continue
			}
			idx[d.dir] = len(out)
			out = append(out, d)
		}
	}
	for _, id := range sessionIDs {
		if id == "" {
			continue
		}
		if p := claudefs.LocateSessionJSONL(c.ClaudeDir, workspace, id); p != "" {
			add(c.Transcripts.read(p))
		}
		for _, pd := range projectDirs {
			for _, p := range claudefs.SubagentTranscripts(pd, id) {
				add(c.Transcripts.read(p))
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
