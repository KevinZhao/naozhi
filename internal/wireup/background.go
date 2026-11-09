package wireup

import (
	"cmp"
	"slices"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/bgtask"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/session"
)

// taskScan is how many trailing events each scan for background tasks
// reads. A task started further back stays open through the carry the
// scans of one process hand on (bgtask.Carry, kept per session in
// backgrounds) until its end is seen: a long turn's tool calls do not push
// a Monitor started at its head out of sight. A restart starts the carry
// afresh: a task started more than taskScan events before it is seen
// again only through the shim's replay.
const taskScan = 1024

// bgCacheFor bounds how long a cached judgment is reused while its
// session's process and latest event stay the same: history a restart
// loads after the attach goes in before the latest event.
const bgCacheFor = 30 * time.Second

// bgSweepAfter is how long an entry no one asked for stays cached: the
// health table asks for every session each minute, so only a session gone
// from the router loses its carry.
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

// bgEntry is one session's scan (bgtask.ScanFrom, its events only) made
// at at, for the process attached at attached whose latest event was
// last, and the carry it hands the process's next scan. The quiet limits
// move with the clock alone and the board is read fresh, so neither is
// kept.
type bgEntry struct {
	attached, at, used time.Time
	last               clievent.EventEntry
	scan               bgtask.State
	carry              bgtask.Carry
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
		// Another process's carry is not this one's.
		var carry bgtask.Carry
		if ok && e.attached.Equal(attached) {
			carry = e.carry
		}
		scan, next := bgtask.ScanFrom(carry, bgtask.Input{
			Alive:    true,
			Since:    s.ProcessStartedAt(),
			Attached: attached,
			Entries:  s.EventLastN(taskScan),
			Replayed: s.ReplayedTasks(),
		})
		e = bgEntry{attached: attached, last: last, at: now, scan: scan, carry: next}
	}
	e.used = now
	c.mu.Lock()
	// Two callers may scan at once: the later scan's carry is kept, so an
	// end only it saw is not lost.
	if cur, ok := c.entries[s]; !ok || !cur.attached.Equal(e.attached) || !cur.at.After(e.at) {
		c.entries[s] = e
	} else {
		cur.used = now
		c.entries[s] = cur
	}
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

// BackgroundOpen is session.BackgroundCheck: s has a Bash
// run_in_background, a Monitor or an Agent subtask still running, so its
// idle CLI is not to be closed. A workflow is left out: the router keeps a
// running one's CLI itself, bounded (workflowPinned), and bgtask sets a
// workflow no quiet limit.
func BackgroundOpen(s *session.ManagedSession) bool {
	return slices.ContainsFunc(background(s, s.Snapshot()).Tasks, func(t bgtask.Task) bool { return t.Kind != bgtask.KindWorkflow })
}
