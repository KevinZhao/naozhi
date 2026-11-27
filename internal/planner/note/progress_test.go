package note

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/bgtask"
)

const progressKey = "dashboard:direct:u:general"

// bash is a session whose turn ended at turn with one background Bash
// started a minute before.
func bash(key string, turn time.Time) Background {
	at := turn.Add(-time.Minute)
	return Background{Key: key, Turn: turn, State: bgtask.State{
		Tasks: []bgtask.Task{{ID: "b1", Kind: bgtask.KindBash, Summary: "npm run bench", Start: at, Last: at}}, LastProgress: at}}
}

// workflow is a session whose turn ended at turn with workflow w1 running
// since then, done of 12 agents done, in phase.
func workflow(key string, turn time.Time, done int, phase string) Background {
	return Background{Key: key, Turn: turn, State: bgtask.WithWorkflows(bgtask.State{},
		[]bgtask.Workflow{{ID: "w1", Name: "review-all", Start: turn, Agents: 12, Done: done, Phase: phase}})}
}

// tick moves the clock to at and runs Progress over list.
func (r *plannerRig) tick(at time.Time, list ...Background) {
	r.t.Helper()
	r.now = at
	r.n.Progress(list)
}

// progress are the pushed progress notes.
func (r *plannerRig) progress() []Note {
	var out []Note
	for _, nt := range r.pushed {
		if nt.Kind == KindProgress {
			out = append(out, nt)
		}
	}
	return out
}

// A session whose turn is over with a task open gets its first note 30
// minutes after the turn ended and one every 30 minutes after; the notes
// of one stretch share its run, and List shows the latest of them only.
func TestProgressEvery(t *testing.T) {
	t.Parallel()
	r := newPlannerRig(t)
	turn := r.now
	for m := 1; m < 30; m++ {
		r.tick(turn.Add(time.Duration(m)*time.Minute), bash(progressKey, turn))
	}
	if got := r.entries(); len(got) != 0 {
		t.Fatalf("notes before 30 minutes: %v", got)
	}
	r.tick(turn.Add(30*time.Minute), bash(progressKey, turn))
	got := r.progress()
	if len(got) != 1 {
		t.Fatalf("at 30 minutes pushed %+v, want one progress note", r.pushed)
	}
	first := got[0]
	if first.Key != progressKey || first.Text != "已跑 31 分钟 · 1 个后台任务：npm run bench" || first.Run == "" ||
		!strings.HasPrefix(first.ID, "g") || !first.Time.Equal(r.now) || first.Why != "" {
		t.Fatalf("first note = %+v", first)
	}
	for m := 31; m < 60; m++ {
		r.tick(turn.Add(time.Duration(m)*time.Minute), bash(progressKey, turn))
	}
	if n := len(r.progress()); n != 1 {
		t.Fatalf("%d notes before the second 30 minutes", n)
	}
	r.tick(turn.Add(60*time.Minute), bash(progressKey, turn))
	got = r.progress()
	if len(got) != 2 || got[1].Run != first.Run || got[1].ID == first.ID || got[1].Text != "已跑 1 小时 1 分钟 · 1 个后台任务：npm run bench" {
		t.Fatalf("second note = %+v, want one more of the same run", got)
	}
	if len(r.told) != 0 {
		t.Errorf("a progress note told the Planner: %v", r.told)
	}
	// Both are journaled; the view lists the stretch's latest.
	if e := r.entries(); len(e) != 2 || e[0] != progressKey+" progress "+first.ID || e[1] != progressKey+" progress "+got[1].ID {
		t.Fatalf("journal = %v", e)
	}
	list := r.n.List(progressKey, r.now)
	if len(list) != 1 || list[0].ID != got[1].ID || list[0].Kind != KindProgress || list[0].Text != got[1].Text {
		t.Fatalf("List = %+v, want the latest note alone", list)
	}
}

