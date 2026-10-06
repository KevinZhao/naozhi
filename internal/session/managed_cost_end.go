package session

import (
	"log/slog"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/osutil"
)

// maxEndBookings bounds the process-end bookings running at once; each may
// scan a session's transcripts.
const maxEndBookings = 2

// endBookingsDrain bounds how long shutdown waits for running bookings.
const endBookingsDrain = 5 * time.Second

// transcriptMark is the size of a process's main transcript before the
// process wrote to it, for the session id it was taken under.
type transcriptMark struct {
	sid  string
	size int64
}

// markTranscript records the main transcript a process of backendID resuming
// sid in workspace appends to. Taken before the process starts, so every line
// it writes lies past the mark. Zero for a fresh session (its transcript
// starts empty) or a backend whose transcripts naozhi does not read.
func markTranscript(claudeDir string, backendDirs map[string]string, backendID, workspace, sid string) transcriptMark {
	path := mainTranscript(claudeDir, backendDirs, backendID, workspace, sid)
	if path == "" {
		return transcriptMark{}
	}
	st, err := os.Stat(path)
	if err != nil {
		return transcriptMark{}
	}
	return transcriptMark{sid: sid, size: st.Size()}
}

// mainTranscript is the main transcript a process of backendID resuming sid in
// workspace appends to, or "" for no sid or a backend whose transcripts naozhi
// does not read.
func mainTranscript(claudeDir string, backendDirs map[string]string, backendID, workspace, sid string) string {
	if sid == "" {
		return ""
	}
	if backendID == "" {
		backendID = "claude"
	}
	p, ok := backendProfile(backendID)
	if !ok || p.TranscriptUsage == nil || p.ResumeTarget == nil {
		return ""
	}
	return p.ResumeTarget(backendDirs[backendID], claudeDir, workspace, sid)
}

func (s *ManagedSession) setEndMark(m transcriptMark) {
	s.costMu.Lock()
	s.endMark = m
	s.costMu.Unlock()
}

// bookProcessEnd binds proc's end to s: whatever the process spent after its
// last result frame, which no result will now report, is booked as a
// Kind=partial entry. This covers every way a process ends with a turn
// unfinished — a death, a watchdog or stuck_running kill, a reset — whether
// or not a Send owned the turn. claudeDir locates the transcripts.
func bookProcessEnd(s *ManagedSession, proc processIface, claudeDir string) {
	if n, ok := proc.(processEndNotifier); ok {
		n.SetOnEnd(func(end cli.ProcessEnd) { s.costAcct.onProcessEnd(s, end, claudeDir) })
	}
}

// onProcessEnd books end's spend off the read loop. A detached CLI, or one
// whose shim outlived the socket, is still running and will report its spend
// itself.
func (c *costAccounting) onProcessEnd(s *ManagedSession, end cli.ProcessEnd, claudeDir string) {
	if c == nil || end.Detached || !c.ledger.Enabled() {
		return
	}
	if end.ShimLive {
		slog.Info("cost: shim socket lost with the shim alive; the reattached CLI reports this turn's spend",
			"session", osutil.SanitizeForLog(s.key, 128))
		return
	}
	c.ends.add()
	go func() {
		defer c.ends.done()
		defer func() {
			if r := recover(); r != nil {
				metrics.PanicRecoveredTotal.Add(1)
				slog.Error("cost: process-end booking panicked", "panic", r, "stack", string(debug.Stack()))
			}
		}()
		c.endSem <- struct{}{}
		defer func() { <-c.endSem }()
		sid := end.SessionID
		if sid == "" { // a passthrough process never learns it; it runs the session's
			sid = s.getSessionID()
		}
		s.bookPartialUsage(s.endUsage(end, sid, claudeDir), sessionRunID("end:", sid))
	}()
}

// sessionRunID is the run id of a ledger entry no run record shares (a
// process-end partial, an unowned result), so it carries the CLI session id
// the spend belongs to: "<prefix><sid>:<id>", or a bare id when sid is unknown.
func sessionRunID(prefix, sid string) string {
	id := newRunID()
	if sid == "" || id == "" {
		return id
	}
	return prefix + sid + ":" + id
}

// waitEnds waits up to d for running process-end bookings.
func (c *costAccounting) waitEnds(d time.Duration) bool { return c.ends.wait(d) }

// inflight counts running tasks. Unlike sync.WaitGroup it may be waited on
// while tasks start: a process can end during shutdown's wait.
type inflight struct {
	mu     sync.Mutex
	n      int
	idle   chan struct{} // closed when n drops to 0
	closed bool          // tryAdd refuses new tasks
}

func (f *inflight) add() {
	f.mu.Lock()
	f.addLocked()
	f.mu.Unlock()
}

// tryAdd is add unless close was called; it reports whether the task counts.
func (f *inflight) tryAdd() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return false
	}
	f.addLocked()
	return true
}

func (f *inflight) addLocked() {
	if f.n == 0 {
		f.idle = make(chan struct{})
	}
	f.n++
}

// close makes every later tryAdd fail; tasks already counted run on.
func (f *inflight) close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}

func (f *inflight) done() {
	f.mu.Lock()
	f.n--
	if f.n == 0 {
		close(f.idle)
	}
	f.mu.Unlock()
}

// wait reports whether no task was running within d.
func (f *inflight) wait(d time.Duration) bool {
	f.mu.Lock()
	if f.n == 0 {
		f.mu.Unlock()
		return true
	}
	idle := f.idle
	f.mu.Unlock()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-idle:
		return true
	case <-t.C:
		return false
	}
}

// endUsage is what end's process, running CLI session sid, spent after its
// last result frame (or after naozhi attached to it): from the session's
// transcripts when the backend reads them, which include sub-agents and
// workflow agents, else the main loop's frames. The window closes at the end,
// so a respawn appending to the same transcript is not counted.
func (s *ManagedSession) endUsage(end cli.ProcessEnd, sid, claudeDir string) clievent.ShadowUsage {
	since := end.StartedAt
	if end.LastResultAt.After(since) {
		since = end.LastResultAt
	}
	id := s.Backend()
	if id == "" {
		id = "claude"
	}
	p, ok := backendProfile(id)
	if !ok || p.TranscriptUsage == nil || since.IsZero() || sid == "" {
		return end.Shadow
	}
	w := claudefs.UsageWindow{Since: since, Until: end.EndedAt}
	s.costMu.Lock()
	if s.endMark.sid == sid {
		w.MainOffset = s.endMark.size
	}
	s.costMu.Unlock()
	u, found, err := p.TranscriptUsage(claudeDir, s.Workspace(), sid, w)
	if err != nil {
		slog.Warn("cost: transcript usage unreadable; booking the main loop's frames only",
			"session", osutil.SanitizeForLog(s.key, 128), "err", err)
		return end.Shadow
	}
	if !found {
		return end.Shadow
	}
	return u
}
