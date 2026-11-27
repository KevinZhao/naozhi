package patrol

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

const debt = `"debt"`

// debts runs a deep check of the first workspace that reports n findings
// of kind debt, debt-<from>… on, and returns their IDs.
func (d *deepEnv) debts(from, n int) []string {
	d.t.Helper()
	var ids, fs []string
	for i := from; i < from+n; i++ {
		id := fmt.Sprintf("debt-%02d", i)
		ids = append(ids, id)
		fs = append(fs, finding(id, map[string]string{"kind": debt}))
	}
	d.found(fs...)
	return ids
}

// settle has the user say do (true) or don't about each finding in turn, a
// minute apart.
func (d *deepEnv) settle(ids []string, taken ...bool) {
	d.t.Helper()
	for i, id := range ids {
		d.now = d.now.Add(time.Minute)
		c := ChoiceSkip
		if taken[i] {
			c = ChoiceDo
		}
		if err := d.p.Decide(ref(id), c); err != nil {
			d.t.Fatalf("Decide(%s, %s): %v", id, c, err)
		}
	}
}

// stops are the kinds the gateway stopped, as "kind adopted/of".
func (d *deepEnv) stops() []string {
	var out []string
	for _, k := range d.declined().Kinds {
		if k.By == "auto" {
			out = append(out, fmt.Sprintf("%s%s %d/%d", k.Workspace, k.Kind, k.Adopted, k.Of))
		}
	}
	return out
}

