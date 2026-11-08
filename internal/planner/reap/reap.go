// Package reap puts away the sessions the Planner dispatched once their task
// is done (docs/rfc/planner.md §5.2, D95). A task: session marked 已完成 —
// by the Planner's status block or by the user in 任务 — is put away when the
// gateway sees, deterministically, that its turn ended normally, nothing
// waits on an AskUserQuestion, nothing runs in the background, the user is
// not at it (custody's "你在旁边": viewing it, or a message to it in the last
// 5 minutes), and it has been idle IdleFor (D105). Until then it waits for
// the next state change or check; nothing is reported.
//
// Putting away takes two steps. First the process is closed and the session
// leaves the session list for the history (Router.Archive): its event log,
// transcript and session ID stay, so reopening it in 任务 puts it back under
// its key and the next message resumes it. Then each git worktree it worked
// in (the directories its transcript ran in) is removed, only once the
// worktree has nothing uncommitted, its branch is on the remote and the
// branch's PR is merged or closed; a worktree with work not committed or not
// pushed is never removed and the user is told once per reason. A worktree
// waiting on its PR is looked at again every recheckAfter. Every step and
// every step not taken is journaled (journal.KindReap) and shown on the task.
package reap

import (
	"cmp"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/planner/task"
)

const (
	// tickInterval is how often the done tasks are looked at.
	tickInterval = time.Minute
	// recheckAfter is how long a worktree left in place waits before it is
	// looked at again (a PR call each time).
	recheckAfter = 10 * time.Minute
	// window is how recent a done mark, or the session's last activity
	// after it, must be for its session to be put away: while the task is
	// still listed (task.Archive), so the user can reopen it there. Counting
	// from the activity too keeps a done task that is talked to for most of
	// a day from outliving its window idle and never being put away.
	window = task.Archive
	// gitTimeout bounds one worktree's checks.
	gitTimeout = time.Minute
	// IdleFor is how long a done session must have been idle (no turn
	// started or ended, whoever sent it) before it is put away (D105): a
	// follow-up from the user, the Planner or the gateway soon after the
	// done mark finds it still there.
	IdleFor = 2 * time.Hour
)

// Worktree states (sidecar.Worktree.State).
const (
	WTPending = "pending" // left in place until its PR is merged or closed
	WTKept    = "kept"    // left in place: work not committed or not pushed
	WTRemoved = "removed"
	WTGone    = "gone" // its directory went away by other hands
)

// Session is what the checks read of a live session.
type Session struct {
	// Running is a turn in progress; Asking an AskUserQuestion unanswered;
	// Background a workflow or background task running.
	Running    bool
	Asking     bool
	Background bool
	// Failed is the error the last turn ended in ("" when it ended normally).
	Failed string
	// LastActive is when a turn last started or ended; zero holds nothing.
	LastActive time.Time
}

// Archived is what Config.Archive took out: the session to resume.
type Archived struct {
	SessionID  string
	Workspace  string
	LastPrompt string
	// Chain are the session IDs the conversation went through.
	Chain []string
}

// Status is a worktree's git state: anything uncommitted (untracked files
// too), its HEAD, and whether HEAD is on a remote-tracking branch.
type Status struct {
	Dirty  bool
	Head   string
	Pushed bool
}

// PR is a branch's pull request: State is GitHub's (OPEN, MERGED, CLOSED),
// "" when the branch has none; Head the commit it points at.
type PR struct {
	State string
	Head  string
	URL   string
}

// Git is the git and GitHub work (git.go's Exec in production).
type Git interface {
	// Linked resolves dir to the linked worktree holding it: its top
	// directory, the repository's common git directory and the branch
	// checked out ("" detached); false for a main worktree or no repository.
	Linked(ctx context.Context, dir string) (top, gitDir, branch string, ok bool)
	Status(ctx context.Context, top string) (Status, error)
	PR(ctx context.Context, top, branch string) (PR, error)
	// Remove removes the worktree, refusing one with changes.
	Remove(ctx context.Context, gitDir, top string) error
	// Add checks branch out again at top.
	Add(ctx context.Context, gitDir, top, branch string) error
}

// Inbox is where the user is told a worktree stays (notify.Notifier).
type Inbox interface {
	Add(it notify.Item) (notify.Item, bool)
}

