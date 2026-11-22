package note

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/planner/journal"
)

// plannerRig is a Notes over a journal in a temp directory, on a clock the
// test moves.
type plannerRig struct {
	t      *testing.T
	dir    string
	j      *journal.Journal
	n      *Notes
	now    time.Time
	paused time.Time
	told   []string
	pushed []Note
	// tellErr fails the receipt, as a Planner switched off does.
	tellErr error
}

func newPlannerRig(t *testing.T) *plannerRig {
	t.Helper()
	r := &plannerRig{t: t, dir: filepath.Join(t.TempDir(), "log"), now: time.Date(2026, 11, 23, 9, 0, 0, 0, time.Local)}
	r.open()
	return r
}

// open builds the Notes afresh over the same directory: a restart.
func (r *plannerRig) open() {
	r.t.Helper()
	if r.j != nil {
		r.j.Close()
	}
	j, err := journal.Open(r.dir)
	if err != nil {
		r.t.Fatal(err)
	}
	r.j = j
	r.n = New(Config{
		Journal:     j,
		PausedUntil: func() time.Time { return r.paused },
		Now:         func() time.Time { return r.now },
		Name:        func(key string) string { return "名字-" + key },
		Tell: func(text string) error {
			r.told = append(r.told, text)
			return r.tellErr
		},
	})
	r.n.SetPush(func(nt Note) { r.pushed = append(r.pushed, nt) })
}

func (r *plannerRig) write(key, text string) Note {
	r.t.Helper()
	nt, err := r.n.Write(key, text, "依据 "+text)
	if err != nil {
		r.t.Fatalf("Write(%s, %s): %v", key, text, err)
	}
	return nt
}

// entries are the journal's kind=note lines, as "unit event id".
func (r *plannerRig) entries() []string {
	r.t.Helper()
	es, err := r.j.Read(r.now.Add(-30*24*time.Hour), r.now.Add(time.Hour))
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		if e.Kind != journal.KindNote {
			continue
		}
		var d PlannerData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			r.t.Fatal(err)
		}
		out = append(out, e.Unit+" "+d.Event+" "+d.ID)
	}
	return out
}

// A note is one kind=note journal line, pushed once as it is written and
// listed with its text and why; a restart lists it still and keeps counting
// from it.
func TestPlannerWrite(t *testing.T) {
	t.Parallel()
	r := newPlannerRig(t)
	nt := r.write("dashboard:direct:u:general", "这个改动和 task:x 的结论冲突")
	if nt.Kind != KindPlanner || nt.Key != "dashboard:direct:u:general" || nt.Text != "这个改动和 task:x 的结论冲突" ||
		nt.Why != "依据 这个改动和 task:x 的结论冲突" || !strings.HasPrefix(nt.ID, "p") || len(nt.ID) != 13 || !nt.Time.Equal(r.now) {
		t.Fatalf("written note = %+v", nt)
	}
	if got := r.entries(); len(got) != 1 || got[0] != "dashboard:direct:u:general write "+nt.ID {
		t.Fatalf("journal = %v, want the one write", got)
	}
	if len(r.pushed) != 1 || r.pushed[0] != nt {
		t.Fatalf("pushed = %+v, want the note once", r.pushed)
	}
	if len(r.told) != 0 {
		t.Errorf("a write told the Planner: %v", r.told)
	}

	r.open()
	got := r.n.List("dashboard:direct:u:general", r.now)
	if len(got) != 1 || !got[0].Time.Equal(nt.Time) {
		t.Fatalf("after a restart List = %+v, want the note", got)
	}
	got[0].Time = nt.Time
	if got[0] != nt {
		t.Fatalf("after a restart List = %+v, want %+v", got[0], nt)
	}
	if other := r.n.List("task:other", r.now); len(other) != 0 {
		t.Errorf("another session lists %+v", other)
	}
	// The count is read from the journal: two more fill the hour.
	r.write("dashboard:direct:u:general", "二")
	r.write("dashboard:direct:u:general", "三")
	if _, err := r.n.Write("dashboard:direct:u:general", "四", "y"); !errors.Is(err, ErrHourFull) {
		t.Fatalf("fourth note after a restart: err = %v, want ErrHourFull", err)
	}
}

// Two notes written in the same instant have IDs of their own.
func TestPlannerWriteIDs(t *testing.T) {
	t.Parallel()
	r := newPlannerRig(t)
	if a, b := r.write("k", "一"), r.write("k", "二"); a.ID == b.ID {
		t.Fatalf("two notes share ID %s", a.ID)
	}
}

