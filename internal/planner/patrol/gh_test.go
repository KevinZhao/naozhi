package patrol

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// run is a check run of a pull request's rollup; status one commit status.
func run(name, status, conclusion, started string) string {
	return fmt.Sprintf(`{"__typename":"CheckRun","name":%q,"workflowName":"CI","status":%q,"conclusion":%q,"startedAt":%q}`, name, status, conclusion, started)
}

func status(context, state string) string {
	return fmt.Sprintf(`{"__typename":"StatusContext","context":%q,"state":%q,"startedAt":"2026-11-14T03:00:00Z"}`, context, state)
}

// TestGreen: CI passed is every check's latest run a success (neutral and
// skipped count), with at least one check.
func TestGreen(t *testing.T) {
	const t1, t2 = "2026-11-14T03:00:00Z", "2026-11-14T04:00:00Z"
	for _, tc := range []struct {
		name   string
		rollup string
		want   bool
	}{
		{"no checks", ``, false},
		{"one success", run("test", "COMPLETED", "SUCCESS", t1), true},
		{"neutral and skipped", run("a", "COMPLETED", "NEUTRAL", t1) + "," + run("b", "COMPLETED", "SKIPPED", t1) + "," + run("c", "COMPLETED", "SUCCESS", t1), true},
		{"one failure", run("test", "COMPLETED", "SUCCESS", t1) + "," + run("lint", "COMPLETED", "FAILURE", t1), false},
		{"still running", run("test", "IN_PROGRESS", "", t1), false},
		{"cancelled", run("test", "COMPLETED", "CANCELLED", t1), false},
		{"mixed with a commit status", run("test", "COMPLETED", "SUCCESS", t1) + "," + status("ci/build", "SUCCESS"), true},
		{"a pending commit status", run("test", "COMPLETED", "SUCCESS", t1) + "," + status("ci/build", "PENDING"), false},
		{"a failure re-run to success", run("test", "COMPLETED", "FAILURE", t1) + "," + run("test", "COMPLETED", "SUCCESS", t2), true},
		{"a success re-run to failure", run("test", "COMPLETED", "FAILURE", t2) + "," + run("test", "COMPLETED", "SUCCESS", t1), false},
	} {
		var checks []check
		if err := json.Unmarshal([]byte("["+tc.rollup+"]"), &checks); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := green(checks); got != tc.want {
			t.Errorf("%s: green = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestStalledPulls: a pull request hits when it is open 7 days, not a
// draft, and its CI passed; the oldest first. Its item's source is its
// link, built from the repository and the number.
func TestStalledPulls(t *testing.T) {
	ok := run("test", "COMPLETED", "SUCCESS", "2026-11-14T03:00:00Z")
	pr := func(n int, created string, draft bool, rollup string) string {
		return fmt.Sprintf(`{"number":%d,"title":"标题 %d","createdAt":%q,"isDraft":%v,"headRefName":"b%d","statusCheckRollup":[%s]}`, n, n, created, draft, n, rollup)
	}
	e := newEnv(t)
	dir := e.workspace(wsA, "gateway")
	e.fe.repos[dir] = &fakeRepo{remotes: githubRemote}
	e.fe.gh["acme/gateway"] = &fakeGH{def: "main", pulls: "[" + strings.Join([]string{
		pr(1, "2026-11-15T06:00:00Z", false, ok),                                                          // 8 days, green
		pr(2, "2026-11-15T06:00:00Z", false, run("test", "COMPLETED", "FAILURE", "2026-11-14T03:00:00Z")), // red
		pr(3, "2026-11-15T06:00:00Z", false, ""),                                                          // no check at all
		pr(4, "2026-11-15T06:00:00Z", true, ok),                                                           // a draft
		pr(5, "2026-11-20T06:00:00Z", false, ok),                                                          // 3 days
		pr(6, "2026-11-01T06:00:00Z", false, ok+","+status("ci/build", "SUCCESS")),                        // 22 days, green
		pr(7, "2026-11-16T06:03:00Z", false, ok),                                                          // a minute short of 7 days
		`{"number":8,"title":"没有检查字段","createdAt":"2026-11-01T06:00:00Z","isDraft":false}`,                // statusCheckRollup absent
	}, ",") + "]"}
	e.round()
	want := []string{
		"https://github.com/acme/gateway/pull/1|PR #1 开了 8 天、CI 通过，还没合：标题 1",
		"https://github.com/acme/gateway/pull/6|PR #6 开了 22 天、CI 通过，还没合：标题 6",
	}
	if got := e.open(); !slices.Equal(got, want) {
		t.Errorf("items =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	md := e.read(wsA + ".md")
	if i, j := strings.Index(md, "- PR #6 "), strings.Index(md, "- PR #1 "); i < 0 || j < i {
		t.Errorf("the oldest pull request is not first:\n%s", md)
	}
}

// TestFailingCI: of each workflow on the default branch the latest
// completed run counts; any of them failed is one item, its source the
// branch's Actions page.
func TestFailingCI(t *testing.T) {
	wf := func(name, status, conclusion, created string) string {
		return fmt.Sprintf(`{"workflowName":%q,"status":%q,"conclusion":%q,"createdAt":%q,"url":"https://github.com/acme/gateway/actions/runs/%s"}`, name, status, conclusion, created, created[11:13])
	}
	e := newEnv(t)
	dir := e.workspace(wsA, "gateway")
	e.fe.repos[dir] = &fakeRepo{remotes: githubRemote}
	g := &fakeGH{def: "release/1.x", runs: "[" + strings.Join([]string{
		wf("test", "in_progress", "", "2026-11-23T05:00:00Z"),          // not completed: the one before counts
		wf("test", "completed", "failure", "2026-11-23T04:00:00Z"),     // failed
		wf("test", "completed", "success", "2026-11-23T03:00:00Z"),     // older
		wf("lint", "completed", "success", "2026-11-23T04:00:00Z"),     // green now
		wf("lint", "completed", "failure", "2026-11-23T02:00:00Z"),     // failed before
		wf("deploy", "completed", "timed_out", "2026-11-23T01:00:00Z"), // timed out
		wf("docs", "completed", "cancelled", "2026-11-23T01:00:00Z"),   // cancelled is not a failure
	}, ",") + "]"}
	e.fe.gh["acme/gateway"] = g
	e.round()
	link := "https://github.com/acme/gateway/actions?query=branch%3Arelease%2F1.x"
	if got, want := e.open(), []string{link + "|release/1.x 最近一次 CI 失败：deploy、test"}; !slices.Equal(got, want) {
		t.Fatalf("items = %v, want %v", got, want)
	}
	if md := e.read(wsA + ".md"); !strings.Contains(md, "- release/1.x 最近一次 CI 失败：deploy（2026-11-23 09:00 · https://github.com/acme/gateway/actions/runs/01）、test（2026-11-23 12:00 · https://github.com/acme/gateway/actions/runs/04）\n") {
		t.Errorf("file:\n%s", md)
	}
	if n := e.fe.count("gh run list -R acme/gateway --branch=release/1.x "); n != 1 {
		t.Errorf("gh run list calls: %v", e.fe.calls)
	}

	// The failed workflow green again: the item closes.
	g.runs = "[" + wf("test", "completed", "success", "2026-11-23T05:30:00Z") + "," + wf("test", "completed", "failure", "2026-11-23T04:00:00Z") + "]"
	e.round()
	if got := e.open(); len(got) != 0 {
		t.Errorf("after CI recovered items = %v", got)
	}
}

// TestStaleIssues: the issues untouched for 30 days are one item with
// their count, "100 个以上" at the list's cap.
func TestStaleIssues(t *testing.T) {
	e := newEnv(t)
	dir := e.workspace(wsA, "gateway")
	e.fe.repos[dir] = &fakeRepo{remotes: githubRemote}
	g := &fakeGH{def: "main", issues: `[{"number":1,"updatedAt":"2026-10-24T06:01:00Z"},{"number":2,"updatedAt":"2026-10-24T06:03:00Z"}]`}
	e.fe.gh["acme/gateway"] = g
	e.round()
	if got, want := e.open(), []string{issuesLink + "|1 个 issue 超过 30 天没动"}; !slices.Equal(got, want) {
		t.Fatalf("items = %v, want %v", got, want)
	}
	// The search is by whole UTC days, as gh takes it.
	if n := e.fe.count("gh issue list -R acme/gateway --state open --search updated:<2026-10-24 "); n != 1 {
		t.Errorf("gh issue list calls: %v", e.fe.calls)
	}

	var many []string
	for i := range issueLimit {
		many = append(many, fmt.Sprintf(`{"number":%d,"updatedAt":"2026-09-01T00:00:00Z"}`, i+1))
	}
	g.issues = "[" + strings.Join(many, ",") + "]"
	e.round()
	if got, want := e.open(), []string{issuesLink + "|100 个以上 issue 超过 30 天没动"}; !slices.Equal(got, want) {
		t.Errorf("at the cap items = %v, want %v", got, want)
	}

	g.issues = `[{"number":2,"updatedAt":"2026-10-24T06:03:00Z"}]`
	e.round()
	if got := e.open(); len(got) != 0 {
		t.Errorf("with none stale items = %v", got)
	}
}

// TestBranchPulls: in a GitHub repository a pushed branch hits when it has
// no pull request and never had one. One with an open pull request costs no
// call; one that had one is asked about once and remembered; a lookup that
// fails leaves the branch items as they were.
func TestBranchPulls(t *testing.T) {
	e := newEnv(t)
	dir := e.workspace(wsA, "gateway")
	old := now.Add(-4*day - time.Hour)
	e.fe.repos[dir] = &fakeRepo{remotes: githubRemote, head: "origin/main",
		remoteRefs: []string{"main", "open", "merged", "none"},
		branches: []string{
			branchLine("open", "origin/open", old, ""),
			branchLine("merged", "origin/merged", old, ""),
			branchLine("none", "origin/none", old, ""),
			// Its upstream is the default branch: never pushed as itself.
			branchLine("tracking", "origin/main", old, ""),
			// Its remote branch was deleted.
			strings.Replace(branchLine("gone", "origin/gone", old, ""), "\x00\x00", "\x00[gone]\x00", 1),
			branchLine("fresh", "origin/fresh", now.Add(-day), ""),
		}}
	g := &fakeGH{def: "main",
		pulls: `[{"number":3,"title":"开着的","createdAt":"2026-11-22T00:00:00Z","isDraft":false,"headRefName":"open","statusCheckRollup":[]}]`,
		heads: map[string]string{"merged": `[{"number":2}]`}}
	e.fe.gh["acme/gateway"] = g
	e.round()
	want := []string{"check:branch:none|分支 none 推上去了还没开 PR，最后的提交在 4 天前"}
	if got := e.open(); !slices.Equal(got, want) {
		t.Fatalf("items = %v, want %v", got, want)
	}
	e.round()
	lookup := "gh pr list -R acme/gateway --head="
	if a, b, c := e.fe.count(lookup+"merged "), e.fe.count(lookup+"none "), e.fe.count(lookup); a != 1 || b != 2 || c != 3 {
		t.Errorf("lookups over two rounds: merged %d (want 1, remembered), none %d (want 2), all %d (want 3)", a, b, c)
	}

	g.fail = "head"
	e.round()
	if got := e.open(); !slices.Equal(got, want) {
		t.Errorf("with the lookup failing items = %v, want %v", got, want)
	}
	if md := e.read(wsA + ".md"); !strings.Contains(md, "- 推上去的分支这一轮没查成（gh pr: error connecting to api.github.com），未了事项里是上一次的结果\n") ||
		!strings.Contains(md, "## GitHub（acme/gateway）\n\n无\n") {
		t.Errorf("file with the lookup failing:\n%s", md)
	}

	// The branch got its pull request.
	g.fail, g.heads["none"] = "", `[{"number":9}]`
	e.round()
	if got := e.open(); len(got) != 0 {
		t.Errorf("after the pull request was opened items = %v", got)
	}
}

// A round asks a repository about at most maxHeadLookups branches; the
// rest are not hits yet, and the file says how many wait.
func TestBranchLookupCap(t *testing.T) {
	e := newEnv(t)
	dir := e.workspace(wsA, "gateway")
	repo := &fakeRepo{remotes: githubRemote, remoteRefs: []string{"main"}}
	for i := range maxHeadLookups + 2 {
		name := fmt.Sprintf("b%02d", i)
		repo.remoteRefs = append(repo.remoteRefs, name)
		repo.branches = append(repo.branches, branchLine(name, "", now.Add(-time.Duration(20-i)*day), ""))
	}
	e.fe.repos[dir] = repo
	e.fe.gh["acme/gateway"] = &fakeGH{def: "main"}
	e.round()
	if n := e.fe.count("gh pr list -R acme/gateway --head="); n != maxHeadLookups {
		t.Errorf("%d lookups, want %d", n, maxHeadLookups)
	}
	if n := len(e.open()); n != maxPerCheck {
		t.Errorf("%d items, want %d", n, maxPerCheck)
	}
	md := e.read(wsA + ".md")
	if strings.Count(md, "- 分支 b") != maxHeadLookups || !strings.Contains(md, "- 另有 2 个分支这一轮没查有没有开过 PR（一轮最多查 10 个）\n") {
		t.Errorf("file:\n%s", md)
	}
}

// Every gh call names the repository parsed from the remote, and nothing
// is fetched or written in the workspace.
func TestCallsReadOnly(t *testing.T) {
	e := newEnv(t)
	gateway(e)
	e.round()
	for _, c := range e.fe.calls {
		f := strings.Fields(c)
		switch f[0] {
		case "gh":
			if !strings.Contains(c, " -R acme/gateway ") && !strings.HasSuffix(c, " -- acme/gateway") {
				t.Errorf("gh call without the repository: %s", c)
			}
			if sub := f[1] + " " + f[2]; !slices.Contains([]string{"repo view", "pr list", "run list", "issue list"}, sub) {
				t.Errorf("gh call that is not a read: %s", c)
			}
		case "git":
			if !slices.Contains([]string{"rev-parse", "status", "remote", "symbolic-ref", "for-each-ref"}, f[1]) {
				t.Errorf("git call that is not a read: %s", c)
			}
		}
	}
}
