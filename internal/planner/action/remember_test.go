package action

import (
	"errors"
	"strings"
	"testing"
)

type remembered struct{ ws, name, text string }

// rememberer wires r's Remember and records what it is asked to keep.
func rememberer(r *Runner, err error) *[]remembered {
	var out []remembered
	r.cfg.Remember = func(ws, name, text string) (string, error) {
		out = append(out, remembered{ws, name, text})
		return "/m/" + name + ".md", err
	}
	return &out
}

// A remember block in a turn the user started is kept, silently: the card
// says it.
func TestRememberUserTurn(t *testing.T) {
	r, g, _, _, _ := newRunner(t)
	userTurn(r, said)
	out := rememberer(r, nil)
	r.Result(r.cfg.Planner, "记下了。\n"+block(`{"type":"remember","workspace":"global","name":"frankfurt-az","text":"法兰克福用 az2"}`))
	r.handle(<-r.ch)
	if len(*out) != 1 || (*out)[0] != (remembered{"global", "frankfurt-az", "法兰克福用 az2"}) {
		t.Fatalf("remembered = %+v", *out)
	}
	if len(g.notes)+len(g.unrun) != 0 {
		t.Errorf("notes %q unrun %q, want none", g.notes, g.unrun)
	}
}

// Only a turn the user started may remember (D41: no outside content in the
// memory); a failure is said in the conversation and the receipt.
func TestRememberRefused(t *testing.T) {
	cases := []struct {
		name, outside, note, receipt string
		words                        bool
		err                          error
	}{
		{"receipt turn", "", "不能记记忆", "不是用户发起的", false, nil},
		{"scout turn", "巡检", "巡检触发的回合不能记记忆", "巡检触发", true, nil},
		{"write fails", "", "没有记下", "没有记下", true, errors.New("找不到工作区")},
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
			out := rememberer(r, c.err)
			r.Result(r.cfg.Planner, block(`{"type":"remember","workspace":"w","name":"n","text":"t"}`))
			r.handle(<-r.ch)
			if c.err == nil && len(*out) != 0 {
				t.Fatalf("remembered %+v, want none", *out)
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

func TestRememberParse(t *testing.T) {
	for _, c := range []struct{ raw, err string }{
		{`{"type":"remember","workspace":"w","name":"ok-1_a","text":"t"}`, ""},
		{`{"type":"remember","workspace":"","name":"n","text":"t"}`, "workspace"},
		{`{"type":"remember","workspace":"w","name":"../x","text":"t"}`, "name"},
		{`{"type":"remember","workspace":"w","name":"Big","text":"t"}`, "name"},
		{`{"type":"remember","workspace":"w","name":"n","text":" "}`, "text"},
		{`{"type":"remember","workspace":"w","name":"n","text":"` + strings.Repeat("字", rememberTextRunes+1) + `"}`, "text"},
		{`{"type":"remember","workspace":"w","name":"n","text":"t","kind":"x"}`, "字段"},
	} {
		b := Parse(block(c.raw))
		if len(b) != 1 {
			t.Fatalf("%s: %d blocks", c.raw, len(b))
		}
		switch {
		case c.err == "" && (b[0].Err != nil || b[0].Remember == nil):
			t.Errorf("%s: %v", c.raw, b[0].Err)
		case c.err != "" && (b[0].Err == nil || !strings.Contains(b[0].Err.Error(), c.err)):
			t.Errorf("%s: err %v, want one with %q", c.raw, b[0].Err, c.err)
		}
	}
}
