package github

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/planner/workspace"
)

func TestParseRemote(t *testing.T) {
	for _, tc := range []struct {
		url         string
		owner, name string
		ok          bool
	}{
		{"https://github.com/KevinZhao/naozhi.git", "KevinZhao", "naozhi", true},
		{"https://github.com/KevinZhao/naozhi", "KevinZhao", "naozhi", true},
		{"https://x-access-token@github.com/a/b.c/", "a", "b.c", true},
		{"git@github.com:KevinZhao/naozhi.git", "KevinZhao", "naozhi", true},
		{"ssh://git@github.com:22/o/r.git", "o", "r", true},
		{"git@gitlab.com:o/r.git", "", "", false},
		{"https://github.com.evil.example/o/r", "", "", false},
		{"https://github.com/o", "", "", false},
		{"/local/path/repo.git", "", "", false},
	} {
		o, n, ok := ParseRemote(tc.url)
		if o != tc.owner || n != tc.name || ok != tc.ok {
			t.Errorf("ParseRemote(%q) = %q %q %v, want %q %q %v", tc.url, o, n, ok, tc.owner, tc.name, tc.ok)
		}
	}
}

// PickRemote prefers origin, else the first GitHub remote by name.
func TestPickRemote(t *testing.T) {
	out := "upstream\tgit@github.com:up/repo.git (fetch)\nupstream\tgit@github.com:up/repo.git (push)\n" +
		"origin\thttps://github.com/me/repo.git (fetch)\norigin\thttps://github.com/me/repo.git (push)\n"
	if o, n, _ := PickRemote(out); o != "me" || n != "repo" {
		t.Errorf("with origin: %s/%s", o, n)
	}
	out = "z\tgit@github.com:z/r.git (fetch)\nb\tgit@github.com:b/r.git (fetch)\norigin\tgit@gitlab.com:x/r.git (fetch)\n"
	if o, _, _ := PickRemote(out); o != "b" {
		t.Errorf("no GitHub origin: owner %s, want b", o)
	}
	if _, _, ok := PickRemote("origin\t/srv/git/r.git (fetch)\n"); ok {
		t.Error("a local remote picked")
	}
}

// fakeExec answers git remote -v per directory and gh issue list per repo
// and state; fail makes gh fail.
type fakeExec struct {
	mu      sync.Mutex
	remotes map[string]string // dir → git remote -v; missing: not a git repository
	issues  map[string]string // "owner/repo open|closed" → JSON
	fail    bool
	calls   []string
}

func (f *fakeExec) run(_ context.Context, dir, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	switch name {
	case "git":
		if out, ok := f.remotes[dir]; ok {
			return out, nil
		}
		return "", errors.New("git remote: fatal: not a git repository (or any of the parent directories): .git")
	case "gh":
		if f.fail {
			return "", errors.New("gh issue: error connecting to api.github.com")
		}
		repo, state := args[slices.Index(args, "-R")+1], args[slices.Index(args, "--state")+1]
		return f.issues[repo+" "+state], nil
	}
	return "", errors.New("unexpected " + name)
}