// One session takes PerHour notes in any 60 minutes; a refused one is not
// journaled, another session is not held to it, and the hour passing frees
// it.
func TestPlannerHourLimit(t *testing.T) {
	t.Parallel()
	r := newPlannerRig(t)
	for i := range PerHour {
		r.write("a", fmt.Sprint("note ", i))
		r.now = r.now.Add(time.Minute)
	}
	if _, err := r.n.Write("a", "多的一条", "y"); !errors.Is(err, ErrHourFull) {
		t.Fatalf("err = %v, want ErrHourFull", err)
	}
	if got := r.entries(); len(got) != PerHour {
		t.Fatalf("journal has %d lines, want %d: the refused note is not written", len(got), PerHour)
	}
	if len(r.pushed) != PerHour {
		t.Fatalf("pushed %d, want %d", len(r.pushed), PerHour)
	}
	r.write("b", "另一个会话照写")
	// Just past an hour after the first: the window lets that one go, not
	// all three.
	r.now = r.now.Add(-time.Duration(PerHour)*time.Minute + time.Hour + 30*time.Second)
	r.write("a", "一小时后")
	if _, err := r.n.Write("a", "又多一条", "y"); !errors.Is(err, ErrHourFull) {
		t.Fatalf("err = %v, want ErrHourFull: only the first note left the window", err)
	}
}

// All sessions together take PerDay notes in a local day; the next day
// starts afresh.
func TestPlannerDayLimit(t *testing.T) {
	t.Parallel()
	r := newPlannerRig(t)
	for i := range PerDay {
		r.write(fmt.Sprint("s", i/PerHour), fmt.Sprint("note ", i))
	}
	if _, err := r.n.Write("fresh", "第 31 条", "y"); !errors.Is(err, ErrDayFull) {
		t.Fatalf("err = %v, want ErrDayFull", err)
	}
	// A session full for the hour is told so, the nearer limit.
	if _, err := r.n.Write("s0", "x", "y"); !errors.Is(err, ErrHourFull) {
		t.Fatalf("err = %v, want ErrHourFull", err)
	}
	if got := r.entries(); len(got) != PerDay {
		t.Fatalf("journal has %d lines, want %d", len(got), PerDay)
	}
	r.open()
	if _, err := r.n.Write("fresh", "重启后", "y"); !errors.Is(err, ErrDayFull) {
		t.Fatalf("after a restart err = %v, want ErrDayFull", err)
	}
	r.now = time.Date(2026, 11, 24, 0, 30, 0, 0, time.Local)
	r.write("fresh", "第二天")
}

// While pushes and custody are paused nothing is written; it ends with the
// pause.
func TestPlannerPaused(t *testing.T) {
	t.Parallel()
	r := newPlannerRig(t)
	r.paused = r.now.Add(time.Hour)
	if _, err := r.n.Write("a", "x", "y"); !errors.Is(err, ErrPaused) {
		t.Fatalf("err = %v, want ErrPaused", err)
	}
	if got := r.entries(); len(got) != 0 || len(r.pushed) != 0 {
		t.Fatalf("journal = %v, pushed = %v: a paused note is not written", got, r.pushed)
	}
	r.now = r.now.Add(2 * time.Hour)
	r.write("a", "暂停过了")
}

// "没用" is journaled, shows on the listed note and reaches the Planner with
// the session and the note; a second click, an unknown note and another
// session's note change nothing.
func TestPlannerUseless(t *testing.T) {
	t.Parallel()
	r := newPlannerRig(t)
	nt := r.write("a", "这和你记下的决定相悖")
	other := r.write("b", "别的会话")
	r.now = r.now.Add(time.Minute)

	got, err := r.n.Useless("a", nt.ID)
	if err != nil || !got.Useless || got.ID != nt.ID || got.Text != nt.Text {
		t.Fatalf("Useless = %+v, %v", got, err)
	}
	want := []string{"a write " + nt.ID, "b write " + other.ID, "a useless " + nt.ID}
	if es := r.entries(); strings.Join(es, "|") != strings.Join(want, "|") {
		t.Fatalf("journal = %v, want %v", es, want)
	}
	if len(r.told) != 1 || !strings.Contains(r.told[0], "a（名字-a）") || !strings.Contains(r.told[0], "「这和你记下的决定相悖」") ||
		!strings.Contains(r.told[0], "“没用”") || strings.Contains(r.told[0], "今天不再给它写") {
		t.Fatalf("receipt = %q", r.told)
	}
	if len(r.pushed) != 2 {
		t.Errorf("pushed %d notes, want the two writes only", len(r.pushed))
	}
	if l := r.n.List("a", r.now); len(l) != 1 || !l[0].Useless {
		t.Fatalf("List(a) = %+v, want the note marked", l)
	}
	if l := r.n.List("b", r.now); len(l) != 1 || l[0].Useless {
		t.Fatalf("List(b) = %+v, want its note unmarked", l)
	}

	// Again: nothing written, no one told.
	if got, err := r.n.Useless("a", nt.ID); err != nil || !got.Useless {
		t.Fatalf("second Useless = %+v, %v", got, err)
	}
	if len(r.entries()) != 3 || len(r.told) != 1 {
		t.Fatalf("a second click wrote or told: journal %v, told %v", r.entries(), r.told)
	}
	for _, c := range []struct{ key, id string }{{"a", "p000000000000"}, {"a", other.ID}, {"nowhere", nt.ID}} {
		if _, err := r.n.Useless(c.key, c.id); !errors.Is(err, ErrNoNote) {
			t.Errorf("Useless(%s, %s) err = %v, want ErrNoNote", c.key, c.id, err)
		}
	}
	if len(r.entries()) != 3 || len(r.told) != 1 {
		t.Fatalf("a refused click wrote or told: journal %v, told %v", r.entries(), r.told)
	}

	// The mark survives a restart, and a Planner that cannot be told.
	r.open()
	r.tellErr = errors.New("planner is off")
	if got, err := r.n.Useless("b", other.ID); err != nil || !got.Useless {
		t.Fatalf("Useless with the Planner off = %+v, %v", got, err)
	}
	if l := r.n.List("b", r.now); len(l) != 1 || !l[0].Useless {
		t.Fatalf("List(b) = %+v, want the note marked", l)
	}
	if l := r.n.List("a", r.now); len(l) != 1 || !l[0].Useless {
		t.Fatalf("after a restart List(a) = %+v, want the note marked", l)
	}
}

