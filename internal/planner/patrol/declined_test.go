package patrol

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/front"
)

// filed files the findings ids name as open items and returns the items'
// IDs.
func (d *deepEnv) filed(ids ...string) []string {
	d.t.Helper()
	var out []string
	for _, id := range ids {
		if _, ok, err := d.p.File(ref(id), "", ""); err != nil || !ok {
			d.t.Fatalf("File(%s): %v, %v", id, ok, err)
		}
		r, _ := d.p.Finding(ref(id))
		out = append(out, r.Item)
	}
	return out
}

// declined is declined.json as it is on disk.
func (d *deepEnv) declined() Declined {
	d.t.Helper()
	var out Declined
	if data := d.read(declinedFile); data != "" {
		if err := json.Unmarshal([]byte(data), &out); err != nil {
			d.t.Fatalf("declined.json: %v\n%s", err, data)
		}
	}
	return out
}

func (d *deepEnv) writeDeclined(body string) {
	d.t.Helper()
	if err := os.MkdirAll(d.cfg.Dir, 0o700); err != nil {
		d.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.cfg.Dir, declinedFile), []byte(body), 0o600); err != nil {
		d.t.Fatal(err)
	}
}

// told are the Planner's tellings so far, cleared.
func (d *deepEnv) told() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.answers
	d.answers = nil
	return out
}

func (d *deepEnv) cardsClosed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.closed
	d.closed = nil
	return out
}

const doc = `"doc"`

// TestDecideSkip: "不做" writes the finding into declined.json, marks its
// record, closes its open item as the user's and its cards, and tells the
// Planner nothing while nothing is left to do.
func TestDecideSkip(t *testing.T) {
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	d.found(finding("bug-one", nil), finding("bug-two", nil))
	items := d.filed("bug-one", "bug-two")
	if err := d.p.Decide(ref("bug-one"), ChoiceSkip); err != nil {
		t.Fatal(err)
	}
	want := `{
  "v": 1,
  "findings": [
    {
      "workspace": "ws_aaaaaaa1",
      "id": "bug-one",
      "kind": "bug",
      "title": "重连时会话为空会崩",
      "at": "2026-11-23T14:02:00+08:00"
    }
  ],
  "kinds": [],
  "workspaces": []
}`
	if got := d.read(declinedFile); got != want {
		t.Errorf("declined.json:\n%s\nwant:\n%s", got, want)
	}
	r, _ := d.p.Finding(ref("bug-one"))
	if r.State != StateDeclined || r.Pending || !r.Disposed.Equal(d.now) || r.Adopted {
		t.Errorf("record %+v, want it declined and settled as not taken", r)
	}
	if other, _ := d.p.Finding(ref("bug-two")); other.State != StateItem || !other.Disposed.IsZero() {
		t.Errorf("the other finding %+v, want it untouched", other)
	}
	if got := d.open(); !slices.Equal(got, []string{"patrol:" + ref("bug-two") + "|重连时会话为空会崩"}) {
		t.Errorf("open items %q, want only the other finding's", got)
	}
	// Probed with another closer: who closed it does not change.
	if it, err := d.items.CloseItem(items[0], front.ByPlanner); err != nil || it.ClosedBy != front.ByUser {
		t.Errorf("item closed by %q, %v; want by the user", it.ClosedBy, err)
	}
	if got := d.cardsClosed(); !slices.Equal(got, []string{ref("bug-one") + "|你已选了不做"}) {
		t.Errorf("cards closed %q", got)
	}
	if got := d.told(); len(got) != 0 {
		t.Errorf("the Planner was told %q with nothing left to do", got)
	}
	// Said twice it is on the list once.
	if err := d.p.Decide(ref("bug-one"), ChoiceSkip); err != nil || len(d.declined().Findings) != 1 {
		t.Errorf("declined twice: %v, %d on the list", err, len(d.declined().Findings))
	}
	for _, bad := range []string{ref("bug-nope"), "ws_zzzzzzz9/bug-one", "bug-one", "", wsA + "/../x"} {
		if err := d.p.Decide(bad, ChoiceSkip); !errors.Is(err, ErrNoFinding) {
			t.Errorf("Decide(%q) = %v, want ErrNoFinding", bad, err)
		}
	}
	if err := d.p.Decide(ref("bug-two"), "merge"); !errors.Is(err, ErrChoice) {
		t.Errorf("an unknown choice: %v, want ErrChoice", err)
	}
	if got := d.declined(); len(got.Findings) != 1 {
		t.Errorf("refused choices wrote the list: %+v", got)
	}
}