// TestAdoptionStops: a kind of which the user settled ten findings and took
// fewer than five is no longer reported, in any workspace; five taken keep
// it, and so do fewer than ten settled.
func TestAdoptionStops(t *testing.T) {
	const (
		yes = true
		no  = false
	)
	for name, tc := range map[string]struct {
		taken []bool
		want  []string
	}{
		"4 of 10 taken":  {[]bool{yes, no, yes, no, no, yes, no, no, yes, no}, []string{"debt 4/10"}},
		"none taken":     {[]bool{no, no, no, no, no, no, no, no, no, no}, []string{"debt 0/10"}},
		"5 of 10 taken":  {[]bool{yes, no, yes, no, yes, no, yes, no, yes, no}, nil},
		"9 settled only": {[]bool{no, no, no, no, no, no, no, no, no}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			d := newDeep(t)
			d.repo(wsA, "gateway", "a")
			ids := d.debts(1, 10)
			d.settle(ids[:len(tc.taken)], tc.taken...)
			if got := d.stops(); !slices.Equal(got, tc.want) {
				t.Errorf("stopped %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAdoptionWindow: the share is of the latest ten settled, in all
// workspaces together, not of all there ever were; a kind is stopped once.
func TestAdoptionWindow(t *testing.T) {
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	other := d.workspace(wsB, "docs")
	d.fe.repos[other] = &fakeRepo{sha: sha("b")}
	d.file(other, "a.go", 0)
	ids := d.debts(1, 10)
	// Six taken, then four not: 6 of 10.
	d.settle(ids, true, true, true, true, true, true, false, false, false, false)
	if got := d.stops(); got != nil {
		t.Fatalf("stopped %q at 6 of 10", got)
	}
	// Two more in the other workspace, not taken. After the first the
	// latest ten hold five taken; after the second, four.
	d.set(func() {
		d.reply = findings(finding("debt-11", map[string]string{"kind": debt, "evidence": `["a.go:1"]`}), finding("debt-12", map[string]string{"kind": debt, "evidence": `["a.go:1"]`}),
			finding("bug-one", map[string]string{"evidence": `["a.go:1"]`}))
	})
	if _, err := d.p.Trigger(wsB); err != nil {
		t.Fatal(err)
	}
	d.wait()
	d.now = d.now.Add(time.Minute)
	if err := d.p.Decide(Ref(wsB, "debt-11"), ChoiceSkip); err != nil {
		t.Fatal(err)
	}
	if got := d.stops(); got != nil {
		t.Fatalf("stopped %q with 5 of the latest 10 taken", got)
	}
	d.now = d.now.Add(time.Minute)
	if err := d.p.Decide(Ref(wsB, "debt-12"), ChoiceSkip); err != nil {
		t.Fatal(err)
	}
	if got := d.stops(); !slices.Equal(got, []string{"debt 4/10"}) {
		t.Fatalf("stopped %q, want debt at 4 of the latest 10 (6 of all 12)", got)
	}
	// Another kind's findings count for their own kind only.
	d.now = d.now.Add(time.Minute)
	if err := d.p.Decide(Ref(wsB, "bug-one"), ChoiceSkip); err != nil {
		t.Fatal(err)
	}
	if got := d.stops(); len(got) != 1 {
		t.Errorf("stopped %q after another kind's finding", got)
	}

	// Stopped everywhere: the prompt says so and what is reported all the
	// same is dropped.
	d.set(func() {
		d.reply = findings(finding("debt-13", map[string]string{"kind": debt}), finding("bug-two", nil))
	})
	if _, err := d.p.Trigger(wsA); err != nil {
		t.Fatal(err)
	}
	d.wait()
	if text := d.calls[len(d.calls)-1].text; !strings.Contains(text, "用户不要再提的类别：debt\n") {
		t.Errorf("the prompt does not carry the stopped kind:\n%s", text)
	}
	if st := d.state(wsA); st.Last.Dropped != 1 || st.Last.New != 1 || st.find("debt-13") != nil {
		t.Errorf("last run %+v, want the stopped kind's finding dropped", st.Last)
	}
}

// TestAdoptionOncePerFinding: a finding counts once, by the user's latest
// word on it; its pull request merged counts as taken and closed unmerged
// as not, unless the user had said already.
func TestAdoptionOncePerFinding(t *testing.T) {
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	d.at(wsA, -time.Hour)
	ids := d.debts(1, 10)
	// Nine not taken; the tenth said "do" first, then "don't": still nine
	// and one, and that one not taken.
	d.settle(ids[:8], false, false, false, false, false, false, false, false)
	d.now = d.now.Add(time.Minute)
	if err := d.p.Decide(ref(ids[9]), ChoiceDo); err != nil {
		t.Fatal(err)
	}
	d.now = d.now.Add(time.Minute)
	if err := d.p.Decide(ref(ids[9]), ChoiceSkip); err != nil {
		t.Fatal(err)
	}
	if got := d.stops(); got != nil {
		t.Fatalf("stopped %q with nine findings settled: one was counted twice", got)
	}
	if r, _ := d.p.Finding(ref(ids[9])); r.Adopted || !r.Disposed.Equal(d.now) {
		t.Errorf("record %+v, want the latest word to stand", r)
	}

	// The ninth: dispatched by the patrol, its pull request closed without
	// a merge. Settled by that fact, not taken: ten, none taken.
	d.p.Dispatched(ref(ids[8]), "task:t1")
	d.p.Reported(ref(ids[8]), "task:t1", "done", []string{"https://github.com/owner/gateway/pull/31"})
	gh := d.fe.gh["owner/gateway"]
	gh.views = map[string]string{"31": `{"state":"CLOSED","statusCheckRollup":` + ciFailing + `}`}
	d.now = d.now.Add(time.Hour)
	d.tick()
	if r, _ := d.p.Finding(ref(ids[8])); r.State != StateClosed || r.Disposed.IsZero() || r.Adopted {
		t.Errorf("record %+v, want it settled as not taken by its pull request closing", r)
	}
	if got := d.stops(); !slices.Equal(got, []string{"debt 0/10"}) {
		t.Errorf("stopped %q, want debt stopped by the tenth", got)
	}
	if got := d.cardsClosed(); !slices.Contains(got, ref(ids[8])+"|它的 PR 已关闭") {
		t.Errorf("cards closed %q, want the finding's once its pull request is over", got)
	}
}

// TestAdoptionMerged: a pull request merged counts the finding as taken; a
// user's earlier word about it stands.
func TestAdoptionMerged(t *testing.T) {
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	d.at(wsA, -time.Hour)
	d.found(finding("bug-one", nil), finding("bug-two", nil))
	said := d.now.Add(time.Minute)
	d.now = said
	if err := d.p.Decide(ref("bug-two"), ChoiceDo); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"bug-one", "bug-two"} {
		key := fmt.Sprintf("task:t%d", i+1)
		d.p.Dispatched(ref(id), key)
		d.p.Reported(ref(id), key, "done", []string{fmt.Sprintf("https://github.com/owner/gateway/pull/%d", 31+i)})
	}
	if r, _ := d.p.Finding(ref("bug-one")); !r.Disposed.IsZero() {
		t.Errorf("record %+v: a pull request waiting is not the finding settled", r)
	}
	d.fe.gh["owner/gateway"].views = map[string]string{
		"31": `{"state":"MERGED","statusCheckRollup":` + ciPassing + `}`,
		"32": `{"state":"CLOSED","statusCheckRollup":` + ciPassing + `}`,
	}
	d.now = d.now.Add(time.Hour)
	d.tick()
	if r, _ := d.p.Finding(ref("bug-one")); r.State != StateMerged || !r.Adopted || !r.Disposed.Equal(d.now) {
		t.Errorf("record %+v, want it taken by the merge", r)
	}
	if r, _ := d.p.Finding(ref("bug-two")); r.State != StateClosed || !r.Adopted || !r.Disposed.Equal(said) {
		t.Errorf("record %+v, want the user's word to stand over the pull request closing", r)
	}
	if got := d.cardsClosed(); !slices.Contains(got, ref("bug-one")+"|它的 PR 已合并") {
		t.Errorf("cards closed %q", got)
	}
}

// TestAdoptionResumedPastWindow: with more than ten settled when the kind
// is stopped, the ones before the last ten are counted too: brought back,
// the kind is stopped again only on ten more, not on the first few that
// fill a window up with what was settled long ago. And a finding counted
// once is not counted again for being settled a second time.
func TestAdoptionResumedPastWindow(t *testing.T) {
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	// Five taken and five not: not stopped. An eleventh, not taken, tips
	// the last ten to 4 of 10.
	first := append(d.debts(1, 10), d.debts(11, 1)...)
	d.settle(first, true, false, true, false, true, false, true, false, true, false, false)
	if got := d.stops(); !slices.Equal(got, []string{"debt 4/10"}) {
		t.Fatalf("stopped %q", got)
	}
	for _, id := range first {
		if r, _ := d.p.Finding(ref(id)); !r.Tallied {
			t.Errorf("%s was settled before the stop and is not counted towards it: %+v", id, r)
		}
	}
	d.writeDeclined(`{"v":1,"findings":[],"kinds":[],"workspaces":[]}`)
	// One counted already, said again: still one finding, still counted.
	if err := d.p.Decide(ref(first[0]), ChoiceSkip); err != nil {
		t.Fatal(err)
	}
	if r, _ := d.p.Finding(ref(first[0])); !r.Tallied || r.Adopted {
		t.Errorf("settled a second time: %+v, want the latest word and still counted", r)
	}
	more := d.debts(12, 10)
	no := make([]bool, 10)
	d.settle(more[:9], no[:9]...)
	if got := d.stops(); got != nil {
		t.Fatalf("stopped %q again on nine more", got)
	}
	d.settle(more[9:], false)
	if got := d.stops(); !slices.Equal(got, []string{"debt 0/10"}) {
		t.Errorf("stopped %q, want it stopped again on the tenth", got)
	}
}

// TestDecided: an answer to a question about a finding. One of the
// Planner's own options counts as the finding taken and leaves it where it
// is; the two the gateway adds turn it down, and say what that leaves to do.
func TestDecided(t *testing.T) {
	d := newDeep(t)
	d.repo(wsA, "gateway", "a")
	d.found(finding("bug-one", nil), finding("bug-two", nil), finding("bug-task", nil), finding("doc-readme", map[string]string{"kind": doc}))
	if err := d.p.Surface(ref("bug-one"), "ask", true); err != nil {
		t.Fatal(err)
	}
	left, err := d.p.Decided(ref("bug-one"), "")
	if left != "" || err != nil {
		t.Fatalf("Decided = %q, %v", left, err)
	}
	if r, _ := d.p.Finding(ref("bug-one")); r.State != StateAsked || !r.Adopted || !r.Disposed.Equal(d.now) {
		t.Errorf("record %+v, want it taken and still asked", r)
	}
	left, err = d.p.Decided(ref("bug-two"), "skip")
	if err != nil || !strings.Contains(left, "记进被否清单") {
		t.Fatalf("Decided(skip) = %q, %v", left, err)
	}
	if r, _ := d.p.Finding(ref("bug-two")); r.State != StateDeclined || r.Adopted || !d.declined().finding(wsA, "bug-two") {
		t.Errorf("record %+v", r)
	}
	d.atWork["task:t1"] = true
	d.p.Dispatched(ref("bug-task"), "task:t1")
	if left, err = d.p.Decided(ref("bug-task"), "skip"); err != nil || !strings.Contains(left, "用 cancel") || !strings.Contains(left, "task:t1") {
		t.Errorf("Decided(skip) with a task at work = %q, %v", left, err)
	}
	if _, err = d.p.Decided(ref("doc-readme"), "mute_kind"); err != nil || !d.declined().kind(wsA, KindDoc) {
		t.Errorf("Decided(mute_kind): %v, %+v", err, d.declined())
	}
	// The gateway's own word to the Planner is the receipt's: no second
	// telling.
	if got := d.told(); len(got) != 0 {
		t.Errorf("told %q besides the answer's receipt", got)
	}
	if got := d.cardsClosed(); len(got) != 3 {
		t.Errorf("cards closed %q", got)
	}
	if _, err = d.p.Decided(ref("bug-nope"), "skip"); err == nil {
		t.Error("an answer about no finding was taken")
	}
}
