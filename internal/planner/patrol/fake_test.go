package patrol

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/planner/front"
	"github.com/naozhi/naozhi/internal/planner/workspace"
)

// now is the rounds' clock in the tests that run no real git.
var now = time.Date(2026, 11, 23, 14, 2, 0, 0, time.FixedZone("CST", 8*3600))

// fakeRepo is what git answers in one directory.
type fakeRepo struct {
	// status is `git status --porcelain=v1 -z`.
	status string
	// remotes is `git remote -v`; "" has none.
	remotes string
	// head is origin/HEAD's target ("origin/main"); "" is not set.
	head string
	// remoteRefs are the branch names on origin.
	remoteRefs []string
	// branches are the lines of the unmerged local branches.
	branches []string
	// linked says the directory is a linked worktree.
	linked bool
	// fail is the git subcommand that fails here.
	fail string
}

// fakeGH is what gh answers for one repository.
type fakeGH struct {
	// def is the default branch; full is owner/name as GitHub spells it,
	// "" for the spelling of the key.
	def, full           string
	pulls, runs, issues string
	// heads answers `gh pr list --head=<branch>` per branch; missing: [].
	heads map[string]string
	// parent is the repository this one is a fork of ("owner/name").
	parent string
	// fail is the "<command> <subcommand>" that fails ("pr list"); "head"
	// fails the per-branch lookups only.
	fail string
}

// fakeExec answers git per directory and gh per repository, and keeps the
// calls made.
type fakeExec struct {
	mu    sync.Mutex
	repos map[string]*fakeRepo // dir → repo; missing: not a git repository
	gh    map[string]*fakeGH   // "owner/name"
	calls []string
}

func (f *fakeExec) run(_ context.Context, dir, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	switch name {
	case "git":
		r, ok := f.repos[dir]
		if !ok {
			return "", errors.New("git rev-parse: fatal: not a git repository (or any of the parent directories): .git")
		}
		if r.fail == args[0] {
			return "", errors.New("git " + args[0] + ": 超时")
		}
		switch args[0] {
		case "rev-parse":
			gitDir := filepath.Join(dir, ".git")
			if r.linked {
				return dir + "\n" + filepath.Join(filepath.Dir(dir), "main", ".git", "worktrees", "x") + "\n" + filepath.Join(filepath.Dir(dir), "main", ".git") + "\n", nil
			}
			return dir + "\n" + gitDir + "\n.git\n", nil
		case "status":
			return r.status, nil
		case "remote":
			return r.remotes, nil
		case "symbolic-ref":
			if r.head == "" {
				return "", errors.New("git symbolic-ref: fatal: ref refs/remotes/origin/HEAD is not a symbolic ref")
			}
			return r.head + "\n", nil
		case "for-each-ref":
			if args[len(args)-1] == "refs/remotes/origin" {
				var b strings.Builder
				for _, n := range r.remoteRefs {
					b.WriteString("refs/remotes/origin/" + n + "\n")
				}
				return b.String(), nil
			}
			return strings.Join(r.branches, "\n"), nil
		}
	case "gh":
		repo := args[len(args)-1]
		if i := slices.Index(args, "-R"); i >= 0 {
			repo = args[i+1]
		}
		g, ok := f.gh[strings.ToLower(repo)]
		if !ok {
			return "", errors.New("gh " + args[0] + ": repository not found")
		}
		cmd := args[0] + " " + args[1]
		head, lookup := "", false
		for _, a := range args {
			if b, ok := strings.CutPrefix(a, "--head="); ok {
				head, lookup = b, true
			}
		}
		if g.fail == cmd && !lookup || g.fail == "head" && lookup {
			return "", errors.New("gh " + args[0] + ": error connecting to api.github.com")
		}
		switch {
		case cmd == "repo view" && g.parent != "":
			owner, name, _ := strings.Cut(g.parent, "/")
			return `{"nameWithOwner":"` + strings.ToLower(repo) + `","defaultBranchRef":{"name":"` + g.def + `"},"parent":{"name":"` + name + `","owner":{"login":"` + owner + `"}}}`, nil
		case lookup:
			if out, ok := g.heads[head]; ok {
				return out, nil
			}
			return "[]", nil
		case cmd == "repo view":
			return `{"nameWithOwner":"` + cmp.Or(g.full, strings.ToLower(repo)) + `","defaultBranchRef":{"name":"` + g.def + `"}}`, nil
		case cmd == "pr list":
			return orEmpty(g.pulls), nil
		case cmd == "run list":
			if i := slices.Index(args, "--event"); i >= 0 {
				return byEvent(orEmpty(g.runs), args[i+1])
			}
			return orEmpty(g.runs), nil
		case cmd == "issue list":
			return orEmpty(g.issues), nil
		}
	}
	return "", errors.New("unexpected " + name + " " + strings.Join(args, " "))
}