// TestDecideSkipLeftOver: a finding turned down while a task still fixes
// it, or with its pull request open, leaves the Planner something to do,
// and only then is it told, in the user's words.
func TestDecideSkipLeftOver(t *testing.T) {
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	d.found(finding("bug-task", nil), finding("bug-pr", nil), finding("bug-gone", nil))
	link := "https://github.com/owner/gateway/pull/31"
	d.atWork["task:t1"] = true
	d.p.Dispatched(ref("bug-task"), "task:t1")
	d.p.Dispatched(ref("bug-pr"), "task:t2")
	d.p.Reported(ref("bug-pr"), "task:t2", "done", []string{link})
	// Its task ended without a pull request and the round has not looked
	// yet: nothing is in anybody's hands.
	d.p.Dispatched(ref("bug-gone"), "task:t3")

	if err := d.p.Decide(ref("bug-task"), ChoiceSkip); err != nil {
		t.Fatal(err)
	}
	got := d.told()
	if len(got) != 1 || !strings.HasPrefix(got[0], "不做|项目巡查的发现「重连时会话为空会崩」（"+ref("bug-task")+"）怎么处置|") ||
		!strings.Contains(got[0], "工作区 gateway）选了：不做") || !strings.Contains(got[0], "任务 task:t1 还在修它：用 cancel") {
		t.Errorf("told %q, want the task to stop", got)
	}
	if err := d.p.Decide(ref("bug-pr"), ChoiceMuteKind); err != nil {
		t.Fatal(err)
	}
	got = d.told()
	if len(got) != 1 || !strings.HasPrefix(got[0], "这类别再提|") || !strings.Contains(got[0], "它的 PR "+link+" 还开着：用 relay 让 task:t2 把这个 PR 关掉") ||
		!strings.Contains(got[0], "这个工作区的 bug 类以后不再报") {
		t.Errorf("told %q, want the pull request to close", got)
	}
	if err := d.p.Decide(ref("bug-gone"), ChoiceSkip); err != nil {
		t.Fatal(err)
	}
	if got := d.told(); len(got) != 0 {
		t.Errorf("told %q for a finding in nobody's hands", got)
	}
	if got := states(d.state(wsA)); got != "bug-task=declined bug-pr=declined bug-gone=declined" {
		t.Errorf("records: %s", got)
	}
	if got := d.open(); len(got) != 0 {
		t.Errorf("open items %q, want the pull request's item closed with it", got)
	}
	if got := d.cardsClosed(); len(got) != 3 {
		t.Errorf("cards closed %q, want each finding's", got)
	}
	// The pull request of a finding turned down is no longer followed.
	n := d.fe.count("gh pr view")
	d.at(wsA, -time.Hour)
	d.tick()
	if got := d.fe.count("gh pr view"); got != n {
		t.Errorf("%d gh pr view calls for a finding turned down", got-n)
	}
	// The Planner off: the choice stands.
	d.found(finding("doc-late", map[string]string{"kind": doc}))
	d.atWork["task:t4"] = true
	d.p.Dispatched(ref("doc-late"), "task:t4")
	d.set(func() { d.answerErr = errors.New("planner is off") })
	if err := d.p.Decide(ref("doc-late"), ChoiceSkip); err != nil {
		t.Errorf("Decide with the Planner off: %v", err)
	}
	if r, _ := d.p.Finding(ref("doc-late")); r.State != StateDeclined || !d.declined().finding(wsA, "doc-late") {
		t.Errorf("record %+v, want the choice kept without the Planner", r)
	}
}

// TestDecideMuteKind: "这类别再提" turns the finding down and stops its kind
// in its workspace: the kind's other findings still to be settled go with
// it, those a task or a pull request has in hand stay, and so do the other
// kinds and the other workspaces.
func TestDecideMuteKind(t *testing.T) {
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	other := d.workspace(wsB, "docs")
	d.fe.repos[other] = &fakeRepo{sha: sha("b"), remotes: "origin\tgit@github.com:owner/docs.git (fetch)\n"}
	d.file(other, "a.go", 0)
	d.found(finding("bug-one", nil), finding("bug-two", nil), finding("bug-task", nil), finding("doc-readme", map[string]string{"kind": doc}))
	d.filed("bug-one", "bug-two", "doc-readme")
	d.atWork["task:t1"] = true
	d.p.Dispatched(ref("bug-task"), "task:t1")
	d.set(func() { d.reply = findings(finding("bug-there", map[string]string{"evidence": `["a.go:1"]`})) })
	if _, err := d.p.Trigger(wsB); err != nil {
		t.Fatal(err)
	}
	d.wait()

	if err := d.p.Decide(ref("bug-one"), ChoiceMuteKind); err != nil {
		t.Fatal(err)
	}
	got := d.declined()
	if len(got.Findings) != 1 || got.Findings[0].ID != "bug-one" || len(got.Workspaces) != 0 || len(got.Kinds) != 1 {
		t.Fatalf("declined.json %+v", got)
	}
	if k := got.Kinds[0]; k.Workspace != wsA || k.Kind != KindBug || k.By != "user" || !k.At.Equal(d.now) || k.Adopted != 0 || k.Of != 0 || k.Briefed {
		t.Errorf("the kind on the list: %+v", k)
	}
	if s := states(d.state(wsA)); s != "bug-one=declined bug-two=declined bug-task=dispatched doc-readme=item" {
		t.Errorf("records: %s", s)
	}
	if s := states(d.state(wsB)); s != "bug-there=told" {
		t.Errorf("the other workspace's records: %s", s)
	}
	// Only the one the user answered about is settled: the rest went along.
	if r, _ := d.p.Finding(ref("bug-two")); !r.Disposed.IsZero() {
		t.Errorf("a finding that went along counts as settled: %+v", r)
	}
	if got := d.open(); !slices.Equal(got, []string{"patrol:" + ref("doc-readme") + "|重连时会话为空会崩"}) {
		t.Errorf("open items %q, want the other kind's only", got)
	}
	if got := d.cardsClosed(); !slices.Equal(got, []string{ref("bug-one") + "|你已选了这类别再提", ref("bug-two") + "|你已选了这类别再提"}) {
		t.Errorf("cards closed %q", got)
	}
	// Said again about another finding of the kind: the kind is listed once.
	if err := d.p.Decide(ref("bug-task"), ChoiceMuteKind); err != nil || len(d.declined().Kinds) != 1 || len(d.declined().Findings) != 2 {
		t.Errorf("the kind stopped twice: %v, %+v", err, d.declined())
	}
}

