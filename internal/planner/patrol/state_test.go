package patrol

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/planner/front"
)

// found runs a deep check of wsA that reports fs, and returns their refs.
func (d *deepEnv) found(fs ...string) {
	d.t.Helper()
	d.set(func() { d.reply = findings(fs...) })
	if _, err := d.p.Trigger(d.dirs[0].ID); err != nil {
		d.t.Fatal(err)
	}
	d.wait()
}

func ref(id string) string { return Ref(wsA, id) }

// TestCanDispatch: with no word of the user's a finding is dispatched only
// when it is small, reversible and sure, in a repository on GitHub, not in
// hand already, and the workspace has room; the user asking needs only the
// finding.
func TestCanDispatch(t *testing.T) {
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	d.found(finding("bug-ok", nil),
		finding("bug-medium", map[string]string{"size": `"M"`}),
		finding("bug-large", map[string]string{"size": `"L"`}),
		finding("bug-oneway", map[string]string{"reversible": "false"}),
		finding("bug-unsure", map[string]string{"confidence": `"medium"`}),
		finding("bug-guess", map[string]string{"confidence": `"low"`}))
	for id, why := range map[string]string{
		"bug-ok": "", "bug-medium": "不是 S", "bug-large": "不是 S", "bug-oneway": "不可逆", "bug-unsure": "把握不是 high", "bug-guess": "把握不是 high",
	} {
		ws, err := d.p.CanDispatch(ref(id), true)
		if why == "" && (err != nil || ws != wsA) {
			t.Errorf("%s: %q, %v; want it dispatched", id, ws, err)
		}
		if why != "" && (err == nil || !strings.Contains(err.Error(), why)) {
			t.Errorf("%s: %v, want refused for %q", id, err, why)
		}
		// The user asking: the finding is enough.
		if ws, err := d.p.CanDispatch(ref(id), false); err != nil || ws != wsA {
			t.Errorf("%s asked by the user: %q, %v", id, ws, err)
		}
	}
	for _, unknown := range []string{ref("bug-nope"), "ws_zzzzzzz9/bug-ok", "bug-ok", ""} {
		for _, unasked := range []bool{true, false} {
			if _, err := d.p.CanDispatch(unknown, unasked); !errors.Is(err, ErrNoFinding) {
				t.Errorf("CanDispatch(%q, %v) = %v, want ErrNoFinding", unknown, unasked, err)
			}
		}
	}

	// A repository that is not on GitHub has nowhere for a pull request.
	local := newDeep(t)
	dir := local.workspace(wsA, "local")
	local.fe.repos[dir] = &fakeRepo{sha: sha("a"), remotes: "origin\t/srv/git/local.git (fetch)\n"}
	local.file(dir, "a.go", 0)
	local.found(finding("bug-ok", map[string]string{"evidence": `["a.go:1"]`}))
	if _, err := local.p.CanDispatch(ref("bug-ok"), true); err == nil || !strings.Contains(err.Error(), "不在 GitHub 上") {
		t.Errorf("outside GitHub: %v", err)
	}
	if _, err := local.p.CanDispatch(ref("bug-ok"), false); err != nil {
		t.Errorf("outside GitHub, asked by the user: %v", err)
	}
}

