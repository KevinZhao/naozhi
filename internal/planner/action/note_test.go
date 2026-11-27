package action

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/note"
)

type sideNoted struct{ key, text, why string }

// noteRig is a runner whose side notes, relays and custody sends are
// recorded: a note block must reach the first and never the other two.
type noteRig struct {
	r       *Runner
	g       *fakeGateway
	written []sideNoted
	relayed *[]relayed
	fc      *fakeCustody
	// err is what writing a note returns; target what NoteTarget does.
	err, target error
}

func newNoteRig(t *testing.T) *noteRig {
	t.Helper()
	r, g, _, _, _ := newRunner(t)
	rig := &noteRig{r: r, g: g, fc: &fakeCustody{on: map[string]bool{}}}
	rig.relayed = userTurn(r)
	r.cfg.Custody = rig.fc
	r.cfg.SideNote = func(key, text, why string) error {
		if rig.err != nil {
			return rig.err
		}
		rig.written = append(rig.written, sideNoted{key, text, why})
		return nil
	}
	r.cfg.NoteTarget = func(string) error { return rig.target }
	return rig
}

// run handles one Planner reply and checks nothing was sent to a session.
func (rig *noteRig) run(t *testing.T, text string) {
	t.Helper()
	rig.r.Result(rig.r.cfg.Planner, text)
	rig.r.handle(<-rig.r.ch)
	if len(*rig.relayed) != 0 || len(rig.fc.delivered) != 0 || len(rig.g.spawned) != 0 {
		t.Fatalf("a note block sent to a session: relayed %+v, custody %v, spawned %v", *rig.relayed, rig.fc.delivered, rig.g.spawned)
	}
}

const noteBlock = `{"type":"note","key":"dashboard:direct:u:general","text":"这和 task:ab12  的结论冲突","why":"task:ab12 的摘要：\n已改成方案 B"}`

// A note block is written in a turn of any source: the user's, a receipt's
// (no words), and one outside content started. It leaves no line in the
// Planner conversation and no receipt, and nothing is sent to the session.
func TestNoteWritten(t *testing.T) {
	for _, c := range []struct {
		name, outside string
		words         []Words
	}{
		{"user turn", "", []Words{said}},
		{"receipt turn", "", nil},
		{"scout turn", "巡检", nil},
		{"lookup turn", "网页查询结果", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			rig := newNoteRig(t)
			rig.r.cfg.Words = func() []Words { return c.words }
			if c.outside != "" {
				rig.r.cfg.Outside = func() string { return c.outside }
			}
			rig.run(t, "它和另一个会话的结论冲突。\n"+block(noteBlock))
			want := sideNoted{"dashboard:direct:u:general", "这和 task:ab12 的结论冲突", "task:ab12 的摘要： 已改成方案 B"}
			if len(rig.written) != 1 || rig.written[0] != want {
				t.Fatalf("written = %+v, want %+v", rig.written, want)
			}
			if n := len(rig.g.notes) + len(rig.g.unrun) + len(rig.g.refused) + len(rig.g.told); n != 0 {
				t.Errorf("notes %q unrun %q refused %q told %q, want none", rig.g.notes, rig.g.unrun, rig.g.refused, rig.g.told)
			}
		})
	}
}

// An outside turn's other blocks are still refused beside its note, and the
// receipt names note among what such a turn runs.
func TestNoteBesideRefusedOutside(t *testing.T) {
	rig := newNoteRig(t)
	rig.r.cfg.Outside = func() string { return "巡检" }
	rig.run(t, block(noteBlock)+"\n"+block(`{"type":"relay","key":"task:ab12","text":"合并"}`))
	if len(rig.written) != 1 {
		t.Fatalf("written = %+v, want the note", rig.written)
	}
	if len(rig.g.refused) != 1 || !strings.Contains(rig.g.refused[0], "网关只执行 notify、promise、ask 与 note") ||
		!strings.Contains(rig.g.refused[0], "第 2 块") || strings.Contains(rig.g.refused[0], "第 1 块") {
		t.Fatalf("refused = %q", rig.g.refused)
	}
}

// A note not written — a limit, the mute, the pause — is said in the Planner
// conversation and goes back to the Planner with why, by the receipt of the
// turn's kind.
func TestNoteNotWritten(t *testing.T) {
	for _, c := range []struct {
		name    string
		err     error
		outside string
		want    string
	}{
		{"hour limit", note.ErrHourFull, "", "这个会话一小时内已经写了 3 条旁注"},
		{"day limit", note.ErrDayFull, "", "今天的旁注已到 30 条上限"},
		{"muted", note.ErrMuted, "", "点了 3 次“没用”，今天不再给它写旁注"},
		{"paused", note.ErrPaused, "", "用户暂停了推送与托管"},
		{"hour limit in an outside turn", note.ErrHourFull, "巡检", "这个会话一小时内已经写了 3 条旁注"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rig := newNoteRig(t)
			rig.err = c.err
			if c.outside != "" {
				rig.r.cfg.Outside = func() string { return c.outside }
			}
			rig.run(t, block(noteBlock))
			if len(rig.g.notes) != 1 || !strings.HasPrefix(rig.g.notes[0], "动作块没有执行：给「名字 dashboard:direct:u:general」的旁注没有写：") || !strings.Contains(rig.g.notes[0], c.want) {
				t.Fatalf("notes = %q, want one with %q", rig.g.notes, c.want)
			}
			got, other := rig.g.unrun, rig.g.refused
			if c.outside != "" {
				got, other = other, got
			}
			if len(got) != 1 || len(other) != 0 || !strings.Contains(got[0], "第 1 块（note「这和 task:ab12 的结论冲突」）") || !strings.Contains(got[0], c.want) {
				t.Fatalf("receipts = %q / %q, want one with %q", got, other, c.want)
			}
			// Nothing to correct and resend: the block was well formed.
			if strings.Contains(got[0], "格式不对") {
				t.Errorf("receipt asks for a resend: %q", got[0])
			}
		})
	}
}

