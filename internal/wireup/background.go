package wireup

import (
	"cmp"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/bgtask"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/session"
)

// taskScan is how many trailing events are searched for background tasks
// still running; one started further back is not seen. Cached per session
// (backgrounds), so a long turn's tool calls do not push a Monitor started
// at its head out of sight.
const taskScan = 1024

// bgCacheFor bounds how long a cached judgment is reused while its
// session's process and latest event stay the same: history a restart
// loads after the attach goes in before the latest event.
const bgCacheFor = 30 * time.Second

// bgSweepAfter is how long an entry no one asked for stays cached.
const bgSweepAfter = 10 * time.Minute

// backgrounds is the one entry to bgtask.Judge for the router's sessions
// (the health table and index.json, the task cards, custody, the Planner's
// receipts, the put-away all read it), cached per session: the scan copies
// taskScan entries, and every reconcile asks for every session.
var backgrounds = &bgCache{entries: map[*session.ManagedSession]bgEntry{}, now: time.Now}

type bgCache struct {
	mu      sync.Mutex
	entries map[*session.ManagedSession]bgEntry
	swept   time.Time
	now     func() time.Time
}

// bgEntry is one session's scan (bgtask.Scan, its events only) made at
// at, for the process attached at attached whose latest event was last.
// The quiet limits move with the clock alone and the board is read fresh,
// so neither is kept.
type bgEntry struct {
	attached, at, used time.Time
	last               clievent.EventEntry
	scan               bgtask.State
}

// background is s's open background tasks, snap its snapshot.
func background(s *session.ManagedSession, snap session.SessionSnapshot) bgtask.State {
	return backgrounds.state(s, snap)
}

func (c *bgCache) state(s *session.ManagedSession, snap session.SessionSnapshot) bgtask.State {
	now := c.now()
	attached := s.ProcessAttachedAt()
	// A dead process's tasks died with it; no process, none ran.
	if attached.IsZero() || snap.State == "dead" {
		return bgtask.State{}
	}
	// The latest event, by its identity: two appends may share a clock
	// reading, never a UUID.
	var last clievent.EventEntry
	if tail := s.EventLastN(1); len(tail) == 1 {
		last = tail[0]
	}
	c.mu.Lock()
	e, ok := c.entries[s]
	c.mu.Unlock()
	if !ok || !e.attached.Equal(attached) || !sameEntry(e.last, last) || now.Sub(e.at) >= bgCacheFor {
		e = bgEntry{attached: attached, last: last, at: now, scan: bgtask.Scan(bgtask.Input{
			Alive:    true,
			Since:    s.ProcessStartedAt(),
			Entries:  s.EventLastN(taskScan),
			Replayed: s.ReplayedTasks(),
		})}
	}
	e.used = now
	c.mu.Lock()
	c.entries[s] = e
	if now.Sub(c.swept) >= bgSweepAfter {
		c.swept = now
		for k, v := range c.entries {
			if now.Sub(v.used) >= bgSweepAfter {
				delete(c.entries, k)
			}
		}
	}
	c.mu.Unlock()
	// The board is read fresh: a snapshot is cheap, and a workflow's end
	// moves no event of the session's.
	wfs := runningWorkflows(snap.Workflows, unixMilli(s.WorkflowBoard().LastObservedAt()))
	return bgtask.WithWorkflows(e.scan, wfs).At(now)
}

// runningWorkflows are the running workflows of a board's summaries, seen
// the board's latest observation; a paused one waits on purpose and is no
// work going on.
func runningWorkflows(sums []workflow.Summary, seen time.Time) []bgtask.Workflow {
	var out []bgtask.Workflow
	for _, s := range sums {
		if s.Status == workflow.StatusRunning {
			start := unixMilli(s.StartedAt)
			out = append(out, bgtask.Workflow{ID: s.TaskID, Name: cmp.Or(s.Name, s.TaskID), Start: start, Seen: later(start, seen)})
		}
	}
	return out
}

// sameEntry reports whether a and b are one event, as far as the cache
// needs to know.
func sameEntry(a, b clievent.EventEntry) bool {
	return a.UUID == b.UUID && a.Time == b.Time && a.Type == b.Type && a.TaskID == b.TaskID
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
