package workflow

import (
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// Per-process bounds (RFC §5.3).
const (
	maxRowWorkflows = 16  // unsettled workflows that keep agent rows
	maxTracked      = 32  // unsettled workflows tracked at all
	maxTerminal     = 5   // terminal workflows kept, by EndedAt
	maxRemembered   = 256 // task ids remembered as workflow / evicted
)

// idFlags is what the Tracker remembers about a task id beyond its entry.
type idFlags uint8

const (
	flagWorkflow  idFlags = 1 << iota // judged or handed in as a workflow task
	flagGone                          // terminal entry evicted: later frames are dropped
	flagUntracked                     // refused at maxTracked, already counted
)

// Tracker builds one CLI process's workflows from its frames. Writers are
// the read loop (Observe, NoteDropped), SeedFromReplay before it starts,
// and the session board (KnowTasks, ApplyResultFile); readers only Load.
// onChange runs after every published change, outside the lock but on the
// writer's goroutine, and carries no data: the reader Loads the latest Set
// itself. A writer must not hold a lock onChange takes (the board's b.mu).
type Tracker struct {
	mu          sync.Mutex
	builders    map[string]*builder
	ids         map[string]idFlags
	idOrder     []string // FIFO of ids, bounds ids at maxRemembered
	version     uint64
	seedWrapped bool
	cur         atomic.Pointer[Set]
	onChange    func()
}

// New returns an empty Tracker; onChange may be nil.
func New(onChange func()) *Tracker {
	t := &Tracker{builders: map[string]*builder{}, ids: map[string]idFlags{}, onChange: onChange}
	t.cur.Store(&Set{})
	return t
}

// Load returns the current Set, never nil. Lock-free.
func (t *Tracker) Load() *Set { return t.cur.Load() }

// builder is one workflow's mutable build state; wf is its latest
// published value.
type builder struct {
	wf                          *Workflow
	memos                       map[int]*agentMemo
	phaseMemos                  map[int]*memo
	name, desc, current, notify memo
	rows                        bool // granted rows (one of the first maxRowWorkflows)
	tooMany                     bool // a snapshot arrived but rows were not granted
	phasesCapped, decodeErr     bool
	dropped                     bool // NoteDropped since the last accepted snapshot
}

func newBuilder(id string, src Source) *builder {
	return &builder{
		wf:         &Workflow{TaskID: id, Status: StatusRunning, Source: src, Degraded: DegradedNoSnapshot},
		memos:      map[int]*agentMemo{},
		phaseMemos: map[int]*memo{},
	}
}

// degraded picks the first Degraded value that holds.
func (b *builder) degraded(w *Workflow) string {
	switch {
	case b.dropped:
		return DegradedSnapshotDropped
	case b.tooMany:
		return DegradedTooMany
	case b.phasesCapped:
		return DegradedPhasesCapped
	case b.decodeErr:
		return DegradedDecodeError
	case w.SnapshotSeq == 0 && !w.ResultLoaded:
		return DegradedNoSnapshot
	}
	return ""
}

// Observe feeds one decoded frame: task_* frames of workflow tasks and the
// launch receipt build entries; anything else is ignored. now is the
// frame's arrival. It sets ev.WorkflowTask on task_* frames.
func (t *Tracker) Observe(ev *clievent.Event, now time.Time) {
	kind := kindOf(ev)
	if kind == kindNone {
		return
	}
	var ms int64
	if !now.IsZero() {
		ms = now.UnixMilli()
	}
	t.mu.Lock()
	changed := t.observeLocked(ev, kind, ms, SourceStream)
	if changed {
		t.publishLocked()
	}
	t.mu.Unlock()
	if changed {
		t.notify()
	}
}

// observeLocked applies one frame; ms is 0 for a replayed frame, which
// carries no time. It reports whether an entry changed.
func (t *Tracker) observeLocked(ev *clievent.Event, kind frameKind, ms int64, src Source) bool {
	id := taskIDOf(ev, kind)
	if kind != kindLaunch {
		ev.WorkflowTask = t.isWorkflowLocked(ev, kind)
		if !ev.WorkflowTask {
			return false
		}
	}
	t.rememberLocked(id, flagWorkflow)
	if t.ids[id]&flagGone != 0 {
		return false
	}
	b := t.builders[id]
	if b == nil {
		if t.unsettledLocked(false) >= maxTracked {
			if t.ids[id]&flagUntracked == 0 {
				t.ids[id] |= flagUntracked
				untrackedTotal.Add(1)
			}
			return false
		}
		b = newBuilder(id, src)
		t.builders[id] = b
	}
	if !t.applyLocked(b, ev, kind, ms, src) {
		return false
	}
	t.evictTerminalLocked()
	return true
}

// applyLocked folds a frame into b. Only a task_progress after the
// terminal state is ignored (CC sends none; defensive).
func (t *Tracker) applyLocked(b *builder, ev *clievent.Event, kind frameKind, ms int64, src Source) bool {
	old := b.wf
	if kind == kindProgress && IsTerminal(old.Status) {
		return false
	}
	w := *old
	if ms > 0 {
		w.LastObservedAt = ms
	}
	if !w.ResultLoaded {
		w.Source = src
	}
	switch kind {
	case kindStarted:
		b.setName(&w, ev.WorkflowName, NameFromLaunch)
		b.setName(&w, ev.Description, NameFromSummary)
		if w.Description == "" {
			w.Description = b.desc.clip(ev.Description, maxHeaderTextRunes)
		}
		setSession(&w, ev.SessionID, SessionFromLaunch)
		if ms > 0 {
			setStarted(&w, ms, StartedFromLive)
		}
	case kindLaunch:
		l := ev.WorkflowLaunch
		if l.RunID != "" {
			w.RunID = l.RunID
		}
		if l.TranscriptDir != "" {
			w.LaunchTranscriptDir = l.TranscriptDir
		}
		b.setName(&w, l.WorkflowName, NameFromLaunch)
		b.setName(&w, l.Summary, NameFromSummary)
		if w.Description == "" {
			w.Description = b.desc.clip(l.Summary, maxHeaderTextRunes)
		}
		setSession(&w, ev.SessionID, SessionFromLaunch)
	case kindProgress:
		if ev.TaskSummary != "" {
			w.Description = b.desc.clip(ev.TaskSummary, maxHeaderTextRunes)
			b.setName(&w, ev.TaskSummary, NameFromSummary)
		}
		if ev.Description != "" {
			w.Current = b.current.clip(ev.Description, maxHeaderTextRunes)
		}
		setUsage(&w, ev.Usage)
		setSession(&w, ev.SessionID, SessionFromProgress)
		t.applySnapshotLocked(b, &w, ev)
	case kindUpdated:
		setSession(&w, ev.SessionID, SessionFromProgress)
		if p := ev.Patch; p != nil && p.Status != "" {
			st, raw := patchStatus(p.Status)
			setStatus(&w, st, raw, p.EndTime, ms)
		} else if p != nil && w.EndedAt == 0 && IsTerminal(w.Status) {
			w.EndedAt = p.EndTime
		}
	case kindNotification:
		setSession(&w, ev.SessionID, SessionFromProgress)
		if ev.TaskSummary != "" {
			w.NotifySummary = b.notify.clip(ev.TaskSummary, maxHeaderTextRunes)
		}
		setUsage(&w, ev.Usage)
		if ev.Status != "" {
			st, raw := notificationStatus(ev.Status)
			setStatus(&w, st, raw, 0, ms)
		}
	}
	if IsTerminal(w.Status) && !IsTerminal(old.Status) {
		stopAll(&w)
	}
	w.Degraded = b.degraded(&w)
	w.TrackerVersion++
	b.wf = &w
	return true
}

// applySnapshotLocked replaces rows, phases and counts from a snapshot that
// decoded (fully or partially); a failed one keeps the previous rows.
func (t *Tracker) applySnapshotLocked(b *builder, w *Workflow, ev *clievent.Event) {
	switch {
	case ev.WorkflowDecode == clievent.WorkflowDecodeFailed:
		b.decodeErr = true
		itemsIdentityTotal.Add(1)
		return
	case ev.WorkflowProgress == nil:
		return
	}
	if !b.rows {
		b.rows = t.rowWorkflowsLocked() < maxRowWorkflows
	}
	s := normalizeItems(ev.WorkflowProgress, b.memos, b.phaseMemos, b.rows)
	w.Phases, w.Counts, w.AgentsCapped = s.phases, s.counts, s.capped
	if b.rows {
		w.Agents = s.agents
	}
	b.tooMany, b.phasesCapped, b.dropped = !b.rows, s.phasesCapped, false
	b.decodeErr = ev.WorkflowDecode == clievent.WorkflowDecodePartial
	if b.decodeErr {
		itemsPartialTotal.Add(1)
	}
	pruneMemos(w, b.memos, b.phaseMemos)
	w.SnapshotSeq++
	if s.earliest > 0 {
		setStarted(w, s.earliest, StartedFromSnapshot)
	}
}

func (b *builder) setName(w *Workflow, s string, lvl uint8) {
	if s != "" && lvl > w.Src.Name {
		w.Name, w.Src.Name = b.name.clip(s, maxLabelRunes), lvl
	}
}

// setSession keeps the first value seen at the highest grade.
func setSession(w *Workflow, s string, lvl uint8) {
	if s != "" && lvl > w.Src.SessionID {
		w.SessionID, w.Src.SessionID = s, lvl
	}
}

// setStarted takes a higher grade, or an earlier time at the snapshot grade.
func setStarted(w *Workflow, at int64, lvl uint8) {
	if at <= 0 {
		return
	}
	if lvl > w.Src.StartedAt || (lvl == StartedFromSnapshot && w.Src.StartedAt == lvl && at < w.StartedAt) {
		w.StartedAt, w.Src.StartedAt = at, lvl
	}
}

// setUsage takes a frame's run totals unless a result file already set them.
func setUsage(w *Workflow, u *clievent.TaskUsage) {
	if u == nil || w.ResultLoaded {
		return
	}
	w.Tokens, w.ToolCalls, w.DurationMS = int64(u.TotalTokens), u.ToolUses, u.DurationMS
}

// setStatus moves an unsettled workflow to st; the first terminal status
// sticks, and only a result file overrides it (ApplyResultFile).
func setStatus(w *Workflow, st Status, raw string, endTime, ms int64) {
	if IsTerminal(w.Status) {
		if w.EndedAt == 0 && endTime > 0 {
			w.EndedAt = endTime
		}
		return
	}
	w.Status, w.RawStatus = st, raw
	if IsTerminal(st) {
		switch {
		case endTime > 0:
			w.EndedAt = endTime
		case w.EndedAt == 0:
			w.EndedAt = ms
		}
	}
}

// NoteDropped records that a snapshot line of task id was too long to read:
// the rows freeze and the entry shows snapshot_dropped until the next
// accepted snapshot. It reports whether id is a tracked, unsettled workflow.
func (t *Tracker) NoteDropped(id string, now time.Time) bool {
	t.mu.Lock()
	b := t.builders[id]
	if b == nil || IsTerminal(b.wf.Status) {
		t.mu.Unlock()
		return false
	}
	w := *b.wf
	if !now.IsZero() {
		w.LastObservedAt = now.UnixMilli()
	}
	b.dropped = true
	w.Degraded = b.degraded(&w)
	w.TrackerVersion++
	b.wf = &w
	t.publishLocked()
	t.mu.Unlock()
	t.notify()
	return true
}

// KnowTasks marks task ids as workflow tasks, so their frames build entries
// even when the replay no longer holds the task_started or launch frame.
// The board hands in the ids of its persisted and retained entries.
func (t *Tracker) KnowTasks(ids []string) {
	t.mu.Lock()
	for _, id := range ids {
		if id != "" {
			t.rememberLocked(id, flagWorkflow)
		}
	}
	t.mu.Unlock()
}

// ApplyResultFile merges a run's result file into its entry when the file's
// taskId names it (a resumed run shares the runId with older attempts) and
// its status is terminal. The file is authoritative from then on. It
// reports whether it merged; onChange runs before it returns.
func (t *Tracker) ApplyResultFile(rf *ResultFile) bool {
	if rf == nil {
		return false
	}
	t.mu.Lock()
	b := t.builders[rf.TaskID]
	if b == nil {
		t.mu.Unlock()
		return false
	}
	w, ok := mergeResult(b.wf, rf, b.memos, b.phaseMemos)
	if ok {
		b.rows, b.tooMany, b.decodeErr, b.dropped = true, false, false, false
		b.phasesCapped = w.Degraded == DegradedPhasesCapped
		w.TrackerVersion++
		b.wf = w
		t.evictTerminalLocked()
		t.publishLocked()
	}
	t.mu.Unlock()
	if ok {
		t.notify()
	}
	return ok
}

func (t *Tracker) rememberLocked(id string, f idFlags) {
	if _, ok := t.ids[id]; !ok {
		t.idOrder = append(t.idOrder, id)
		if len(t.idOrder) > maxRemembered {
			delete(t.ids, t.idOrder[0])
			t.idOrder = slices.Delete(t.idOrder, 0, 1)
		}
	}
	t.ids[id] |= f
}

// unsettledLocked counts unsettled entries, or only those with rows.
func (t *Tracker) unsettledLocked(withRows bool) int {
	n := 0
	for _, b := range t.builders {
		if !IsTerminal(b.wf.Status) && (!withRows || b.rows) {
			n++
		}
	}
	return n
}

func (t *Tracker) rowWorkflowsLocked() int { return t.unsettledLocked(true) }

// evictTerminalLocked keeps the maxTerminal most recently ended entries.
// An evicted id stays remembered so a late frame cannot rebuild a stub.
func (t *Tracker) evictTerminalLocked() {
	var ended []*Workflow
	for _, b := range t.builders {
		if IsTerminal(b.wf.Status) {
			ended = append(ended, b.wf)
		}
	}
	if len(ended) <= maxTerminal {
		return
	}
	slices.SortFunc(ended, byEndedDesc)
	for _, w := range ended[maxTerminal:] {
		delete(t.builders, w.TaskID)
		t.rememberLocked(w.TaskID, flagGone)
	}
}

func (t *Tracker) publishLocked() {
	wfs := make([]*Workflow, 0, len(t.builders))
	for _, b := range t.builders {
		wfs = append(wfs, b.wf)
	}
	sortWorkflows(wfs)
	t.version++
	t.cur.Store(&Set{Workflows: wfs, Version: t.version, SeedWrapped: t.seedWrapped})
}

func (t *Tracker) notify() {
	if t.onChange != nil {
		t.onChange()
	}
}

// sortWorkflows orders unsettled entries first by StartedAt (unknown last),
// then terminal ones by EndedAt, newest first; ties by TaskID.
func sortWorkflows(wfs []*Workflow) {
	slices.SortFunc(wfs, func(a, b *Workflow) int {
		ua, ub := !IsTerminal(a.Status), !IsTerminal(b.Status)
		switch {
		case ua != ub:
			if ua {
				return -1
			}
			return 1
		case ua:
			if a.StartedAt != b.StartedAt {
				return cmpKnownFirst(a.StartedAt, b.StartedAt)
			}
		default:
			if c := byEndedDesc(a, b); c != 0 {
				return c
			}
		}
		return strings.Compare(a.TaskID, b.TaskID)
	})
}

func byEndedDesc(a, b *Workflow) int {
	switch {
	case a.EndedAt > b.EndedAt:
		return -1
	case a.EndedAt < b.EndedAt:
		return 1
	}
	return strings.Compare(a.TaskID, b.TaskID)
}

// cmpKnownFirst orders ascending with 0 (unknown) last.
func cmpKnownFirst(a, b int64) int {
	switch {
	case a == 0:
		return 1
	case b == 0:
		return -1
	case a < b:
		return -1
	}
	return 1
}