// UselessMute notes of one session marked in a day end its notes for the
// day: the third click's receipt says so, other sessions and the next day
// are not held to it.
func TestPlannerMuted(t *testing.T) {
	t.Parallel()
	r := newPlannerRig(t)
	// Notes of yesterday marked today count: the day is the click's.
	r.now = r.now.Add(-24 * time.Hour)
	old := []Note{r.write("a", "昨天一"), r.write("a", "昨天二")}
	r.now = r.now.Add(24 * time.Hour)
	today := r.write("a", "今天")

	for i, nt := range []Note{old[0], old[1]} {
		if _, err := r.n.Useless("a", nt.ID); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(r.told[i], "今天不再给它写") {
			t.Fatalf("click %d already says muted: %q", i+1, r.told[i])
		}
	}
	// Two marked: still written.
	r.write("a", "还能写")
	if _, err := r.n.Useless("a", today.ID); err != nil {
		t.Fatal(err)
	}
	if last := r.told[len(r.told)-1]; !strings.Contains(last, "第 3 次") || !strings.Contains(last, "今天不再给它写旁注") {
		t.Fatalf("third click's receipt = %q", last)
	}
	if _, err := r.n.Write("a", "x", "y"); !errors.Is(err, ErrMuted) {
		t.Fatalf("err = %v, want ErrMuted", err)
	}
	r.write("b", "别的会话不受影响")
	r.open()
	if _, err := r.n.Write("a", "x", "y"); !errors.Is(err, ErrMuted) {
		t.Fatalf("after a restart err = %v, want ErrMuted", err)
	}
	r.now = time.Date(2026, 11, 24, 8, 0, 0, 0, time.Local)
	r.write("a", "第二天恢复")
}

// Only the writing of a note is a note: the mark, an event of a later
// version and an entry with no ID are none.
func TestFromEntryPlanner(t *testing.T) {
	ts := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		unit, data string
		want       *Note
	}{
		{"k", `{"event":"write","id":"p0123456789ab","text":"正文","why":"依据"}`, &Note{ID: "p0123456789ab", Key: "k", Time: ts, Kind: KindPlanner, Text: "正文", Why: "依据"}},
		{"k", `{"event":"useless","id":"p0123456789ab"}`, nil},
		{"k", `{"event":"progress","id":"g1","text":"跑了 30 分钟"}`, nil},
		{"k", `{"event":"write","text":"没有 id"}`, nil},
		{"", `{"event":"write","id":"p0123456789ab","text":"没有会话"}`, nil},
		{"k", `not json`, nil},
	} {
		got, ok := FromEntry(journal.Entry{TS: ts, Kind: journal.KindNote, Unit: c.unit, Data: json.RawMessage(c.data)})
		if ok != (c.want != nil) || ok && got != *c.want {
			t.Errorf("FromEntry(%s %s) = %+v %v, want %+v", c.unit, c.data, got, ok, c.want)
		}
	}
}

// With no journal there is nowhere to keep a note.
func TestPlannerNoJournal(t *testing.T) {
	n := New(Config{})
	if _, err := n.Write("a", "x", "y"); err == nil {
		t.Error("Write without a journal succeeded")
	}
	if _, err := n.Useless("a", "p0123456789ab"); err == nil {
		t.Error("Useless without a journal succeeded")
	}
}