// TestDecideDo: "做" hands the finding to the Planner in the user's words;
// the task comes of the dispatch every finding goes by, which the patrol
// allows for the user whatever the finding's size and counts towards the
// workspace's tasks at work.
func TestDecideDo(t *testing.T) {
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	d.found(finding("bug-big", map[string]string{"size": `"L"`, "confidence": `"medium"`}), finding("bug-two", nil), finding("bug-three", nil))
	if err := d.p.Decide(ref("bug-big"), ChoiceDo); err != nil {
		t.Fatal(err)
	}
	r, _ := d.p.Finding(ref("bug-big"))
	if r.State != StateDoing || !r.Adopted || !r.Disposed.Equal(d.now) || r.Item == "" {
		t.Errorf("record %+v, want it doing, taken and filed", r)
	}
	if got := d.open(); !slices.Equal(got, []string{"patrol:" + ref("bug-big") + "|重连时会话为空会崩"}) {
		t.Errorf("open items %q, want the finding shown until a task has it", got)
	}
	got := d.told()
	if len(got) != 1 {
		t.Fatalf("told %q, want one telling", got)
	}
	for _, want := range []string{"做|项目巡查的发现「重连时会话为空会崩」（" + ref("bug-big") + "）怎么处置|", "工作区 gateway）选了：做", `dispatch 带 "finding":"` + ref("bug-big") + `"`,
		`{"ref":"` + ref("bug-big") + `","title":"重连时会话为空会崩","kind":"bug","evidence":["a.go:2"]`, `"size":"L"`, "不合并"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("telling lacks %q:\n%s", want, got[0])
		}
	}
	if got := d.cardsClosed(); len(got) != 0 {
		t.Errorf("cards closed %q for a finding to be done", got)
	}
	if d.read(declinedFile) != "" {
		t.Errorf("declined.json written for a finding to be done:\n%s", d.read(declinedFile))
	}

	// The dispatch: the user's turn passes whatever the size, a turn with
	// none of their words does not.
	if ws, err := d.p.CanDispatch(ref("bug-big"), false); err != nil || ws != wsA {
		t.Fatalf("the dispatch the user asked for: %q, %v", ws, err)
	}
	if _, err := d.p.CanDispatch(ref("bug-big"), true); err == nil || !strings.Contains(err.Error(), "不是 S") {
		t.Errorf("the same dispatch unasked: %v", err)
	}
	d.atWork["task:t1"], d.atWork["task:t2"] = true, true
	d.p.Dispatched(ref("bug-big"), "task:t1")
	d.p.Dispatched(ref("bug-two"), "task:t2")
	// It takes a place of the workspace's two: the patrol starts no third.
	if _, err := d.p.CanDispatch(ref("bug-three"), true); err == nil || !strings.Contains(err.Error(), "已有 2 个在做") {
		t.Errorf("a third task unasked: %v, want the workspace full", err)
	}
	// In a task's hands it is not handed over again.
	if err := d.p.Decide(ref("bug-big"), ChoiceDo); !errors.Is(err, ErrChoice) || !strings.Contains(err.Error(), "task:t1") {
		t.Errorf("do, while a task has it: %v", err)
	}
	// Its task gone without a pull request, it can be.
	delete(d.atWork, "task:t1")
	if err := d.p.Decide(ref("bug-big"), ChoiceDo); err != nil || len(d.told()) != 1 {
		t.Errorf("do, after the task ended: %v", err)
	}

	// With its pull request ready "do" is merging it, on GitHub.
	d.p.Reported(ref("bug-two"), "task:t2", "done", []string{"https://github.com/owner/gateway/pull/31"})
	if err := d.p.Decide(ref("bug-two"), ChoiceDo); !errors.Is(err, ErrChoice) || !strings.Contains(err.Error(), "pull/31") {
		t.Errorf("do, with the pull request ready: %v", err)
	}
	// Turned down, it is done only once off the list.
	if err := d.p.Decide(ref("bug-three"), ChoiceSkip); err != nil {
		t.Fatal(err)
	}
	if err := d.p.Decide(ref("bug-three"), ChoiceDo); !errors.Is(err, ErrChoice) {
		t.Errorf("do, after it was turned down: %v", err)
	}
	// The Planner out of reach: nothing else would start the task, so the
	// choice fails, loudly, and nothing is recorded.
	d.found(finding("doc-four", map[string]string{"kind": doc}))
	d.set(func() { d.answerErr = errors.New("planner is stopped") })
	if err := d.p.Decide(ref("doc-four"), ChoiceDo); !errors.Is(err, ErrPlanner) || !strings.Contains(err.Error(), "planner is stopped") {
		t.Errorf("do with the Planner stopped: %v, want ErrPlanner", err)
	}
	if r, _ := d.p.Finding(ref("doc-four")); r.State != StateTold || !r.Disposed.IsZero() || r.Item != "" {
		t.Errorf("record %+v, want it untouched", r)
	}
	// Reachable again, the same choice goes through.
	d.set(func() { d.answerErr = nil })
	if err := d.p.Decide(ref("doc-four"), ChoiceDo); err != nil {
		t.Errorf("do with the Planner back: %v", err)
	}
	if r, _ := d.p.Finding(ref("doc-four")); r.State != StateDoing || !r.Adopted {
		t.Errorf("record %+v, want it handed over", r)
	}
	// Not wired at all is out of reach too.
	d.cfg.Answer = nil
	bare := New(d.cfg)
	t.Cleanup(bare.Stop)
	if err := bare.Decide(ref("bug-two"), ChoiceSkip); err != nil {
		t.Errorf("skip with nobody to tell: %v", err)
	}
	d.found(finding("doc-five", map[string]string{"kind": doc}))
	if err := bare.Decide(ref("doc-five"), ChoiceDo); !errors.Is(err, ErrPlanner) {
		t.Errorf("do with nobody to tell: %v, want ErrPlanner", err)
	}
}

