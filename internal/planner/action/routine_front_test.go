package action

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/planner/front"
)

// fakeRoutines records what the routine blocks ask of the scheduler.
type fakeRoutines struct {
	added    [][4]string // title, dir, schedule, prompt
	switched []string    // "<id> off|on"
	edited   [][4]string // id, title, schedule, prompt
	deleted  []string
	err      error
}

func (f *fakeRoutines) Edit(id, title, schedule, prompt string) error {
	f.edited = append(f.edited, [4]string{id, title, schedule, prompt})
	return f.err
}

func (f *fakeRoutines) Delete(id string) error {
	f.deleted = append(f.deleted, id)
	return f.err
}

func (f *fakeRoutines) Add(title, dir, schedule, prompt string) error {
	f.added = append(f.added, [4]string{title, dir, schedule, prompt})
	return f.err
}

func (f *fakeRoutines) Switch(id string, off bool) (string, error) {
	state := " on"
	if off {
		state = " off"
	}
	f.switched = append(f.switched, id+state)
	return "标题", f.err
}

// A routine block in a turn the user started makes the standing task in the
// workspace's directory, or switches one, silently: the card says it
// (§15.17, D131).
func TestRoutineUserTurn(t *testing.T) {
	r, g, _, _, dir := newRunner(t)
	userTurn(r, said)
	rt := &fakeRoutines{}
	r.cfg.Routines = rt
	r.Result(r.cfg.Planner, "好，每个工作日早上查一次。\n"+
		block(`{"type":"routine","title":"内网模型动态","workspace":"`+filepath.Base(dir)+`","schedule":" 0 9 * * 1-5 ","prompt":"查内网 Anthropic 新模型动态"}`)+"\n"+
		block(`{"type":"routine","id":"1cc584f384db82fc","off":true}`)+"\n"+
		block(`{"type":"routine","id":"aa11","schedule":"0 8 * * *"}`)+"\n"+
		block(`{"type":"routine","id":"bb22","delete":true}`))
	r.handle(<-r.ch)
	if len(rt.edited) != 1 || rt.edited[0] != [4]string{"aa11", "", "0 8 * * *", ""} {
		t.Fatalf("edited = %q", rt.edited)
	}
	if len(rt.deleted) != 1 || rt.deleted[0] != "bb22" {
		t.Fatalf("deleted = %q", rt.deleted)
	}
	if len(rt.added) != 1 || rt.added[0] != [4]string{"内网模型动态", dir, "0 9 * * 1-5", "查内网 Anthropic 新模型动态"} {
		t.Fatalf("added = %q", rt.added)
	}
	if len(rt.switched) != 1 || rt.switched[0] != "1cc584f384db82fc off" {
		t.Fatalf("switched = %q", rt.switched)
	}
	if len(g.notes)+len(g.unrun) != 0 {
		t.Errorf("notes %q unrun %q, want none", g.notes, g.unrun)
	}
}

// Only a turn the user started may make or switch a standing task: a
// session's report or a mail asking for one is not the user agreeing. A
// failure is said in the conversation and the receipt.
func TestRoutineRefused(t *testing.T) {
	cases := []struct {
		name, outside, workspace, note, receipt string
		words                                   bool
		err                                     error
	}{
		{"receipt turn", "", "", "不能加或改例行任务", "不是用户发起的", false, nil},
		{"scout turn", "巡检", "", "巡检触发的回合不能改例行任务", "巡检触发", true, nil},
		{"unknown workspace", "", "nowhere", "找不到工作区", "找不到工作区", true, nil},
		{"scheduler refuses", "", "", "schedule 不对", "schedule 不对", true, errors.New("schedule 不对")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, g, _, _, dir := newRunner(t)
			userTurn(r)
			if c.words {
				userTurn(r, said)
			}
			if c.outside != "" {
				r.cfg.Outside = func() string { return c.outside }
			}
			rt := &fakeRoutines{err: c.err}
			r.cfg.Routines = rt
			ws := c.workspace
			if ws == "" {
				ws = filepath.Base(dir)
			}
			r.Result(r.cfg.Planner, block(`{"type":"routine","title":"t","workspace":"`+ws+`","schedule":"@daily","prompt":"p"}`))
			r.handle(<-r.ch)
			if c.err == nil && len(rt.added) != 0 {
				t.Fatalf("added %q, want none", rt.added)
			}
			if len(g.notes) != 1 || !strings.Contains(g.notes[0], c.note) {
				t.Errorf("notes = %q, want one with %q", g.notes, c.note)
			}
			receipts := append(append([]string{}, g.unrun...), g.refused...)
			if len(receipts) != 1 || !strings.Contains(receipts[0], c.receipt) {
				t.Errorf("receipts = %q, want one with %q", receipts, c.receipt)
			}
		})
	}
}

