// Package patrol is the project patrol's deterministic checks
// (docs/rfc/planner.md §15.23 ①, D142): every hour, with no model, the
// registered workspaces on this node that had a session in the last 7 days
// are looked over — changes left uncommitted, branches pushed and never
// opened as a pull request or merged, worktrees a put-away left, pull
// requests green and waiting, the default branch's CI failing, issues
// nobody touched — and so are the standing tasks failing in a row and
// naozhi's own week. What is found goes to planner/patrol/<workspace ID>.md
// and planner/patrol/naozhi.md, where the Planner reads, and each hit is an
// open item of the ledger (from patrol) that closes when the check no longer
// hits.
//
// The checks only read: no git fetch, nothing written in a workspace. A
// check that could not be made (gh failing, git timing out) leaves its part
// of the file and its items as they were: not being able to look is not the
// thing having gone away.
package patrol

import (
	"cmp"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/planner/front"
	"github.com/naozhi/naozhi/internal/planner/github"
	"github.com/naozhi/naozhi/internal/planner/workspace"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

const (
	// Interval is how often a round runs; firstDelay holds the first one
	// off startup's busiest minutes (planner/github's refresh is at one).
	Interval   = time.Hour
	firstDelay = 2 * time.Minute
	// ActiveDays is how recent a workspace's last session must be for it
	// to be looked over.
	ActiveDays = 7
	// The thresholds of the checks, the spec's.
	dirtyDays    = 3  // an uncommitted change left this long
	branchDays   = 3  // a pushed branch with no pull request, not merged
	prDays       = 7  // a pull request open this long with its CI green
	issueDays    = 30 // an issue nobody touched
	routineFails = 2  // a standing task's failures in a row
	// maxPerCheck caps the items one check files for a workspace (or a
	// repository); the hits past it are in the file only. Forty stale
	// branches are one thing to know, not forty things to do.
	maxPerCheck = 5
	// callTimeout bounds one git or gh call.
	callTimeout = time.Minute
	// stopWait is how long Stop waits for the round in flight.
	stopWait   = 5 * time.Second
	timeLayout = "2006-01-02 15:04"
	day        = 24 * time.Hour
)

// Config wires a Patrol.
type Config struct {
	// Dir is planner/patrol; "" turns the patrol off.
	Dir string
	// Workspaces lists the registered directories on this node
	// (workspace.Registry.LocalDirs).
	Workspaces func() []workspace.Dir
	// Active is when each workspace's last turn of one of the user's
	// sessions ended, by workspace ID. nil leaves every workspace out.
	Active func() map[string]time.Time
	// Items is the ledger's open items (front.Store).
	Items Items
	// Worktrees lists the worktrees a put-away left in place and that are
	// still there by the sidecars. nil checks none.
	Worktrees func() []Worktree
	// Routines lists the standing tasks failing in a row. nil checks none.
	Routines func() []Routine
	// Self counts naozhi's own run in [since, until). nil writes no weekly
	// line.
	Self func(since, until time.Time) Self
	// PausedUntil is until when pushes and custody are paused
	// (notify.Notifier.PausedUntil); no round runs meanwhile.
	PausedUntil func() time.Time
	// Isolated turns the patrol off: an isolated instance does not look
	// into the user's repositories on a schedule (as the scout).
	Isolated bool
	// Exec runs git and gh; nil is github.Exec.
	Exec github.ExecFunc
	// Now is the wall clock; nil is time.Now.
	Now func() time.Time

	// Session runs a deep check's one-shot session (wireup.PatrolSession);
	// nil makes no deep checks (§15.23 ②).
	Session Session
	// Wake hands a workspace's new findings to the Planner, a block a
	// batch, each batch a turn of its own (agent.Agent.WakeEnded from
	// agent.SourcePatrol); sent is told how each later batch went, ended
	// how each batch's turn ended. Whatever the error, the findings go
	// again at the next round, twice in all: a deep check is a week away,
	// so none is dropped for a Planner that is off. nil hands none over.
	Wake func(sent, ended func(batch int, err error), blocks ...string) error
	// Deny is the read rules a deep check's session is denied besides the
	// credential files by name: the Planner's own (agent.Agent.ReadDeny),
	// read as each check starts. nil makes no deep checks: a session that
	// reads a repository does not run without them. It is nil too where the
	// session settings are not naozhi's own: the user's own allow rules
	// would be added to the session's, which are to be all it may read.
	Deny func() []string
	// TaskOpen reports whether dispatched session key is still going: not
	// marked done or failed, not stopped, not gone from 任务
	// (wireup.PatrolTaskOpen). A failure custody sends on is still going.
	// nil counts every one as going.
	TaskOpen func(key string) bool
	// Alarm opens an error in "等你": a file of the patrol's the user
	// edits does not read. nil only logs.
	Alarm func(source, title, why string)
	// Ctx bounds the deep checks' sessions; nil is Background. Stop cuts
	// them as well.
	Ctx context.Context
	// Answer tells the Planner what the user chose about a finding, as a
	// turn the user started whose words are the choice (words, in answer to
	// question): the Planner may dispatch on it (agent.Agent.TellAnswer).
	// nil reaches nobody: a finding turned down stands all the same, one
	// to be done is refused (ErrPlanner).
	Answer func(text, question, words string) error
	// CloseCards closes the cards in "通知" about a finding, saying why
	// (wireup.PatrolCloseCards): it was turned down, or its pull request is
	// over. nil closes none.
	CloseCards func(ref, why string)
}

// Session runs one deep check: key's session started with opts and sent
// text. It returns the reply; the session is gone when it returns.
type Session func(ctx context.Context, key string, opts sessionview.AgentOpts, text string) (reply string, err error)

// Items is the part of the ledger the patrol writes: Reconcile makes a
// check's open items be its hits, CloseItems closes those of a workspace no
// longer registered and of a finding whose pull request is over, AddItem
// and EditItem file a deep check's finding and say its pull request is
// ready.
type Items interface {
	Reconcile(match func(front.Item) bool, want []front.Item, by string) (added, closed []front.Item)
	CloseItems(match func(front.Item) bool, by string) []front.Item
	AddItem(front.Item) (front.Item, bool, error)
	EditItem(id string, text, source, frontID *string) (front.Item, error)
}

// Worktree is a git worktree a put-away session left in place: its
// directory, the repository's common git directory, and why it stayed.
type Worktree struct {
	Dir, GitDir, Why string
}

// Routine is a standing task with failures in a row.
type Routine struct {
	ID, Title string
	// Workspace is the ID of the workspace its directory is in, "" for
	// none.
	Workspace string
	Failures  int
	// AutoPaused says the scheduler paused it for them.
	AutoPaused bool
}

// Self is naozhi's own run over a stretch of time.
type Self struct {
	// ScoutRuns are the scout's runs, ScoutFailed those that failed.
	ScoutRuns, ScoutFailed int
	// Unrun are the Planner's action blocks that were malformed or failed.
	Unrun int
	// CustodyStopped is how often custody stopped at a limit.
	CustodyStopped int
	// Crashes are the starts after a run that did not end normally.
	Crashes int
}

// Patrol runs the rounds.
type Patrol struct {
	cfg Config

	// mu is held by the round in flight: two never overlap. It guards
	// hasPR.
	mu sync.Mutex
	// hasPR are the branches known to have had a pull request, by
	// repository and branch name: a fact that does not change, so the
	// lookup is made once per run of naozhi.
	hasPR map[string]bool

	runMu  sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}

	// deepMu guards the deep checks' records on disk (deep/*.json) and
	// declined.json, each change a read, an edit and a write under it, and
	// the fields below. Four goroutines come here: a deep check's, the
	// round's, the action runner's and a request's. Nothing slow runs
	// under it (no session, no gh, no wake, no telling the Planner, no
	// card closed); the ledger is written under it (deepMu, then
	// front.Store's lock), and the ledger never calls back.
	deepMu sync.Mutex
	// deepRunning is the deep check in flight: one at a time.
	deepRunning bool
	// sending are the findings (by ref) handed to the Planner by this
	// process whose turn has not ended: a round does not hand them over a
	// second time. had are those whose turn ended normally, and when: what
	// the Planner left without a word after its turn is the gateway's to
	// file.
	sending map[string]bool
	had     map[string]time.Time
	// declinedAlarmed says declined.json was unreadable at the last look,
	// and the user was told; stateAlarmed the same of the workspaces'
	// records, by workspace ID.
	declinedAlarmed bool
	stateAlarmed    map[string]bool
	// deepCtx bounds the deep checks' sessions, cut by Stop; runs are the
	// ones in flight, for Stop to wait on.
	deepCtx    context.Context
	deepCancel context.CancelFunc
	runs       sync.WaitGroup
}