// TestDecideOver: a finding whose pull request was merged or closed takes
// no choice any more.
func TestDecideOver(t *testing.T) {
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	d.at(wsA, -time.Hour)
	d.found(finding("bug-one", nil))
	d.p.Dispatched(ref("bug-one"), "task:t1")
	d.p.Reported(ref("bug-one"), "task:t1", "done", []string{"https://github.com/owner/gateway/pull/31"})
	d.fe.gh["owner/gateway"].views = map[string]string{"31": `{"state":"MERGED","statusCheckRollup":` + ciPassing + `}`}
	d.tick()
	for _, c := range []Choice{ChoiceDo, ChoiceSkip, ChoiceMuteKind} {
		if err := d.p.Decide(ref("bug-one"), c); !errors.Is(err, ErrChoice) {
			t.Errorf("%s on a merged finding: %v, want ErrChoice", c, err)
		}
	}
	if d.read(declinedFile) != "" {
		t.Errorf("declined.json written:\n%s", d.read(declinedFile))
	}
}

// TestDeclinedNotReportedAgain: what the user turned down is in the next
// deep check's prompt, and whatever the session reports all the same is
// dropped: the finding under its ID, the kind under any.
func TestDeclinedNotReportedAgain(t *testing.T) {
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	d.found(finding("bug-one", nil), finding("doc-readme", map[string]string{"kind": doc, "title": `"README 的安装步骤与脚本不符"`}))
	if text := d.calls[0].text; !strings.Contains(text, "用户说了不做的：（还没有）") || !strings.Contains(text, "用户不要再提的类别：（没有）") {
		t.Errorf("the first prompt:\n%s", text)
	}
	if err := d.p.Decide(ref("bug-one"), ChoiceSkip); err != nil {
		t.Fatal(err)
	}
	d.now = d.now.Add(time.Minute)
	if err := d.p.Decide(ref("doc-readme"), ChoiceMuteKind); err != nil {
		t.Fatal(err)
	}
	woke := len(d.blocks())

	d.found(finding("bug-one", nil), finding("doc-readme", map[string]string{"kind": doc}),
		finding("doc-config", map[string]string{"kind": doc}), finding("bug-new", nil))
	text := d.calls[1].text
	for _, want := range []string{
		"用户说了不做的：\n- doc-readme · README 的安装步骤与脚本不符\n- bug-one · 重连时会话为空会崩\n",
		"用户不要再提的类别：doc\n",
		"同一件事不要再报，换一个 id、换一种说法也不要",
		"已报过的：（还没有）",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("prompt lacks %q:\n%s", want, text)
		}
	}
	st := d.state(wsA)
	if st.Last.State != RunOK || st.Last.Findings != 1 || st.Last.New != 1 || st.Last.Dropped != 3 {
		t.Errorf("last run %+v, want one kept and three dropped", st.Last)
	}
	if got := states(st); got != "bug-one=declined doc-readme=declined bug-new=told" {
		t.Errorf("records: %s", got)
	}
	blocks := d.blocks()
	if len(blocks) != woke+1 || !strings.Contains(blocks[woke], ref("bug-new")) || strings.Contains(blocks[woke], "bug-one") || strings.Contains(blocks[woke], "doc-") {
		t.Errorf("blocks after the second check: %q", blocks[woke:])
	}
	if !strings.Contains(blocks[woke], filepath.Join(d.cfg.Dir, declinedFile)) {
		t.Errorf("the block does not point the Planner at declined.json:\n%s", blocks[woke])
	}
	// The Planner bringing one up all the same, with none of the user's
	// words, is refused.
	if _, err := d.p.CanDispatch(ref("bug-one"), true); !errors.Is(err, ErrTurnedDown) {
		t.Errorf("dispatching a finding turned down: %v", err)
	}
	for _, kind := range []string{"ask", "pr"} {
		if err := d.p.Surface(ref("bug-one"), kind, true); !errors.Is(err, ErrTurnedDown) {
			t.Errorf("Surface(%s) of a finding turned down: %v", kind, err)
		}
	}
	if known, filed, err := d.p.File(ref("bug-one"), "", ""); !known || filed || err != nil {
		t.Errorf("File of a finding turned down: %v %v %v, want it known and not filed", known, filed, err)
	}
	// Another workspace is not held to this one's list.
	other := d.workspace(wsB, "docs")
	d.fe.repos[other] = &fakeRepo{sha: sha("b")}
	d.file(other, "a.go", 0)
	d.set(func() {
		d.reply = findings(finding("bug-one", map[string]string{"evidence": `["a.go:1"]`}), finding("doc-readme", map[string]string{"kind": doc, "evidence": `["a.go:1"]`}))
	})
	if _, err := d.p.Trigger(wsB); err != nil {
		t.Fatal(err)
	}
	d.wait()
	if got := states(d.state(wsB)); got != "bug-one=told doc-readme=told" {
		t.Errorf("the other workspace's records: %s", got)
	}
	if text := d.calls[2].text; !strings.Contains(text, "用户说了不做的：（还没有）") || !strings.Contains(text, "类别：（没有）") {
		t.Errorf("the other workspace's prompt carries this one's list:\n%s", text)
	}
}

