package patrol

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/osutil/jsonfile"
	"github.com/naozhi/naozhi/internal/planner/front"
)

// The deep checks' records live in planner/patrol/deep/<workspace ID>.json:
// the last run, and every finding with what became of it. A finding is told
// by (workspace, id); the file is what "already reported" means, so one that
// cannot be read stops the workspace's deep checks rather than reading as
// empty, which would report everything again.
const (
	deepDir       = "deep"
	stateVersion  = 1
	maxStateBytes = 4 << 20
	// recordKeep is how long a finding that is over (merged, closed) stays
	// on record after it was last seen.
	recordKeep = 90 * day
	// MaxOpenTasks is how many tasks the patrol dispatched may be going in
	// one workspace at a time; MaxSurfaced how many findings a day are put
	// in front of the user (a question, a pull request ready), all
	// workspaces together. Past either the finding is an open item only.
	MaxOpenTasks = 2
	MaxSurfaced  = 3
)

// The states of a finding's record.
const (
	// StateNew is found and not handed to the Planner yet.
	StateNew = "new"
	// StateTold is handed to the Planner.
	StateTold = "told"
	// StateItem is filed as an open item of the ledger.
	StateItem = "item"
	// StateAsked is put to the user as a question.
	StateAsked = "asked"
	// StateDispatched is given to a task: session to fix.
	StateDispatched = "dispatched"
	// StatePR is fixed in a pull request that waits on the user.
	StatePR = "pr"
	// StateMerged and StateClosed are that pull request merged, or closed
	// without a merge.
	StateMerged = "merged"
	StateClosed = "closed"
)

// The states of a deep run.
const (
	RunOK     = "ok"
	RunFailed = "failed"
)

// The ways a block naming a finding is not run, in the words the Planner's
// receipt uses.
var (
	// ErrNoFinding is a ref no finding on record has.
	ErrNoFinding = errors.New("没有这条巡查发现（finding 写 <naozhi-patrol> 里那条的 ref）")
	// ErrDailyCap is one more finding put in front of the user today than
	// MaxSurfaced.
	ErrDailyCap = fmt.Errorf("今天提到用户面前的巡查事项已有 %d 条", MaxSurfaced)
	// ErrState is the records unreadable: nothing is judged on a guess.
	ErrState = errors.New("巡查的记录读不了")
)

// Record is a finding on record: what the deep check reported, and what
// became of it.
type Record struct {
	Finding
	// Links are the evidence's file lines as GitHub links at the commit the
	// check ran on; none for a repository not on GitHub.
	Links []string `json:"links,omitempty"`
	// First is when it was first found, Seen when a check last reported it.
	First time.Time `json:"first"`
	Seen  time.Time `json:"seen"`
	State string    `json:"state"`
	// Pending is set until the Planner was handed it.
	Pending bool `json:"pending,omitempty"`
	// Task is the session it was dispatched to, Item the open item it is
	// filed as, PR the pull request that fixes it and CI that one's checks
	// as last looked at ("通过", "失败", "进行中").
	Task string `json:"task,omitempty"`
	Item string `json:"item,omitempty"`
	PR   string `json:"pr,omitempty"`
	CI   string `json:"ci,omitempty"`
	// Surfaced is when it was last put in front of the user in a turn that
	// had none of their words: what the daily cap counts.
	Surfaced time.Time `json:"surfaced,omitzero"`
}

// DeepRun is one deep check as recorded.
type DeepRun struct {
	ID      string    `json:"id"`
	Trigger string    `json:"trigger"`
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended"`
	State   string    `json:"state"`
	Error   string    `json:"error,omitempty"`
	// Head is the commit the check ran on, "" outside a git repository.
	Head string `json:"head,omitempty"`
	// Findings is how many it reported and the gateway kept, New those not
	// on record before, Dropped those the gateway dropped.
	Findings int `json:"findings"`
	New      int `json:"new"`
	Dropped  int `json:"dropped"`
	// Woke says the new ones were handed to the Planner by the run's end.
	Woke bool `json:"woke"`
}

// deepState is one workspace's file.
type deepState struct {
	V         int    `json:"v"`
	Workspace string `json:"workspace"`
	// Week is the week (weekLabel) the schedule last looked at the
	// workspace, Tries the runs it started in it.
	Week  string `json:"week,omitempty"`
	Tries int    `json:"tries,omitempty"`
	// Repo is the GitHub repository (owner/name) the workspace's directory
	// is in as of the last run, "" for none.
	Repo string `json:"repo,omitempty"`
	// Head and Checked are the commit and the start of the last run that
	// went through: with no commit and no session since, there is nothing
	// new to read.
	Head     string    `json:"head,omitempty"`
	Checked  time.Time `json:"checked,omitzero"`
	Last     *DeepRun  `json:"last,omitempty"`
	Findings []Record  `json:"findings"`
}

