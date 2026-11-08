// Package bgtask is the one judgment of a session's background tasks still
// running (docs/rfc/planner.md D109): a Bash run_in_background, a Monitor,
// an Agent subtask, a workflow. The CLI writes task_start, task_progress and
// task_done for each; a task started and not yet ended is open. Everything
// that asks whether a session's work is still going — the health table and
// index.json, the task card and the sidebar, the Planner's receipts,
// custody, the put-away — reads it from Judge, so they cannot disagree.
//
// A task is open only while the CLI process that started it lives: Since
// is when that process started, and entries before it are an earlier
// process's (a /new, a --resume, a crash and respawn). Nothing is open
// once the process is gone (Alive false). A task_done the gateway never
// saw (its process killed, a frame lost) would leave a task open forever,
// so a task quiet past its kind's limit (Quiet) is no longer counted.
package bgtask

import (
	"cmp"
	"slices"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// Kind is what sort of background task a task is.
type Kind string

const (
	KindBash     Kind = "bash"     // a Bash run_in_background
	KindMonitor  Kind = "monitor"  // a Monitor stream
	KindAgent    Kind = "agent"    // an Agent subtask or a teammate
	KindWorkflow Kind = "workflow" // a Workflow run (the workflow board's)
)

// Quiet limits, from a task's last task_start or task_progress. A Bash or a
// Monitor writes nothing between its start and its end, so its limit is
// how long one may run: 12 hours holds every one in 196 local event logs
// (11-08: the longest ran under 12 hours, 15 ran 1–3 hours) and a remote
// benchmark waited on locally; past it a session that shows 后台中 with no
// word for half a day says less than one that shows 空闲. An agent reports
// progress every few tool calls, so 2 hours without one is a lost end. A
// workflow is the board's: its status says when it ended, and the health
// table judges its quiet (health.StallAfter), so it has no limit here.
const (
	BashQuiet  = 12 * time.Hour
	AgentQuiet = 2 * time.Hour
)

// Quiet is kind's limit; zero is none.
func Quiet(k Kind) time.Duration {
	switch k {
	case KindBash, KindMonitor:
		return BashQuiet
	case KindAgent:
		return AgentQuiet
	}
	return 0
}

// Task is one open background task. Last is its latest task_start or
// task_progress (a workflow's latest observation); Summary is the CLI's
// description of it, the session's own text.
type Task struct {
	ID      string    `json:"id"`
	Kind    Kind      `json:"kind"`
	Summary string    `json:"summary"`
	Start   time.Time `json:"start"`
	Last    time.Time `json:"last"`
}

// State is a session's open background tasks, in the order they started,
// and the latest progress among them; the zero State is none open.
type State struct {
	Tasks []Task
	// LastProgress is the latest Task.Last.
	LastProgress time.Time
}

// Count is how many tasks are open.
func (s State) Count() int { return len(s.Tasks) }

// Open reports whether any task is open.
func (s State) Open() bool { return len(s.Tasks) > 0 }

// Workflow is a running workflow as the workflow board has it.
type Workflow struct {
	ID    string
	Name  string
	Start time.Time
	// Seen is the board's latest observation of it.
	Seen time.Time
}

// Input is what Judge reads of one session.
type Input struct {
	// Alive is a live CLI process; false judges nothing open.
	Alive bool
	// Since is when that process started: entries before it are another
	// process's. A process reattached through its shim started when the
	// shim did, before the gateway's restart.
	Since time.Time
	// Entries are the session's latest events, oldest first.
	Entries []clievent.EventEntry
	// Replayed are the task frames a shim reattach replayed, which reach
	// no event log: a task that ended while the gateway was down ended in
	// them. Their order and times are not known; an end anywhere ends its
	// task, as a task ID is never started twice.
	Replayed []clievent.EventEntry
	// Workflows are the running workflows on the session's board.
	Workflows []Workflow
	Now       time.Time
}

// Judge returns in's open background tasks: Scan, then At in.Now.
func Judge(in Input) State { return Scan(in).At(in.Now) }

// Scan returns in's open background tasks, the workflows first, with no
// quiet limit applied (in.Now unread): what a cache may keep while the
// session's events stay the same.
func Scan(in Input) State {
	if !in.Alive {
		return State{}
	}
	open := map[string]*Task{}
	ended := map[string]bool{}
	var order []string
	since := in.Since.UnixMilli()
	// tool is the latest tool_use before an entry: a Monitor's task_start
	// carries task_type local_bash, as a Bash's does, right after the call.
	tool := ""
	scan := func(e clievent.EventEntry, replay bool) {
		switch {
		case e.Type == clievent.KindToolUse:
			tool = e.Tool
			return
		case e.TaskID == "":
			return
		case Ended(e):
			ended[e.TaskID] = true
			return
		case !replay && e.Time < since:
			return
		}
		at := time.UnixMilli(e.Time)
		if replay {
			at = in.Since
		}
		switch e.Type {
		case clievent.KindTaskStart:
			// A workflow is the board's: its task frames come and go
			// with the panel, not with its run.
			if e.TaskType == clievent.TaskTypeWorkflow {
				return
			}
			t := open[e.TaskID]
			if t == nil {
				t = &Task{ID: e.TaskID, Kind: kindOf(e.TaskType, tool), Start: at}
				open[e.TaskID] = t
				order = append(order, e.TaskID)
			}
			t.Summary = cmp.Or(e.Summary, t.Summary, e.TaskID)
			t.Last = later(t.Last, at)
		case clievent.KindTaskProgress:
			if t := open[e.TaskID]; t != nil {
				t.Last = later(t.Last, at)
			}
		}
	}
	for _, e := range in.Entries {
		scan(e, false)
	}
	tool = ""
	for _, e := range in.Replayed {
		scan(e, true)
	}
	out := WithWorkflows(State{}, in.Workflows)
	for _, id := range order {
		if !ended[id] && !slices.ContainsFunc(out.Tasks, func(o Task) bool { return o.ID == id }) {
			out.add(*open[id])
		}
	}
	return out
}

// WithWorkflows is s with the running workflows ws before its tasks.
func WithWorkflows(s State, ws []Workflow) State {
	if len(ws) == 0 {
		return s
	}
	var out State
	for _, w := range ws {
		out.add(Task{ID: w.ID, Kind: KindWorkflow, Summary: cmp.Or(w.Name, w.ID), Start: w.Start, Last: cmp.Or(w.Seen, w.Start)})
	}
	for _, t := range s.Tasks {
		out.add(t)
	}
	return out
}

// At is s without the tasks quiet past their kind's limit at now.
func (s State) At(now time.Time) State {
	var out State
	for _, t := range s.Tasks {
		if q := Quiet(t.Kind); q == 0 || now.Sub(t.Last) <= q {
			out.add(t)
		}
	}
	return out
}

func (s *State) add(t Task) {
	s.Tasks = append(s.Tasks, t)
	s.LastProgress = later(s.LastProgress, t.Last)
}

// Ended reports whether e ends its background task: its done notification,
// or a task_updated patch with any status but running.
func Ended(e clievent.EventEntry) bool {
	return e.TaskID != "" && (e.Type == clievent.KindTaskDone ||
		e.Type == clievent.KindTaskProgress && e.Status != "" && e.Status != "running")
}

// kindOf is a task's kind by its task_type and the tool call before it.
func kindOf(taskType, tool string) Kind {
	switch {
	case taskType == clievent.TaskTypeWorkflow:
		return KindWorkflow
	case taskType == "local_bash" && tool == "Monitor":
		return KindMonitor
	case taskType == "local_bash":
		return KindBash
	}
	// local_agent, in_process_teammate, and a type the CLI adds later:
	// the shorter limit, so an unknown kind is not held open for 12 hours.
	return KindAgent
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