// TestRefresh: one file per GitHub repository the registered directories
// are in (two worktrees of one repo share it; a non-git directory and a
// non-GitHub remote get none), with the open issues and the recently
// closed, newest first, labels and update times; a failed gh keeps the old
// lists and says since when they are stale; a repository no longer
// registered loses its file.
func TestRefresh(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	root := t.TempDir()
	dirs := map[string]string{}
	for _, n := range []string{"naozhi", "naozhi-wt", "notes", "gl"} {
		dirs[n] = filepath.Join(root, n)
		if err := os.Mkdir(dirs[n], 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ws := []workspace.Dir{{Name: "naozhi", Path: dirs["naozhi"]}, {Name: "naozhi-wt", Path: dirs["naozhi-wt"]}, {Name: "notes", Path: dirs["notes"]}, {Name: "gl", Path: dirs["gl"]}, {Name: "gone", Path: filepath.Join(root, "missing")}}
	fe := &fakeExec{
		remotes: map[string]string{
			dirs["naozhi"]:    "origin\tgit@github.com:KevinZhao/naozhi.git (fetch)\n",
			dirs["naozhi-wt"]: "origin\thttps://github.com/KevinZhao/naozhi.git (fetch)\n",
			dirs["gl"]:        "origin\tgit@gitlab.com:x/y.git (fetch)\n",
		},
		issues: map[string]string{
			"KevinZhao/naozhi open": `[{"number":3942,"title":"Planner 可用工具 | 内部网页\n（讨论）","labels":[{"name":"planner"},{"name":"讨论"}],"updatedAt":"2026-11-15T14:10:00Z"},
				{"number":3950,"title":"新的","labels":[],"updatedAt":"2026-11-16T03:00:00Z"}]`,
			"KevinZhao/naozhi closed": `[{"number":3900,"title":"修好了","labels":[{"name":"bug"}],"updatedAt":"2026-11-14T02:00:00Z","closedAt":"2026-11-14T02:00:00Z"},
				{"number":3800,"title":"边界外","labels":[],"updatedAt":"2026-11-09T01:00:00Z","closedAt":"2026-11-09T01:00:00Z"}]`,
		},
	}
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, loc)
	gh := filepath.Join(root, "planner", "github")
	w := New(Config{Dir: gh, Workspaces: func() []workspace.Dir { return ws }, Exec: fe.run, Now: func() time.Time { return now }})
	w.Refresh(context.Background())

	entries, _ := os.ReadDir(gh)
	if len(entries) != 1 || entries[0].Name() != "KevinZhao-naozhi.md" {
		t.Fatalf("files = %v, want KevinZhao-naozhi.md only", entries)
	}
	data, _ := os.ReadFile(filepath.Join(gh, "KevinZhao-naozhi.md"))
	md := string(data)
	for _, want := range []string{
		"# KevinZhao/naozhi 的 GitHub issue",
		"更新于 2026-11-16 12:00（naozhi 每小时用 gh 刷新；工作区：naozhi、naozhi-wt）",
		"## 开着的（2）",
		`| #3942 | Planner 可用工具 \| 内部网页 （讨论） | planner、讨论 | 2026-11-15 22:10 |`,
		"| #3950 | 新的 | — | 2026-11-16 11:00 |",
		"## 近 7 天关闭的（1）",
		"| #3900 | 修好了 | bug | 2026-11-14 10:00 |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("file lacks %q:\n%s", want, md)
		}
	}
	if strings.Index(md, "#3950") > strings.Index(md, "#3942") {
		t.Errorf("not newest first:\n%s", md)
	}
	if strings.Contains(md, "#3800") {
		t.Errorf("an issue closed before 7 days listed:\n%s", md)
	}
	var closedCall string
	for _, c := range fe.calls {
		if strings.Contains(c, "--state closed") {
			closedCall = c
		}
	}
	if !strings.Contains(closedCall, "--search closed:>=2026-11-09") {
		t.Errorf("closed list call = %q", closedCall)
	}

	// gh fails: the old lists stay, with the time of this failure, once
	// however often it fails.
	fe.fail = true
	for range 2 {
		now = now.Add(time.Hour)
		w.Refresh(context.Background())
	}
	data, _ = os.ReadFile(filepath.Join(gh, "KevinZhao-naozhi.md"))
	stale := string(data)
	if !strings.Contains(stale, "更新于 2026-11-16 12:00") || !strings.Contains(stale, "#3942") ||
		!strings.Contains(stale, "> 最近一次刷新失败：2026-11-16 14:00（gh issue: error connecting to api.github.com）") ||
		strings.Count(stale, "最近一次刷新失败") != 1 {
		t.Errorf("stale file:\n%s", stale)
	}
	// Back again: the note goes.
	fe.fail = false
	w.Refresh(context.Background())
	data, _ = os.ReadFile(filepath.Join(gh, "KevinZhao-naozhi.md"))
	if strings.Contains(string(data), "刷新失败") || !strings.Contains(string(data), "更新于 2026-11-16 14:00") {
		t.Errorf("recovered file:\n%s", data)
	}

	// The repository unregistered: its file goes.
	ws = ws[2:]
	w.Refresh(context.Background())
	if _, err := os.Stat(filepath.Join(gh, "KevinZhao-naozhi.md")); !os.IsNotExist(err) {
		t.Errorf("unregistered repository's file kept: %v", err)
	}
}

// A refresh failing before any success writes a file saying so; a remote
// unreadable for another reason than not being git keeps the other files.
func TestRefreshNeverOK(t *testing.T) {
	root := t.TempDir()
	d := filepath.Join(root, "r")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	gh := filepath.Join(root, "github")
	if err := os.MkdirAll(gh, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(gh, "o-other.md")
	if err := os.WriteFile(keep, []byte("# o/other 的 GitHub issue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fe := &fakeExec{remotes: map[string]string{d: "origin\tgit@github.com:o/r.git (fetch)\n"}, fail: true}
	exec := func(ctx context.Context, dir, name string, args ...string) (string, error) {
		if name == "git" && dir == filepath.Join(root, "broken") {
			return "", errors.New("git remote: fatal: detected dubious ownership")
		}
		return fe.run(ctx, dir, name, args...)
	}
	if err := os.Mkdir(filepath.Join(root, "broken"), 0o700); err != nil {
		t.Fatal(err)
	}
	w := New(Config{Dir: gh, Workspaces: func() []workspace.Dir {
		return []workspace.Dir{{Name: "r", Path: d}, {Name: "broken", Path: filepath.Join(root, "broken")}}
	}, Exec: exec,
		Now: func() time.Time { return time.Date(2026, 11, 16, 12, 0, 0, 0, time.Local) }})
	w.Refresh(context.Background())
	data, err := os.ReadFile(filepath.Join(gh, "o-r.md"))
	if err != nil || !strings.Contains(string(data), "还没有成功刷新过") || !strings.Contains(string(data), "最近一次刷新失败") {
		t.Errorf("file = %s, %v", data, err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("a file pruned while a remote was unreadable: %v", err)
	}
}