func TestRoutineParse(t *testing.T) {
	for _, c := range []struct{ raw, err string }{
		{`{"type":"routine","title":"t","workspace":"w","schedule":"0 9 * * 1-5","prompt":"p"}`, ""},
		{`{"type":"routine","id":"abc","off":false}`, ""},
		{`{"type":"routine","title":"t","workspace":"w","schedule":"@daily"}`, "prompt"},
		{`{"type":"routine","title":"t","workspace":"w","prompt":"p"}`, "schedule"},
		{`{"type":"routine","title":"t","schedule":"@daily","prompt":"p"}`, "workspace"},
		{`{"type":"routine","workspace":"w","schedule":"@daily","prompt":"p"}`, "title"},
		{`{"type":"routine","id":"abc","delete":true}`, ""},
		{`{"type":"routine","id":"abc","title":"新标题","prompt":"新的"}`, ""},
		{`{"type":"routine","id":"abc"}`, "要写 off"},
		{`{"type":"routine","id":"abc","off":true,"prompt":"新的"}`, "一块只做一件事"},
		{`{"type":"routine","id":"abc","delete":true,"off":true}`, "一块只做一件事"},
		{`{"type":"routine","id":"abc","workspace":"w","prompt":"p"}`, "工作区不能改"},
		{`{"type":"routine","title":"t","workspace":"w","schedule":"@daily","prompt":"p","off":true}`, "只和 id 一起用"},
		{`{"type":"routine","title":"t","workspace":"w","schedule":"@daily","prompt":"p","delete":true}`, "只和 id 一起用"},
		{`{"type":"routine","title":"t","workspace":"w","schedule":"@daily","prompt":"` + strings.Repeat("字", routinePromptRunes+1) + `"}`, "prompt"},
	} {
		b := Parse(block(c.raw))
		if len(b) != 1 {
			t.Fatalf("%s: %d blocks", c.raw, len(b))
		}
		switch {
		case c.err == "" && (b[0].Err != nil || b[0].Routine == nil):
			t.Errorf("%s: %v", c.raw, b[0].Err)
		case c.err != "" && (b[0].Err == nil || !strings.Contains(b[0].Err.Error(), c.err)):
			t.Errorf("%s: err %v, want one with %q", c.raw, b[0].Err, c.err)
		}
	}
}

// fronter wires r's Front and records the updates.
func fronter(r *Runner, err error) *[]front.Update {
	var out []front.Update
	r.cfg.Front = func(u front.Update) (front.Front, error) {
		out = append(out, u)
		return front.Front{ID: u.ID}, err
	}
	return &out
}

// A front block writes the ledger from a turn the user started and from a
// receipt's (a session's report moves a line of work on), silently; a field
// left out is left alone, one written empty is cleared (§15.17, D132).
func TestFrontTurns(t *testing.T) {
	for _, words := range []bool{true, false} {
		r, g, _, _, _ := newRunner(t)
		userTurn(r)
		if words {
			userTurn(r, said)
		}
		out := fronter(r, nil)
		r.Result(r.cfg.Planner, block(`{"type":"front","id":"jd-tgw","name":"JD TGW 迁移","state":" 方案被否 ","waiting":"","sessions":["dashboard:direct:a:general"],"decision":"先迁回 JDOS VPC"}`))
		r.handle(<-r.ch)
		if len(*out) != 1 {
			t.Fatalf("words %v: updates = %+v", words, *out)
		}
		u := (*out)[0]
		if u.ID != "jd-tgw" || u.Name != "JD TGW 迁移" || u.State == nil || *u.State != "方案被否" || u.Next != nil ||
			u.Waiting == nil || *u.Waiting != "" || len(u.Sessions) != 1 || u.Decision != "先迁回 JDOS VPC" || u.Close {
			t.Errorf("words %v: update = %+v", words, u)
		}
		if len(g.notes)+len(g.unrun) != 0 {
			t.Errorf("words %v: notes %q unrun %q, want none", words, g.notes, g.unrun)
		}
	}
}

// What a scout run or a pre-meeting brief brought in must not write the
// ledger; a write the store refuses is said in the conversation and the
// receipt.
func TestFrontRefused(t *testing.T) {
	cases := []struct {
		name, outside, note, receipt string
		err                          error
	}{
		{"scout turn", "巡检", "巡检触发的回合不能改工作线台账", "外部内容", nil},
		{"store refuses", "", "没有记", "新的工作线要有 name", front.ErrNoName},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, g, _, _, _ := newRunner(t)
			userTurn(r, said)
			if c.outside != "" {
				r.cfg.Outside = func() string { return c.outside }
			}
			out := fronter(r, c.err)
			r.Result(r.cfg.Planner, block(`{"type":"front","id":"x","state":"s"}`))
			r.handle(<-r.ch)
			if c.err == nil && len(*out) != 0 {
				t.Fatalf("wrote %+v, want none", *out)
			}
			if len(g.notes) != 1 || !strings.Contains(g.notes[0], c.note) {
				t.Errorf("notes = %q, want one with %q", g.notes, c.note)
			}
			receipts := append(append([]string{}, g.unrun...), g.refused...)
			if len(receipts) != 1 || !strings.Contains(receipts[0], c.receipt) {
				t.Errorf("receipts = %q, want one with %q", receipts, c.receipt)
			}
		})
	}
}

func TestFrontParse(t *testing.T) {
	for _, c := range []struct{ raw, err string }{
		{`{"type":"front","id":"jd-tgw","name":"JD","state":"s","next":"n","waiting":"w","sessions":["k"],"decision":"d","close":false}`, ""},
		{`{"type":"front","id":"jd-tgw","close":true}`, ""},
		{`{"type":"front","id":"JD TGW","name":"x"}`, "id"},
		{`{"type":"front","name":"x"}`, "id"},
		{`{"type":"front","id":"a","state":"` + strings.Repeat("字", front.TextRunes+1) + `"}`, "state"},
		{`{"type":"front","id":"a","sessions":[""]}`, "sessions"},
		{`{"type":"front","id":"a","workspace":"w"}`, "字段"},
	} {
		b := Parse(block(c.raw))
		if len(b) != 1 {
			t.Fatalf("%s: %d blocks", c.raw, len(b))
		}
		switch {
		case c.err == "" && (b[0].Err != nil || b[0].Front == nil):
			t.Errorf("%s: %v", c.raw, b[0].Err)
		case c.err != "" && (b[0].Err == nil || !strings.Contains(b[0].Err.Error(), c.err)):
			t.Errorf("%s: err %v, want one with %q", c.raw, b[0].Err, c.err)
		}
	}
}