// A workflow moving to another phase is noted without waiting out the 30
// minutes, at most once in 5; the phase first seen is not a move, and the
// 30 minutes count on from the note.
func TestProgressPhase(t *testing.T) {
	t.Parallel()
	r := newPlannerRig(t)
	turn := r.now
	at := func(m int) time.Time { return turn.Add(time.Duration(m) * time.Minute) }
	r.tick(at(1), workflow(progressKey, turn, 0, "探索"))
	r.tick(at(2), workflow(progressKey, turn, 3, "实现"))
	r.tick(at(4), workflow(progressKey, turn, 3, "实现"))
	if got := r.progress(); len(got) != 0 {
		t.Fatalf("noted within 5 minutes of the turn's end: %+v", got)
	}
	r.tick(at(5), workflow(progressKey, turn, 4, "实现"))
	got := r.progress()
	if len(got) != 1 || got[0].Text != "已跑 5 分钟 · 工作流 review-all：12 个子代理完成 4 个，阶段「实现」" {
		t.Fatalf("after the phase moved: %+v", got)
	}
	// Flipping back within 5 minutes of the note is not one more.
	r.tick(at(7), workflow(progressKey, turn, 4, "探索"))
	r.tick(at(9), workflow(progressKey, turn, 5, "实现"))
	r.tick(at(12), workflow(progressKey, turn, 5, "实现"))
	if got := r.progress(); len(got) != 1 {
		t.Fatalf("a phase back where the note had it: %+v", got)
	}
	r.tick(at(13), workflow(progressKey, turn, 9, "验证"))
	if got := r.progress(); len(got) != 2 || !strings.Contains(got[1].Text, "完成 9 个，阶段「验证」") || got[1].Run != got[0].Run {
		t.Fatalf("second move: %+v", got)
	}
	// No move: the next is 30 minutes after the last note.
	r.tick(at(42), workflow(progressKey, turn, 9, "验证"))
	if got := r.progress(); len(got) != 2 {
		t.Fatalf("noted 29 minutes after the last: %+v", got)
	}
	r.tick(at(43), workflow(progressKey, turn, 11, "验证"))
	if got := r.progress(); len(got) != 3 {
		t.Fatalf("not noted 30 minutes after the last: %+v", got)
	}
	// A workflow that starts later has no phase to have moved from.
	later := workflow(progressKey, turn, 11, "验证")
	later.State = bgtask.WithWorkflows(bgtask.State{Tasks: later.State.Tasks[:1]}, []bgtask.Workflow{{ID: "w2", Name: "fix", Phase: "实现"}})
	r.tick(at(50), later)
	if got := r.progress(); len(got) != 3 {
		t.Fatalf("a new workflow read as a move: %+v", got)
	}
}

// A workflow whose phase is not known when the session is first seen (its
// board has no frame yet after a restart, its script runs agents before its
// first phase) moves from the phase it is first seen in.
func TestProgressPhaseSeenLater(t *testing.T) {
	t.Parallel()
	r := newPlannerRig(t)
	turn := r.now
	at := func(m int) time.Time { return turn.Add(time.Duration(m) * time.Minute) }
	r.tick(at(1), workflow(progressKey, turn, 0, ""))
	r.tick(at(2), workflow(progressKey, turn, 1, "探索"))
	if got := r.progress(); len(got) != 0 {
		t.Fatalf("the phase first seen read as a move: %+v", got)
	}
	r.tick(at(8), workflow(progressKey, turn, 3, "实现"))
	r.tick(at(15), workflow(progressKey, turn, 9, "验证"))
	got := r.progress()
	if len(got) != 2 || !strings.Contains(got[0].Text, "阶段「实现」") || !strings.Contains(got[1].Text, "阶段「验证」") {
		t.Fatalf("after two moves: %+v, want a note each", got)
	}
	r.tick(at(45), workflow(progressKey, turn, 9, "验证"))
	if got := r.progress(); len(got) != 3 {
		t.Fatalf("30 minutes after the last: %+v", got)
	}

	// The same after a restart, the stretch read back from the journal.
	r.pushed = nil
	r.now = at(50)
	r.open()
	r.tick(at(51), workflow(progressKey, turn, 9, ""))
	r.tick(at(52), workflow(progressKey, turn, 9, "验证"))
	if got := r.progress(); len(got) != 0 {
		t.Fatalf("after the restart, the phase first seen: %+v", got)
	}
	r.tick(at(58), workflow(progressKey, turn, 11, "汇总"))
	if got := r.progress(); len(got) != 1 || !strings.Contains(got[0].Text, "阶段「汇总」") {
		t.Fatalf("a move after the restart: %+v", got)
	}
}

