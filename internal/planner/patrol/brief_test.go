package patrol

import (
	"reflect"
	"testing"
	"time"
)

// TestBrief: the brief's part is the pull requests that wait (each with
// its checks as last looked at, and whether it became ready in the
// window), the findings first found in the window still to be settled, and
// the kinds the gateway stopped, each said in one brief only.
func TestBrief(t *testing.T) {
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	d.at(wsA, -time.Hour)
	start := d.now
	d.found(finding("bug-old", nil), finding("bug-pr", map[string]string{"title": `"修正重连时的会话恢复"`}), finding("bug-merged", nil))
	for i, id := range []string{"bug-pr", "bug-merged"} {
		key := "task:t" + string(rune('1'+i))
		d.p.Dispatched(ref(id), key)
		d.p.Reported(ref(id), key, "done", []string{"https://github.com/owner/gateway/pull/3" + string(rune('1'+i))})
	}
	d.fe.gh["owner/gateway"].views = map[string]string{
		"31": `{"state":"OPEN","statusCheckRollup":` + ciPassing + `}`,
		"32": `{"state":"MERGED","statusCheckRollup":` + ciPassing + `}`,
	}
	d.now = d.now.Add(time.Hour)
	d.tick()

	// The window opens a day later: what was found since.
	since := d.now.Add(23 * time.Hour)
	d.now = since.Add(time.Hour)
	d.found(finding("bug-new", map[string]string{"title": `"配置热加载丢了 watchdog 的新值"`}), finding("doc-readme", map[string]string{"kind": doc, "title": `"README 与脚本不符"`}),
		finding("bug-no", nil), finding("bug-task", map[string]string{"title": `"派出去在修的"`}))
	if err := d.p.Decide(ref("bug-no"), ChoiceSkip); err != nil {
		t.Fatal(err)
	}
	d.atWork["task:t9"] = true
	d.p.Dispatched(ref("bug-task"), "task:t9")
	d.writeDeclined(`{"v":1,"findings":[],"kinds":[
		{"workspace":"","kind":"debt","by":"auto","at":"2026-11-24T08:10:00+08:00","adopted":3,"of":10},
		{"workspace":"","kind":"stale","by":"auto","at":"2026-11-20T08:10:00+08:00","adopted":1,"of":10,"briefed":true},
		{"workspace":"` + wsA + `","kind":"risk","by":"user","at":"2026-11-24T08:10:00+08:00"}],"workspaces":[]}`)

	want := Report{
		PRs: []ReadyPR{{Title: "修正重连时的会话恢复", URL: "https://github.com/owner/gateway/pull/31", CI: "通过", Workspace: "gateway"}},
		New: []NewFinding{{Title: "配置热加载丢了 watchdog 的新值", Kind: "bug", Workspace: "gateway"}, {Title: "README 与脚本不符", Kind: "doc", Workspace: "gateway"},
			{Title: "派出去在修的", Kind: "bug", Workspace: "gateway"}},
		Stopped: []StoppedKind{{Kind: "debt", Adopted: 3, Of: 10}},
	}
	if got := d.p.Brief(since); !reflect.DeepEqual(got, want) {
		t.Errorf("Brief:\n%+v\nwant:\n%+v", got, want)
	}
	// A window that holds the day the pull request became ready has it
	// fresh, and the findings of that day new.
	got := d.p.Brief(start)
	if len(got.PRs) != 1 || !got.PRs[0].Fresh || len(got.New) != 4 || got.New[0].Title != "重连时会话为空会崩" {
		t.Errorf("Brief over the whole time: %+v", got)
	}

	d.p.Briefed([]string{"debt", "risk"})
	if got := d.p.Brief(since); len(got.Stopped) != 0 || len(got.PRs) != 1 {
		t.Errorf("Brief after Briefed: %+v, want the stopped kind said once", got)
	}
	ks := d.declined().Kinds
	if len(ks) != 3 || !ks[0].Briefed || !ks[1].Briefed || ks[2].Briefed {
		t.Errorf("declined.json kinds after Briefed: %+v", ks)
	}
	// An unreadable list leaves the stopped kinds out and the rest in.
	d.writeDeclined("{")
	if got := d.p.Brief(since); len(got.Stopped) != 0 || len(got.PRs) != 1 || len(got.New) != 3 {
		t.Errorf("Brief with the list unreadable: %+v", got)
	}
	d.p.Briefed([]string{"debt"})
	if d.read(declinedFile) != "{" {
		t.Errorf("Briefed wrote over an unreadable list")
	}
	if got := New(Config{}).Brief(since); !reflect.DeepEqual(got, Report{}) {
		t.Errorf("Brief with no state root: %+v", got)
	}
}

// TestDetails: a finding as the page shows it: each piece of evidence with
// where it opens — a file line at its GitHub link, a link at itself, a file
// line of a repository that is not on GitHub nowhere.
func TestDetails(t *testing.T) {
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	d.found(finding("bug-one", map[string]string{"evidence": `["a.go:2","https://example.com/issue/7","a.go:1-3"]`, "size": `"M"`, "confidence": `"medium"`}))
	d.p.Dispatched(ref("bug-one"), "task:t1")
	d.p.Reported(ref("bug-one"), "task:t1", "done", []string{"https://github.com/owner/gateway/pull/31"})
	blob := "https://github.com/owner/gateway/blob/" + sha("a") + "/a.go"
	want := map[string]Detail{ref("bug-one"): {
		Kind: "bug", Size: "M", Confidence: "medium", Impact: "重连即崩", Fix: "先判空", State: StatePR, PR: "https://github.com/owner/gateway/pull/31",
		Evidence: []Evidence{{Text: "a.go:2", Link: blob + "#L2"}, {Text: "https://example.com/issue/7", Link: "https://example.com/issue/7"}, {Text: "a.go:1-3", Link: blob + "#L1-L3"}},
	}}
	if got := d.p.Details([]string{ref("bug-one"), ref("bug-nope"), "x"}); !reflect.DeepEqual(got, want) {
		t.Errorf("Details:\n%+v\nwant:\n%+v", got, want)
	}

	local := newDeep(t)
	dir := local.workspace(wsA, "local")
	local.fe.repos[dir] = &fakeRepo{sha: sha("a"), remotes: "origin\t/srv/git/local.git (fetch)\n"}
	local.file(dir, "a.go", 0)
	local.found(finding("bug-one", map[string]string{"evidence": `["a.go:1","https://example.com/x"]`}))
	got := local.p.Details([]string{ref("bug-one")})[ref("bug-one")]
	if !reflect.DeepEqual(got.Evidence, []Evidence{{Text: "a.go:1"}, {Text: "https://example.com/x", Link: "https://example.com/x"}}) {
		t.Errorf("evidence outside GitHub: %+v", got.Evidence)
	}
}