// New returns a Patrol; nothing runs until Run, Round or Trigger.
func New(cfg Config) *Patrol {
	if cfg.Exec == nil {
		cfg.Exec = github.Exec
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Ctx == nil {
		cfg.Ctx = context.Background()
	}
	p := &Patrol{cfg: cfg, hasPR: map[string]bool{}, sending: map[string]bool{}, had: map[string]time.Time{}, stateAlarmed: map[string]bool{}}
	p.deepCtx, p.deepCancel = context.WithCancel(cfg.Ctx)
	return p
}

// Enabled reports whether rounds run here.
func (p *Patrol) Enabled() bool {
	return p != nil && p.cfg.Dir != "" && p.cfg.Workspaces != nil && p.cfg.Items != nil && !p.cfg.Isolated
}

// Run makes a round two minutes after start and every Interval after, until
// ctx ends or Stop.
func (p *Patrol) Run(ctx context.Context) {
	if !p.Enabled() {
		slog.Info("planner patrol: off (isolated instance, or no state root)")
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	p.runMu.Lock()
	p.cancel, p.done = cancel, done
	p.runMu.Unlock()
	t := time.NewTimer(firstDelay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		p.Round(ctx)
		t.Reset(Interval)
	}
}

// Stop ends Run and waits for its round in flight, whose git and gh calls
// the cancel cuts, and for the deep check in flight: its session calls into
// the router, so Stop comes before the router's shutdown. A deep check cut
// short leaves no record and runs again after the next start.
func (p *Patrol) Stop() {
	if p == nil {
		return
	}
	// Under deepMu: a deep check starting now either is counted before the
	// wait below or finds the patrol stopped.
	p.deepMu.Lock()
	p.deepCancel()
	p.deepMu.Unlock()
	p.runMu.Lock()
	cancel, done := p.cancel, p.done
	p.runMu.Unlock()
	if cancel != nil {
		cancel()
	}
	gone := make(chan struct{})
	go func() {
		if done != nil {
			<-done
		}
		p.runs.Wait()
		close(gone)
	}()
	select {
	case <-gone:
	case <-time.After(stopWait):
		slog.Warn("planner patrol: round or deep check still going at shutdown")
	}
}

// Round makes one round now, unless one is in flight, pushes and custody
// are paused, or declined.json does not read: such a round looks at
// nothing, writes nothing and leaves every item as it is. A panic ends the
// round, not the process.
func (p *Patrol) Round(ctx context.Context) {
	if !p.Enabled() || !p.mu.TryLock() {
		return
	}
	defer p.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("planner patrol: round panicked", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	now := p.cfg.Now()
	if p.cfg.PausedUntil != nil {
		if until := p.cfg.PausedUntil(); until.After(now) {
			slog.Info("planner patrol: paused with pushes and custody; no round", "until", until)
			return
		}
	}
	// What the user turned down decides what is looked at (§15.23 ④): not
	// knowing it, the round looks at nothing.
	declined, err := p.readDeclined()
	if err != nil {
		return
	}
	p.checks(ctx, now, declined)
	if ctx.Err() != nil {
		return
	}
	// The deep checks ride the hourly round (§15.23 ②): what became of
	// the findings handed on is looked at, then the check due is started.
	p.follow(ctx)
	p.deep(ctx, now, declined)
}

// round is what one round shares between its workspaces.
type round struct {
	now  time.Time
	dirs []workspace.Dir
	// repos are the GitHub repositories looked at, by lowercased
	// owner/name: one that several directories are in is asked once.
	repos map[string]*repoReport
	// own are the pull requests the deep checks' findings led to (ownPulls).
	own map[string]bool
	// failed counts the checks that could not be made; added and closed
	// the items filed and closed.
	failed, added, closed int
}

// checks makes the deterministic checks of one round: the files and items
// of workspaces no longer registered go first, so what another workspace
// still finds is filed again in the same round; then each workspace in
// scope the user has not taken out of the patrol, then the standing tasks
// and naozhi's own week.
func (p *Patrol) checks(ctx context.Context, now time.Time, declined Declined) {
	if err := datadir.EnsureDir(p.cfg.Dir); err != nil {
		slog.Warn("planner patrol: directory unavailable", "dir", p.cfg.Dir, "err", err)
		return
	}
	rd := &round{now: now, dirs: p.cfg.Workspaces(), repos: map[string]*repoReport{}, own: p.ownPulls()}
	var active map[string]time.Time
	if p.cfg.Active != nil {
		active = p.cfg.Active()
	}
	p.prune(rd)
	targets := unmuted(scope(rd.dirs, active, now), declined)
	for _, t := range targets {
		if ctx.Err() != nil {
			return
		}
		p.workspace(ctx, rd, t)
	}
	if ctx.Err() != nil {
		return
	}
	p.own(rd)
	slog.Info("planner patrol: round done", "workspaces", len(targets), "repos", len(rd.repos),
		"checks_failed", rd.failed, "items_filed", rd.added, "items_closed", rd.closed)
}

// target is a workspace a round looks over, with its directories that
// exist.
type target struct {
	ID, Name string
	Dirs     []string
}

// wsRE is a workspace ID: it names the workspace's file, so nothing else
// may be one.
var wsRE = regexp.MustCompile(`^ws_[a-z0-9]{8}$`)

// scope is the workspaces of dirs a round at now looks over, in
// registration order: those with a session's turn in the last ActiveDays
// and a directory still on disk. A workspace out of scope is not looked at
// and keeps its file and its items: the changes nobody touched for eight
// days are still there, and are still worth knowing.
func scope(dirs []workspace.Dir, active map[string]time.Time, now time.Time) []target {
	var out []target
	for _, d := range dirs {
		at, ok := active[d.ID]
		if !ok || now.Sub(at) > ActiveDays*day || !wsRE.MatchString(d.ID) {
			continue
		}
		if fi, err := os.Stat(d.Path); err != nil || !fi.IsDir() {
			continue
		}
		i := slices.IndexFunc(out, func(t target) bool { return t.ID == d.ID })
		if i < 0 {
			i = len(out)
			out = append(out, target{ID: d.ID, Name: d.Name})
		}
		if !slices.Contains(out[i].Dirs, d.Path) {
			out[i].Dirs = append(out[i].Dirs, d.Path)
		}
	}
	return out
}

// workspace looks over t and writes its file and its items.
func (p *Patrol) workspace(ctx context.Context, rd *round, t target) {
	reps := make([]dirReport, 0, len(t.Dirs))
	for _, dir := range t.Dirs {
		reps = append(reps, p.inspect(ctx, rd, t, dir))
	}
	if ctx.Err() != nil {
		// The cancel cut a call short: what it left is not a finding.
		return
	}
	// A workspace of several directories may hold several repositories,
	// each with a branch of one name: there the directory tells the two
	// items apart. Counted over the registered directories, not those on
	// disk today: one unmounted for a round must not file the rest again.
	if n := len(slices.DeleteFunc(slices.Clone(rd.dirs), func(d workspace.Dir) bool { return d.ID != t.ID })); n > 1 {
		for _, r := range reps {
			for i := range r.branches {
				r.branches[i].Source += ":" + r.dir
			}
		}
	}
	// A check's items are the workspace's, whichever of its directories
	// they were found in; one directory it could not be made in leaves the
	// whole check as it was.
	checks := []struct {
		name string
		ok   func(dirReport) bool
		hits func(dirReport) []hit
	}{
		{checkDirty, func(r dirReport) bool { return r.err == nil }, func(r dirReport) []hit { return r.dirty }},
		{checkBranch, func(r dirReport) bool { return r.err == nil && r.branchErr == nil }, func(r dirReport) []hit { return r.branches }},
		{checkWorktree, func(dirReport) bool { return true }, func(r dirReport) []hit { return r.worktrees }},
	}
	for _, c := range checks {
		if c.name == checkWorktree && p.cfg.Worktrees == nil {
			continue // not wired: not checked, its items not this round's to close
		}
		var hits []hit
		ok := true
		for _, r := range reps {
			ok = ok && c.ok(r)
			hits = append(hits, c.hits(r)...)
		}
		if !ok {
			rd.failed++
			continue
		}
		p.reconcile(rd, t.ID, hits, func(it front.Item) bool {
			g, _ := groupOf(it.Source)
			return it.From == front.FromPatrol && it.Workspace == t.ID && g == c.name
		})
		// The cap is over the directories together; each one's section
		// says which of its own hits it left out.
		n := 0
		for _, r := range reps {
			for i := range c.hits(r) {
				c.hits(r)[i].unfiled = hits[n].unfiled
				n++
			}
		}
	}
	path := filepath.Join(p.cfg.Dir, t.ID+".md")
	old, _ := os.ReadFile(path)
	if err := osutil.WriteFileAtomic(path, renderWorkspace(t, rd.now, reps, string(old)), 0o600); err != nil {
		slog.Warn("planner patrol: file not written", "path", path, "err", err)
	}
}

// hit is one thing a check found.
type hit struct {
	// Source is the open item's source, the same every round the thing is
	// still there: it is what tells the item apart.
	Source string
	// Text is the item's text; Line the file's line, which may say more.
	Text, Line string
	// Workspace is the workspace the item is filed under when the check is
	// not one workspace's.
	Workspace string
	// unfiled says the hit is past its check's cap: in the file only.
	unfiled bool
}

// reconcile makes the open items match takes be hits, the first
// maxPerCheck of them, filed under workspace ws; it marks the rest unfiled.
func (p *Patrol) reconcile(rd *round, ws string, hits []hit, match func(front.Item) bool) {
	want := make([]front.Item, 0, min(len(hits), maxPerCheck))
	for i := maxPerCheck; i < len(hits); i++ {
		hits[i].unfiled = true
	}
	for _, h := range hits[:min(len(hits), maxPerCheck)] {
		// The ledger refuses a text or a source past its bounds; a source
		// cannot be cut and still tell the item apart.
		if utf8.RuneCountInString(h.Source) > front.SourceRunes {
			continue
		}
		want = append(want, front.Item{Text: clip(fold(h.Text), front.ItemRunes), Workspace: cmp.Or(h.Workspace, ws),
			Source: h.Source, From: front.FromPatrol})
	}
	added, closed := p.cfg.Items.Reconcile(match, want, front.ByPatrol)
	rd.added += len(added)
	rd.closed += len(closed)
}

// The checks, as the first part of their items' source names them.
const (
	checkDirty    = "dirty"
	checkBranch   = "branch"
	checkWorktree = "worktree"
	checkRoutine  = "routine"
	// The GitHub checks' items are told by the shape of their link.
	checkPR     = "pr"
	checkCI     = "ci"
	checkIssues = "issues"

	sourcePrefix = "check:"
)

var (
	ciRE     = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/actions\?query=branch%3A`)
	issuesRE = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/issues\?q=is%3Aissue\+is%3Aopen\+sort%3Aupdated-asc$`)
	// repoRE is an owner/name fit to be an argument: a fork's parent comes
	// from gh's answer, not from a remote's URL.
	repoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

// groupOf is the check an item's source belongs to, and for a GitHub check
// the repository (lowercased owner/name); "" for a source no deterministic
// check wrote, a deep check's finding ("patrol:…") among them, which no
// round here closes.
func groupOf(source string) (check, repo string) {
	if rest, ok := strings.CutPrefix(source, sourcePrefix); ok {
		name, _, _ := strings.Cut(rest, ":")
		switch name {
		case checkDirty, checkBranch, checkWorktree, checkRoutine:
			return name, ""
		}
		return "", ""
	}
	if o, r, _, ok := front.SourcePR(source); ok {
		return checkPR, strings.ToLower(o + "/" + r)
	}
	if m := ciRE.FindStringSubmatch(source); m != nil {
		return checkCI, strings.ToLower(m[1] + "/" + m[2])
	}
	if m := issuesRE.FindStringSubmatch(source); m != nil {
		return checkIssues, strings.ToLower(m[1] + "/" + m[2])
	}
	return "", ""
}

// prune removes the files of workspaces no longer registered — the checks'
// result and the deep checks' record — and closes the patrol's items filed
// under them, the deep checks' findings among them: nothing looks at those
// again, and a finding left on record would still be handed to the Planner
// for a workspace it cannot dispatch into. naozhi.md and whatever else
// lives in the directory stay. A registry with nothing on this node prunes
// nothing: that is one that could not be read, or a node whose ID changed,
// far more often than every workspace gone at once, and the files and
// items of the workspaces out of scope would not come back.
func (p *Patrol) prune(rd *round) {
	if len(rd.dirs) == 0 {
		return
	}
	registered := map[string]bool{}
	for _, d := range rd.dirs {
		registered[d.ID] = true
	}
	gone := func(dir, suffix string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			id, ok := strings.CutSuffix(e.Name(), suffix)
			if e.IsDir() || !ok || !wsRE.MatchString(id) || registered[id] {
				continue
			}
			if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
				slog.Info("planner patrol: workspace no longer registered; file removed", "file", e.Name())
			}
		}
	}
	gone(p.cfg.Dir, ".md")
	p.deepMu.Lock()
	gone(filepath.Join(p.cfg.Dir, deepDir), ".json")
	p.deepMu.Unlock()
	closed := p.cfg.Items.CloseItems(func(it front.Item) bool {
		g, _ := groupOf(it.Source)
		return it.From == front.FromPatrol && (g != "" || strings.HasPrefix(it.Source, front.PatrolSource)) && it.Workspace != "" && !registered[it.Workspace]
	}, front.ByPatrol)
	rd.closed += len(closed)
}

// call runs name once under the call timeout.
func (p *Patrol) call(ctx context.Context, dir, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	return p.cfg.Exec(ctx, dir, name, args...)
}