// byEvent is the runs of a JSON list an event triggered, as gh's --event
// keeps them.
func byEvent(runs, event string) (string, error) {
	var all, kept []map[string]any
	if err := json.Unmarshal([]byte(runs), &all); err != nil {
		return "", err
	}
	for _, r := range all {
		if r["event"] == event {
			kept = append(kept, r)
		}
	}
	out, err := json.Marshal(append([]map[string]any{}, kept...))
	return string(out), err
}

func orEmpty(s string) string {
	if s == "" {
		return "[]"
	}
	return s
}

// count is how many calls made start with prefix.
func (f *fakeExec) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// branchLine is one line of the unmerged local branches as git lists them;
// upstream is a remote-tracking branch ("origin/x") or a full ref name.
func branchLine(name, upstream string, tip time.Time, worktree string) string {
	if upstream != "" && !strings.HasPrefix(upstream, "refs/") {
		upstream = "refs/remotes/" + upstream
	}
	return strings.Join([]string{"refs/heads/" + name, upstream, "", strconv.FormatInt(tip.Unix(), 10), worktree}, "\x00")
}

// env is a patrol over fake git and gh, a real ledger and a state root in a
// temporary directory.
type env struct {
	t      *testing.T
	root   string
	fe     *fakeExec
	items  *front.Store
	dirs   []workspace.Dir
	active map[string]time.Time
	paused time.Time
	cfg    Config
	p      *Patrol
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, root: root, fe: &fakeExec{repos: map[string]*fakeRepo{}, gh: map[string]*fakeGH{}}, active: map[string]time.Time{}}
	e.items = front.New(filepath.Join(root, "planner", "fronts.json"), func() time.Time { return now })
	e.cfg = Config{
		Dir:         filepath.Join(root, "planner", "patrol"),
		Workspaces:  func() []workspace.Dir { return e.dirs },
		Active:      func() map[string]time.Time { return e.active },
		Items:       e.items,
		PausedUntil: func() time.Time { return e.paused },
		Exec:        e.fe.run,
		Now:         func() time.Time { return now },
	}
	e.p = New(e.cfg)
	return e
}

// workspace registers a directory named name as workspace id, with a
// session a day ago, and returns its path.
func (e *env) workspace(id, name string) string {
	e.t.Helper()
	dir := filepath.Join(e.root, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		e.t.Fatal(err)
	}
	e.dirs = append(e.dirs, workspace.Dir{ID: id, Name: name, Path: dir})
	if _, ok := e.active[id]; !ok {
		e.active[id] = now.Add(-day)
	}
	return dir
}

// file writes dir/rel last touched age ago.
func (e *env) file(dir, rel string, age time.Duration) {
	e.t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		e.t.Fatal(err)
	}
	at := now.Add(-age)
	if err := os.Chtimes(path, at, at); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) round() {
	e.t.Helper()
	e.p.Round(context.Background())
}

// read is a file under planner/patrol, "" when there is none.
func (e *env) read(name string) string {
	data, _ := os.ReadFile(filepath.Join(e.cfg.Dir, name))
	return string(data)
}

// open is the open items' "source|text" lines, sorted.
func (e *env) open() []string {
	var out []string
	for _, it := range e.items.Items() {
		out = append(out, it.Source+"|"+it.Text)
	}
	slices.Sort(out)
	return out
}
