package patrol

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/planner/front"
)

// handedOver is a patrol with findings ids found in wsA and handed to the
// Planner, whose turn has not ended.
func handedOver(t *testing.T, ids ...string) *deepEnv {
	t.Helper()
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	d.at(wsA, 0)
	var fs []string
	for _, id := range ids {
		fs = append(fs, finding(id, nil))
	}
	d.found(fs...)
	if got := len(d.blocks()); got != 1 {
		t.Fatalf("%d blocks handed over, want 1", got)
	}
	return d
}

// later is a round d after now, and the deep check it started.
func (d *deepEnv) later(after time.Duration) {
	d.t.Helper()
	d.now = d.now.Add(after)
	d.tick()
}

// TestHandedIsNotSettled: handing a finding to the Planner is not the end
// of it. Once the Planner's turn ended, what it said nothing of is filed as
// an open item by the gateway, not before the reply's blocks had their
// time; what it did say something of — a task, a question, an item, let be
// — is left as the Planner put it. Nothing found stays told for ever.
func TestHandedIsNotSettled(t *testing.T) {
	d := handedOver(t, "bug-task", "bug-ask", "bug-item", "bug-pass", "bug-left")
	const all = "bug-task=told bug-ask=told bug-item=told bug-pass=told bug-left=told"
	if got := states(d.state(wsA)); got != all {
		t.Fatalf("handed over: %s", got)
	}
	// The turn waits its place in the Planner's queue, then runs: rounds
	// meanwhile neither hand the findings over again nor file them.
	d.later(time.Hour)
	d.later(time.Hour)
	if got := states(d.state(wsA)); got != all || len(d.blocks()) != 1 || len(d.items.Items()) != 0 {
		t.Fatalf("while the turn is queued: %s, %d blocks, %d items", got, len(d.blocks()), len(d.items.Items()))
	}
	// The turn ends: its reply's blocks are run a moment after, so a round
	// that comes right then leaves the findings to them.
	d.over(nil)
	d.later(handGrace - time.Minute)
	if got := states(d.state(wsA)); got != all || len(d.items.Items()) != 0 {
		t.Fatalf("within the grace: %s, %d items", got, len(d.items.Items()))
	}
	// The blocks: a dispatch, a question, an item in the Planner's words,
	// and one let be.
	d.atWork["task:t1"] = true
	d.p.Dispatched(ref("bug-task"), "task:t1")
	if err := d.p.Surface(ref("bug-ask"), "ask", true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.p.File(ref("bug-item"), "重连要先判空（Planner 的话）", ""); err != nil {
		t.Fatal(err)
	}
	if known, err := d.p.Pass(ref("bug-pass")); !known || err != nil {
		t.Fatalf("Pass = %v, %v", known, err)
	}
	// The round after: only the one the Planner said nothing of is filed,
	// in the finding's own words, once.
	d.later(time.Hour)
	if got := states(d.state(wsA)); got != "bug-task=dispatched bug-ask=asked bug-item=item bug-pass=passed bug-left=item" {
		t.Fatalf("after the turn: %s", got)
	}
	want := []string{"patrol:" + ref("bug-item") + "|重连要先判空（Planner 的话）", "patrol:" + ref("bug-left") + "|重连时会话为空会崩"}
	if got := d.open(); !slices.Equal(got, want) {
		t.Fatalf("open items %q, want %q", got, want)
	}
	if r, _ := d.p.Finding(ref("bug-left")); r.Item == "" || r.Sent != 1 {
		t.Errorf("the finding the gateway filed: %+v", r)
	}
	// And that is the end of it: no second block, no second item.
	d.later(time.Hour)
	d.later(7 * day)
	if len(d.blocks()) != 1 || len(d.items.Items()) != 2 {
		t.Errorf("%d blocks and %d items a week on, want 1 and 2", len(d.blocks()), len(d.items.Items()))
	}
}

// TestHandedTurnFailed: a turn that failed, or was dropped from the
// Planner's queue, did not deal with its findings: they go once more, and
// after a second such turn the gateway files them itself. They are never
// handed over a third time, and never lost.
func TestHandedTurnFailed(t *testing.T) {
	for name, fail := range map[string]error{"the turn failed": errors.New("CLI exited"), "dropped from the queue": errors.New("消息未被处理")} {
		t.Run(name, func(t *testing.T) {
			d := handedOver(t, "bug-one", "bug-two")
			d.over(fail)
			if got := states(d.state(wsA)); got != "bug-one=told bug-two=told" || len(d.items.Items()) != 0 {
				t.Fatalf("after the failed turn: %s, %d items", got, len(d.items.Items()))
			}
			d.later(time.Hour)
			blocks := d.blocks()
			if len(blocks) != 2 || !strings.Contains(blocks[1], ref("bug-one")) || !strings.Contains(blocks[1], ref("bug-two")) || !strings.Contains(blocks[1], "这是最后一次交给你") {
				t.Fatalf("blocks after the failed turn: %q", blocks)
			}
			if strings.Contains(blocks[0], "最后一次") {
				t.Errorf("the first block says it is the last:\n%s", blocks[0])
			}
			st := d.state(wsA)
			if st.Findings[0].Sent != 2 || st.Findings[1].Sent != 2 {
				t.Fatalf("handed over %d and %d times, want 2 each", st.Findings[0].Sent, st.Findings[1].Sent)
			}
			// The second turn deals with one and fails all the same.
			d.atWork["task:t1"] = true
			d.p.Dispatched(ref("bug-one"), "task:t1")
			d.over(fail)
			d.later(time.Hour)
			if got := states(d.state(wsA)); got != "bug-one=dispatched bug-two=item" || len(d.blocks()) != 2 {
				t.Fatalf("after the second failed turn: %s, %d blocks", got, len(d.blocks()))
			}
			if got := d.open(); !slices.Equal(got, []string{"patrol:" + ref("bug-two") + "|重连时会话为空会崩"}) {
				t.Errorf("open items %q", got)
			}
		})
	}
}

// TestHandedLostToRestart: a turn queued, or running, when naozhi restarts
// is lost with the process's memory. The findings it carried are still told
// on disk, so the next run hands them over once more; a second loss and
// they are filed. One the Planner had dealt with before the restart is not
// sent again.
func TestHandedLostToRestart(t *testing.T) {
	d := handedOver(t, "bug-one", "bug-two")
	d.atWork["task:t1"] = true
	d.p.Dispatched(ref("bug-one"), "task:t1")
	d.restart()
	d.later(time.Hour)
	blocks := d.blocks()
	if len(blocks) != 2 || strings.Contains(blocks[1], ref("bug-one")) || !strings.Contains(blocks[1], ref("bug-two")) || !strings.Contains(blocks[1], `findings="1"`) {
		t.Fatalf("blocks after the restart: %q", blocks)
	}
	if r, _ := d.p.Finding(ref("bug-two")); r.State != StateTold || r.Sent != 2 {
		t.Fatalf("after the restart: %+v", r)
	}
	// Restarted again before that turn ran: not a third time.
	d.restart()
	d.later(time.Hour)
	if got := states(d.state(wsA)); got != "bug-one=dispatched bug-two=item" || len(d.blocks()) != 2 {
		t.Fatalf("after the second restart: %s, %d blocks", got, len(d.blocks()))
	}
	if got := d.open(); !slices.Equal(got, []string{"patrol:" + ref("bug-two") + "|重连时会话为空会崩"}) {
		t.Errorf("open items %q", got)
	}
	// A restart after the turn ended and before the gateway filed what was
	// left: the same, one more hand-over and then the item.
	d = handedOver(t, "bug-left")
	d.over(nil)
	d.restart()
	d.later(time.Hour)
	if got := states(d.state(wsA)); got != "bug-left=told" || len(d.blocks()) != 2 {
		t.Fatalf("restarted after the turn: %s, %d blocks", got, len(d.blocks()))
	}
	d.over(nil)
	d.later(time.Hour)
	if got := states(d.state(wsA)); got != "bug-left=item" || len(d.blocks()) != 2 || len(d.items.Items()) != 1 {
		t.Errorf("after its second turn: %s, %d blocks, %d items", got, len(d.blocks()), len(d.items.Items()))
	}
}

// TestHandedFilingFails: the ledger full, a finding the Planner left stays
// told and is filed at a later round, not dropped and not sent again.
func TestHandedFilingFails(t *testing.T) {
	d := handedOver(t, "bug-left")
	d.over(nil)
	for i := range front.MaxOpenItems {
		if _, _, err := d.items.AddItem(front.Item{Text: "占位", Source: "x:" + string(rune('a'+i%26)) + string(rune('a'+i/26)), From: front.FromUser}); err != nil {
			t.Fatalf("filling the ledger at %d: %v", i, err)
		}
	}
	d.later(time.Hour)
	if got := states(d.state(wsA)); got != "bug-left=told" || len(d.blocks()) != 1 {
		t.Fatalf("with the ledger full: %s, %d blocks", got, len(d.blocks()))
	}
	for _, it := range d.items.Items()[:3] {
		if _, err := d.items.CloseItem(it.ID, front.ByUser); err != nil {
			t.Fatal(err)
		}
	}
	d.later(time.Hour)
	if got := states(d.state(wsA)); got != "bug-left=item" || len(d.blocks()) != 1 {
		t.Errorf("once the ledger has room: %s, %d blocks", got, len(d.blocks()))
	}
}

// TestPass: the Planner lets a finding be when it is covered already. It
// gets no open item; one the gateway filed meanwhile is closed. What a
// task, a pull request or the user has in hand is not the Planner's to
// pass over.
func TestPass(t *testing.T) {
	d := handedOver(t, "bug-new", "bug-filed", "bug-asked", "bug-pr", "bug-no")
	d.filed("bug-filed")
	if err := d.p.Surface(ref("bug-asked"), "ask", true); err != nil {
		t.Fatal(err)
	}
	d.atWork["task:t2"] = true
	d.p.Dispatched(ref("bug-pr"), "task:t2")
	if known, err := d.p.Pass(ref("bug-pr")); !known || err == nil {
		t.Errorf("Pass of a finding a task has in hand = %v, %v; want it refused", known, err)
	}
	d.p.Reported(ref("bug-pr"), "task:t2", "done", []string{"https://github.com/owner/gateway/pull/31"})
	if err := d.p.Decide(ref("bug-no"), ChoiceSkip); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"bug-new", "bug-filed"} {
		if known, err := d.p.Pass(ref(id)); !known || err != nil {
			t.Errorf("Pass(%s) = %v, %v", id, known, err)
		}
	}
	for _, id := range []string{"bug-asked", "bug-pr"} {
		if known, err := d.p.Pass(ref(id)); !known || err == nil {
			t.Errorf("Pass(%s) = %v, %v; want it refused", id, known, err)
		}
	}
	// Over already: nothing to do, and no error.
	if known, err := d.p.Pass(ref("bug-no")); !known || err != nil {
		t.Errorf("Pass of a finding turned down = %v, %v", known, err)
	}
	if known, err := d.p.Pass(ref("bug-nope")); known || err != nil {
		t.Errorf("Pass of an unknown ref = %v, %v", known, err)
	}
	if known, err := d.p.Pass("not a ref"); known || err != nil {
		t.Errorf("Pass of no ref = %v, %v", known, err)
	}
	if got := states(d.state(wsA)); got != "bug-new=passed bug-filed=passed bug-asked=asked bug-pr=pr bug-no=declined" {
		t.Errorf("records: %s", got)
	}
	if got := d.open(); !slices.Equal(got, []string{"patrol:" + ref("bug-pr") + "|已备好 PR #31：重连时会话为空会崩"}) {
		t.Errorf("open items %q, want the passed finding's item closed", got)
	}
	// Passed over, it is not handed over again, not filed, not listed as
	// new in the brief, and the next check is told it is covered.
	d.over(nil)
	d.later(time.Hour)
	if len(d.blocks()) != 1 || len(d.items.Items()) != 1 {
		t.Errorf("%d blocks and %d items after a round, want 1 and 1", len(d.blocks()), len(d.items.Items()))
	}
	if n := len(d.p.Brief(d.now.Add(-30 * day)).New); n != 1 {
		t.Errorf("%d new findings in the brief, want only the one asked about", n)
	}
	d.found(finding("bug-new", nil))
	if text := d.calls[len(d.calls)-1].text; !strings.Contains(text, "- bug-new · 重连时会话为空会崩 · 已有 issue 或未了事项覆盖") {
		t.Errorf("the next check is not told the finding is covered:\n%s", text)
	}
	if got := states(d.state(wsA)); !strings.HasPrefix(got, "bug-new=passed ") {
		t.Errorf("found again: %s", got)
	}
}

