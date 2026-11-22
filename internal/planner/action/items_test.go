package action

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/planner/front"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
)

// ledger gives r a ledger for its front blocks and its reports' open
// items, with the line "gateway-refactor" in it.
func ledger(t *testing.T, r *Runner) *front.Store {
	t.Helper()
	s := front.New("", r.cfg.Now)
	if _, err := s.Apply(front.Update{ID: "gateway-refactor", Name: "网关重构"}); err != nil {
		t.Fatal(err)
	}
	r.cfg.Front, r.cfg.Items = s.Apply, s
	return s
}

func itemTexts(items []front.Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Text
	}
	return out
}

func leftReply(state string, left ...string) string {
	body := `{"state":"` + state + `","summary":"迁了一半"`
	if len(left) > 0 {
		body += `,"left":["` + strings.Join(left, `","`) + `"]`
	}
	return "做了什么：迁了 internal/a。\n\n" + reportBlock(body+"}")
}

// What a dispatched session's report leaves is filed as open items, the
// session their source and its workspace theirs, once however often it is
// said; the receipt says how many were filed. They stay through a report
// that leaves more or is not done, and close when the session reports done
// with nothing left (§15.23, D143).
func TestReportLeft(t *testing.T) {
	e := endedRunner(t)
	s := ledger(t, e.r)
	m, _ := e.side.Get(e.key)
	if m.Workspace == "" {
		t.Fatal("the dispatched session has no workspace")
	}

	e.end(leftReply("progress", "迁完 internal/b 的调用点", "跑一遍集成测试"))
	open := s.Items()
	if !slices.Equal(itemTexts(open), []string{"迁完 internal/b 的调用点", "跑一遍集成测试"}) {
		t.Fatalf("filed %+v", open)
	}
	for _, it := range open {
		if it.From != front.FromReport || it.Source != e.key || it.Workspace != m.Workspace || it.Front != "" {
			t.Errorf("item %+v", it)
		}
	}
	if len(e.g.told) != 1 || !strings.Contains(e.g.told[0], "汇报：进度 · 「迁了一半」 · 留下 2 条未了事项（已入列）") {
		t.Fatalf("told = %q", e.g.told)
	}

	// The same line again files nothing; a new one is one more. Done with
	// something left closes none.
	e.g.told = nil
	e.wait(time.Minute)
	e.end(leftReply("done", "跑一遍集成测试", "更新 README"))
	if got := itemTexts(s.Items()); !slices.Equal(got, []string{"迁完 internal/b 的调用点", "跑一遍集成测试", "更新 README"}) {
		t.Fatalf("after done with something left: %q", got)
	}
	if len(e.g.told) != 1 || !strings.Contains(e.g.told[0], "留下 1 条未了事项（已入列）") {
		t.Errorf("told = %q", e.g.told)
	}

	// Not done and nothing left: they stay.
	e.g.told = nil
	e.wait(time.Minute)
	e.end(leftReply("blocked"))
	if len(s.Items()) != 3 {
		t.Fatalf("after blocked: %q", itemTexts(s.Items()))
	}
	if len(e.g.told) != 1 || strings.Contains(e.g.told[0], "未了事项") {
		t.Errorf("told = %q, want no word of items", e.g.told)
	}

	// Another session's items, and one the Planner wrote of this session,
	// are not this report's to close.
	other, _, err := s.AddItem(front.Item{Text: "别的会话留下的", Source: "task:other", From: front.FromReport})
	if err != nil {
		t.Fatal(err)
	}
	own, _, err := s.AddItem(front.Item{Text: "Planner 自己记的", Source: e.key, From: front.FromPlanner})
	if err != nil {
		t.Fatal(err)
	}
	e.wait(time.Minute)
	e.end(leftReply("done"))
	if got := s.Items(); len(got) != 2 || got[0].ID != other.ID || got[1].ID != own.ID {
		t.Fatalf("after done with nothing left: %+v", got)
	}

	// With no ledger the report goes on as before.
	e.r.cfg.Items = nil
	e.g.told = nil
	e.wait(time.Minute)
	e.end(leftReply("progress", "没有台账"))
	if len(e.g.told) != 1 || strings.Contains(e.g.told[0], "未了事项") {
		t.Errorf("with no ledger: told = %q", e.g.told)
	}
}

