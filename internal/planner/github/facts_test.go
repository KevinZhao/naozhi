package github

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/planner/front"
	"github.com/naozhi/naozhi/internal/planner/workspace"
)

// factExec answers git remote -v for one repository's directory, the issue
// lists with none, and gh pr|issue view by "<kind> <owner/repo>#<n>".
type factExec struct {
	mu     sync.Mutex
	dir    string
	states map[string]string // "pr owner/repo#12" → state; missing: gh fails
	views  []string
}

func (f *factExec) run(_ context.Context, dir, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case name == "git" && dir == f.dir:
		return "origin\tgit@github.com:owner/repo.git (fetch)\n", nil
	case name == "git":
		return "", errors.New("git remote: fatal: not a git repository (or any of the parent directories): .git")
	case name == "gh" && args[1] == "list":
		return "[]", nil
	case name == "gh" && args[1] == "view":
		key := args[0] + " " + args[slices.Index(args, "-R")+1] + "#" + args[2]
		f.views = append(f.views, key)
		if st, ok := f.states[key]; ok {
			return `{"state":"` + st + `"}`, nil
		}
		return "", errors.New("gh " + args[0] + ": error connecting to api.github.com")
	}
	return "", errors.New("unexpected " + name + " " + strings.Join(args, " "))
}

// An open item whose source is a pull request closes when it is merged or
// closed, one whose source is an issue when the issue is closed, by the
// fact (§15.23, D143). "#n" is an issue of the item's workspace's
// repository. One still open, one gh could not look up, one of a workspace
// with no GitHub repository and one whose source is no such thing stay.
// Two items naming one pull request cost one lookup.
func TestSettle(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "repo")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	ledger := front.New("", func() time.Time { return now })
	add := func(text, ws, source string) string {
		t.Helper()
		now = now.Add(time.Second)
		it, _, err := ledger.AddItem(front.Item{Text: text, Workspace: ws, Source: source, From: front.FromPlanner})
		if err != nil {
			t.Fatal(err)
		}
		return it.ID
	}
	merged := add("PR 12 合了就没事了", "", "https://github.com/owner/repo/pull/12")
	merged2 := add("PR 12 的另一条", "ws_ab12cd34", "https://github.com/Owner/Repo/pull/12/files")
	closedPR := add("PR 13 关了", "", "https://github.com/owner/repo/pull/13")
	openPR := add("PR 14 还开着", "", "https://github.com/owner/repo/pull/14")
	issue := add("issue 7 关了", "", "https://github.com/other/tool/issues/7")
	hash := add("#8 关了", "ws_ab12cd34", "#8")
	hashOpen := add("#9 还开着", "ws_ab12cd34", "#9")
	noRepo := add("别的工作区的 #8", "ws_zz99zz99", "#8")
	noWorkspace := add("没有工作区的 #8", "", "#8")
	failed := add("gh 查不到", "", "https://github.com/owner/repo/pull/15")
	session := add("会话留下的", "ws_ab12cd34", "task:9f3c2a71d0")

	fe := &factExec{dir: dir, states: map[string]string{
		"pr owner/repo#12":   "MERGED",
		"pr owner/repo#13":   "CLOSED",
		"pr owner/repo#14":   "OPEN",
		"issue other/tool#7": "CLOSED",
		"issue owner/repo#8": "CLOSED",
		"issue owner/repo#9": "OPEN",
	}}
	w := New(Config{Dir: filepath.Join(root, "github"), Ledger: ledger, Exec: fe.run, Now: func() time.Time { return now },
		Workspaces: func() []workspace.Dir { return []workspace.Dir{{ID: "ws_ab12cd34", Name: "repo", Path: dir}} }})
	w.Refresh(context.Background())

	var open []string
	for _, it := range ledger.Items() {
		open = append(open, it.ID)
	}
	if want := []string{openPR, hashOpen, noRepo, noWorkspace, failed, session}; !slices.Equal(open, want) {
		t.Fatalf("open after the refresh = %v, want %v (closed: %v)", open, want, []string{merged, merged2, closedPR, issue, hash})
	}
	for _, id := range []string{merged, merged2, closedPR, issue, hash} {
		// Closing a closed item again returns it as it was closed (only the
		// user's strike would take it over).
		if it, err := ledger.CloseItem(id, front.ByPlanner); err != nil || it.ClosedBy != front.ByFact {
			t.Errorf("item %s: %+v, %v, want closed by the fact", id, it, err)
		}
	}
	// One lookup per source, the first item's spelling of it; none for a
	// source with no repository to ask.
	slices.Sort(fe.views)
	if want := []string{"issue other/tool#7", "issue owner/repo#8", "issue owner/repo#9", "pr owner/repo#12", "pr owner/repo#13", "pr owner/repo#14", "pr owner/repo#15"}; !slices.Equal(fe.views, want) {
		t.Errorf("gh views = %v, want %v", fe.views, want)
	}

	// With no ledger the refresh looks nothing up.
	fe.views = nil
	New(Config{Dir: filepath.Join(root, "github"), Exec: fe.run, Workspaces: func() []workspace.Dir { return nil }}).Refresh(context.Background())
	if len(fe.views) != 0 {
		t.Errorf("with no ledger: %v", fe.views)
	}
}

// A round looks up factsMax items; the next goes on where it stopped, so
// the ones past the first fifty are reached though those stay open.
func TestSettleRounds(t *testing.T) {
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	ledger := front.New("", func() time.Time { return now })
	fe := &factExec{states: map[string]string{}}
	for i := 1; i <= factsMax+10; i++ {
		now = now.Add(time.Second)
		if _, _, err := ledger.AddItem(front.Item{Text: fmt.Sprintf("PR %d", i), Source: fmt.Sprintf("https://github.com/owner/repo/pull/%d", i), From: front.FromPlanner}); err != nil {
			t.Fatal(err)
		}
		fe.states[fmt.Sprintf("pr owner/repo#%d", i)] = "OPEN"
	}
	last := fmt.Sprintf("pr owner/repo#%d", factsMax+10)
	fe.states[last] = "MERGED"
	w := New(Config{Dir: t.TempDir(), Ledger: ledger, Exec: fe.run, Workspaces: func() []workspace.Dir { return nil }})
	w.Refresh(context.Background())
	if len(fe.views) != factsMax || fe.views[0] != "pr owner/repo#1" || slices.Contains(fe.views, last) {
		t.Fatalf("first round looked up %d: %v", len(fe.views), fe.views)
	}
	if len(ledger.Items()) != factsMax+10 {
		t.Fatalf("first round closed some: %d open", len(ledger.Items()))
	}
	fe.views = nil
	w.Refresh(context.Background())
	if len(fe.views) != factsMax || fe.views[0] != fmt.Sprintf("pr owner/repo#%d", factsMax+1) || !slices.Contains(fe.views, last) {
		t.Fatalf("second round looked up %d from %s", len(fe.views), fe.views[0])
	}
	if open := ledger.Items(); len(open) != factsMax+9 || open[len(open)-1].Text == fmt.Sprintf("PR %d", factsMax+10) {
		t.Errorf("after the second round: %d open", len(open))
	}
}