// A key that is no session of this node is refused like a malformed block,
// with why; the note is not written.
func TestNoteTarget(t *testing.T) {
	rig := newNoteRig(t)
	rig.target = errors.New("找不到会话 dashboard:direct:u:general（填 index.json 里本机会话的 key；远程节点的会话写不了旁注）")
	rig.run(t, block(noteBlock))
	if len(rig.written) != 0 {
		t.Fatalf("written = %+v, want none", rig.written)
	}
	if len(rig.g.notes) != 1 || !strings.HasPrefix(rig.g.notes[0], "动作块格式不对，没有执行：找不到会话") {
		t.Fatalf("notes = %q", rig.g.notes)
	}
	if len(rig.g.unrun) != 1 || !strings.Contains(rig.g.unrun[0], "找不到会话 dashboard:direct:u:general") || !strings.Contains(rig.g.unrun[0], "格式不对的块") {
		t.Fatalf("unrun = %q", rig.g.unrun)
	}
}

// With no side notes wired a note block is not run, and says so.
func TestNoteNotWired(t *testing.T) {
	rig := newNoteRig(t)
	rig.r.cfg.SideNote = nil
	rig.run(t, block(noteBlock))
	if len(rig.g.notes) != 1 || !strings.Contains(rig.g.notes[0], "旁注没有接通") || len(rig.g.unrun) != 1 || !strings.Contains(rig.g.unrun[0], "旁注没有接通") {
		t.Fatalf("notes = %q, unrun = %q", rig.g.notes, rig.g.unrun)
	}
}

// End to end over planner/note: the block is one kind=note journal line on
// its session, and the fourth in an hour comes back in the receipt.
func TestNoteJournaled(t *testing.T) {
	j, err := journal.Open(filepath.Join(t.TempDir(), "log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	notes := note.New(note.Config{Journal: j})
	rig := newNoteRig(t)
	rig.r.cfg.SideNote = func(key, text, why string) error {
		_, err := notes.Write(key, text, why)
		return err
	}
	now := rig.r.cfg.Now()
	rig.run(t, strings.Repeat(block(noteBlock)+"\n", note.PerHour+1))
	got := notes.List("dashboard:direct:u:general", now)
	if len(got) != note.PerHour {
		t.Fatalf("listed %d notes, want %d: %+v", len(got), note.PerHour, got)
	}
	for _, nt := range got {
		if nt.Kind != note.KindPlanner || nt.Text != "这和 task:ab12 的结论冲突" || nt.Why != "task:ab12 的摘要： 已改成方案 B" {
			t.Errorf("note = %+v", nt)
		}
	}
	if len(rig.g.unrun) != 1 || !strings.Contains(rig.g.unrun[0], "有 1 个动作块没有执行") || !strings.Contains(rig.g.unrun[0], "第 4 块") ||
		!strings.Contains(rig.g.unrun[0], note.ErrHourFull.Error()) {
		t.Fatalf("unrun = %q", rig.g.unrun)
	}
}

func TestNoteParse(t *testing.T) {
	parseCases(t, []parseCase{
		{`{"type":"note","key":"dashboard:direct:u:general","text":"t","why":"w"}`, ""},
		{`{"type":"note","key":"task:ab12","text":"` + strings.Repeat("字", noteTextRunes) + `","why":"` + strings.Repeat("字", textRunes) + `"}`, ""},
		{`{"type":"note","key":"cron:job1","text":"t","why":"w"}`, ""},
		{`{"type":"note","key":"k","text":" \n ","why":"w"}`, "note 缺少 text"},
		{`{"type":"note","key":"k","why":"w"}`, "note 缺少 text"},
		{`{"type":"note","key":"k","text":"` + strings.Repeat("字", noteTextRunes+1) + `","why":"w"}`, "text 超过 200 字"},
		{`{"type":"note","key":"k","text":"t","why":" "}`, "note 缺少 why"},
		{`{"type":"note","key":"k","text":"t"}`, "note 缺少 why"},
		{`{"type":"note","key":"k","text":"t","why":"` + strings.Repeat("字", textRunes+1) + `"}`, "why 超过 500 字"},
		{`{"type":"note","key":" ","text":"t","why":"w"}`, "note 缺少 key"},
		{`{"type":"note","text":"t","why":"w"}`, "note 缺少 key"},
		{`{"type":"note","key":"a\nb","text":"t","why":"w"}`, "会话 key"},
		{`{"type":"note","key":"planner:main","text":"t","why":"w"}`, "不能是 planner:main 或一次性会话"},
		{`{"type":"note","key":"scout:r1","text":"t","why":"w"}`, "不能是 planner:main 或一次性会话"},
		{`{"type":"note","key":"reader:r1","text":"t","why":"w"}`, "不能是 planner:main 或一次性会话"},
		{`{"type":"note","key":"k","text":"t","why":"w","level":"timely"}`, "字段不对"},
		{`{"type":"nope"}`, "front、note）"},
	})
	// Text and why are one line each; the key is trimmed.
	n := Parse(block(`{"type":"note","key":" task:ab12 ","text":" 一句\n话 ","why":" 出处：\n\tgh pr view 12 "}`))[0].Note
	if n == nil || *n != (Note{Key: "task:ab12", Text: "一句 话", Why: "出处： gh pr view 12"}) {
		t.Errorf("note = %+v", n)
	}
}