// A front block's item is the user's in a turn they started and the
// Planner's own otherwise, by the turn's source; it goes under the block's
// line, and its workspace is the one named, else its source session's.
func TestFrontItem(t *testing.T) {
	for _, c := range []struct {
		name  string
		words bool
		from  front.From
	}{
		{"the user's turn", true, front.FromUser},
		{"a receipt's turn", false, front.FromPlanner},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, g, _, side, dir := newRunner(t)
			userTurn(r)
			if c.words {
				userTurn(r, said)
			}
			s := ledger(t, r)
			ws, _ := r.cfg.Workspaces.Lookup(dir)
			if err := side.Put(sidecar.Meta{Key: "task:ab12", Purpose: sidecar.PurposeTask, Workspace: ws.ID}); err != nil {
				t.Fatal(err)
			}
			r.cfg.SessionDir = func(key string) string {
				if key == "dashboard:direct:a:general" {
					return filepath.Join(dir, "sub")
				}
				return ""
			}
			r.Result(r.cfg.Planner,
				block(`{"type":"front","id":"gateway-refactor","state":"迁了一半","item":{"text":" 给 README 补上\n安装步骤 ","workspace":"`+filepath.Base(dir)+`","source":"11-23 用户说"}}`)+"\n"+
					block(`{"type":"front","item":{"text":"迁完剩下的调用点","source":"task:ab12"}}`)+"\n"+
					block(`{"type":"front","item":{"text":"登录页的空状态","source":"dashboard:direct:a:general"}}`)+"\n"+
					block(`{"type":"front","item":{"text":"没有出处的一条"}}`))
			r.handle(<-r.ch)
			if len(g.notes)+len(g.unrun) != 0 {
				t.Fatalf("notes %q unrun %q, want none", g.notes, g.unrun)
			}
			open := s.Items()
			if len(open) != 4 {
				t.Fatalf("filed %+v", open)
			}
			want := []front.Item{
				{Text: "给 README 补上 安装步骤", Workspace: ws.ID, Front: "gateway-refactor", Source: "11-23 用户说"},
				{Text: "迁完剩下的调用点", Workspace: ws.ID, Source: "task:ab12"},
				{Text: "登录页的空状态", Workspace: ws.ID, Source: "dashboard:direct:a:general"},
				{Text: "没有出处的一条"},
			}
			for i, it := range open {
				w := want[i]
				if it.Text != w.Text || it.Workspace != w.Workspace || it.Front != w.Front || it.Source != w.Source || it.From != c.from {
					t.Errorf("item %d = %+v, want %+v from %s", i, it, w, c.from)
				}
			}
			if f := s.Open(); len(f) != 1 || f[0].State != "迁了一半" {
				t.Errorf("the line: %+v", f)
			}
		})
	}
}

// done closes the items it lists and touches no line: a closed one stays
// closed. An item with an id takes a new source and stays the same item.
func TestFrontDone(t *testing.T) {
	r, g, _, _, _ := newRunner(t)
	userTurn(r)
	s := ledger(t, r)
	var ids []string
	for _, text := range []string{"第一条", "第二条", "第三条"} {
		it, _, err := s.AddItem(front.Item{Text: text, Front: "gateway-refactor", Source: "task:ab12", From: front.FromReport})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, it.ID)
	}
	if _, err := s.Apply(front.Update{ID: "gateway-refactor", Close: true}); err != nil {
		t.Fatal(err)
	}
	var applied []front.Update
	r.cfg.Front = func(u front.Update) (front.Front, error) {
		applied = append(applied, u)
		return s.Apply(u)
	}
	r.Result(r.cfg.Planner,
		block(`{"type":"front","id":"gateway-refactor","done":["`+ids[0]+`"]}`)+"\n"+
			block(`{"type":"front","done":["`+ids[1]+`"]}`)+"\n"+
			block(`{"type":"front","item":{"id":"`+ids[2]+`","source":"https://github.com/owner/repo/issues/7"}}`))
	r.handle(<-r.ch)
	if len(g.notes)+len(g.unrun) != 0 {
		t.Fatalf("notes %q unrun %q, want none", g.notes, g.unrun)
	}
	if len(applied) != 0 || len(s.Open()) != 0 {
		t.Errorf("closing items wrote the line: %+v, open %+v", applied, s.Open())
	}
	open := s.Items()
	if len(open) != 1 || open[0].ID != ids[2] || open[0].Source != "https://github.com/owner/repo/issues/7" || open[0].Text != "第三条" || open[0].From != front.FromReport {
		t.Fatalf("open = %+v", open)
	}
}