func (s *deepState) find(id string) *Record {
	for i := range s.Findings {
		if s.Findings[i].ID == id {
			return &s.Findings[i]
		}
	}
	return nil
}

func (p *Patrol) deepPath(ws string) string {
	return filepath.Join(p.cfg.Dir, deepDir, ws+".json")
}

// loadDeep reads ws's file: none is an empty record; one that does not
// parse, or that a newer naozhi wrote, is an error and is left as it is.
// Callers hold deepMu.
func (p *Patrol) loadDeep(ws string) (deepState, error) {
	st, out, err := jsonfile.Load[deepState](p.deepPath(ws), jsonfile.Options{MaxBytes: maxStateBytes, Label: "planner patrol record", Corrupt: jsonfile.LeaveCorrupt})
	switch {
	case err != nil:
		return deepState{}, fmt.Errorf("%w：%w", ErrState, err)
	case out == jsonfile.CorruptLeft:
		return deepState{}, fmt.Errorf("%w：%s 不是合格的 JSON", ErrState, ws+".json")
	case out == jsonfile.Parsed && st.V > stateVersion:
		return deepState{}, fmt.Errorf("%w：%s 是更新版本的 naozhi 写的", ErrState, ws+".json")
	case out == jsonfile.Parsed:
		st.Workspace = ws
		return st, nil
	}
	return deepState{V: stateVersion, Workspace: ws}, nil
}