// TestTaskStillGoing: a task that is not over keeps its finding and its
// place in the workspace's cap, whatever the task board shows of it for a
// while; only one that is over without a pull request gives the finding
// back as an open item. A report that comes after that — the task was
// stopped and taken up again — still makes the pull request the finding's.
func TestTaskStillGoing(t *testing.T) {
	d := handedOver(t, "bug-one", "bug-two", "bug-three")
	for i, id := range []string{"bug-one", "bug-two"} {
		key := "task:t" + string(rune('1'+i))
		d.atWork[key] = true
		d.p.Dispatched(ref(id), key)
	}
	// Both going: the workspace is full, for a turn with none of the
	// user's words.
	if _, err := d.p.CanDispatch(ref("bug-three"), true); err == nil || !strings.Contains(err.Error(), "已有 2 个在做") {
		t.Fatalf("a third task with two going: %v", err)
	}
	d.later(time.Hour)
	if got := states(d.state(wsA)); !strings.HasPrefix(got, "bug-one=dispatched bug-two=dispatched") {
		t.Fatalf("with both going: %s", got)
	}
	// The first is stopped by the user and left no pull request: the
	// finding is an open item again and its place is free.
	d.set(func() { d.atWork["task:t1"] = false })
	d.later(time.Hour)
	if r, _ := d.p.Finding(ref("bug-one")); r.State != StateItem || r.Item == "" || r.Task != "task:t1" {
		t.Fatalf("after the task was stopped: %+v", r)
	}
	if _, err := d.p.CanDispatch(ref("bug-three"), true); err != nil {
		t.Fatalf("a task with one going: %v", err)
	}
	// The user takes the task up again and it reports its pull request:
	// recorded, the item says so, and the round follows it.
	d.set(func() { d.atWork["task:t1"] = true })
	d.p.Reported(ref("bug-one"), "task:t1", "done", []string{"https://github.com/owner/gateway/pull/41"})
	r, _ := d.p.Finding(ref("bug-one"))
	if r.State != StatePR || r.PR != "https://github.com/owner/gateway/pull/41" || r.Ready.IsZero() {
		t.Fatalf("a report after the finding went back to the list: %+v", r)
	}
	if got := d.open(); !slices.Contains(got, "patrol:"+ref("bug-one")+"|已备好 PR #41：重连时会话为空会崩") {
		t.Errorf("open items %q, want the finding's item to say the pull request is ready", got)
	}
	d.fe.gh["owner/gateway"].views = map[string]string{"41": `{"state":"MERGED","statusCheckRollup":[]}`}
	d.later(time.Hour)
	if r, _ := d.p.Finding(ref("bug-one")); r.State != StateMerged || !r.Adopted {
		t.Errorf("after the pull request was merged: %+v", r)
	}
	// Another task's report is not this finding's.
	d.p.Reported(ref("bug-two"), "task:t1", "done", []string{"https://github.com/owner/gateway/pull/42"})
	if r, _ := d.p.Finding(ref("bug-two")); r.State != StateDispatched || r.PR != "" {
		t.Errorf("a report of another task's: %+v", r)
	}
}