// TestOpenTaskCap: at most MaxOpenTasks of a workspace's findings are with
// a task at work; one ending makes room.
func TestOpenTaskCap(t *testing.T) {
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	d.found(finding("bug-one", nil), finding("bug-two", nil), finding("bug-three", nil))
	for i, id := range []string{"bug-one", "bug-two"} {
		if _, err := d.p.CanDispatch(ref(id), true); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		key := fmt.Sprintf("task:t%d", i+1)
		d.atWork[key] = true
		d.p.Dispatched(ref(id), key)
	}
	if got := states(d.state(wsA)); got != "bug-one=dispatched bug-two=dispatched bug-three=told" {
		t.Fatalf("records: %s", got)
	}
	if r, ok := d.p.Finding(ref("bug-one")); !ok || r.Task != "task:t1" {
		t.Errorf("Finding = %+v, %v", r, ok)
	}
	_, err := d.p.CanDispatch(ref("bug-three"), true)
	if err == nil || !strings.Contains(err.Error(), "已有 2 个在做") {
		t.Fatalf("third task: %v, want the workspace full", err)
	}
	// A finding in a task's hands is not given to another.
	if _, err := d.p.CanDispatch(ref("bug-one"), true); err == nil || !strings.Contains(err.Error(), "已经派给 task:t1") {
		t.Errorf("dispatching one twice: %v", err)
	}
	// The user asking is not held to the cap.
	if _, err := d.p.CanDispatch(ref("bug-three"), false); err != nil {
		t.Errorf("the user's own dispatch: %v", err)
	}
	// Another workspace has its own room.
	other := d.workspace(wsB, "docs")
	d.fe.repos[other] = &fakeRepo{sha: sha("b"), remotes: "origin\tgit@github.com:owner/docs.git (fetch)\n"}
	d.file(other, "a.go", 0)
	d.set(func() { d.reply = findings(finding("bug-elsewhere", map[string]string{"evidence": `["a.go:1"]`})) })
	if _, err := d.p.Trigger(wsB); err != nil {
		t.Fatal(err)
	}
	d.wait()
	if _, err := d.p.CanDispatch(Ref(wsB, "bug-elsewhere"), true); err != nil {
		t.Errorf("another workspace: %v", err)
	}
	// One task done: room again, and the block offers auto again.
	d.set(func() { d.atWork["task:t1"] = false })
	if _, err := d.p.CanDispatch(ref("bug-three"), true); err != nil {
		t.Errorf("after one ended: %v", err)
	}
}

// TestBlockAutoFull: a workspace with no room offers none of its new
// findings as auto.
func TestBlockAutoFull(t *testing.T) {
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	d.found(finding("bug-one", nil), finding("bug-two", nil))
	for i, id := range []string{"bug-one", "bug-two"} {
		key := fmt.Sprintf("task:t%d", i+1)
		d.atWork[key] = true
		d.p.Dispatched(ref(id), key)
	}
	d.found(finding("bug-one", nil), finding("bug-two", nil), finding("bug-three", nil))
	blocks := d.blocks()
	last := blocks[len(blocks)-1]
	if !strings.Contains(last, `"ref":"`+ref("bug-three")+`"`) || !strings.Contains(last, `"auto":false`) || strings.Contains(last, "bug-one") {
		t.Errorf("block with the workspace full:\n%s", last)
	}
}

