package task

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
)

// yoursRig is a board over a dispatched session and the units of the
// health table: the user's own sessions (watched or not), a cron run and a
// remote node's session.
type yoursRig struct {
	b        *Board
	sc       *sidecar.Store
	now      time.Time
	units    []health.Unit
	off      map[string]bool
	lastUser map[string]time.Time
	live     map[string]Live
	states   []string // OnState calls
}

const (
	yDash = "dashboard:direct:u:general"
	yIM   = "feishu:group:oc_1#t9:general"
	ySlk  = "slack:direct:u1:general"
	yProj = "project:naozhi:planner"
	yOff  = "dashboard:direct:off:general"
)

func newYoursRig(t *testing.T) *yoursRig {
	t.Helper()
	r := &yoursRig{now: t0, off: map[string]bool{yOff: true}, lastUser: map[string]time.Time{},
		live: map[string]Live{yDash: {Running: true, Activity: "Bash  go\ntest", LastActive: t0.Add(-time.Minute)}}}
	clock := func() time.Time { return r.now }
	r.sc = sidecar.Open(t.TempDir(), clock)
	if err := r.sc.Put(sidecar.Meta{Key: "task:aaaaaaaaaaaa", Purpose: sidecar.PurposeTask, Title: "派出的", Created: t0.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	r.units = []health.Unit{
		{ID: "task:aaaaaaaaaaaa", Kind: health.KindSession, State: health.Active, Since: t0.Add(-time.Hour)},
		{ID: yDash, Kind: health.KindSession, State: health.Active, Since: t0.Add(-5 * time.Minute)},
		{ID: yIM, Kind: health.KindSession, State: health.Waiting, Since: t0.Add(-10 * time.Minute)},
		{ID: ySlk, Kind: health.KindSession, State: health.Failed, Evidence: "death_reason=shim_eof", Since: t0.Add(-20 * time.Minute)},
		{ID: yProj, Kind: health.KindSession, State: health.Idle, Since: t0.Add(-25 * time.Hour)},
		{ID: yOff, Kind: health.KindSession, State: health.Idle, Since: t0.Add(-time.Hour)},
		{ID: "cron:nightly", Kind: health.KindCron, State: health.Idle, Since: t0},
		{ID: "dashboard:direct:r:general", Kind: health.KindRemote, State: health.Active, Since: t0},
	}
	r.b = New(Config{
		Sessions: r.sc,
		Now:      clock,
		Health:   func() health.Snapshot { return health.Snapshot{At: r.now, Units: r.units} },
		Watched: func(key string) bool {
			// custody's: this node's sessions the user opened, switch on.
			return key != "cron:nightly" && key != "dashboard:direct:r:general" && !r.off[key]
		},
		Name:      func(key string) string { return "名字 " + key },
		UserSince: func(key string, at time.Time) bool { return r.lastUser[key].After(at) },
		Live: func() map[string]Live {
			out := map[string]Live{}
			for k, l := range r.live {
				out[k] = l
			}
			return out
		},
		OnState: func(key string, st State) { r.states = append(r.states, key+"="+string(st)) },
	})
	return r
}

func (r *yoursRig) yours() map[string]Task {
	out := map[string]Task{}
	for _, t := range r.b.List() {
		if t.Group == GroupYours {
			out[t.ID] = t
		}
	}
	return out
}

// 你开的 lists the watched sessions from the health table, states by
// health; not the unwatched, a cron run, a remote one or a dispatched one,
// and not one idle a day.
func TestYoursGroup(t *testing.T) {
	r := newYoursRig(t)
	got := r.yours()
	ids := make([]string, 0, len(got))
	for id := range got {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if want := []string{yDash, yIM, ySlk}; !slices.Equal(ids, want) {
		t.Fatalf("你开的 = %v, want %v", ids, want)
	}
	if d := got[yDash]; d.State != Doing || d.Kind != KindSession || d.Title != "名字 "+yDash || d.Health != health.Active ||
		d.Live == nil || !d.Live.Running || d.Live.Activity != "Bash go test" || d.New {
		t.Errorf("dash = %+v", d)
	}
	if w := got[yIM]; w.State != Waiting || w.By != ByHealth {
		t.Errorf("IM = %+v, want 等你 by health", w)
	}
	if f := got[ySlk]; f.State != Failed || f.Why != "death_reason=shim_eof" || !f.New {
		t.Errorf("slack = %+v, want 失败 with why, new", f)
	}
	// The dispatched one is still the Planner's, with no group.
	for _, tk := range r.b.List() {
		if tk.ID == "task:aaaaaaaaaaaa" && tk.Group != "" {
			t.Errorf("dispatched session's group = %q", tk.Group)
		}
	}
	// Idle under a day it is listed.
	r.units[4].Since = t0.Add(-23 * time.Hour)
	if _, ok := r.yours()[yProj]; !ok {
		t.Error("a session idle 23 hours is not listed")
	}
	// Idle a day with a background task open is 后台中, still listed;
	// the card has the tasks from Live.
	r.units[4].Since = t0.Add(-25 * time.Hour)
	r.live[yProj] = Live{Background: &health.IndexBackground{Count: 1, Tasks: []health.IndexTask{{ID: "b1", Summary: "ssh bench ./run.sh"}}}}
	if p, ok := r.yours()[yProj]; !ok || p.Live == nil || p.Live.Background == nil || p.Live.Background.Count != 1 {
		t.Errorf("a session idle a day with its benchmark running = %+v, %v; want listed with it", p, ok)
	}
	delete(r.live, yProj)
	// Listing one writes nothing.
	if _, ok := r.sc.Get(yDash); ok {
		t.Error("listing gave a watched session a sidecar")
	}
	// The title and the status block reach it.
	if title, ok := r.b.Title(yDash); !ok || title != "名字 "+yDash {
		t.Errorf("Title = %q, %v", title, ok)
	}
	if _, ok := r.b.Title(yOff); ok {
		t.Error("Title found an unwatched session")
	}
	if err := r.b.Set(yOff, Done, ByPlanner, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("Set(unwatched) = %v, want ErrNotFound", err)
	}
	// 停止 is a dispatched session's only.
	if err := r.b.Stop(yDash); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stop(yours) = %v, want ErrNotFound", err)
	}
}

// A watched session marked done is 已完成 until the user sends it another
// message; it leaves the list a day after it was done. Each mark is told on
// (OnState): planner/reap puts the session away on the user's own (D137),
// and leaves it alone on the Planner's.
func TestYoursMarkThenTalk(t *testing.T) {
	r := newYoursRig(t)
	r.lastUser[yDash] = t0.Add(-10 * time.Minute)
	if err := r.b.Set(yDash, Done, ByPlanner, "PR 合了"); err != nil {
		t.Fatal(err)
	}
	m, ok := r.sc.Get(yDash)
	if !ok || m.Purpose != "" || m.Mark == nil || m.Mark.State != string(Done) {
		t.Fatalf("sidecar = %+v (%v), want a mark, purpose left empty", m, ok)
	}
	if !slices.Equal(r.states, []string{yDash + "=done"}) {
		t.Errorf("OnState = %v, want the mark told on", r.states)
	}
	if !r.b.YoursDone(m) {
		t.Error("YoursDone false for a mark that holds")
	}
	d := r.yours()[yDash]
	if d.State != Done || d.By != ByPlanner || d.Why != "PR 合了" || !d.New {
		t.Errorf("after the mark: %+v, want 已完成 by the Planner, new", d)
	}

	// The user talks to it again: in progress.
	r.now = t0.Add(time.Hour)
	r.lastUser[yDash] = r.now
	if d := r.yours()[yDash]; d.State != Doing {
		t.Errorf("talked to after the mark: %s, want doing", d.State)
	}
	if r.b.YoursDone(m) {
		t.Error("YoursDone true for a mark the user talked past: the session would be put away under them")
	}

	// Marked done again, then a day later it is archived.
	if err := r.b.Set(yDash, Done, ByUser, ""); err != nil {
		t.Fatal(err)
	}
	if d := r.yours()[yDash]; d.State != Done || d.New {
		t.Errorf("the user's mark: %+v, want 已完成, no new result", d)
	}
	r.now = r.now.Add(Archive - time.Minute)
	if _, ok := r.yours()[yDash]; !ok {
		t.Error("done under a day: not listed")
	}
	r.now = r.now.Add(2 * time.Minute)
	if _, ok := r.yours()[yDash]; ok {
		t.Error("done a day ago: still listed")
	}
	if !slices.Equal(r.states, []string{yDash + "=done", yDash + "=done"}) {
		t.Errorf("OnState = %v", r.states)
	}
}

// A session the user opened and marked done themselves is put away
// (planner/reap, D137): out of the health table, its card stays in 已完成
// from its sidecar, named as it was, and marking it in progress again is
// told on, which reopens it; a day on it is archived.
func TestYoursPutAwayStaysListed(t *testing.T) {
	r := newYoursRig(t)
	if err := r.b.Set(yDash, Done, ByUser, ""); err != nil {
		t.Fatal(err)
	}
	// planner/reap put it away: the record on its sidecar, the session gone.
	if _, err := r.sc.Update(yDash, func(m *sidecar.Meta) bool {
		m.Reap = &sidecar.Reap{At: r.now, By: string(ByUser), SessionID: "sid-1", LastPrompt: "分析 TGW 迁移\n第二行"}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	r.units = slices.DeleteFunc(r.units, func(u health.Unit) bool { return u.ID == yDash })
	r.off[yDash] = true // no longer eligible: not in the router

	d, ok := r.yours()[yDash]
	if !ok || d.State != Done || d.By != ByUser || d.Reap == nil || d.Group != GroupYours || d.Title != "分析 TGW 迁移" {
		t.Fatalf("put away: %+v (listed %v)", d, ok)
	}
	if title, ok := r.b.Title(yDash); !ok || title != "分析 TGW 迁移" {
		t.Errorf("Title = %q, %v", title, ok)
	}
	// Reopened from the card.
	r.states = nil
	if err := r.b.Set(yDash, Doing, ByUser, ""); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !slices.Equal(r.states, []string{yDash + "=doing"}) {
		t.Errorf("OnState = %v, want the reopening told on", r.states)
	}

	// Left put away instead (the mark as it was): archived a day after.
	if _, err := r.sc.Update(yDash, func(m *sidecar.Meta) bool {
		m.Mark = &sidecar.Mark{State: string(Done), By: string(ByUser), At: r.now}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(Archive - time.Minute)
	if _, ok := r.yours()[yDash]; !ok {
		t.Error("put away under a day: not listed")
	}
	r.now = r.now.Add(2 * time.Minute)
	if _, ok := r.yours()[yDash]; ok {
		t.Error("put away a day ago: still listed")
	}
}

// A session the user opened, marked done, that custody or a relay keeps
// going (no message of the user's, so the mark holds) stays listed a day
// past its last activity, not the mark.
func TestYoursDoneArchivedByActivity(t *testing.T) {
	r := newYoursRig(t)
	if err := r.b.Set(yDash, Done, ByPlanner, "PR 合了"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.b.Seen([]string{yDash}, false); err != nil { // looked at
		t.Fatal(err)
	}
	r.live[yDash] = Live{LastActive: t0.Add(20 * time.Hour)}
	r.now = t0.Add(Archive + time.Minute)
	if d, ok := r.yours()[yDash]; !ok || d.State != Done {
		t.Errorf("a day after the mark, active since: %+v (%v), want still listed done", d, ok)
	}
	r.now = t0.Add(20*time.Hour + Archive)
	if _, ok := r.yours()[yDash]; ok {
		t.Error("a day after its last activity: still listed")
	}
}

// A watched session's turn that ended with the user away (ResultAt) and a
// failure are new until seen: opening it or Seen marks them, giving a
// session with no sidecar one; its failures stay health's (Observe writes
// none).
func TestYoursSeen(t *testing.T) {
	r := newYoursRig(t)
	if err := r.sc.Upsert(yDash, func(m *sidecar.Meta) bool { m.ResultAt = t0.Add(-time.Minute); return true }); err != nil {
		t.Fatal(err)
	}
	got := r.yours()
	if d := got[yDash]; !d.New || !d.ResultAt.Equal(t0.Add(-time.Minute)) {
		t.Fatalf("dash = %+v, want its result new", d)
	}
	r.b.SetViewing(yDash, "ws1", true)
	if d := r.yours()[yDash]; d.New {
		t.Error("opened, still new")
	}
	if n, err := r.b.Seen([]string{ySlk, yOff}, false); n != 1 || err != nil {
		t.Fatalf("Seen = %d, %v; want the failed one marked", n, err)
	}
	if m, ok := r.sc.Get(ySlk); !ok || !m.SeenAt.Equal(r.now) {
		t.Errorf("slack sidecar = %+v (%v)", m, ok)
	}
	if _, ok := r.sc.Get(yOff); ok {
		t.Error("an unwatched session got a sidecar")
	}
	// Viewing one with nothing new writes nothing.
	r.b.SetViewing(yIM, "ws1", true)
	if _, ok := r.sc.Get(yIM); ok {
		t.Error("viewing with nothing new gave a sidecar")
	}
	r.b.Observe(health.Event{Transition: &health.Transition{Unit: r.units[3]}})
	if m, _ := r.sc.Get(ySlk); m.Failure != nil {
		t.Errorf("Observe wrote a failure on a watched session: %+v", m.Failure)
	}
}
