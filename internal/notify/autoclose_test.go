package notify

import (
	"context"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/monitor/health"
)

// A failure's item closes itself once its unit is idle again after it
// opened: a turn ended normally since. Anything short of that keeps it.
func TestFailureClosesOnRecovery(t *testing.T) {
	opened := local(10, 0)
	tests := []struct {
		name  string
		units []health.Unit
		at    time.Time // the snapshot's; zero before the first reconcile
		open  bool
	}{
		{"idle since after it opened", []health.Unit{{ID: "a", State: health.Idle, Since: opened.Add(2 * time.Minute)}}, opened.Add(3 * time.Minute), false},
		{"idle since before it opened", []health.Unit{{ID: "a", State: health.Idle, Since: opened.Add(-time.Minute)}}, opened.Add(3 * time.Minute), true},
		{"running again", []health.Unit{{ID: "a", State: health.Active, Since: opened.Add(2 * time.Minute)}}, opened.Add(3 * time.Minute), true},
		{"still failed", []health.Unit{{ID: "a", State: health.Failed, Since: opened}}, opened.Add(3 * time.Minute), true},
		{"gone from the table", nil, opened.Add(3 * time.Minute), true},
		{"not reconciled yet", []health.Unit{{ID: "a", State: health.Idle, Since: opened.Add(2 * time.Minute)}}, time.Time{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, clock, _ := newTest(t, opened, datadir.Layout{})
			h := &fakeHealth{}
			n.SetHealth(h)
			n.HandleHealth(health.Event{At: opened, Transition: &health.Transition{From: health.Idle,
				Unit: health.Unit{ID: "a", Workspace: "ws_1", State: health.Failed, ErrClass: health.ErrNetwork, Since: opened}}})
			clock.advance(3 * time.Minute)
			h.snap = health.Snapshot{At: tt.at, Units: tt.units}
			n.Check()
			open, closed := n.List(10)
			if got := len(open) == 1; got != tt.open {
				t.Fatalf("open = %+v, want open %v", open, tt.open)
			}
			if !tt.open && (len(closed) != 1 || closed[0].ClosedBy != ClosedByAuto) {
				t.Errorf("closed = %+v, want it closed automatically", closed)
			}
		})
	}
}

// A background task done is a notice for the user's own sessions while no
// one is at the session; opening the session, or the session going away,
// closes it.
func TestTaskDone(t *testing.T) {
	done := func(id string, kind health.Kind) health.Event {
		return health.Event{TaskDone: &health.TaskDone{Unit: health.Unit{ID: id, Kind: kind, Workspace: "ws_1", State: health.Idle},
			Task: "sleep 300", Reply: "跑完了：3 个文件"}}
	}
	tests := []struct {
		name    string
		ev      health.Event
		viewing bool
		want    string // the pushed title, "" for none
	}{
		{"a direct chat", done("dashboard:direct:a:general", health.KindSession), false, "dashboard:direct:a:general 后台任务跑完了"},
		{"the user is at it", done("dashboard:direct:a:general", health.KindSession), true, ""},
		{"the Planner's", done("planner:main", health.KindSession), false, ""},
		{"a cron run", done("cron:nightly", health.KindCron), false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, _, b := newTest(t, local(10, 0), datadir.Layout{})
			key := tt.ev.TaskDone.Unit.ID
			if tt.viewing {
				n.SetViewing(key, "ws1", true)
			}
			n.HandleHealth(tt.ev)
			titles := b.titles()
			if got := ""; len(titles) == 1 {
				got = titles[0]
				if got != tt.want {
					t.Errorf("pushed %q, want %q", got, tt.want)
				}
			} else if len(titles) != 0 || tt.want != "" {
				t.Errorf("pushed %q, want %q", titles, tt.want)
			}
			if tt.want != "" && b.pushes[0].Body != "跑完了：3 个文件" {
				t.Errorf("body = %q, want the reply's first line", b.pushes[0].Body)
			}
		})
	}

	// Opening the session closes it; so does the session going away or
	// starting a turn since; one left idle keeps it.
	n, clock, _ := newTest(t, local(10, 0), datadir.Layout{})
	h := &fakeHealth{}
	n.SetHealth(h)
	for _, id := range []string{"a", "b", "c", "d"} {
		n.HandleHealth(done(id, health.KindSession))
	}
	n.SetViewing("a", "ws1", true)
	clock.advance(time.Minute)
	h.snap = health.Snapshot{At: clock.Now(), Units: []health.Unit{{ID: "a", State: health.Idle},
		{ID: "c", State: health.Active, Since: clock.Now()}, {ID: "d", State: health.Idle, Since: clock.Now()}}}
	n.Check()
	if open, closed := n.List(10); len(open) != 1 || open[0].Unit != "d" || len(closed) != 3 || closed[0].ClosedBy != ClosedByAuto {
		t.Errorf("open %+v, closed %+v; want all but d's closed automatically", open, closed)
	}
	n.Dismiss(func() string { open, _ := n.List(0); return open[0].ID }())
	// A viewing report lapses: a tab that died without saying so does not
	// keep the user "at" the session.
	clock.advance(viewFor)
	n.HandleHealth(done("a", health.KindSession))
	if open, _ := n.List(0); len(open) != 1 {
		t.Errorf("open = %+v, want the done item once the view lapsed", open)
	}
	n.DropViewer("ws1")
	if len(n.viewers) != 0 {
		t.Errorf("viewers = %v, want none after the connection closed", n.viewers)
	}
}