// Config wires a Reaper. Sessions, Session, Archive and Git are required.
type Config struct {
	Sessions *sidecar.Store
	// Session reads key's live session; false when it is not in the router.
	Session func(key string) (Session, bool)
	// LastActive is key's Session.LastActive alone, read before Session to
	// leave out a done task past its window without Session's event log
	// scans; false when key is not in the router. nil reads Session.
	LastActive func(key string) (time.Time, bool)
	// Nearby reports whether the user is at key (custody.Engine.Nearby).
	Nearby func(key string) bool
	// Archive closes key's process and takes it out of the session list,
	// keeping its history (Router.Archive).
	Archive func(key string) (Archived, bool)
	// Restore puts a session back under key, to resume sessionID
	// (Router.RegisterForResume).
	Restore func(key, sessionID, workspace, lastPrompt string) error
	// Dirs are the directories sessionIDs' transcripts ran in.
	Dirs func(workspace string, sessionIDs []string) []string
	// InUse reports whether a session other than key works in dir.
	InUse func(dir, key string) bool
	Git   Git
	Inbox Inbox
	// Journal records each step; nil records none.
	Journal *journal.Journal
	// Name is how the user reads key; nil uses the task's title.
	Name func(key string) string
	Now  func() time.Time
}

// Reaper puts done tasks' sessions away. Safe for concurrent use.
type Reaper struct {
	cfg  Config
	poke chan struct{}

	// mu serializes putting away, reopening and the worktree checks.
	mu sync.Mutex
	// wait is why each done session is not put away yet.
	waitMu sync.Mutex
	wait   map[string]waiting
	// checked is when each worktree left in place was last looked at.
	checked map[string]time.Time
}

// waiting is why a done session is not put away yet, and when it will be
// when idleness is the only thing left holding it (zero otherwise).
type waiting struct {
	why string
	at  time.Time
}

// New returns a Reaper.
func New(cfg Config) *Reaper {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Reaper{cfg: cfg, poke: make(chan struct{}, 1), wait: map[string]waiting{}, checked: map[string]time.Time{}}
}

// Poke has the next look run now. It never blocks.
func (r *Reaper) Poke() {
	select {
	case r.poke <- struct{}{}:
	default:
	}
}

// OnState is task.Config.OnState: done looks for sessions to put away,
// doing puts key back if it was put away.
func (r *Reaper) OnState(key string, state task.State) {
	switch state {
	case task.Done:
		r.Poke()
	case task.Doing:
		r.Reopen(context.Background(), key)
	}
}

// Wait is task.Config.ReapWait: why done session key is not put away yet,
// and when it is expected to be if only its idleness holds it (zero when
// something else does, or nothing).
func (r *Reaper) Wait(key string) (string, time.Time) {
	r.waitMu.Lock()
	defer r.waitMu.Unlock()
	w := r.wait[key]
	return w.why, w.at
}

func (r *Reaper) setWait(key string, w waiting) {
	r.waitMu.Lock()
	defer r.waitMu.Unlock()
	if w.why == "" {
		delete(r.wait, key)
	} else {
		r.wait[key] = w
	}
}

// Run looks every tickInterval, and when poked, until ctx ends.
func (r *Reaper) Run(ctx context.Context) {
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for {
		r.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-r.poke:
		}
	}
}

// Tick puts away every done session that passes its checks, and looks again
// at the worktrees left in place that are due.
func (r *Reaper) Tick(ctx context.Context) {
	for _, m := range r.cfg.Sessions.List() {
		if ctx.Err() != nil {
			return
		}
		if m.Purpose != sidecar.PurposeTask {
			continue
		}
		if m.Reap == nil {
			r.tryPutAway(ctx, m.Key)
		} else {
			r.recheck(ctx, m.Key, false)
		}
	}
}

// done reports whether m is a done task not put away yet.
func done(m sidecar.Meta) bool {
	return m.Purpose == sidecar.PurposeTask && m.Reap == nil && m.Mark != nil &&
		task.State(m.Mark.State) == task.Done
}

