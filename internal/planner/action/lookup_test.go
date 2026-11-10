package action

import (
	"strings"
	"testing"
)

func TestLookupParse(t *testing.T) {
	for _, c := range []struct{ raw, err string }{
		{`{"type":"lookup","q":"M4 Max 的内存带宽","why":"用户问规格"}`, ""},
		{`{"type":"lookup","q":"x"}`, ""},
		{`{"type":"lookup","q":" ","why":"w"}`, "q"},
		{`{"type":"lookup","q":"` + strings.Repeat("字", lookupQRunes+1) + `"}`, "q"},
		{`{"type":"lookup","q":"x","why":"` + strings.Repeat("字", lookupWhyRunes+1) + `"}`, "why"},
		{`{"type":"lookup","q":"x","url":"https://e.com"}`, "字段"},
	} {
		b := Parse(block(c.raw))
		if len(b) != 1 {
			t.Fatalf("%s: %d blocks", c.raw, len(b))
		}
		switch {
		case c.err == "" && (b[0].Err != nil || b[0].Lookup == nil):
			t.Errorf("%s: %v", c.raw, b[0].Err)
		case c.err != "" && (b[0].Err == nil || !strings.Contains(b[0].Err.Error(), c.err)):
			t.Errorf("%s: err %v, want one with %q", c.raw, b[0].Err, c.err)
		}
	}
}

// A lookup runs from any turn but an outside one: a scout's, a brief's, or
// a lookup answer's own, which may only notify, promise and ask (§15.12 ⑨).
func TestLookupTurns(t *testing.T) {
	for _, c := range []struct {
		name, outside string
		ran           bool
	}{
		{"user turn", "", true},
		{"scout turn", "巡检", false},
		{"lookup answer turn", "网页查询结果", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, g, _, _, dir := newRunner(t)
			var asked []string
			r.cfg.Lookup = func(q, _ string) error { asked = append(asked, q); return nil }
			if c.outside != "" {
				r.cfg.Outside = func() string { return c.outside }
			}
			r.Result(r.cfg.Planner, block(`{"type":"lookup","q":"规格","why":"w"}`)+"\n"+
				block(`{"type":"dispatch","workspace":"`+dir+`","title":"t","message":"m"}`)+"\n"+
				block(`{"type":"notify","title":"规格查到了","why":"w","level":"timely","link":"","id":""}`))
			r.handle(<-r.ch)
			if got := len(asked) == 1; got != c.ran {
				t.Fatalf("looked up %q, want ran=%v", asked, c.ran)
			}
			if c.ran {
				return
			}
			if len(g.spawned) != 0 {
				t.Errorf("spawned %q in an outside turn", g.spawned)
			}
			if len(g.refused) != 1 || !strings.Contains(g.refused[0], "不能发起网页查询") || !strings.Contains(g.refused[0], "不能派活") ||
				strings.Contains(g.refused[0], "第 3 块") {
				t.Errorf("refusal receipt = %q, want the lookup and the dispatch refused, the notify run", g.refused)
			}
		})
	}
}