// A session whose turn's end is not known (one adopted from a live shim
// with no stored entry) waits 30 minutes from when it is first seen, and its
// notes are one stretch's, a restart included.
func TestProgressTurnUnknown(t *testing.T) {
	t.Parallel()
	r := newPlannerRig(t)
	start := r.now
	at := func(m int) time.Time { return start.Add(time.Duration(m) * time.Minute) }
	b := bash(progressKey, start)
	b.Turn = time.Time{}
	r.tick(at(1), b)
	r.tick(at(30), b)
	if got := r.progress(); len(got) != 0 {
		t.Fatalf("noted within 30 minutes of the first sight: %+v", got)
	}
	r.tick(at(31), b)
	r.tick(at(60), b)
	if got := r.progress(); len(got) != 1 {
		t.Fatalf("30 minutes after the first sight: %+v, want one note", got)
	}
	r.tick(at(61), b)
	got := r.progress()
	if len(got) != 2 || got[1].Run != got[0].Run {
		t.Fatalf("30 minutes after the note: %+v, want one more of the same run", got)
	}
	run := got[0].Run

	r.pushed = nil
	r.now = at(70)
	r.open()
	r.tick(at(71), b)
	r.tick(at(90), b)
	if got := r.progress(); len(got) != 0 {
		t.Fatalf("noted right after the restart: %+v", got)
	}
	r.tick(at(91), b)
	if got := r.progress(); len(got) != 1 || got[0].Run != run {
		t.Fatalf("30 minutes after the last note: %+v, want one of run %s", got, run)
	}
	if list := r.n.List(progressKey, r.now); len(list) != 1 {
		t.Fatalf("List = %+v, want the stretch's latest alone", list)
	}
}

// What the work last wrote is said when it wrote anything.
func TestProgressOutput(t *testing.T) {
	t.Parallel()
	r := newPlannerRig(t)
	turn := r.now
	b := Background{Key: "task:ab12", Turn: turn, State: bgtask.State{Tasks: []bgtask.Task{
		{ID: "a1", Kind: bgtask.KindAgent, Summary: "explore", Start: turn.Add(-time.Minute), Last: turn.Add(25 * time.Minute)}}}}
	r.tick(turn.Add(30*time.Minute), b)
	got := r.progress()
	if len(got) != 1 || got[0].Key != "task:ab12" || got[0].Text != "已跑 31 分钟 · 1 个后台任务：explore · 最近输出 5 分钟前" {
		t.Fatalf("note = %+v", got)
	}
	// A write after now (the clock moved back) is no "ago" to say.
	b.Key, b.State.Tasks[0].Last = progressKey, turn.Add(40*time.Minute)
	r.tick(turn.Add(30*time.Minute), b)
	if got := r.progress(); len(got) != 2 || got[1].Text != "已跑 31 分钟 · 1 个后台任务：explore" {
		t.Fatalf("with a write after now: %+v", got)
	}
}

// Once the work ended, or while a turn runs, the session is not listed and
// gets no note; a turn since starts another stretch, 30 minutes from its
// end, and each stretch keeps its latest note in the list.
func TestProgressEndsAndStartsOver(t *testing.T) {
	t.Parallel()
	r := newPlannerRig(t)
	turn := r.now
	at := func(m int) time.Time { return turn.Add(time.Duration(m) * time.Minute) }
	r.tick(at(30), bash(progressKey, turn))
	r.tick(at(59))
	if len(r.n.stretches) != 1 {
		t.Fatalf("stretches = %v, want the session's kept while its note holds the next back", r.n.stretches)
	}
	r.tick(at(60))
	if len(r.n.stretches) != 0 {
		t.Fatalf("stretches = %v, want none kept of a session whose work ended", r.n.stretches)
	}
	r.tick(at(90))
	if got := r.progress(); len(got) != 1 {
		t.Fatalf("noted after the work ended: %+v", got)
	}
	// A turn ran at 100 minutes and left a task open again.
	turn2 := at(100)
	r.tick(at(101), bash(progressKey, turn2))
	r.tick(at(129), bash(progressKey, turn2))
	if got := r.progress(); len(got) != 1 {
		t.Fatalf("noted within 30 minutes of the new turn's end: %+v", got)
	}
	r.tick(at(130), bash(progressKey, turn2))
	got := r.progress()
	if len(got) != 2 || got[1].Run == got[0].Run {
		t.Fatalf("after the new turn: %+v, want a note of another run", got)
	}
	if list := r.n.List(progressKey, r.now); len(list) != 2 {
		t.Fatalf("List = %+v, want both stretches' notes", list)
	}
	// A turn between two ticks is seen by its end alone: the wait starts
	// over though the session never left the list.
	turn3 := at(150)
	r.tick(at(160), bash(progressKey, turn3))
	if got := r.progress(); len(got) != 2 {
		t.Fatalf("noted 10 minutes after a turn: %+v", got)
	}
	r.tick(at(180), bash(progressKey, turn3))
	if got := r.progress(); len(got) != 3 || got[2].Run == got[1].Run {
		t.Fatalf("30 minutes after the third turn: %+v", got)
	}
}