// due reports whether done task m, its session last active at lastActive,
// is still within window.
func (r *Reaper) due(m sidecar.Meta, lastActive time.Time) bool {
	from := m.Mark.At
	if lastActive.After(from) {
		from = lastActive
	}
	return r.cfg.Now().Sub(from) < window
}

// mayBeDue is due read cheaply: false only when done task key is surely past
// window or not in the router. A done task left in the router for days (a
// mark from before an upgrade, a turn that ended in an error) is then looked
// at every tick without the full Session read.
func (r *Reaper) mayBeDue(m sidecar.Meta, key string) bool {
	if r.cfg.LastActive == nil || r.due(m, time.Time{}) {
		return true
	}
	la, ok := r.cfg.LastActive(key)
	return ok && r.due(m, la)
}

// holds is why s is not to be put away yet ("" when nothing holds it), and
// when it will be if the last check, idleness, is what holds it.
func (r *Reaper) holds(key string, s Session) waiting {
	switch {
	case s.Running:
		return waiting{why: "这一轮还没结束"}
	case s.Failed != "":
		return waiting{why: "这一轮出错结束"}
	case s.Asking:
		return waiting{why: "有提问没回答"}
	case s.Background:
		return waiting{why: "后台还有任务在跑"}
	case r.cfg.Nearby != nil && r.cfg.Nearby(key):
		return waiting{why: "你正看着它，或 5 分钟内给它发过消息"}
	case !s.LastActive.IsZero() && r.cfg.Now().Sub(s.LastActive) < IdleFor:
		return waiting{why: "空闲不到 2 小时", at: s.LastActive.Add(IdleFor)}
	}
	return waiting{}
}

func (r *Reaper) tryPutAway(ctx context.Context, key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.cfg.Sessions.Get(key)
	if !ok || !done(m) {
		r.setWait(key, waiting{})
		return
	}
	if !r.mayBeDue(m, key) {
		r.setWait(key, waiting{})
		return
	}
	s, ok := r.cfg.Session(key)
	if !ok || !r.due(m, s.LastActive) {
		// Not in the session list: there is no process to close or session
		// to put away. Past window: no longer listed to be reopened.
		r.setWait(key, waiting{})
		return
	}
	if w := r.holds(key, s); w.why != "" {
		if why, _ := r.Wait(key); why != w.why {
			slog.Info("reap: done task not put away yet", "key", key, "why", w.why)
		}
		r.setWait(key, w)
		return
	}
	r.setWait(key, waiting{})
	a, ok := r.cfg.Archive(key)
	if !ok {
		return
	}
	now := r.cfg.Now()
	rp := &sidecar.Reap{At: now, By: m.Mark.By, Why: m.Mark.Why, SessionID: a.SessionID, Workspace: a.Workspace, LastPrompt: a.LastPrompt}
	rp.Worktrees = r.worktrees(ctx, a)
	rec := record{Event: "reaped", Key: key, Trigger: m.Mark.By, Why: m.Mark.Why, Steps: []string{"关掉进程，会话移到历史会话"}}
	if len(rp.Worktrees) == 0 {
		rec.Skipped = append(rec.Skipped, skip{Step: "worktree", Why: "不在 worktree 里干活"})
	}
	if _, err := r.cfg.Sessions.Update(key, func(m *sidecar.Meta) bool { m.Reap = rp; return true }); err != nil {
		slog.Warn("reap: put-away session not recorded; reopening it will not resume it", "key", key, "err", err)
	}
	slog.Info("reap: done task's session put away", "key", key, "by", m.Mark.By, "worktrees", len(rp.Worktrees))
	r.checkLocked(ctx, key, true, &rec)
	r.journal(rec)
}

// worktrees are the linked worktrees a's transcripts ran in, its workspace
// first.
func (r *Reaper) worktrees(ctx context.Context, a Archived) []sidecar.Worktree {
	dirs := []string{a.Workspace}
	if r.cfg.Dirs != nil {
		ids := slices.Clone(a.Chain)
		if a.SessionID != "" && !slices.Contains(ids, a.SessionID) {
			ids = append(ids, a.SessionID)
		}
		dirs = append(dirs, r.cfg.Dirs(a.Workspace, ids)...)
	}
	var out []sidecar.Worktree
	seen := map[string]bool{}
	now := r.cfg.Now()
	for _, d := range dirs {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		cctx, cancel := context.WithTimeout(ctx, gitTimeout)
		top, gitDir, branch, ok := r.cfg.Git.Linked(cctx, d)
		cancel()
		if !ok || seen["top:"+top] {
			continue
		}
		seen["top:"+top] = true
		out = append(out, sidecar.Worktree{Dir: top, GitDir: gitDir, Branch: branch, State: WTPending, At: now})
	}
	return out
}