// TestDoOnce: "做" said twice — on a card still open elsewhere, by a
// request sent again — is said to the Planner once; the cards about the
// finding close with the first; and a finding a task has in hand is not
// dispatched a second time, whoever asks.
func TestDoOnce(t *testing.T) {
	d := handedOver(t, "bug-one")
	d.filed("bug-one")
	if err := d.p.Decide(ref("bug-one"), ChoiceDo); err != nil {
		t.Fatal(err)
	}
	if err := d.p.Decide(ref("bug-one"), ChoiceDo); err != nil {
		t.Fatalf("the second 做: %v", err)
	}
	if got := d.told(); len(got) != 1 {
		t.Fatalf("the Planner was told %d times, want once: %q", len(got), got)
	}
	if got := d.cardsClosed(); !slices.Equal(got, []string{ref("bug-one") + "|你已选了做"}) {
		t.Errorf("cards closed %q", got)
	}
	if r, _ := d.p.Finding(ref("bug-one")); r.State != StateDoing {
		t.Fatalf("record %+v", r)
	}
	// The Planner dispatches on the user's word: allowed once.
	if _, err := d.p.CanDispatch(ref("bug-one"), false); err != nil {
		t.Fatalf("the user's dispatch: %v", err)
	}
	d.atWork["task:t1"] = true
	d.p.Dispatched(ref("bug-one"), "task:t1")
	for _, unasked := range []bool{false, true} {
		if _, err := d.p.CanDispatch(ref("bug-one"), unasked); err == nil || !strings.Contains(err.Error(), "已经派给 task:t1 在做") {
			t.Errorf("a second dispatch (unasked %v): %v", unasked, err)
		}
	}
	// Its task over with nothing to show, the user may have it done again.
	d.set(func() { d.atWork["task:t1"] = false })
	if _, err := d.p.CanDispatch(ref("bug-one"), false); err != nil {
		t.Errorf("the user's dispatch once the task is over: %v", err)
	}
}

