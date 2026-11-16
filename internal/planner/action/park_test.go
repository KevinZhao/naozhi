package action

import (
	"errors"
	"strings"
	"testing"
)

type parkedItem struct{ text, source string }

// parker wires r's Park and records what it is asked to set aside.
func parker(r *Runner, err error) *[]parkedItem {
	var out []parkedItem
	r.cfg.Park = func(text, source string) error {
		out = append(out, parkedItem{text, source})
		return err
	}
	return &out
}

// A park block in a turn the user started moves the item, silently: the
// card says it (§15.14 ⑤).
func TestParkUserTurn(t *testing.T) {
	r, g, _, _, _ := newRunner(t)
	userTurn(r, said)
	out := parker(r, nil)
	r.Result(r.cfg.Planner, "好，先放下。\n"+block(`{"type":"park","text":"家里网络  换不换路由器","source":"11-14 15:25 用户说"}`))
	r.handle(<-r.ch)
	if len(*out) != 1 || (*out)[0] != (parkedItem{"家里网络 换不换路由器", "11-14 15:25 用户说"}) {
		t.Fatalf("parked = %+v", *out)
	}
	if len(g.notes)+len(g.unrun) != 0 {
		t.Errorf("notes %q unrun %q, want none", g.notes, g.unrun)
	}
}

// Only a turn the user started may park: a mail or a session's reply must
// not set aside what the user is to decide. A failure is said in the
// conversation and the receipt.
func TestParkRefused(t *testing.T) {
	cases := []struct {
		name, outside, note, receipt string
		words                        bool
		err                          error
	}{
		{"receipt turn", "", "不能搁置", "不是用户发起的", false, nil},
		{"scout turn", "巡检", "巡检触发的回合不能搁置", "巡检触发", true, nil},
		{"write fails", "", "没有搁置", "没有搁置", true, errors.New("写不了搁置区")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, g, _, _, _ := newRunner(t)
			userTurn(r)
			if c.words {
				userTurn(r, said)
			}
			if c.outside != "" {
				r.cfg.Outside = func() string { return c.outside }
			}
			out := parker(r, c.err)
			r.Result(r.cfg.Planner, block(`{"type":"park","text":"t","source":"#1"}`))
			r.handle(<-r.ch)
			if c.err == nil && len(*out) != 0 {
				t.Fatalf("parked %+v, want none", *out)
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

func TestParkParse(t *testing.T) {
	for _, c := range []struct{ raw, err string }{
		{`{"type":"park","text":"t","source":"task:ab12"}`, ""},
		{`{"type":"park","text":" ","source":"#1"}`, "text"},
		{`{"type":"park","text":"t","source":""}`, "source"},
		{`{"type":"park","text":"` + strings.Repeat("字", parkTextRunes+1) + `","source":"#1"}`, "text"},
		{`{"type":"park","text":"t","source":"` + strings.Repeat("字", parkSourceRunes+1) + `"}`, "source"},
		{`{"type":"park","text":"t","source":"#1","why":"x"}`, "字段"},
	} {
		b := Parse(block(c.raw))
		if len(b) != 1 {
			t.Fatalf("%s: %d blocks", c.raw, len(b))
		}
		switch {
		case c.err == "" && (b[0].Err != nil || b[0].Park == nil):
			t.Errorf("%s: %v", c.raw, b[0].Err)
		case c.err != "" && (b[0].Err == nil || !strings.Contains(b[0].Err.Error(), c.err)):
			t.Errorf("%s: err %v, want one with %q", c.raw, b[0].Err, c.err)
		}
	}
}