// recheck looks again at key's worktrees left in place, those due unless
// force.
func (r *Reaper) recheck(ctx context.Context, key string, force bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := record{Event: "worktree", Key: key}
	r.checkLocked(ctx, key, force, &rec)
	if len(rec.Steps) > 0 || len(rec.Skipped) > 0 {
		r.journal(rec)
	}
}

// checkLocked looks at key's worktrees left in place and removes those that
// may go; rec gets each removal and each worktree whose state or reason
// changed. With force every one is looked at, else those due.
func (r *Reaper) checkLocked(ctx context.Context, key string, force bool, rec *record) {
	m, ok := r.cfg.Sessions.Get(key)
	if !ok || m.Reap == nil {
		return
	}
	now := r.cfg.Now()
	wts := slices.Clone(m.Reap.Worktrees)
	changed := false
	for i := range wts {
		wt := &wts[i]
		if wt.State != WTPending && wt.State != WTKept {
			continue
		}
		if !force && now.Sub(r.checked[wt.Dir]) < recheckAfter {
			continue
		}
		r.checked[wt.Dir] = now
		state, why := r.judge(ctx, key, *wt)
		if state == WTRemoved || state == WTGone {
			delete(r.checked, wt.Dir)
		}
		if state == wt.State && why == wt.Why {
			continue
		}
		wt.State, wt.Why, wt.At = state, why, now
		changed = true
		if state == WTRemoved {
			rec.Steps = append(rec.Steps, "删除 worktree "+wt.Dir)
		} else {
			rec.Skipped = append(rec.Skipped, skip{Step: "worktree", Dir: wt.Dir, Why: why})
		}
		if state == WTKept && wt.Notified != why {
			wt.Notified = why
			r.tell(key, m, *wt)
		}
	}
	if !changed {
		return
	}
	if _, err := r.cfg.Sessions.Update(key, func(m *sidecar.Meta) bool {
		if m.Reap == nil {
			return false
		}
		m.Reap.Worktrees = wts
		return true
	}); err != nil {
		slog.Warn("reap: worktree state not saved", "key", key, "err", err)
	}
}