// TestMutedWhileChecking: a workspace taken out of the patrol while its
// deep check runs gets nothing of that run: no finding on record, nothing
// handed to the Planner, nothing to dispatch.
func TestMutedWhileChecking(t *testing.T) {
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	d.at(wsA, 0)
	hold := make(chan struct{})
	d.set(func() {
		d.reply, d.hold = findings(finding("bug-one", nil), finding("doc-two", map[string]string{"kind": `"doc"`})), hold
	})
	if _, err := d.p.Trigger(wsA); err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("deep check did not start")
	}
	if err := d.p.MuteWorkspace(wsA); err != nil {
		t.Fatal(err)
	}
	close(hold)
	d.wait()
	st := d.state(wsA)
	if len(st.Findings) != 0 || st.Last == nil || st.Last.State != RunOK || st.Last.Findings != 0 || st.Last.Dropped != 2 {
		t.Fatalf("records %s, last run %+v; want both findings dropped", states(st), st.Last)
	}
	if len(d.blocks()) != 0 {
		t.Errorf("%d blocks handed to the Planner for a workspace out of the patrol", len(d.blocks()))
	}
	if _, err := d.p.CanDispatch(ref("bug-one"), true); !errors.Is(err, ErrNoFinding) {
		t.Errorf("CanDispatch = %v, want no such finding", err)
	}
}