// TestDeclinedHandEdit: the list is the user's to edit and is read from
// disk each time: an entry taken out brings the thing back, reported anew.
func TestDeclinedHandEdit(t *testing.T) {
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	d.found(finding("bug-one", nil), finding("doc-readme", map[string]string{"kind": doc}))
	d.filed("bug-one")
	if err := d.p.Decide(ref("bug-one"), ChoiceSkip); err != nil {
		t.Fatal(err)
	}
	if err := d.p.Decide(ref("doc-readme"), ChoiceMuteKind); err != nil {
		t.Fatal(err)
	}
	d.found(finding("bug-one", nil), finding("doc-other", map[string]string{"kind": doc}))
	if st := d.state(wsA); st.Last.Dropped != 2 || st.Last.New != 0 {
		t.Fatalf("last run %+v, want both dropped", st.Last)
	}
	// The user takes the finding off the list, and the kind.
	d.writeDeclined(`{"v":1,"findings":[{"workspace":"` + wsA + `","id":"doc-readme","kind":"doc","title":"x","at":"2026-11-23T14:02:00+08:00"}],"kinds":null}`)
	woke := len(d.blocks())
	d.now = d.now.Add(time.Hour)
	d.found(finding("bug-one", nil), finding("doc-other", map[string]string{"kind": doc}), finding("doc-readme", map[string]string{"kind": doc}))
	st := d.state(wsA)
	if st.Last.New != 2 || st.Last.Dropped != 1 {
		t.Errorf("last run %+v, want the two off the list new again and the one still on it dropped", st.Last)
	}
	if got := states(st); got != "bug-one=told doc-readme=declined doc-other=told" {
		t.Errorf("records: %s", got)
	}
	r, _ := d.p.Finding(ref("bug-one"))
	if !r.First.Equal(d.now) || !r.Disposed.IsZero() || r.Item != "" {
		t.Errorf("record %+v, want it started over", r)
	}
	if blocks := d.blocks(); len(blocks) != woke+1 || !strings.Contains(blocks[woke], ref("bug-one")) {
		t.Errorf("the Planner was not handed the finding again: %q", blocks[woke:])
	}
	if text := d.calls[len(d.calls)-1].text; !strings.Contains(text, "用户不要再提的类别：（没有）") || strings.Contains(text, "- bug-one ·") {
		t.Errorf("the prompt still carries what was taken off the list:\n%s", text)
	}
}

// TestDeclinedWhileChecking: the list is read again once the session ends:
// what the user turned down while a deep check ran is not reported by it.
func TestDeclinedWhileChecking(t *testing.T) {
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	hold := make(chan struct{})
	d.set(func() {
		d.hold = hold
		d.reply = findings(finding("bug-one", nil), finding("doc-readme", map[string]string{"kind": doc}))
	})
	if _, err := d.p.Trigger(wsA); err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the deep check did not start")
	}
	// The session is reading the repository; the user stops the kind.
	d.writeDeclined(`{"v":1,"findings":[],"kinds":[{"workspace":"` + wsA + `","kind":"doc","by":"user","at":"2026-11-23T14:02:00+08:00"}],"workspaces":[]}`)
	close(hold)
	d.wait()
	st := d.state(wsA)
	if st.Last.State != RunOK || st.Last.Dropped != 1 || states(st) != "bug-one=told" {
		t.Errorf("last run %+v, records %s; want the kind stopped meanwhile dropped", st.Last, states(st))
	}
	// Unreadable by the session's end: the run fails and takes nothing.
	hold = make(chan struct{})
	d.set(func() { d.hold = hold; d.reply = findings(finding("bug-two", nil)) })
	if _, err := d.p.Trigger(wsA); err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the deep check did not start")
	}
	d.writeDeclined("{")
	close(hold)
	d.wait()
	st = d.state(wsA)
	if st.Last.State != RunFailed || !strings.Contains(st.Last.Error, "declined.json 读不了") || states(st) != "bug-one=told" {
		t.Errorf("last run %+v, records %s; want it failed with nothing taken", st.Last, states(st))
	}
}