// TestSurface: three findings a day are put in front of the user by turns
// that had none of their words; the fourth is refused, the next day starts
// afresh, one finding counts once and the user's own turn counts for
// nothing.
func TestSurface(t *testing.T) {
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	d.at(wsA, 0)
	d.found(finding("bug-one", nil), finding("bug-two", nil), finding("bug-three", nil), finding("bug-four", nil), finding("bug-five", nil))
	for _, id := range []string{"bug-one", "bug-two"} {
		if err := d.p.Surface(ref(id), "ask", true); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	// The same finding again the same day: its pull request announced after
	// the question. Still two.
	if err := d.p.Surface(ref("bug-one"), "pr", true); err != nil {
		t.Fatalf("the same finding again: %v", err)
	}
	// The user's own turn takes none of the day's three.
	if err := d.p.Surface(ref("bug-five"), "ask", false); err != nil {
		t.Fatalf("the user's turn: %v", err)
	}
	if err := d.p.Surface(ref("bug-three"), "pr", true); err != nil {
		t.Fatalf("the third: %v", err)
	}
	if err := d.p.Surface(ref("bug-four"), "ask", true); !errors.Is(err, ErrDailyCap) {
		t.Fatalf("the fourth = %v, want ErrDailyCap", err)
	}
	if err := d.p.Surface(ref("bug-two"), "ask", true); err != nil {
		t.Errorf("one already surfaced today, again at the cap: %v", err)
	}
	if got := states(d.state(wsA)); got != "bug-one=asked bug-two=asked bug-three=told bug-four=told bug-five=asked" {
		t.Errorf("records: %s", got)
	}
	if r, _ := d.p.Finding(ref("bug-four")); !r.Surfaced.IsZero() {
		t.Errorf("the refused one is recorded as surfaced at %s", r.Surfaced)
	}
	if r, _ := d.p.Finding(ref("bug-five")); !r.Surfaced.IsZero() {
		t.Errorf("the user's own question counts against the day: %s", r.Surfaced)
	}
	// The cap is over all workspaces.
	other := d.workspace(wsB, "docs")
	d.fe.repos[other] = &fakeRepo{sha: sha("b"), remotes: "origin\tgit@github.com:owner/docs.git (fetch)\n"}
	d.file(other, "a.go", 0)
	d.set(func() { d.reply = findings(finding("bug-elsewhere", map[string]string{"evidence": `["a.go:1"]`})) })
	if _, err := d.p.Trigger(wsB); err != nil {
		t.Fatal(err)
	}
	d.wait()
	if err := d.p.Surface(Ref(wsB, "bug-elsewhere"), "ask", true); !errors.Is(err, ErrDailyCap) {
		t.Errorf("another workspace's the same day = %v, want ErrDailyCap", err)
	}
	// Just before midnight still today; after it a new day.
	d.now = time.Date(d.now.Year(), d.now.Month(), d.now.Day(), 23, 59, 0, 0, d.now.Location())
	if err := d.p.Surface(ref("bug-four"), "ask", true); !errors.Is(err, ErrDailyCap) {
		t.Errorf("23:59 = %v, want ErrDailyCap", err)
	}
	d.now = d.now.Add(2 * time.Minute)
	if err := d.p.Surface(ref("bug-four"), "ask", true); err != nil {
		t.Errorf("the next day: %v", err)
	}
	if err := d.p.Surface(ref("bug-nope"), "ask", true); !errors.Is(err, ErrNoFinding) {
		t.Errorf("unknown finding = %v, want ErrNoFinding", err)
	}
	// A workspace's record that does not read may hold today's three: the
	// cap is not judged without it. The user asking counts for nothing and
	// goes on.
	rec := d.p.deepPath(wsB)
	kept, err := os.ReadFile(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(rec, []byte(`{"v":1,"findings":[`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.p.Surface(ref("bug-five"), "ask", true); !errors.Is(err, ErrState) {
		t.Errorf("with another workspace's record unreadable = %v, want ErrState", err)
	}
	if r, _ := d.p.Finding(ref("bug-five")); !r.Surfaced.IsZero() {
		t.Errorf("counted on a guess: %+v", r)
	}
	if err := d.p.Surface(ref("bug-five"), "ask", false); err != nil {
		t.Errorf("the user's own question with a record unreadable: %v", err)
	}
	if err := os.WriteFile(rec, kept, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.p.Surface(ref("bug-four"), "pr", true); err != nil {
		t.Errorf("once the record reads again: %v", err)
	}
}

// TestFile: a finding is filed as an open item once, from the patrol, its
// source the finding; a ref no finding has is the caller's to file.
func TestFile(t *testing.T) {
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	d.found(finding("bug-one", nil), finding("bug-two", map[string]string{"title": `"` + strings.Repeat("长", 80) + `"`}), finding("bug-three", nil))
	if known, filed, err := d.p.File(ref("bug-one"), "", ""); !known || !filed || err != nil {
		t.Fatalf("File = %v, %v, %v", known, filed, err)
	}
	if known, filed, err := d.p.File(ref("bug-one"), "换个说法", ""); !known || !filed || err != nil {
		t.Fatalf("File again = %v, %v, %v", known, filed, err)
	}
	items := d.items.Items()
	if len(items) != 1 || items[0].Source != "patrol:"+ref("bug-one") || items[0].From != front.FromPatrol || items[0].Workspace != wsA || items[0].Text != "重连时会话为空会崩" {
		t.Fatalf("items %+v, want the finding filed once", items)
	}
	if r, _ := d.p.Finding(ref("bug-one")); r.State != StateItem || r.Item != items[0].ID {
		t.Errorf("record %+v", r)
	}
	// The Planner's own sentence, under a line.
	if _, err := d.items.Apply(front.Update{ID: "gw", Name: "网关"}); err != nil {
		t.Fatal(err)
	}
	if known, filed, err := d.p.File(ref("bug-two"), "重连崩溃要修", "gw"); !known || !filed || err != nil {
		t.Fatalf("File with text = %v, %v, %v", known, filed, err)
	}
	if it := d.items.Items()[1]; it.Text != "重连崩溃要修" || it.Front != "gw" {
		t.Errorf("item %+v", it)
	}
	// A line the ledger lacks: known, and not filed.
	if known, filed, err := d.p.File(ref("bug-three"), "", "nope"); !known || filed || !errors.Is(err, front.ErrNoFront) {
		t.Errorf("File under an unknown line = %v, %v, %v", known, filed, err)
	}
	if r, _ := d.p.Finding(ref("bug-three")); r.Item != "" || r.State != StateTold {
		t.Errorf("record after a failed filing: %+v", r)
	}
	// One asked about and filed is not filed a second time under other
	// words.
	if err := d.p.Surface(ref("bug-two"), "ask", true); err != nil {
		t.Fatal(err)
	}
	if known, filed, err := d.p.File(ref("bug-two"), "又一种说法", ""); !known || !filed || err != nil || len(d.items.Items()) != 2 {
		t.Errorf("File of an asked, filed finding = %v, %v, %v, %d items", known, filed, err, len(d.items.Items()))
	}
	// In a task's hands it is not filed.
	d.atWork["task:t1"] = true
	d.p.Dispatched(ref("bug-three"), "task:t1")
	if known, filed, err := d.p.File(ref("bug-three"), "", ""); !known || filed || err != nil || len(d.items.Items()) != 2 {
		t.Errorf("File of a dispatched finding = %v, %v, %v, %d items", known, filed, err, len(d.items.Items()))
	}
	// One the user struck is not filed again.
	if _, err := d.items.CloseItem(items[0].ID, front.ByUser); err != nil {
		t.Fatal(err)
	}
	if known, filed, err := d.p.File(ref("bug-one"), "", ""); !known || !filed || err != nil || len(d.items.Items()) != 1 {
		t.Errorf("File of a struck finding = %v, %v, %v, %d items", known, filed, err, len(d.items.Items()))
	}
	for _, unknown := range []string{ref("bug-nope"), "check:dirty:/x", "ws_zzzzzzz9/bug-one", ""} {
		if known, filed, err := d.p.File(unknown, "x", ""); known || filed || err != nil {
			t.Errorf("File(%q) = %v, %v, %v; want unknown", unknown, known, filed, err)
		}
	}
}

const (
	ciPassing = `[{"__typename":"CheckRun","name":"test","workflowName":"CI","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-11-25T03:01:00Z"}]`
	ciRunning = `[{"__typename":"CheckRun","name":"test","workflowName":"CI","status":"IN_PROGRESS","conclusion":"","startedAt":"2026-11-25T03:01:00Z"}]`
	ciFailing = `[{"__typename":"CheckRun","name":"test","workflowName":"CI","status":"COMPLETED","conclusion":"FAILURE","startedAt":"2026-11-25T03:01:00Z"},
		{"__typename":"CheckRun","name":"lint","workflowName":"CI","status":"IN_PROGRESS","conclusion":"","startedAt":"2026-11-25T03:01:00Z"}]`
)

// TestReported: the task a finding was dispatched to reporting done with a
// pull request of the repository makes the finding wait on the user, as an
// open item whose source stays the finding; the hourly round then follows
// the pull request and closes the item once it is merged or closed.
func TestReported(t *testing.T) {
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	d.at(wsA, 0)
	d.found(finding("bug-one", nil), finding("bug-two", nil), finding("bug-three", nil))
	link := "https://github.com/owner/gateway/pull/31"
	for i, id := range []string{"bug-one", "bug-two", "bug-three"} {
		key := fmt.Sprintf("task:t%d", i+1)
		d.atWork[key] = true
		d.p.Dispatched(ref(id), key)
	}
	// Not the finding's task, not done, no pull request, another
	// repository's: the record stays.
	d.p.Reported(ref("bug-one"), "task:other", "done", []string{link})
	d.p.Reported(ref("bug-one"), "task:t1", "blocked", []string{link})
	d.p.Reported(ref("bug-one"), "task:t1", "done", nil)
	d.p.Reported(ref("bug-one"), "task:t1", "done", []string{"https://github.com/someone/else/pull/9"})
	if r, _ := d.p.Finding(ref("bug-one")); r.State != StateDispatched || r.PR != "" || len(d.items.Items()) != 0 {
		t.Fatalf("record %+v, %d items", r, len(d.items.Items()))
	}
	// A report names other pull requests too: the one a fix came of, one
	// of another repository. The task's own is the latest opened, the
	// highest number of this repository's, wherever in the text it stands.
	d.p.Reported(ref("bug-one"), "task:t1", "done", []string{"https://github.com/owner/gateway/pull/12", "https://github.com/someone/else/pull/99", link, "https://github.com/owner/gateway/pull/7"})
	r, _ := d.p.Finding(ref("bug-one"))
	items := d.items.Items()
	if r.State != StatePR || r.PR != link || len(items) != 1 || r.Item != items[0].ID {
		t.Fatalf("record %+v, items %+v", r, items)
	}
	if it := items[0]; it.Source != "patrol:"+ref("bug-one") || it.Text != "已备好 PR #31：重连时会话为空会崩" || it.From != front.FromPatrol || it.Workspace != wsA {
		t.Errorf("item %+v", it)
	}
	// A finding filed before keeps its item, retitled.
	d.found(finding("bug-filed", nil))
	if _, _, err := d.p.File(ref("bug-filed"), "", ""); err != nil {
		t.Fatal(err)
	}
	filed, _ := d.p.Finding(ref("bug-filed"))
	d.p.Dispatched(ref("bug-filed"), "task:t9")
	d.p.Reported(ref("bug-filed"), "task:t9", "done", []string{"https://github.com/Owner/Gateway/pull/40"})
	if r, _ := d.p.Finding(ref("bug-filed")); r.State != StatePR || r.Item != filed.Item {
		t.Fatalf("record %+v, want the item %s kept", r, filed.Item)
	}
	if got := d.open(); !slices.Equal(got, []string{"patrol:" + ref("bug-filed") + "|已备好 PR #40：重连时会话为空会崩", "patrol:" + ref("bug-one") + "|已备好 PR #31：重连时会话为空会崩"}) {
		t.Fatalf("open items %q", got)
	}

	// The round: the checks' state is kept for the brief, the stalled-PR
	// check leaves the pull request to its finding's item, and nothing of
	// the deterministic checks closes that item.
	gh := d.fe.gh["owner/gateway"]
	gh.pulls = `[{"number":31,"title":"修重连","createdAt":"2026-11-10T03:00:00Z","isDraft":false,"headRefName":"fix/reconnect","statusCheckRollup":` + ciPassing + `}]`
	gh.views = map[string]string{"31": `{"state":"OPEN","statusCheckRollup":` + ciRunning + `}`, "40": `{"state":"OPEN","statusCheckRollup":` + ciFailing + `}`}
	d.now = d.now.Add(time.Hour)
	d.tick()
	if r, _ := d.p.Finding(ref("bug-one")); r.State != StatePR || r.CI != "进行中" {
		t.Errorf("record %+v, want CI 进行中", r)
	}
	if r, _ := d.p.Finding(ref("bug-filed")); r.CI != "失败" {
		t.Errorf("record %+v, want CI 失败", r)
	}
	if got := d.open(); len(got) != 2 {
		t.Fatalf("open items after a round: %q, want the two findings' and no stalled-PR item", got)
	}
	gh.views["31"] = `{"state":"OPEN","statusCheckRollup":` + ciPassing + `}`
	delete(gh.views, "40") // gh failing for one: its record stays
	d.now = d.now.Add(time.Hour)
	d.tick()
	if r, _ := d.p.Finding(ref("bug-one")); r.CI != "通过" {
		t.Errorf("record %+v, want CI 通过", r)
	}
	if r, _ := d.p.Finding(ref("bug-filed")); r.State != StatePR || r.CI != "失败" {
		t.Errorf("record after gh failed: %+v", r)
	}
	// Merged: the finding is over and its item closed by the fact. Closed
	// without a merge: the same.
	gh.views["31"] = `{"state":"MERGED","statusCheckRollup":` + ciPassing + `}`
	gh.views["40"] = `{"state":"CLOSED","statusCheckRollup":` + ciFailing + `}`
	gh.pulls = ""
	d.now = d.now.Add(time.Hour)
	d.tick()
	if got := states(d.state(wsA)); !strings.Contains(got, "bug-one=merged") || !strings.Contains(got, "bug-filed=closed") {
		t.Errorf("records: %s", got)
	}
	if got := d.open(); len(got) != 0 {
		t.Errorf("open items after the pull requests ended: %q", got)
	}
	// Probed with another closer: who closed it does not change.
	if it, err := d.items.CloseItem(filed.Item, front.ByPlanner); err != nil || it.ClosedBy != front.ByFact {
		t.Errorf("item closed by %q, %v; want by the fact", it.ClosedBy, err)
	}
	// A finding that is over is neither dispatched again nor filed.
	if _, err := d.p.CanDispatch(ref("bug-one"), true); err == nil || !strings.Contains(err.Error(), "已经合并或关闭") {
		t.Errorf("dispatching a merged finding: %v", err)
	}
	n := d.fe.count("gh pr view")
	d.now = d.now.Add(time.Hour)
	d.tick()
	if got := d.fe.count("gh pr view"); got != n {
		t.Errorf("%d more gh pr view calls for findings that are over", got-n)
	}
}

func TestRollup(t *testing.T) {
	run := func(name, status, conclusion string) check {
		return check{Typename: "CheckRun", Name: name, Status: status, Conclusion: conclusion}
	}
	for _, tc := range []struct {
		name   string
		checks []check
		want   string
	}{
		{"none", nil, rollNone},
		{"passed", []check{run("a", "COMPLETED", "SUCCESS"), run("b", "COMPLETED", "SKIPPED")}, rollPass},
		{"running", []check{run("a", "COMPLETED", "SUCCESS"), run("b", "IN_PROGRESS", "")}, rollPending},
		{"failed and running", []check{run("a", "COMPLETED", "FAILURE"), run("b", "QUEUED", "")}, rollFail},
		{"running and failed", []check{run("a", "QUEUED", ""), run("b", "COMPLETED", "CANCELLED")}, rollFail},
		{"status pending", []check{{Typename: "StatusContext", Context: "ci", State: "PENDING"}}, rollPending},
		{"status failed", []check{{Typename: "StatusContext", Context: "ci", State: "FAILURE"}}, rollFail},
		{"status passed", []check{{Typename: "StatusContext", Context: "ci", State: "SUCCESS"}}, rollPass},
	} {
		if got := rollup(tc.checks); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestRecordsKept: a finding that is over leaves the record 90 days after
// it was last seen; the others stay.
func TestRecordsKept(t *testing.T) {
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	d.found(finding("bug-one", nil), finding("bug-two", nil))
	d.p.Dispatched(ref("bug-one"), "task:t1")
	d.p.Reported(ref("bug-one"), "task:t1", "done", []string{"https://github.com/owner/gateway/pull/31"})
	d.fe.gh["owner/gateway"].views = map[string]string{"31": `{"state":"MERGED","statusCheckRollup":[]}`}
	seen := d.state(wsA).Findings[0].Seen
	d.at(wsA, 0)
	d.tick()
	// The check of that round reported both again. One whose pull request
	// is merged is no new finding and is not seen again either: the
	// checkout may not have the merge yet, and a record seen for ever would
	// keep a real return from being reported.
	st := d.state(wsA)
	if got := states(st); got != "bug-one=merged bug-two=told" || st.Last == nil || st.Last.New != 0 || !st.Findings[0].Seen.Equal(seen) || !st.Findings[1].Seen.After(seen) {
		t.Fatalf("after the round: %s, last %+v, seen %s and %s", got, st.Last, st.Findings[0].Seen, st.Findings[1].Seen)
	}
	d.now = seen.AddDate(0, 0, 89)
	d.p.Dispatched(ref("bug-two"), "task:t2")
	if got := states(d.state(wsA)); got != "bug-one=merged bug-two=dispatched" {
		t.Fatalf("at 89 days: %s", got)
	}
	d.now = seen.AddDate(0, 0, 91)
	d.p.Dispatched(ref("bug-two"), "task:t3")
	if got := states(d.state(wsA)); got != "bug-two=dispatched" {
		t.Fatalf("past 90 days: %s", got)
	}
	// Gone from the record, the same thing found again is new.
	d.over(nil)
	d.found(finding("bug-one", nil))
	if st := d.state(wsA); st.Last.New != 1 || !strings.Contains(states(st), "bug-one=told") {
		t.Errorf("found again once the record lapsed: %s, last %+v", states(st), st.Last)
	}
}

// TestTaskLeftUnfixed: a finding given to a task that is no longer at work
// and left no pull request goes back to being an open item at the next
// round: what was found is not dropped for a task not having fixed it. One
// whose task is still at work is left with it.
func TestTaskLeftUnfixed(t *testing.T) {
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	d.at(wsA, 0)
	d.found(finding("bug-one", nil), finding("bug-two", nil), finding("bug-three", nil))
	// The third was filed before, in the Planner's own words.
	if _, _, err := d.p.File(ref("bug-three"), "重连要先判空", ""); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"bug-one", "bug-two", "bug-three"} {
		key := fmt.Sprintf("task:t%d", i+1)
		d.atWork[key] = true
		d.p.Dispatched(ref(id), key)
	}
	d.now = d.now.Add(time.Hour)
	d.tick()
	if got := states(d.state(wsA)); got != "bug-one=dispatched bug-two=dispatched bug-three=dispatched" || len(d.items.Items()) != 1 {
		t.Fatalf("tasks at work: %s, %d items", got, len(d.items.Items()))
	}
	// The first failed, the third was stopped: both are open items again,
	// the third the one it already was.
	d.set(func() { d.atWork["task:t1"], d.atWork["task:t3"] = false, false })
	d.now = d.now.Add(time.Hour)
	d.tick()
	if got := states(d.state(wsA)); got != "bug-one=item bug-two=dispatched bug-three=item" {
		t.Fatalf("after two tasks ended: %s", got)
	}
	if got := d.open(); !slices.Equal(got, []string{"patrol:" + ref("bug-one") + "|重连时会话为空会崩", "patrol:" + ref("bug-three") + "|重连要先判空"}) {
		t.Fatalf("open items %q, want one each, the third's as it was filed", got)
	}
	if r, _ := d.p.Finding(ref("bug-one")); r.Item == "" || r.Task != "task:t1" {
		t.Errorf("record %+v", r)
	}
	// The workspace has room again, and the finding may be dispatched anew.
	if _, err := d.p.CanDispatch(ref("bug-one"), true); err != nil {
		t.Errorf("dispatching it again: %v", err)
	}
	// Nothing more happens to it on the rounds after.
	d.now = d.now.Add(time.Hour)
	d.tick()
	if len(d.items.Items()) != 2 {
		t.Errorf("%d items after another round, want 2", len(d.items.Items()))
	}
}