// saveDeep writes st, the findings long over left out. Callers hold deepMu.
func (p *Patrol) saveDeep(st deepState, now time.Time) error {
	st.V = stateVersion
	st.Findings = slices.DeleteFunc(slices.Clone(st.Findings), func(r Record) bool {
		return (r.State == StateMerged || r.State == StateClosed) && now.Sub(r.Seen) > recordKeep
	})
	if st.Findings == nil {
		st.Findings = []Record{}
	}
	dir := filepath.Dir(p.deepPath(st.Workspace))
	if err := datadir.EnsureDir(dir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return osutil.WriteFileAtomic(p.deepPath(st.Workspace), data, 0o600)
}

// editDeep changes ws's record under the lock: edit reports whether it
// changed anything to write. A record that cannot be read is not edited.
func (p *Patrol) editDeep(ws string, edit func(st *deepState) bool) error {
	now := p.cfg.Now()
	p.deepMu.Lock()
	defer p.deepMu.Unlock()
	st, err := p.loadDeep(ws)
	if err != nil {
		return err
	}
	if !edit(&st) {
		return nil
	}
	return p.saveDeep(st, now)
}

// deepStates is every workspace's record that reads, by workspace ID.
func (p *Patrol) deepStates() []deepState {
	if p.cfg.Dir == "" {
		return nil
	}
	p.deepMu.Lock()
	defer p.deepMu.Unlock()
	return p.deepStatesLocked()
}

func (p *Patrol) deepStatesLocked() []deepState {
	entries, err := os.ReadDir(filepath.Join(p.cfg.Dir, deepDir))
	if err != nil {
		return nil
	}
	var out []deepState
	for _, e := range entries {
		ws, ok := strings.CutSuffix(e.Name(), ".json")
		if e.IsDir() || !ok || !wsRE.MatchString(ws) {
			continue
		}
		st, err := p.loadDeep(ws)
		if err != nil {
			slog.Warn("planner patrol: a deep check record unreadable; left as it is", "workspace", ws, "err", err)
			continue
		}
		out = append(out, st)
	}
	return out
}

// Finding is the finding ref names as it is on record.
func (p *Patrol) Finding(ref string) (Record, bool) {
	ws, id, ok := SplitRef(ref)
	if p == nil || p.cfg.Dir == "" || !ok {
		return Record{}, false
	}
	p.deepMu.Lock()
	st, err := p.loadDeep(ws)
	p.deepMu.Unlock()
	if err != nil {
		return Record{}, false
	}
	if r := st.find(id); r != nil {
		return *r, true
	}
	return Record{}, false
}

// Findings is workspace ws's findings on record, as found.
func (p *Patrol) Findings(ws string) []Record {
	if p == nil || p.cfg.Dir == "" || !wsRE.MatchString(ws) {
		return nil
	}
	p.deepMu.Lock()
	st, err := p.loadDeep(ws)
	p.deepMu.Unlock()
	if err != nil {
		return nil
	}
	return st.Findings
}

// taskOpen reports whether dispatched session key is still at work.
// Unknown counts as at work: the cap errs towards fewer tasks.
func (p *Patrol) taskOpen(key string) bool {
	return key != "" && (p.cfg.TaskOpen == nil || p.cfg.TaskOpen(key))
}

// openTasks is how many of st's findings are with a task still at work. It
// asks the task board, so it is not called under deepMu.
func (p *Patrol) openTasks(st deepState) int {
	n := 0
	for _, r := range st.Findings {
		if r.State == StateDispatched && p.taskOpen(r.Task) {
			n++
		}
	}
	return n
}

// auto reports whether the Planner may have r fixed with no word of the
// user's (§15.23 ③): small, reversible, sure, in a repository on GitHub
// where the fix can wait as a pull request; why not otherwise.
func auto(st deepState, r Record) error {
	switch {
	case r.Size != SizeS:
		return errors.New("这条发现不是 S（一个小 PR 改得完）")
	case !r.Reversible:
		return errors.New("这条发现的改动不可逆")
	case r.Confidence != ConfidenceHigh:
		return errors.New("这条发现的把握不是 high")
	case st.Repo == "":
		return errors.New("这个工作区的仓库不在 GitHub 上，开不了 PR")
	}
	return nil
}

// CanDispatch is why the finding ref names may not be dispatched now, nil
// when it may; ws is its workspace. A turn with the user's words (auto
// false) needs only that the finding is on record: the user asked. One
// without is held to what the patrol may do on its own: the finding small,
// reversible and sure, its repository on GitHub, not in a task's or a pull
// request's hands already, and fewer than MaxOpenTasks of the workspace's
// findings with a task at work.
func (p *Patrol) CanDispatch(ref string, unasked bool) (ws string, err error) {
	ws, id, ok := SplitRef(ref)
	if p == nil || p.cfg.Dir == "" || !ok {
		return "", ErrNoFinding
	}
	p.deepMu.Lock()
	st, err := p.loadDeep(ws)
	p.deepMu.Unlock()
	if err != nil {
		return "", err
	}
	r := st.find(id)
	switch {
	case r == nil:
		return "", ErrNoFinding
	case !unasked:
		return ws, nil
	}
	if err := auto(st, *r); err != nil {
		return "", err
	}
	switch {
	case r.State == StateDispatched && p.taskOpen(r.Task):
		return "", fmt.Errorf("这条发现已经派给 %s 在做", r.Task)
	case r.State == StatePR:
		return "", fmt.Errorf("这条发现已有 PR %s", r.PR)
	case r.State == StateMerged || r.State == StateClosed:
		return "", errors.New("这条发现的 PR 已经合并或关闭")
	}
	if n := p.openTasks(st); n >= MaxOpenTasks {
		return "", fmt.Errorf("这个工作区巡查派出的任务已有 %d 个在做（上限 %d）", n, MaxOpenTasks)
	}
	return ws, nil
}

// Dispatched records that the finding ref names was given to session task.
func (p *Patrol) Dispatched(ref, task string) {
	p.record(ref, func(_ *deepState, r *Record) bool {
		r.State, r.Task, r.Pending = StateDispatched, task, false
		return true
	})
}

// record edits the finding ref names, logging what keeps it from being
// written: the caller has acted already.
func (p *Patrol) record(ref string, edit func(st *deepState, r *Record) bool) {
	ws, id, ok := SplitRef(ref)
	if p == nil || p.cfg.Dir == "" || !ok {
		return
	}
	err := p.editDeep(ws, func(st *deepState) bool {
		r := st.find(id)
		return r != nil && edit(st, r)
	})
	if err != nil {
		slog.Warn("planner patrol: a finding's record not updated", "finding", ref, "err", err)
	}
}

// Reported takes the report of session task, the one the finding ref names
// was dispatched to: done with a pull request of the workspace's repository
// among prs, the finding is fixed and waits on the user, and is an open
// item saying so. The item's source stays the finding (ItemSource). A
// report without a pull request leaves the record alone: the Planner has
// the report and decides, and once the task is no longer at work the round
// files the finding as an open item (follow).
func (p *Patrol) Reported(ref, task, state string, prs []string) {
	if state != "done" || len(prs) == 0 {
		return
	}
	p.record(ref, func(st *deepState, r *Record) bool {
		if r.Task != task || r.State != StateDispatched && r.State != StatePR {
			return false
		}
		link, n := "", 0
		for _, u := range prs {
			o, name, num, ok := front.SourcePR(u)
			if ok && (st.Repo == "" || strings.EqualFold(o+"/"+name, st.Repo)) {
				link, n = u, num
				break
			}
		}
		if link == "" {
			return false
		}
		r.State, r.PR, r.Pending = StatePR, link, false
		text := clip(fmt.Sprintf("已备好 PR #%d：%s", n, r.Title), front.ItemRunes)
		if r.Item != "" {
			// One the user struck, or closed since, stays closed.
			if _, err := p.cfg.Items.EditItem(r.Item, &text, nil, nil); err != nil {
				slog.Info("planner patrol: a finding's item not retitled", "finding", ref, "err", err)
			}
			return true
		}
		it, _, err := p.cfg.Items.AddItem(front.Item{Text: text, Workspace: st.Workspace, Source: ItemSource(ref), From: front.FromPatrol})
		if err != nil {
			slog.Warn("planner patrol: a finding with its pull request ready not filed", "finding", ref, "err", err)
			return true
		}
		r.Item = it.ID
		return true
	})
}

// Surface counts the finding ref names as put in front of the user: asked
// about (kind "ask"), or its pull request announced ("pr"). A turn with
// none of the user's words (unasked) is held to MaxSurfaced findings a
// day, all workspaces together, a finding surfaced twice a day counting
// once: past it ErrDailyCap, and nothing is recorded. A turn the user
// started is theirs to ask in and counts for nothing.
func (p *Patrol) Surface(ref, kind string, unasked bool) error {
	ws, id, ok := SplitRef(ref)
	if p == nil || p.cfg.Dir == "" || !ok {
		return ErrNoFinding
	}
	now := p.cfg.Now()
	today := 0
	for _, st := range p.deepStates() {
		for _, r := range st.Findings {
			if sameDay(r.Surfaced, now) {
				today++
			}
		}
	}
	found, capped := false, false
	err := p.editDeep(ws, func(st *deepState) bool {
		r := st.find(id)
		if found = r != nil; !found {
			return false
		}
		if unasked && !sameDay(r.Surfaced, now) && today >= MaxSurfaced {
			capped = true
			return false
		}
		if unasked {
			r.Surfaced = now
		}
		if kind == "ask" && (r.State == StateNew || r.State == StateTold || r.State == StateItem) {
			r.State = StateAsked
		}
		r.Pending = false
		return true
	})
	switch {
	case err != nil:
		return err
	case !found:
		return ErrNoFinding
	case capped:
		return ErrDailyCap
	}
	return nil
}

// File files the finding ref names as an open item of the ledger, once:
// text its sentence ("" for the finding's title), line the front it goes
// under ("" for none). known is false for a ref no finding has, or whose
// record does not read (the error then): the caller files it as any other
// item. filed says the finding has its item, this one or an earlier one;
// a finding in a task's hands, or whose pull request is over, gets none.
func (p *Patrol) File(ref, text, line string) (known, filed bool, err error) {
	ws, id, ok := SplitRef(ref)
	if p == nil || p.cfg.Dir == "" || !ok {
		return false, false, nil
	}
	var failed error
	err = p.editDeep(ws, func(st *deepState) bool {
		r := st.find(id)
		if known = r != nil; !known {
			return false
		}
		if filed = r.Item != ""; filed || !slices.Contains([]string{StateNew, StateTold, StateAsked}, r.State) {
			return false
		}
		if failed = p.fileLocked(st, r, text, line); failed != nil {
			return false
		}
		filed = true
		if r.State != StateAsked {
			r.State = StateItem
		}
		return true
	})
	return known, filed, cmp.Or(err, failed)
}

// fileLocked files r of st as an open item from the patrol, its source the
// finding. Callers hold deepMu.
func (p *Patrol) fileLocked(st *deepState, r *Record, text, line string) error {
	it, _, err := p.cfg.Items.AddItem(front.Item{Text: clip(fold(cmp.Or(text, r.Title)), front.ItemRunes), Workspace: st.Workspace, Front: line,
		Source: ItemSource(Ref(st.Workspace, r.ID)), From: front.FromPatrol})
	if err != nil {
		return err
	}
	r.Item, r.Pending = it.ID, false
	return nil
}

// sameDay reports whether t is on now's calendar day, in now's location.
func sameDay(t, now time.Time) bool {
	if t.IsZero() {
		return false
	}
	y1, m1, d1 := t.In(now.Location()).Date()
	y2, m2, d2 := now.Date()
	return y1 == y2 && m1 == m2 && d1 == d2
}