// What a front block could not do is said part by part: the line written
// stays written, and the receipt names the item that was not.
func TestFrontItemFailures(t *testing.T) {
	for _, c := range []struct {
		name, body string
		// receipt and note are in the receipt and the conversation line.
		receipt, note string
		// state is the line's state after, items the open items.
		state string
		items int
	}{
		{"an item not there", `{"type":"front","id":"gateway-refactor","state":"s2","done":["i0000000","i0000001"]}`,
			"工作线「gateway-refactor」已记；未了事项 i0000000 没有了结：找不到这条未了事项（id 见 fronts.json 的 items）；未了事项 i0000001 没有了结", "动作块没有执行", "s2", 0},
		{"only done, not there", `{"type":"front","done":["i0000000"]}`, "第 1 块（front）：未了事项 i0000000 没有了结：找不到", "动作块没有执行", "", 0},
		{"a new source for none", `{"type":"front","item":{"id":"i0000000","source":"#7"}}`, "未了事项 i0000000 的出处没有改：找不到", "动作块没有执行", "", 0},
		// Checked before anything is written: the line stays as it was.
		{"an unknown workspace", `{"type":"front","id":"gateway-refactor","state":"s2","item":{"text":"x","workspace":"nowhere"}}`,
			"第 1 块（front「gateway-refactor」）：未了事项「x」没有记：找不到工作区「nowhere」", "动作块格式不对", "", 0},
		{"a line not there", `{"type":"front","id":"nope","item":{"text":"x"}}`, "工作线「nope」没有记：新的工作线要有 name", "动作块没有执行", "", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, g, _, _, _ := newRunner(t)
			userTurn(r)
			s := ledger(t, r)
			r.Result(r.cfg.Planner, block(c.body))
			r.handle(<-r.ch)
			if len(g.unrun) != 1 || !strings.Contains(g.unrun[0], c.receipt) {
				t.Errorf("receipt = %q, want one with %q", g.unrun, c.receipt)
			}
			if len(g.notes) != 1 || !strings.Contains(g.notes[0], c.note) {
				t.Errorf("notes = %q, want one with %q", g.notes, c.note)
			}
			if f := s.Open(); len(f) != 1 || f[0].State != c.state || len(s.Items()) != c.items {
				t.Errorf("the ledger: %+v, items %+v", f, s.Items())
			}
		})
	}

	// A gateway whose ledger takes no items writes nothing of such a block.
	r, g, _, _, _ := newRunner(t)
	userTurn(r)
	out := fronter(r, nil)
	r.Result(r.cfg.Planner, block(`{"type":"front","id":"gateway-refactor","state":"s","item":{"text":"x"}}`))
	r.handle(<-r.ch)
	if len(*out) != 0 || len(g.unrun) != 1 || !strings.Contains(g.unrun[0], "不记未了事项") {
		t.Errorf("with no items: wrote %+v, receipt %q", *out, g.unrun)
	}
}