// A start whose version differs from the last start's announces the
// upgrade once: a notice closed after UpgradeFor while every session found
// in a shim was taken back, else an error.
func TestUpgrade(t *testing.T) {
	tests := []struct {
		name      string
		prev      string
		start     Start
		wantTitle string
		wantWhy   string
		wantKind  Kind
	}{
		{"first start", "", Start{Version: "v0.5.1"}, "", "", ""},
		{"same version", "v0.5.1", Start{Version: "v0.5.1", Shims: 2, Reattached: 2}, "", "", ""},
		{"a dev build", "v0.5.1", Start{Version: "dev"}, "", "", ""},
		{"upgraded", "v0.5.0", Start{Version: "v0.5.1", Shims: 3, Reattached: 3}, "已升级到 v0.5.1（来自 v0.5.0）", "会话接回 3/3", KindNotice},
		{"upgraded with no session running", "v0.5.0", Start{Version: "v0.5.1"}, "已升级到 v0.5.1（来自 v0.5.0）", "", KindNotice},
		{"a session lost", "v0.5.0", Start{Version: "v0.5.1", Shims: 3, Reattached: 2}, "已升级到 v0.5.1（来自 v0.5.0）", "会话接回 2/3，没接回的会话要重新发消息才会继续", KindError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lay := datadir.FromRoot(t.TempDir())
			if err := datadir.EnsureDir(lay.PlannerRoot()); err != nil {
				t.Fatal(err)
			}
			clock := &fakeClock{now: local(10, 0)}
			cfg := Config{Clock: clock, Layout: lay, System: func(string, string) error { return nil },
				DiskFree: func() (float64, error) { return 1, nil }}
			if tt.prev != "" {
				cfg.Start = Start{Version: tt.prev}
				New(cfg).Close()
			}
			cfg.Start = tt.start
			n := New(cfg)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			n.Run(ctx)
			open, _ := n.List(0)
			if tt.wantTitle == "" {
				if len(open) != 0 {
					t.Fatalf("open = %+v, want none", open)
				}
				return
			}
			if len(open) != 1 || open[0].Title != tt.wantTitle || open[0].Why != tt.wantWhy || open[0].Kind != tt.wantKind {
				t.Fatalf("open = %+v, want %q %q (%s)", open, tt.wantTitle, tt.wantWhy, tt.wantKind)
			}
			// Announced once: the next start of the same version says nothing.
			n.Close()
			if again, _ := New(cfg).List(0); len(again) != 1 {
				t.Errorf("after another start: %d open, want still the one", len(again))
			}
			clock.advance(UpgradeFor)
			n.Check()
			open, _ = n.List(0)
			if wantOpen := tt.wantKind == KindError; (len(open) == 1) != wantOpen {
				t.Errorf("after %v: open = %+v, want open %v", UpgradeFor, open, wantOpen)
			}
		})
	}

	// A later upgrade closes an earlier one's notice.
	n := New(Config{Clock: &fakeClock{now: local(10, 0)}, Start: Start{Version: "v0.5.2"}, System: func(string, string) error { return nil }})
	n.Add(Item{Kind: KindNotice, Source: upgradePrefix + "v0.5.1", AutoClose: true, Title: "已升级到 v0.5.1（来自 v0.5.0）"})
	n.Check()
	if open, _ := n.List(0); len(open) != 0 {
		t.Errorf("open = %+v, want the earlier upgrade's notice closed", open)
	}
}

// Carried closes the notices the brief listed, and nothing else.
func TestCarried(t *testing.T) {
	n, _, _ := newTest(t, local(10, 0), datadir.Layout{})
	notice, _ := n.Add(Item{Kind: KindNotice, Source: "planner:x", Title: "巡检发现"})
	errItem, _ := n.Add(Item{Kind: KindError, Source: "failed:a", Title: "a 失败"})
	n.Carried([]string{notice.ID, errItem.ID, "in_missing"})
	open, closed := n.List(10)
	if len(open) != 1 || open[0].ID != errItem.ID || len(closed) != 1 || closed[0].ClosedBy != ClosedByAuto {
		t.Errorf("open %+v, closed %+v; want only the notice closed, automatically", open, closed)
	}
}