// judge decides wt's state: removed (it was, here), gone, kept (work not
// committed or not pushed) or pending (no PR merged or closed yet, or a check
// that did not answer), with why.
func (r *Reaper) judge(ctx context.Context, key string, wt sidecar.Worktree) (string, string) {
	if _, err := os.Stat(wt.Dir); os.IsNotExist(err) {
		return WTGone, "目录已经不在了"
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	st, err := r.cfg.Git.Status(ctx, wt.Dir)
	if err != nil {
		return WTPending, "读不到 git 状态：" + short(err)
	}
	if st.Dirty {
		return WTKept, "有未提交的改动"
	}
	if wt.Branch == "" {
		if !st.Pushed {
			return WTKept, "有没推送的提交"
		}
		return WTPending, "不在分支上，找不到对应的 PR"
	}
	pr, err := r.cfg.Git.PR(ctx, wt.Dir, wt.Branch)
	if err != nil {
		if !st.Pushed {
			return WTKept, "有没推送的提交"
		}
		return WTPending, "查不到 PR：" + short(err)
	}
	if !st.Pushed && (pr.Head == "" || pr.Head != st.Head) {
		return WTKept, "有没推送的提交"
	}
	switch pr.State {
	case "MERGED", "CLOSED":
	case "":
		return WTPending, "分支还没有 PR"
	default:
		return WTPending, "PR 还没合并或关闭"
	}
	if r.cfg.InUse != nil && r.cfg.InUse(wt.Dir, key) {
		return WTPending, "另有会话在这个目录里"
	}
	if err := r.cfg.Git.Remove(ctx, wt.GitDir, wt.Dir); err != nil {
		return WTPending, "删除失败：" + short(err)
	}
	return WTRemoved, "PR 已" + map[string]string{"MERGED": "合并", "CLOSED": "关闭"}[pr.State]
}

// tell tells the user a worktree stays and why.
func (r *Reaper) tell(key string, m sidecar.Meta, wt sidecar.Worktree) {
	if r.cfg.Inbox == nil {
		return
	}
	name := cmp.Or(m.Title, key)
	if r.cfg.Name != nil {
		name = cmp.Or(r.cfg.Name(key), name)
	}
	r.cfg.Inbox.Add(notify.Item{Kind: notify.KindNotice, Level: notify.LevelTimely, Unit: key, WS: m.Workspace,
		Source: "reap:" + wt.Dir, Title: name + " 已收起，worktree 没删",
		Why: wt.Why + "：" + wt.Dir + "。提交并推送后，等 PR 合并或关闭会自动删除。"})
}

// Reopen puts key's session back, if it was put away: under its key in the
// session list, to resume its session, and the worktrees removed checked
// out again on their branches. Done when the task is set back to 进行中.
func (r *Reaper) Reopen(ctx context.Context, key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.cfg.Sessions.Get(key)
	if !ok || m.Reap == nil {
		return
	}
	rp := *m.Reap
	rec := record{Event: "reopened", Key: key}
	if rp.SessionID == "" {
		rec.Skipped = append(rec.Skipped, skip{Step: "session", Why: "没有记下会话 ID"})
	} else if r.cfg.Restore != nil {
		if err := r.cfg.Restore(key, rp.SessionID, rp.Workspace, rp.LastPrompt); err != nil {
			rec.Skipped = append(rec.Skipped, skip{Step: "session", Why: "放回失败：" + short(err)})
		} else {
			rec.Steps = append(rec.Steps, "会话放回会话列表，下一条消息接着上下文继续")
		}
	}
	for _, wt := range rp.Worktrees {
		delete(r.checked, wt.Dir)
		if wt.State != WTRemoved {
			continue
		}
		if _, err := os.Stat(wt.Dir); err == nil {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, gitTimeout)
		err := r.cfg.Git.Add(cctx, wt.GitDir, wt.Dir, wt.Branch)
		cancel()
		if err != nil {
			rec.Skipped = append(rec.Skipped, skip{Step: "worktree", Dir: wt.Dir, Why: "没能重新检出：" + short(err)})
		} else {
			rec.Steps = append(rec.Steps, "重新检出 worktree "+wt.Dir)
		}
	}
	if _, err := r.cfg.Sessions.Update(key, func(m *sidecar.Meta) bool {
		m.Reap, m.Retired = nil, time.Time{}
		return true
	}); err != nil {
		slog.Warn("reap: reopen not recorded", "key", key, "err", err)
	}
	slog.Info("reap: put-away session reopened", "key", key)
	r.journal(rec)
}

// record is a KindReap journal entry: the session, what set it done
// (Trigger, Why), the steps taken and the ones not.
type record struct {
	Event   string   `json:"event"` // reaped | worktree | reopened
	Key     string   `json:"key"`
	Trigger string   `json:"trigger,omitempty"`
	Why     string   `json:"why,omitempty"`
	Steps   []string `json:"steps,omitempty"`
	Skipped []skip   `json:"skipped,omitempty"`
}

type skip struct {
	Step string `json:"step"`
	Dir  string `json:"dir,omitempty"`
	Why  string `json:"why"`
}

func (r *Reaper) journal(rec record) {
	if r.cfg.Journal == nil {
		return
	}
	data, err := json.Marshal(rec)
	if err == nil {
		err = r.cfg.Journal.Append(journal.Entry{TS: r.cfg.Now(), Kind: journal.KindReap, Unit: rec.Key, Data: data})
	}
	if err != nil {
		slog.Warn("reap: journal append failed", "key", rec.Key, "err", err)
	}
}

// short is err's first line, cut to a reason's length.
func short(err error) string {
	s, _, _ := strings.Cut(strings.TrimSpace(err.Error()), "\n")
	if len([]rune(s)) > 200 {
		s = string([]rune(s)[:200]) + "…"
	}
	return s
}
