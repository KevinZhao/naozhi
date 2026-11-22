package front

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func texts(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Text
	}
	return out
}

// An item is filed once however often it is sent, survives a reload, leaves
// the list when closed and the file a week after; the open ones are capped.
func TestItems(t *testing.T) {
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "fronts.json")
	s := New(path, func() time.Time { return now })

	a, added, err := s.AddItem(Item{Text: "  补上 reconnect\n路径的单测 ", Workspace: "ws_ab12cd34", Source: "task:9f3c2a71d0", From: FromReport})
	if err != nil || !added || !ItemID(a.ID) || a.Text != "补上 reconnect 路径的单测" || !a.Created.Equal(now) || !a.Open() {
		t.Fatalf("filed %+v, %v, %v", a, added, err)
	}
	now = now.Add(time.Minute)
	again, added, err := s.AddItem(Item{Text: "补上 reconnect 路径的单测", Source: "task:9f3c2a71d0", From: FromReport})
	if err != nil || added || again.ID != a.ID {
		t.Fatalf("the same report filed twice: %+v, %v, %v", again, added, err)
	}
	// Another source, or another origin, is another item.
	b, added, err := s.AddItem(Item{Text: "补上 reconnect 路径的单测", Source: "task:9f3c2a71d0", From: FromPlanner})
	if err != nil || !added || b.ID == a.ID {
		t.Fatalf("the Planner's own item: %+v, %v, %v", b, added, err)
	}
	for _, bad := range []Item{
		{Text: " ", From: FromUser},
		{Text: strings.Repeat("字", ItemRunes+1), From: FromUser},
		{Text: "x", Source: strings.Repeat("s", SourceRunes+1), From: FromUser},
		{Text: "x", From: "someone"},
		{Text: "x"},
	} {
		if _, _, err := s.AddItem(bad); err == nil {
			t.Errorf("filed %+v", bad)
		}
	}
	if _, _, err := s.AddItem(Item{Text: "x", Front: "nope", From: FromUser}); !errors.Is(err, ErrNoFront) {
		t.Errorf("an item of a line the ledger lacks: %v", err)
	}

	reloaded := New(path, func() time.Time { return now })
	if got := reloaded.Items(); len(got) != 2 || got[0].ID != a.ID || got[1].ID != b.ID || got[0].Workspace != "ws_ab12cd34" {
		t.Fatalf("after a reload: %+v", got)
	}

	if _, err := reloaded.CloseItem("i0000000", ByUser); !errors.Is(err, ErrNoItem) {
		t.Errorf("closing an item that is not there: %v", err)
	}
	closedAt := now
	c, err := reloaded.CloseItem(a.ID, ByPlanner)
	if err != nil || c.Open() || c.ClosedBy != ByPlanner || !c.Closed.Equal(closedAt) {
		t.Fatalf("closed %+v, %v", c, err)
	}
	now = now.Add(time.Hour)
	if c, err := reloaded.CloseItem(a.ID, ByUser); err != nil || c.ClosedBy != ByPlanner || !c.Closed.Equal(closedAt) {
		t.Errorf("closing a closed item changed it: %+v, %v", c, err)
	}
	if got := reloaded.Items(); len(got) != 1 || got[0].ID != b.ID {
		t.Fatalf("open after a close: %+v", got)
	}
	// The same text from the same source again is a new item once the first
	// is closed.
	if n, added, err := reloaded.AddItem(Item{Text: "补上 reconnect 路径的单测", Source: "task:9f3c2a71d0", From: FromReport}); err != nil || !added || n.ID == a.ID {
		t.Errorf("filed again after its close: %+v, %v, %v", n, added, err)
	}

	kept := func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Contains(string(data), a.ID)
	}
	now = closedAt.Add(itemRetention - time.Minute)
	if _, _, err := reloaded.AddItem(Item{Text: "一周内", From: FromUser}); err != nil || !kept() {
		t.Fatalf("a closed item gone within its retention (%v)", err)
	}
	now = closedAt.Add(itemRetention + time.Minute)
	if _, _, err := reloaded.AddItem(Item{Text: "一周后", From: FromUser}); err != nil || kept() {
		t.Fatalf("a closed item kept past its retention (%v)", err)
	}

	full := New("", func() time.Time { return now })
	for i := range MaxOpenItems {
		if _, _, err := full.AddItem(Item{Text: fmt.Sprintf("第 %d 条", i), From: FromUser}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := full.AddItem(Item{Text: "多的一条", From: FromUser}); !errors.Is(err, ErrItemsFull) {
		t.Fatalf("past the cap: %v", err)
	}
	if got := full.CloseItems(func(it Item) bool { return it.Text == "第 0 条" || it.Text == "第 1 条" }, ByUser); len(got) != 2 || got[0].ClosedBy != ByUser {
		t.Fatalf("CloseItems closed %+v", got)
	}
	if _, _, err := full.AddItem(Item{Text: "多的一条", From: FromUser}); err != nil {
		t.Fatalf("after a close: %v", err)
	}
}

// An edit changes only the fields it names and keeps the item's identity;
// a closed item takes none.
func TestEditItem(t *testing.T) {
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	s := New("", func() time.Time { return now })
	if _, err := s.Apply(Update{ID: "gateway-refactor", Name: "网关重构"}); err != nil {
		t.Fatal(err)
	}
	it, _, err := s.AddItem(Item{Text: "迁完剩下的调用点", Source: "task:9f3c2a71d0", From: FromReport})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	got, err := s.EditItem(it.ID, nil, ptr("https://github.com/owner/repo/issues/7"), nil)
	if err != nil || got.Source != "https://github.com/owner/repo/issues/7" || got.Text != it.Text || !got.Created.Equal(it.Created) || got.From != FromReport {
		t.Fatalf("source edited: %+v, %v", got, err)
	}
	if got, err = s.EditItem(it.ID, ptr("已备好 PR #12"), nil, ptr("gateway-refactor")); err != nil || got.Text != "已备好 PR #12" || got.Front != "gateway-refactor" {
		t.Fatalf("text and line edited: %+v, %v", got, err)
	}
	if got, err = s.EditItem(it.ID, nil, nil, ptr("")); err != nil || got.Front != "" || got.Text != "已备好 PR #12" {
		t.Fatalf("taken off its line: %+v, %v", got, err)
	}
	for name, err := range map[string]error{
		"no such line": second(s.EditItem(it.ID, nil, nil, ptr("nope"))),
		"empty text":   second(s.EditItem(it.ID, ptr(" "), nil, nil)),
		"long text":    second(s.EditItem(it.ID, ptr(strings.Repeat("字", ItemRunes+1)), nil, nil)),
		"long source":  second(s.EditItem(it.ID, nil, ptr(strings.Repeat("s", SourceRunes+1)), nil)),
		"no such item": second(s.EditItem("i0000000", ptr("x"), nil, nil)),
	} {
		if err == nil {
			t.Errorf("%s: edited", name)
		}
	}
	if got := s.Items(); len(got) != 1 || got[0].Text != "已备好 PR #12" {
		t.Fatalf("a refused edit changed the item: %+v", got)
	}
	if _, err := s.CloseItem(it.ID, ByUser); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EditItem(it.ID, ptr("x"), nil, nil); !errors.Is(err, ErrItemClosed) {
		t.Errorf("a closed item edited: %v", err)
	}
}

func second[T any](_ T, err error) error { return err }

// An item shows under the line it was filed under while that is open, else
// under the open line its session is on, else under none; a line closing
// leaves its items open, and they go back under it when it reopens.
func TestItemFront(t *testing.T) {
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	s := New("", func() time.Time { return now })
	for _, u := range []Update{
		{ID: "gateway-refactor", Name: "网关重构"},
		{ID: "docs", Name: "文档"},
	} {
		if _, err := s.Apply(u); err != nil {
			t.Fatal(err)
		}
	}
	add := func(it Item) string {
		t.Helper()
		now = now.Add(time.Second)
		out, _, err := s.AddItem(it)
		if err != nil {
			t.Fatal(err)
		}
		return out.ID
	}
	filed := add(Item{Text: "归在线上的", Front: "gateway-refactor", From: FromUser})
	bySession := add(Item{Text: "会话留下的", Source: "task:9f3c2a71d0", From: FromReport})
	loose := add(Item{Text: "没归线的", Workspace: "ws_ab12cd34", Source: "https://github.com/owner/repo/pull/12", From: FromPlanner})
	fronts := func() map[string]string {
		out := map[string]string{}
		for _, it := range s.Items() {
			out[it.ID] = it.Front
		}
		return out
	}
	if got := fronts(); got[filed] != "gateway-refactor" || got[bySession] != "" || got[loose] != "" {
		t.Fatalf("before the session is on a line: %v", got)
	}
	// The Planner puts the session on a line after its report.
	now = now.Add(time.Minute)
	if _, err := s.Apply(Update{ID: "docs", Sessions: []string{"task:9f3c2a71d0"}}); err != nil {
		t.Fatal(err)
	}
	if got := fronts(); got[bySession] != "docs" || got[loose] != "" {
		t.Fatalf("with the session on a line: %v", got)
	}
	// The line it was filed under wins over the session's.
	if _, err := s.EditItem(bySession, nil, nil, ptr("gateway-refactor")); err != nil {
		t.Fatal(err)
	}
	if got := fronts(); got[bySession] != "gateway-refactor" {
		t.Fatalf("filed under another line than its session's: %v", got)
	}
	now = now.Add(time.Minute)
	if _, err := s.Apply(Update{ID: "gateway-refactor", Close: true}); err != nil {
		t.Fatal(err)
	}
	if got := fronts(); len(got) != 3 || got[filed] != "" || got[bySession] != "docs" {
		t.Fatalf("after its line closed: %v", got)
	}
	now = now.Add(time.Minute)
	if _, err := s.Apply(Update{ID: "gateway-refactor", Next: ptr("接着迁")}); err != nil {
		t.Fatal(err)
	}
	if got := fronts(); got[filed] != "gateway-refactor" || got[bySession] != "gateway-refactor" {
		t.Fatalf("after its line reopened: %v", got)
	}
	// Oldest first, whatever was edited since.
	if got := s.Items(); got[0].ID != filed || got[1].ID != bySession || got[2].ID != loose {
		t.Errorf("order: %v", texts(got))
	}
}

// Reconcile makes a check's group be its hits: new ones filed, the ones
// still hitting kept under their ID with the new text, the rest closed; an
// item outside the group is left alone, and one the user struck stays
// struck for a week.
func TestReconcile(t *testing.T) {
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "fronts.json")
	s := New(path, func() time.Time { return now })
	group := func(it Item) bool {
		return it.From == FromPatrol && it.Workspace == "ws_ab12cd34" && strings.HasPrefix(it.Source, "check:git:")
	}
	hit := func(source, text string) Item {
		return Item{Text: text, Workspace: "ws_ab12cd34", Source: source, From: FromPatrol}
	}
	other, _, err := s.AddItem(Item{Text: "别的工作区的", Workspace: "ws_zz99zz99", Source: "check:git:dirty", From: FromPatrol})
	if err != nil {
		t.Fatal(err)
	}
	mine, _, err := s.AddItem(Item{Text: "用户自己记的", Workspace: "ws_ab12cd34", Source: "check:git:dirty", From: FromUser})
	if err != nil {
		t.Fatal(err)
	}

	added, closed := s.Reconcile(group, []Item{hit("check:git:dirty", "12 个文件 5 天没提交"), hit("check:git:branch:feat-x", "分支 feat-x 推了 4 天没开 PR")}, ByPatrol)
	if len(added) != 2 || len(closed) != 0 || added[0].From != FromPatrol || !ItemID(added[0].ID) {
		t.Fatalf("first round: added %+v, closed %+v", added, closed)
	}
	dirty := added[0]

	now = now.Add(time.Hour)
	added, closed = s.Reconcile(group, []Item{hit("check:git:dirty", "14 个文件 5 天没提交")}, ByPatrol)
	if len(added) != 0 || len(closed) != 1 || closed[0].Source != "check:git:branch:feat-x" || closed[0].ClosedBy != ByPatrol {
		t.Fatalf("second round: added %+v, closed %+v", added, closed)
	}
	open := New(path, nil).Items()
	if len(open) != 3 || open[0].ID != other.ID || open[1].ID != mine.ID || open[2].ID != dirty.ID ||
		open[2].Text != "14 个文件 5 天没提交" || !open[2].Created.Equal(dirty.Created) {
		t.Fatalf("after the second round: %+v", open)
	}

	// The same hits again write nothing.
	before, _ := os.Stat(path)
	if err := os.Chtimes(path, before.ModTime().Add(-time.Hour), before.ModTime().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	before, _ = os.Stat(path)
	if added, closed = s.Reconcile(group, []Item{hit("check:git:dirty", "14 个文件 5 天没提交")}, ByPatrol); len(added)+len(closed) != 0 {
		t.Fatalf("an unchanged round: added %+v, closed %+v", added, closed)
	}
	if after, _ := os.Stat(path); !after.ModTime().Equal(before.ModTime()) {
		t.Error("an unchanged round rewrote the file")
	}

	// Struck by the user: not filed again while the record is kept.
	struck := now
	if _, err := s.CloseItem(dirty.ID, ByUser); err != nil {
		t.Fatal(err)
	}
	now = struck.Add(itemRetention - time.Hour)
	if added, closed = s.Reconcile(group, []Item{hit("check:git:dirty", "20 个文件 12 天没提交")}, ByPatrol); len(added)+len(closed) != 0 {
		t.Fatalf("a struck item filed again: added %+v, closed %+v", added, closed)
	}
	now = struck.Add(itemRetention + time.Hour)
	if added, _ = s.Reconcile(group, []Item{hit("check:git:dirty", "20 个文件 12 天没提交")}, ByPatrol); len(added) != 1 || added[0].ID == dirty.ID {
		t.Fatalf("past the retention: added %+v", added)
	}

	// One closed by the check itself comes back when it hits again; a hit
	// with no source or no text, or one the group does not take (it would
	// be filed again every round), is left out.
	now = now.Add(time.Hour)
	if _, closed = s.Reconcile(group, nil, ByPatrol); len(closed) != 1 {
		t.Fatalf("no hit: closed %+v", closed)
	}
	now = now.Add(time.Hour)
	added, _ = s.Reconcile(group, []Item{hit("check:git:dirty", "3 个文件 4 天没提交"), hit("", "没有出处"), hit("check:git:empty", " "),
		{Text: "圈外的", Workspace: "ws_zz99zz99", Source: "check:git:stray", From: FromPatrol}}, ByPatrol)
	if len(added) != 1 || added[0].Text != "3 个文件 4 天没提交" {
		t.Fatalf("hitting again: added %+v", added)
	}
	if got := s.Items(); !slices.Equal(texts(got), []string{"别的工作区的", "用户自己记的", "3 个文件 4 天没提交"}) {
		t.Errorf("in the end: %v", texts(got))
	}
}

func TestSource(t *testing.T) {
	for _, tc := range []struct {
		source      string
		pr, issue   bool
		owner, repo string
		n           int
	}{
		{"https://github.com/owner/repo/pull/12", true, false, "owner", "repo", 12},
		{" https://github.com/owner/repo.js/pull/12/files ", true, false, "owner", "repo.js", 12},
		{"https://github.com/owner/repo/pull/12#issuecomment-1", true, false, "owner", "repo", 12},
		{"https://github.com/owner/repo/issues/7", false, true, "owner", "repo", 7},
		{"#4025", false, true, "", "", 4025},
		{"https://github.com/owner/repo/pull/abc", false, false, "", "", 0},
		{"https://github.com/owner/repo/pull/0", false, false, "", "", 0},
		{"https://github.com/owner/repo/pull/12abc", false, false, "", "", 0},
		{"https://gitlab.com/owner/repo/pull/12", false, false, "", "", 0},
		{"https://github.com.example.org/owner/repo/pull/12", false, false, "", "", 0},
		{"http://github.com/owner/repo/pull/12", false, false, "", "", 0},
		{"https://github.com/owner/../pull/12", false, false, "", "", 0},
		{"https://github.com/-R/x --web/pull/1", false, false, "", "", 0},
		{"task:9f3c2a71d0", false, false, "", "", 0},
		{"#", false, false, "", "", 0},
		{"# 12", false, false, "", "", 0},
		{"", false, false, "", "", 0},
	} {
		o, r, n, ok := SourcePR(tc.source)
		if ok != tc.pr || (ok && (o != tc.owner || r != tc.repo || n != tc.n)) {
			t.Errorf("SourcePR(%q) = %q %q %d %v", tc.source, o, r, n, ok)
		}
		o, r, n, ok = SourceIssue(tc.source)
		if ok != tc.issue || (ok && (o != tc.owner || r != tc.repo || n != tc.n)) {
			t.Errorf("SourceIssue(%q) = %q %q %d %v", tc.source, o, r, n, ok)
		}
	}
}

// A ledger a newer build wrote is not overwritten by an item, and one from
// before the items reads as having none.
func TestItemsReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fronts.json")
	newer := fmt.Sprintf(`{"v":%d,"fronts":[],"items":[{"id":"i7k2m9qa","text":"x","from":"user","created":"2026-11-23T10:00:00Z"}]}`, Version+1)
	if err := os.WriteFile(path, []byte(newer), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(path, nil)
	if it, added, err := s.AddItem(Item{Text: "只在内存里", From: FromUser}); err != nil || !added || len(s.Items()) != 1 {
		t.Fatalf("in memory: %+v, %v, %v", it, added, err)
	}
	if got, _ := os.ReadFile(path); string(got) != newer {
		t.Errorf("a newer build's ledger overwritten: %s", got)
	}

	old := filepath.Join(t.TempDir(), "fronts.json")
	if err := os.WriteFile(old, []byte(`{"v":1,"fronts":[{"id":"docs","name":"文档","created":"2026-11-20T10:00:00Z","updated":"2026-11-20T10:00:00Z"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s = New(old, nil)
	if len(s.Items()) != 0 || len(s.Open()) != 1 {
		t.Fatalf("a ledger from before the items: %+v, %+v", s.Items(), s.Open())
	}
	if _, _, err := s.AddItem(Item{Text: "新的一条", Front: "docs", From: FromUser}); err != nil {
		t.Fatal(err)
	}
	if again := New(old, nil); len(again.Items()) != 1 || len(again.Open()) != 1 || again.Items()[0].Front != "docs" {
		t.Errorf("after a reload: %+v", again.Items())
	}
}