// TestDeclinedUnreadable: a list that does not read is not a list of
// nothing. No choice is written over it, no deep check runs, the round
// looks at nothing, and the user is told once, until it reads again.
func TestDeclinedUnreadable(t *testing.T) {
	entry := `{"workspace":"` + wsB + `","at":"2026-11-20T10:00:00+08:00"}`
	for name, body := range map[string]string{
		"not JSON":        `{"v":1,"findings":[`,
		"a newer version": `{"v":2,"findings":[],"kinds":[],"workspaces":[]}`,
		// The file is edited by hand, which is where these come from: a save
		// half way, a key renamed, the version line lost. Each would read as
		// nothing declined to a lenient reader.
		"an empty file":              ``,
		"only blanks":                " \n\t\n",
		"null":                       `null`,
		"a list, not an object":      `[]`,
		"a string":                   `"declined"`,
		"an unknown key":             `{"v":1,"findings":[],"kinds":[],"workspace":[` + entry + `]}`,
		"an unknown key in a kind":   `{"v":1,"findings":[],"kinds":[{"workspace":"","kind":"debt","by":"user","when":"2026-11-20T10:00:00+08:00"}],"workspaces":[]}`,
		"an unknown key in a list":   `{"v":1,"findings":[{"workspace":"` + wsB + `","finding":"bug-x"}],"kinds":[],"workspaces":[]}`,
		"no version":                 `{"findings":[],"kinds":[],"workspaces":[` + entry + `]}`,
		"version zero":               `{"v":0,"findings":[],"kinds":[],"workspaces":[]}`,
		"a version that is no int":   `{"v":"1","findings":[],"kinds":[],"workspaces":[]}`,
		"a list that is an object":   `{"v":1,"findings":{},"kinds":[],"workspaces":[]}`,
		"something after the object": `{"v":1,"findings":[],"kinds":[],"workspaces":[]} {"v":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			d := newDeep(t)
			dir := d.repo(wsA, "gateway", "a")
			d.fe.repos[dir].status = " M old.go\x00"
			d.file(dir, "old.go", 9*day)
			d.found(finding("bug-one", nil))
			d.filed("bug-one")
			d.writeDeclined(body)

			for _, c := range []Choice{ChoiceSkip, ChoiceMuteKind, ChoiceDo} {
				if err := d.p.Decide(ref("bug-one"), c); !errors.Is(err, ErrDeclined) {
					t.Errorf("Decide(%s) = %v, want ErrDeclined", c, err)
				}
			}
			if err := d.p.MuteWorkspace(wsA); !errors.Is(err, ErrDeclined) {
				t.Errorf("MuteWorkspace = %v, want ErrDeclined", err)
			}
			if _, err := d.p.Decided(ref("bug-one"), "skip"); !errors.Is(err, ErrDeclined) {
				t.Errorf("Decided(skip) = %v, want ErrDeclined", err)
			}
			if _, err := d.p.Trigger(wsA); !errors.Is(err, ErrDeclined) {
				t.Errorf("Trigger = %v, want ErrDeclined", err)
			}
			if got := d.read(declinedFile); got != body {
				t.Errorf("the file was written over:\n%s", got)
			}
			if r, _ := d.p.Finding(ref("bug-one")); r.State != StateItem || !r.Disposed.IsZero() {
				t.Errorf("record %+v, want it untouched", r)
			}
			// Due, and with a change to find: the round does neither.
			d.at(wsA, 0)
			sessions := d.sessions()
			d.tick()
			d.now = d.now.Add(time.Hour)
			d.tick()
			if d.sessions() != sessions {
				t.Errorf("%d deep checks ran with the list unreadable", d.sessions()-sessions)
			}
			if got := d.open(); !slices.Equal(got, []string{"patrol:" + ref("bug-one") + "|重连时会话为空会崩"}) || d.read(wsA+".md") != "" {
				t.Errorf("the round looked: items %q, file %q", got, d.read(wsA+".md"))
			}
			if len(d.alarms) != 1 || !strings.HasPrefix(d.alarms[0], "patrol:declined|") {
				t.Errorf("alarms %q, want one for the list", d.alarms)
			}
			// Removed, the patrol goes on; unreadable again, the user is
			// told again.
			if err := os.Remove(filepath.Join(d.cfg.Dir, declinedFile)); err != nil {
				t.Fatal(err)
			}
			d.now = d.now.Add(time.Hour)
			d.tick()
			if d.sessions() != sessions+1 || len(d.open()) != 2 {
				t.Errorf("after the list was removed: %d deep checks, items %q", d.sessions()-sessions, d.open())
			}
			d.writeDeclined(body)
			d.now = d.now.Add(time.Hour)
			d.tick()
			if len(d.alarms) != 2 {
				t.Errorf("alarms %q, want a second once it is unreadable again", d.alarms)
			}
		})
	}
}

// TestDeclinedAbsent: no file is nothing declined, and so is one whose
// lists are left out or null (a list emptied by hand): the patrol goes on,
// and the next choice writes the lists back.
func TestDeclinedAbsent(t *testing.T) {
	for name, body := range map[string]string{"no file": "", "no lists": `{"v":1}`, "null lists": `{"v":1,"findings":null,"kinds":null,"workspaces":null}`,
		"blanks around": "\n  {\"v\": 1, \"findings\": []}\n\n"} {
		t.Run(name, func(t *testing.T) {
			d := newDeep(t)
			d.repo(wsA, "gateway", "a")
			d.found(finding("bug-one", nil))
			if body != "" {
				d.writeDeclined(body)
			}
			d.round()
			if len(d.alarms) != 0 || d.read(wsA+".md") == "" {
				t.Fatalf("the round did not run: alarms %q", d.alarms)
			}
			if err := d.p.Decide(ref("bug-one"), ChoiceSkip); err != nil {
				t.Fatal(err)
			}
			got := d.declined()
			if got.V != 1 || len(got.Findings) != 1 || got.Kinds == nil || got.Workspaces == nil {
				t.Errorf("declined.json after a choice: %+v", got)
			}
		})
	}
}

// TestMuteBeforeFirstRound: a workspace taken out of the patrol before any
// round made planner/patrol is written all the same.
func TestMuteBeforeFirstRound(t *testing.T) {
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	if _, err := os.Stat(d.cfg.Dir); !os.IsNotExist(err) {
		t.Fatalf("planner/patrol is there before a round: %v", err)
	}
	if err := d.p.MuteWorkspace(wsA); err != nil {
		t.Fatalf("MuteWorkspace before the first round: %v", err)
	}
	if got := d.declined(); len(got.Workspaces) != 1 || got.Workspaces[0].Workspace != wsA {
		t.Errorf("declined.json %+v", got)
	}
}

// TestDeclinedKeep: a finding turned down stays on the list 180 days; the
// kinds and the workspaces stay until the user takes them out.
func TestDeclinedKeep(t *testing.T) {
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	d.found(finding("bug-one", nil))
	old, recent := d.now.Add(-181*day).Format(time.RFC3339), d.now.Add(-179*day).Format(time.RFC3339)
	d.writeDeclined(`{"v":1,"findings":[
		{"workspace":"` + wsA + `","id":"bug-old","kind":"bug","title":"旧的","at":"` + old + `"},
		{"workspace":"` + wsA + `","id":"bug-recent","kind":"bug","title":"近的","at":"` + recent + `"}],
		"kinds":[{"workspace":"` + wsB + `","kind":"debt","by":"user","at":"` + old + `"}],
		"workspaces":[{"workspace":"` + wsB + `","at":"` + old + `"}]}`)
	if err := d.p.Decide(ref("bug-one"), ChoiceSkip); err != nil {
		t.Fatal(err)
	}
	got := d.declined()
	var ids []string
	for _, f := range got.Findings {
		ids = append(ids, f.ID)
	}
	if !slices.Equal(ids, []string{"bug-recent", "bug-one"}) || len(got.Kinds) != 1 || len(got.Workspaces) != 1 {
		t.Errorf("declined.json %+v, want the finding past 180 days gone and the rest kept", got)
	}
}

// TestMuteWorkspace: "这个工作区别巡查了" takes the workspace out of the
// hourly checks and the deep check alike, closes the patrol's items filed
// under it and turns down its findings still to be settled; a task at work
// in it and the other workspaces are left alone. Taken off the list by
// hand it is patrolled again.
func TestMuteWorkspace(t *testing.T) {
	d := newDeep(t)
	dir := d.repo(wsA, "gateway", "a")
	d.fe.repos[dir].status = " M old.go\x00"
	d.file(dir, "old.go", 9*day)
	other := d.repo(wsB, "docs", "b")
	d.fe.repos[other].status = " M old.go\x00"
	d.file(other, "old.go", 9*day)
	d.cfg.Routines = func() []Routine { return []Routine{{ID: "job1", Title: "夜间构建", Workspace: wsA, Failures: 3}} }
	d.p = New(d.cfg)
	t.Cleanup(d.p.Stop)
	d.at(wsA, -time.Hour)
	if slot(wsB, d.now).Before(d.now) {
		// Keep the other workspace's deep check out of the rounds below.
		d.at(wsB, -time.Hour)
	}
	d.found(finding("bug-one", nil), finding("bug-task", nil))
	d.filed("bug-one")
	d.atWork["task:t1"] = true
	d.p.Dispatched(ref("bug-task"), "task:t1")
	d.round()
	// sources are the open items' sources, sorted.
	sources := func() []string {
		var out []string
		for _, it := range d.items.Items() {
			out = append(out, it.Source)
		}
		slices.Sort(out)
		return out
	}
	dirty := func(dir string) string { return "check:dirty:" + dir }
	const routine = "check:routine:job1"
	if got, want := sources(), []string{dirty(other), dirty(dir), routine, "patrol:" + ref("bug-one")}; !slices.Equal(got, want) {
		t.Fatalf("open items before: %q, want %q", got, want)
	}

	for _, bad := range []string{"ws_zzzzzzz9", "gateway", "", "../" + wsA} {
		if err := d.p.MuteWorkspace(bad); !errors.Is(err, ErrNoWorkspace) {
			t.Errorf("MuteWorkspace(%q) = %v, want ErrNoWorkspace", bad, err)
		}
	}
	if d.read(declinedFile) != "" {
		t.Fatalf("declined.json written for workspaces this node does not have:\n%s", d.read(declinedFile))
	}
	if err := d.p.MuteWorkspace(wsA); err != nil {
		t.Fatal(err)
	}
	if got := d.declined(); len(got.Workspaces) != 1 || got.Workspaces[0].Workspace != wsA || !got.Workspaces[0].At.Equal(d.now) || len(got.Findings) != 0 || len(got.Kinds) != 0 {
		t.Errorf("declined.json %+v", got)
	}
	if got := sources(); !slices.Equal(got, []string{dirty(other), routine}) {
		t.Errorf("open items %q, want the other workspace's and the standing task's", got)
	}
	if got := states(d.state(wsA)); got != "bug-one=declined bug-task=dispatched" {
		t.Errorf("records: %s", got)
	}
	if got := d.cardsClosed(); !slices.Equal(got, []string{ref("bug-one") + "|这个工作区已停止巡查"}) {
		t.Errorf("cards closed %q", got)
	}
	note := d.read(wsA + ".md")
	if !strings.Contains(note, "已于 "+d.now.Format(timeLayout)+" 停止") || !strings.Contains(note, filepath.Join(d.cfg.Dir, declinedFile)) || !strings.Contains(note, `"`+wsA+`"`) {
		t.Errorf("the workspace's file:\n%s", note)
	}

	// The rounds after: neither check looks at it, due or not.
	calls := d.fe.count("git status")
	d.at(wsA, 0)
	sessions := d.sessions()
	d.tick()
	if d.sessions() != sessions {
		t.Errorf("a deep check ran in a workspace out of the patrol")
	}
	if got := d.fe.count("git status") - calls; got != 1 {
		t.Errorf("%d git status calls in the round, want the other workspace's only", got)
	}
	if got := sources(); !slices.Equal(got, []string{dirty(other), routine}) || d.read(wsA+".md") != note {
		t.Errorf("after a round: items %q", got)
	}
	if _, err := d.p.Trigger(wsA); !errors.Is(err, ErrMuted) {
		t.Errorf("Trigger = %v, want ErrMuted", err)
	}
	// Said twice it is listed once.
	if err := d.p.MuteWorkspace(wsA); err != nil || len(d.declined().Workspaces) != 1 {
		t.Errorf("muted twice: %v, %+v", err, d.declined().Workspaces)
	}

	// Taken off the list: patrolled again, and the finding turned down
	// with it is reported anew.
	d.writeDeclined(`{"v":1,"findings":[],"kinds":[],"workspaces":[]}`)
	d.now = d.now.Add(time.Hour)
	d.active[wsA] = d.now // a session since the last deep check
	d.set(func() { d.reply = findings(finding("bug-one", nil)) })
	d.tick()
	if d.sessions() != sessions+1 || !slices.Contains(sources(), dirty(dir)) {
		t.Errorf("after it was taken off the list: %d deep checks, items %q", d.sessions()-sessions, sources())
	}
	if got := states(d.state(wsA)); got != "bug-one=told bug-task=dispatched" {
		t.Errorf("records: %s", got)
	}
	if strings.Contains(d.read(wsA+".md"), "停止") {
		t.Errorf("the workspace's file still says it is stopped:\n%s", d.read(wsA+".md"))
	}
}

// TestCard: a notice about a finding carries the user's choices, "do"
// only while nobody has the finding in hand; the card's answers are the
// same choices.
func TestCard(t *testing.T) {
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	d.found(finding("bug-one", nil), finding("bug-pr", nil), finding("bug-task", nil), finding("bug-no", nil), finding("doc-readme", map[string]string{"kind": doc}))
	d.atWork["task:t1"] = true
	d.p.Dispatched(ref("bug-task"), "task:t1")
	d.p.Dispatched(ref("bug-pr"), "task:t2")
	d.p.Reported(ref("bug-pr"), "task:t2", "done", []string{"https://github.com/owner/gateway/pull/31"})
	full := []notify.Action{"做", "不做", "这类别再提"}
	for id, want := range map[string][]notify.Action{"bug-one": full, "bug-pr": full[1:], "bug-task": full[1:]} {
		c := d.p.Card(ref(id))
		if c == nil || c.Handler != Handler || string(c.Payload) != `{"ref":"`+ref(id)+`"}` || !slices.Equal(c.Actions, want) ||
			!slices.Equal(c.More, []notify.Action{"这个工作区别巡查了"}) {
			t.Errorf("Card(%s) = %+v, want %q", id, c, want)
		}
	}
	if err := d.p.Decide(ref("bug-no"), ChoiceSkip); err != nil {
		t.Fatal(err)
	}
	for _, none := range []string{ref("bug-no"), ref("bug-nope"), "x"} {
		if c := d.p.Card(none); c != nil {
			t.Errorf("Card(%q) = %+v, want none", none, c)
		}
	}

	card := func(id string) notify.Item { return notify.Item{ID: "n1", Decision: d.p.Card(ref(id))} }
	if got := CardRef(card("bug-one")); got != ref("bug-one") {
		t.Errorf("CardRef = %q", got)
	}
	if got := CardRef(notify.Item{Decision: &notify.Decision{Handler: "ask", Payload: []byte(`{"ref":"` + ref("bug-one") + `"}`)}}); got != "" {
		t.Errorf("CardRef of another handler's card = %q", got)
	}
	if err := d.p.DecideCard(card("bug-one"), "合并"); err == nil {
		t.Error("an answer the card does not have was taken")
	}
	if err := d.p.DecideCard(notify.Item{Decision: &notify.Decision{Handler: Handler, Payload: []byte(`{"ref":"nope"}`)}}, LabelSkip); err == nil {
		t.Error("a card naming no finding was taken")
	}
	// What the patrol does not take is refused in words the user reads,
	// and the card stays.
	d.set(func() { d.answerErr = errors.New("planner is stopped") })
	var refused notify.Refused
	if err := d.p.DecideCard(card("bug-one"), LabelDo); !errors.As(err, &refused) || !strings.Contains(refused.Why, ErrPlanner.Error()) {
		t.Errorf("做 with the Planner stopped: %v, want it refused with the reason", err)
	}
	d.set(func() { d.answerErr = nil })
	if err := d.p.DecideCard(card("bug-one"), LabelDo); err != nil {
		t.Fatal(err)
	}
	if r, _ := d.p.Finding(ref("bug-one")); r.State != StateDoing || len(d.told()) != 1 {
		t.Errorf("做 on the card: record %+v", r)
	}
	pr := card("bug-pr")
	if err := d.p.DecideCard(pr, LabelSkip); err != nil {
		t.Fatal(err)
	}
	if r, _ := d.p.Finding(ref("bug-pr")); r.State != StateDeclined || !d.declined().finding(wsA, "bug-pr") {
		t.Errorf("不做 on the card: record %+v", r)
	}
	doc := card("doc-readme")
	if err := d.p.DecideCard(doc, LabelMuteKind); err != nil {
		t.Fatal(err)
	}
	if !d.declined().kind(wsA, KindDoc) {
		t.Errorf("这类别再提 on the card: %+v", d.declined())
	}
	if err := d.p.DecideCard(card("bug-task"), LabelMuteWS); err != nil {
		t.Fatal(err)
	}
	if !d.declined().workspace(wsA) {
		t.Errorf("这个工作区别巡查了 on the card: %+v", d.declined())
	}
}
