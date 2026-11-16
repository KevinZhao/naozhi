package parked

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T, now *time.Time) *Store {
	t.Helper()
	dir := t.TempDir()
	return New(filepath.Join(dir, "parked.md"), filepath.Join(dir, "handoff", "decide.json"), func() time.Time { return *now })
}

// An item written unchanged in the handoffs moves in once more than
// StaleDays calendar days passed since it was first written so; one
// rewritten starts its count again; one the handoff stops listing is
// forgotten.
func TestSweepStale(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 11, 14, 8, 30, 0, 0, loc)
	s := newStore(t, &now)
	items := []string{"HYD 选型（出处：11-12 10:00 用户说）", "M1 要不要升级（出处：#3901）"}
	for day := 14; day <= 17; day++ {
		now = time.Date(2026, 11, day, 8, 31, 0, 0, loc)
		if day == 16 {
			items[1] = "M1 要不要升级：已问过价格（出处：#3901）"
		}
		keep, moved, total, err := s.Sweep(items)
		if err != nil || moved != 0 || total != 0 || !slices.Equal(keep, []bool{true, true}) {
			t.Fatalf("11-%d: keep %v moved %d total %d err %v", day, keep, moved, total, err)
		}
	}
	now = time.Date(2026, 11, 18, 8, 29, 0, 0, loc)
	keep, moved, total, err := s.Sweep(items)
	if err != nil || moved != 1 || total != 1 || !slices.Equal(keep, []bool{false, true}) {
		t.Fatalf("11-18: keep %v moved %d total %d err %v", keep, moved, total, err)
	}
	list, _ := s.List()
	if len(list) != 1 || list[0] != (Entry{Text: items[0], Day: "2026-11-18"}) {
		t.Fatalf("list = %+v", list)
	}
	// Copied again, the parked one stays out of the handoff.
	now = now.Add(24 * time.Hour)
	if keep, moved, total, _ := s.Sweep(items); moved != 0 || total != 1 || !slices.Equal(keep, []bool{false, true}) {
		t.Fatalf("copied again: keep %v moved %d total %d", keep, moved, total)
	}
	// Decided: the tracking forgets M1, so it would start afresh.
	if _, _, _, err := s.Sweep(nil); err != nil {
		t.Fatal(err)
	}
	if d := s.readDecided(); len(d.Items) != 0 {
		t.Errorf("tracking kept %v", d.Items)
	}
}

// Park writes the user's item with its source and day, once; lines the
// user wrote and the header stay, and a line the user deleted is no longer
// listed.
func TestPark(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.Local)
	s := newStore(t, &now)
	e, err := s.Park("家里网络\n换不换路由器", "11-14 15:25 用户说")
	if err != nil || e != (Entry{Text: "家里网络 换不换路由器（出处：11-14 15:25 用户说）", Day: "2026-11-16", ByUser: true}) {
		t.Fatalf("park = %+v, %v", e, err)
	}
	if _, err := s.Park("家里网络 换不换路由器", "11-14 15:25 用户说"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Park("x", ""); err == nil {
		t.Error("parked without a source")
	}
	data, _ := os.ReadFile(s.Path())
	text := string(data)
	if !strings.HasPrefix(text, "# 搁置区\n") || strings.Count(text, "家里网络") != 1 ||
		!strings.Contains(text, "- 家里网络 换不换路由器（出处：11-14 15:25 用户说） · 移入 2026-11-16 · 你说放下\n") {
		t.Fatalf("parked.md = %q", text)
	}
	// The user's own note and a deletion.
	text = strings.Replace(text, "# 搁置区\n", "# 搁置区\n我的备注\n", 1)
	text += "- CDE 要不要做（出处：#3800） · 移入 2026-11-10\n"
	if err := os.WriteFile(s.Path(), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.List(); len(list) != 2 {
		t.Fatalf("list = %+v", list)
	}
	if _, err := s.Park("M1", "#3901"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(s.Path())
	if got := string(data); !strings.Contains(got, "我的备注\n") || strings.Count(got, "# 搁置区") != 1 || !strings.Contains(got, "CDE") {
		t.Fatalf("parked.md lost the user's lines: %q", got)
	}
}

// With no state root nothing is parked and every item stays.
func TestNoRoot(t *testing.T) {
	s := New("", "", nil)
	keep, _, _, err := s.Sweep([]string{"a"})
	if err != nil || !slices.Equal(keep, []bool{true}) {
		t.Fatalf("keep %v err %v", keep, err)
	}
	if _, err := s.Park("a", "#1"); err == nil {
		t.Error("parked without a root")
	}
}