// A turn a watched session's turn end started files an item (the Planner's
// own); one whose session read outside content does not, nor does a scout
// run's turn.
func TestFrontItemTurnSources(t *testing.T) {
	const item = `{"type":"front","item":{"text":"登录页的空状态还没做","source":"` + dashKey + `"}}`
	w := newWatchTurn(t, "")
	s := ledger(t, w.r)
	w.run(block(item))
	if open := s.Items(); len(open) != 1 || open[0].From != front.FromPlanner || open[0].Source != dashKey {
		t.Fatalf("a watch turn's item: %+v", open)
	}
	if len(w.g.notes)+len(w.g.watchRefused) != 0 {
		t.Errorf("notes %q receipts %q, want none", w.g.notes, w.g.watchRefused)
	}

	w = newWatchTurn(t, "读过外部内容的会话")
	s = ledger(t, w.r)
	w.run(block(item) + "\n" + block(`{"type":"front","done":["i0000000"]}`))
	if len(s.Items()) != 0 {
		t.Fatalf("a tainted session's watch turn filed %+v", s.Items())
	}
	if len(w.g.notes) != 2 || !strings.Contains(w.g.notes[0], "不能改工作线台账：「登录页的空状态还没做」没有记") || !strings.Contains(w.g.notes[1], "「未了事项」没有记") {
		t.Errorf("conversation lines = %q", w.g.notes)
	}

	r, g, _, _, _ := newRunner(t)
	userTurn(r, said)
	s = ledger(t, r)
	r.cfg.Outside = func() string { return "巡检" }
	r.Result(r.cfg.Planner, block(item))
	r.handle(<-r.ch)
	if len(s.Items()) != 0 || len(g.refused) != 1 || !strings.Contains(g.refused[0], "不能改工作线台账") {
		t.Errorf("a scout turn: filed %+v, receipts %q", s.Items(), g.refused)
	}
}

func TestFrontItemParse(t *testing.T) {
	long := strings.Repeat("字", front.ItemRunes+1)
	for _, c := range []struct{ raw, err string }{
		{`{"type":"front","item":{"text":"迁完剩下的调用点"}}`, ""},
		{`{"type":"front","id":"gateway-refactor","item":{"text":"x","workspace":"ws_ab12cd34","source":"task:ab12"},"done":["i7k2m9qa"]}`, ""},
		{`{"type":"front","done":["i7k2m9qa"," i0c4x8zn "]}`, ""},
		{`{"type":"front","item":{"id":"i7k2m9qa","source":"https://github.com/owner/repo/issues/7"}}`, ""},
		{`{"type":"front"}`, "id"},
		{`{"type":"front","state":"s","item":{"text":"x"}}`, "id"},
		{`{"type":"front","close":true,"done":["i7k2m9qa"]}`, "id"},
		{`{"type":"front","item":{"text":" "}}`, "缺少 text"},
		{`{"type":"front","item":{"text":"` + long + `"}}`, "超过 100 字"},
		{`{"type":"front","item":{"text":"x","source":"` + strings.Repeat("s", front.SourceRunes+1) + `"}}`, "source 超过"},
		{`{"type":"front","item":{"text":"x","priority":"high"}}`, "字段"},
		{`{"type":"front","item":{"id":"7","source":"#7"}}`, "id"},
		{`{"type":"front","item":{"id":"i7k2m9qa"}}`, "要有 source"},
		{`{"type":"front","item":{"id":"i7k2m9qa","text":"改个说法","source":"#7"}}`, "只能再带 source"},
		{`{"type":"front","done":["gateway-refactor"]}`, "done 里要写未了事项的 id"},
		{`{"type":"front","done":["i7k2m9qa","i7k2m9qb","i7k2m9qc","i7k2m9qd","i7k2m9qe","i7k2m9qf","i7k2m9qg","i7k2m9qh","i7k2m9qi","i7k2m9qj","i7k2m9qk"]}`, "最多 10 个"},
		{`{"type":"front","done":"i7k2m9qa"}`, "字段"},
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
	if b := Parse(block(`{"type":"front","done":[" i0c4x8zn "]}`))[0]; b.Front == nil || b.Front.Done[0] != "i0c4x8zn" {
		t.Errorf("done not trimmed: %+v", b.Front)
	}
}