// A session that drops out of the list for a tick (its switch flipped, a
// scan that missed) is not noted again on its return.
func TestProgressBackInTheList(t *testing.T) {
	t.Parallel()
	r := newPlannerRig(t)
	turn := r.now
	at := func(m int) time.Time { return turn.Add(time.Duration(m) * time.Minute) }
	r.tick(at(30), bash(progressKey, turn))
	r.tick(at(31))
	r.tick(at(32), bash(progressKey, turn))
	r.tick(at(59), bash(progressKey, turn))
	if got := r.progress(); len(got) != 1 {
		t.Fatalf("noted again on its return: %+v", got)
	}
	r.tick(at(60), bash(progressKey, turn))
	if got := r.progress(); len(got) != 2 || got[1].Run != got[0].Run {
		t.Fatalf("at 60 minutes: %+v", got)
	}
}

// Nothing is written while pushes and custody are paused; the note due is
// written once the pause ends.
func TestProgressPaused(t *testing.T) {
	t.Parallel()
	r := newPlannerRig(t)
	turn := r.now
	r.paused = turn.Add(45 * time.Minute)
	r.tick(turn.Add(30*time.Minute), bash(progressKey, turn))
	r.tick(turn.Add(44*time.Minute), bash(progressKey, turn))
	if got := r.entries(); len(got) != 0 {
		t.Fatalf("written while paused: %v", got)
	}
	r.tick(turn.Add(45*time.Minute), bash(progressKey, turn))
	if got := r.progress(); len(got) != 1 {
		t.Fatalf("after the pause: %+v, want the note", got)
	}
}

// A restart does not note every session at once: the next note waits out
// what is left of the 30 minutes, read from the journal, and is of the same
// stretch.
func TestProgressRestart(t *testing.T) {
	t.Parallel()
	r := newPlannerRig(t)
	turn := r.now
	at := func(m int) time.Time { return turn.Add(time.Duration(m) * time.Minute) }
	other := "task:ab12"
	r.tick(at(30), bash(progressKey, turn), bash(other, turn))
	if got := r.progress(); len(got) != 2 {
		t.Fatalf("before the restart: %+v", got)
	}
	run := r.progress()[0].Run

	r.pushed = nil
	r.now = at(40)
	r.open()
	r.tick(at(41), bash(progressKey, turn), bash(other, turn))
	r.tick(at(59), bash(progressKey, turn), bash(other, turn))
	if got := r.progress(); len(got) != 0 {
		t.Fatalf("noted right after the restart: %+v", got)
	}
	r.tick(at(60), bash(progressKey, turn), bash(other, turn))
	got := r.progress()
	if len(got) != 2 || got[0].Run != run {
		t.Fatalf("30 minutes after the last note: %+v, want one each, of run %s", got, run)
	}
	if list := r.n.List(progressKey, r.now); len(list) != 1 || list[0].ID != got[0].ID && list[0].ID != got[1].ID {
		t.Fatalf("List = %+v, want the stretch's latest alone", list)
	}
}

// The progress notes are not the Planner's: they fill neither its hourly
// nor its daily limit, and take no "没用".
func TestProgressNotCounted(t *testing.T) {
	t.Parallel()
	r := newPlannerRig(t)
	turn := r.now
	// One note every 5 minutes, the phase moving each time: more than a
	// day's limit of the Planner's, a dozen an hour.
	for i := 1; i <= PerDay+1; i++ {
		r.tick(turn.Add(time.Duration(i)*progressGap), workflow(progressKey, turn, i, fmt.Sprint("阶段", i)))
	}
	if n := len(r.progress()); n != PerDay {
		t.Fatalf("%d progress notes, want %d (the first sight is no move)", n, PerDay)
	}
	for i := range PerHour {
		r.write(progressKey, fmt.Sprint("旁注", i))
	}
	if _, err := r.n.Write(progressKey, "多一条", "y"); !errors.Is(err, ErrHourFull) {
		t.Fatalf("the Planner's fourth note: err = %v, want ErrHourFull", err)
	}
	r.write("task:other", "别的会话")

	id := r.progress()[0].ID
	if _, err := r.n.Useless(progressKey, id); !errors.Is(err, ErrNoNote) {
		t.Fatalf("没用 on a progress note: err = %v, want ErrNoNote", err)
	}
	if len(r.told) != 0 {
		t.Errorf("the Planner was told: %v", r.told)
	}
	for _, nt := range r.n.List(progressKey, r.now) {
		if nt.Useless {
			t.Errorf("note %+v listed useless", nt)
		}
	}
}
