package health

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/workspace"
)

// fakeClock moves wall and monotonic time together on tick; sleep moves only
// the wall clock, as a machine sleep does.
type fakeClock struct {
	wall time.Time
	mono time.Duration
}

func (c *fakeClock) Now() time.Time      { return c.wall }
func (c *fakeClock) Mono() time.Duration { return c.mono }

// step is one reconcile: advance the clocks, then reconcile over facts.
type step struct {
	tick, sleep time.Duration
	facts       []Fact
}

type rig struct {
	clock  *fakeClock
	facts  []Fact
	m      *Machine
	events []Event
}

func newRig(t *testing.T, cfg Config, at ...time.Time) *rig {
	t.Helper()
	r := &rig{clock: &fakeClock{wall: time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)}}
	if len(at) > 0 {
		r.clock.wall = at[0]
	}
	cfg.Clock = r.clock
	cfg.Collect = func(time.Time) []Fact { return slices.Clone(r.facts) }
	r.m = New(cfg)
	r.m.Subscribe(func(ev Event) { r.events = append(r.events, ev) })
	return r
}

func (r *rig) run(steps []step) {
	for _, s := range steps {
		r.clock.wall = r.clock.wall.Add(s.tick + s.sleep)
		r.clock.mono += s.tick
		r.facts = s.facts
		r.m.Reconcile()
	}
}

// transitions renders the transition events as "unit:from>to".
func (r *rig) transitions() []string {
	var out []string
	for _, ev := range r.events {
		if t := ev.Transition; t != nil {
			out = append(out, t.Unit.ID+":"+string(t.From)+">"+string(t.Unit.State))
		}
	}
	return out
}

func (r *rig) state(id string) State {
	for _, u := range r.m.Snapshot().Units {
		if u.ID == id {
			return u.State
		}
	}
	return ""
}

// owned is a session fact whose workspace is set by the test rig's registry.
func owned(dir string, f Fact) Fact {
	f.Kind, f.Dir, f.Register = KindSession, dir, true
	if f.ID == "" {
		f.ID = "s"
	}
	return f
}

func TestStates(t *testing.T) {
	dir := t.TempDir()
	idle := owned(dir, Fact{})
	failed := owned(dir, Fact{Failed: "death_reason=cli_exited_code_1"})
	start := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	workflow := func(seen time.Time) Fact {
		return owned(dir, Fact{Workflow: "review", WorkflowSeen: seen})
	}
	remote := func(down bool) Fact {
		return Fact{ID: "n1/k", Kind: KindRemote, Node: "n1", NodeDown: down}
	}
	tests := []struct {
		name  string
		steps []step
		want  State
		trans []string
	}{
		{"idle on first sight is not news", []step{{facts: []Fact{idle}}}, Idle, nil},
		{"running is active", []step{{facts: []Fact{owned(dir, Fact{Running: true})}}}, Active, nil},
		{"no data is unknown", []step{{facts: []Fact{owned(dir, Fact{NoData: true})}}}, Unknown, nil},
		{"one failed reconcile is not enough", []step{
			{facts: []Fact{idle}},
			{tick: Interval, facts: []Fact{failed}},
		}, Idle, nil},
		{"two failed reconciles fail", []step{
			{facts: []Fact{idle}},
			{tick: Interval, facts: []Fact{failed}},
			{tick: Interval, facts: []Fact{failed}},
		}, Failed, []string{"s:idle>failed"}},
		{"a flap between failed and idle never fails", []step{
			{facts: []Fact{idle}},
			{tick: Interval, facts: []Fact{failed}},
			{tick: Interval, facts: []Fact{idle}},
			{tick: Interval, facts: []Fact{failed}},
		}, Idle, nil},
		{"recovery is immediate", []step{
			{facts: []Fact{failed}},
			{tick: Interval, facts: []Fact{failed}},
			{tick: Interval, facts: []Fact{idle}},
		}, Idle, []string{"s:unknown>failed", "s:failed>idle"}},
		{"unanswered question waits", []step{
			{facts: []Fact{owned(dir, Fact{Asking: true})}},
			{tick: Interval, facts: []Fact{owned(dir, Fact{Asking: true})}},
		}, Waiting, []string{"s:unknown>waiting"}},
		{"workflow observed recently is active", []step{
			{tick: 30 * time.Minute, facts: []Fact{workflow(start.Add(20 * time.Minute))}},
		}, Active, nil},
		{"workflow unobserved for an hour stalls", []step{
			{facts: []Fact{workflow(start)}},
			{tick: StallAfter, facts: []Fact{workflow(start)}},
			{tick: Interval, facts: []Fact{workflow(start)}},
		}, Stalled, []string{"s:active>stalled"}},
		{"a workflow seen before the start counts from the start", []step{
			{facts: []Fact{workflow(start.Add(-3 * time.Hour))}},
			{tick: StallAfter - 2*Interval, facts: []Fact{workflow(start.Add(-3 * time.Hour))}},
			{tick: Interval, facts: []Fact{workflow(start.Add(-3 * time.Hour))}},
		}, Active, nil},
		{"a sleep restarts the stall timer", []step{
			{facts: []Fact{workflow(start)}},
			{tick: 50 * time.Minute, facts: []Fact{workflow(start)}},
			{sleep: 2 * time.Hour, facts: []Fact{workflow(start)}},
			{tick: Interval, facts: []Fact{workflow(start)}},
			{tick: 30 * time.Minute, facts: []Fact{workflow(start)}},
		}, Active, nil},
		{"stalls an hour after the wake", []step{
			{facts: []Fact{workflow(start)}},
			{sleep: 2 * time.Hour, facts: []Fact{workflow(start)}},
			{tick: StallAfter, facts: []Fact{workflow(start)}},
			{tick: Interval, facts: []Fact{workflow(start)}},
		}, Stalled, []string{"s:active>stalled"}},
		{"a failure seen just before a sleep is debounced afresh", []step{
			{facts: []Fact{idle}},
			{tick: Interval, facts: []Fact{failed}},
			{sleep: time.Hour, facts: []Fact{failed}},
		}, Idle, nil},
		{"a node failing under a minute keeps the snapshot", []step{
			{facts: []Fact{remote(false)}},
			{tick: Interval, facts: []Fact{remote(true)}},
			{tick: 30 * time.Second, facts: []Fact{remote(true)}},
		}, Idle, nil},
		{"a node failing past a minute loses its sessions", []step{
			{facts: []Fact{remote(false)}},
			{tick: Interval, facts: []Fact{remote(true)}},
			{tick: Interval, facts: []Fact{remote(true)}},
		}, Lost, nil}, // unowned: journaled, not delivered
		{"a purged node's sessions stay lost", []step{
			{facts: []Fact{remote(false)}},
			{tick: Interval, facts: []Fact{remote(true)}},
			{tick: Interval, facts: []Fact{{Node: "n1", NodeDown: true}}},
			{tick: Interval, facts: []Fact{{Node: "n1", NodeDown: true}}},
		}, Lost, nil},
		{"a running cron job has not missed", []step{
			{facts: []Fact{owned(dir, Fact{Running: true, Missed: start.Add(time.Minute)})}},
			{tick: 2 * Interval, facts: []Fact{owned(dir, Fact{Running: true, Missed: start.Add(time.Minute)})}},
			{tick: Interval, facts: []Fact{owned(dir, Fact{Running: true, Missed: start.Add(time.Minute)})}},
		}, Active, nil},
		{"a missed cron run fails", []step{
			{facts: []Fact{owned(dir, Fact{Missed: start.Add(time.Minute)})}},
			{tick: 2 * Interval, facts: []Fact{owned(dir, Fact{Missed: start.Add(time.Minute)})}},
			{tick: Interval, facts: []Fact{owned(dir, Fact{Missed: start.Add(time.Minute)})}},
		}, Failed, []string{"s:unknown>failed"}},
		{"a cron run missed while asleep is not a failure", []step{
			{facts: []Fact{idle}},
			{sleep: 8 * time.Hour, facts: []Fact{owned(dir, Fact{Missed: start.Add(time.Hour)})}},
			{tick: Interval, facts: []Fact{owned(dir, Fact{Missed: start.Add(time.Hour)})}},
			{tick: Interval, facts: []Fact{owned(dir, Fact{Missed: start.Add(time.Hour)})}},
		}, Idle, nil},
		{"a vanished unit is dropped", []step{
			{facts: []Fact{idle}},
			{tick: Interval},
		}, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t, Config{Workspaces: workspace.Load("", "node")})
			r.run(tt.steps)
			id := "s"
			if tt.want == Lost || tt.name == "a node failing under a minute keeps the snapshot" {
				id = "n1/k"
			}
			if got := r.state(id); got != tt.want {
				t.Errorf("state = %q, want %q", got, tt.want)
			}
			if got := r.transitions(); !slices.Equal(got, tt.trans) {
				t.Errorf("transitions = %q, want %q", got, tt.trans)
			}
		})
	}
}

func TestSystemicFault(t *testing.T) {
	dir := t.TempDir()
	auth := func(id string) Fact {
		return owned(dir, Fact{ID: id, Failed: "api error", ErrText: "API Error: 403 permission_error"})
	}
	other := func(id string) Fact { return owned(dir, Fact{ID: id, Failed: "death_reason=cli_exited_code_1"}) }
	idle := func(id string) Fact { return owned(dir, Fact{ID: id}) }
	type fault struct {
		class    ErrorClass
		units    int
		resolved bool
	}
	tests := []struct {
		name   string
		steps  []step
		want   []fault
		merged int // failed transitions marked InFault
	}{
		{"one unit is no fault", []step{
			{facts: []Fact{auth("a"), idle("b")}},
			{tick: Interval, facts: []Fact{auth("a"), idle("b")}},
		}, nil, 0},
		{"failures older than the first sight do not merge", []step{
			{facts: []Fact{auth("a"), auth("b")}},
			{tick: Interval, facts: []Fact{auth("a"), auth("b")}},
		}, nil, 0},
		{"two units with one class merge once", []step{
			{facts: []Fact{idle("a"), idle("b")}},
			{tick: Interval, facts: []Fact{auth("a"), auth("b")}},
			{tick: Interval, facts: []Fact{auth("a"), auth("b")}},
			{tick: Interval, facts: []Fact{auth("a"), auth("b"), auth("c")}},
			{tick: Interval, facts: []Fact{auth("a"), auth("b"), auth("c")}},
		}, []fault{{ErrAuth, 2, false}}, 2},
		{"failures further apart than the window do not merge", []step{
			{facts: []Fact{auth("a"), idle("b")}},
			{tick: Interval, facts: []Fact{auth("a"), idle("b")}},
			{tick: FaultWindow, facts: []Fact{auth("a"), auth("b")}},
			{tick: Interval, facts: []Fact{auth("a"), auth("b")}},
		}, nil, 0},
		{"unclassified failures never merge", []step{
			{facts: []Fact{other("a"), other("b")}},
			{tick: Interval, facts: []Fact{other("a"), other("b")}},
		}, nil, 0},
		{"resolves once every unit recovered", []step{
			{facts: []Fact{idle("a"), idle("b")}},
			{tick: Interval, facts: []Fact{auth("a"), auth("b")}},
			{tick: Interval, facts: []Fact{auth("a"), auth("b")}},
			{tick: Interval, facts: []Fact{idle("a"), auth("b")}},
			{tick: Interval, facts: []Fact{idle("a"), idle("b")}},
			{tick: Interval, facts: []Fact{auth("a"), auth("b")}},
			{tick: Interval, facts: []Fact{auth("a"), auth("b")}},
		}, []fault{{ErrAuth, 2, false}, {ErrAuth, 0, true}, {ErrAuth, 2, false}}, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t, Config{Workspaces: workspace.Load("", "node")})
			r.run(tt.steps)
			var got []fault
			merged := 0
			for _, ev := range r.events {
				if f := ev.Fault; f != nil {
					got = append(got, fault{f.Class, len(f.Units), f.Resolved})
				}
				if tr := ev.Transition; tr != nil && tr.InFault {
					merged++
				}
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("faults = %v, want %v", got, tt.want)
			}
			if merged != tt.merged {
				t.Errorf("merged failures = %d, want %d", merged, tt.merged)
			}
		})
	}
}

func TestJournal(t *testing.T) {
	dir := t.TempDir()
	j, err := journal.Open(filepath.Join(dir, "log"))
	if err != nil {
		t.Fatal(err)
	}
	failed := owned(dir, Fact{Failed: "death_reason=no_output_timeout"})
	unowned := Fact{ID: "sys:x", Kind: KindSysession, Failed: "circuit breaker tripped"}

	r := newRig(t, Config{Journal: j, Workspaces: workspace.Load("", "node")})
	r.run([]step{
		{facts: []Fact{failed, unowned}},
		{tick: Interval, facts: []Fact{failed, unowned}},
	})
	if got, want := r.transitions(), []string{"s:unknown>failed", "sys:x:unknown>failed"}; !slices.Equal(got, want) {
		t.Errorf("delivered = %q, want %q (an unowned daemon still fails loudly)", got, want)
	}
	entries, err := j.Read(time.Time{}, r.clock.wall.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var units []string
	for _, e := range entries {
		var d journalData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatal(err)
		}
		units = append(units, e.Unit+":"+string(d.To))
		if e.Unit == "s" && (e.WS == "" || d.Evidence == "") {
			t.Errorf("entry %+v lacks workspace or evidence", e)
		}
	}
	slices.Sort(units)
	if want := []string{"s:failed", "sys:x:failed"}; !slices.Equal(units, want) {
		t.Errorf("journaled %q, want %q", units, want)
	}

	// A restart reports only what changed since the journal's last record.
	r2 := newRig(t, Config{Journal: j, Workspaces: workspace.Load("", "node")}, r.clock.wall.Add(time.Hour))
	r2.run([]step{
		{facts: []Fact{failed, {ID: "sys:x", Kind: KindSysession}}},
		{tick: Interval, facts: []Fact{failed, {ID: "sys:x", Kind: KindSysession}}},
	})
	if got, want := r2.transitions(), []string{"sys:x:failed>idle"}; !slices.Equal(got, want) {
		t.Errorf("after restart = %q, want %q", got, want)
	}
}

// TestFailureAcrossRestart: a session's death reason does not survive a
// restart, so a session that failed reads idle after it; it stays failed
// until something happens to it.
func TestFailureAcrossRestart(t *testing.T) {
	failedAt := time.Date(2026, 11, 4, 9, 1, 0, 0, time.UTC)
	before, after := failedAt.Add(-time.Minute), failedAt.Add(30*time.Minute)
	tests := []struct {
		name  string
		steps []Fact // one reconcile each, a minute apart
		want  State
		trans []string
	}{
		{"still idle, nothing since", []Fact{{LastActive: before}, {LastActive: before}, {LastActive: before}}, Failed, nil},
		{"resumed since", []Fact{{LastActive: after}}, Idle, []string{"s:failed>idle"}},
		{"running", []Fact{{Running: true, LastActive: after}}, Active, []string{"s:failed>active"}},
		{"held, then resumed", []Fact{{LastActive: before}, {LastActive: before}, {LastActive: after}}, Idle, []string{"s:failed>idle"}},
		{"held, then active, then idle again", []Fact{{LastActive: before}, {LastActive: before}, {Running: true}, {LastActive: before}}, Idle,
			[]string{"s:failed>active", "s:active>idle"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			j, err := journal.Open(filepath.Join(dir, "log"))
			if err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(journalData{From: Idle, To: Failed, Kind: KindSession, Evidence: "death_reason=no_output_timeout"})
			if err := j.Append(journal.Entry{TS: failedAt, Kind: journal.KindHealth, Unit: "s", Data: data}); err != nil {
				t.Fatal(err)
			}
			r := newRig(t, Config{Journal: j, Workspaces: workspace.Load("", "node")}, failedAt.Add(time.Hour))
			steps := make([]step, len(tt.steps))
			for i, f := range tt.steps {
				steps[i] = step{tick: Interval, facts: []Fact{owned(dir, f)}}
			}
			r.run(steps)
			if got := r.state("s"); got != tt.want {
				t.Errorf("state = %s, want %s", got, tt.want)
			}
			if got := r.transitions(); !slices.Equal(got, tt.trans) {
				t.Errorf("transitions = %q, want %q", got, tt.trans)
			}
			if tt.want == Failed {
				if u := r.m.Snapshot().Units[0]; !u.Since.Equal(failedAt) || u.Evidence == "" {
					t.Errorf("held unit = %+v, want the journaled failure", u)
				}
			}
		})
	}
}

// TestFirstSightAfterRestart: a unit first seen in the state the journal
// last recorded for it takes it at once; any other state still needs two
// agreeing reconciles.
func TestFirstSightAfterRestart(t *testing.T) {
	failedAt := time.Date(2026, 11, 4, 9, 1, 0, 0, time.UTC)
	failed := Fact{Failed: "death_reason=cli_exited_code_1", LastActive: failedAt.Add(-time.Minute)}
	tests := []struct {
		name     string
		recorded State
		fact     Fact
		want     []State // after each of two reconciles
		trans    []string
	}{
		{"failed, held across the restart", Failed, Fact{LastActive: failedAt.Add(-time.Minute)}, []State{Failed, Failed}, nil},
		{"failed, still failing", Failed, failed, []State{Failed, Failed}, nil},
		{"waiting, still waiting", Waiting, Fact{Asking: true}, []State{Waiting, Waiting}, nil},
		{"waiting, now failed", Waiting, failed, []State{Unknown, Failed}, []string{"s:waiting>failed"}},
		{"stalled, now waiting", Stalled, Fact{Asking: true}, []State{Unknown, Waiting}, []string{"s:stalled>waiting"}},
		{"stalled, workflow unseen since", Stalled, Fact{Workflow: "build", WorkflowSeen: failedAt.Add(-time.Hour)}, []State{Stalled, Stalled}, nil},
		{"stalled, workflow seen since", Stalled, Fact{Workflow: "build", WorkflowSeen: failedAt.Add(30 * time.Minute)}, []State{Active, Active},
			[]string{"s:stalled>active"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			j, err := journal.Open(filepath.Join(dir, "log"))
			if err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(journalData{From: Idle, To: tt.recorded, Kind: KindSession, Evidence: "recorded"})
			if err := j.Append(journal.Entry{TS: failedAt, Kind: journal.KindHealth, Unit: "s", Data: data}); err != nil {
				t.Fatal(err)
			}
			r := newRig(t, Config{Journal: j, Workspaces: workspace.Load("", "node")}, failedAt.Add(time.Hour))
			for i, want := range tt.want {
				r.run([]step{{tick: Interval, facts: []Fact{owned(dir, tt.fact)}}})
				if got := r.state("s"); got != want {
					t.Errorf("reconcile %d: state = %s, want %s", i+1, got, want)
				}
			}
			if got := r.transitions(); !slices.Equal(got, tt.trans) {
				t.Errorf("transitions = %q, want %q", got, tt.trans)
			}
		})
	}
}

func TestFaultAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	j, err := journal.Open(filepath.Join(dir, "log"))
	if err != nil {
		t.Fatal(err)
	}
	auth := func(id string) Fact {
		return owned(dir, Fact{ID: id, Failed: "api error", ErrText: "API Error: 403 permission_error"})
	}
	faults := func(r *rig) (out []bool) {
		for _, ev := range r.events {
			if ev.Fault != nil {
				out = append(out, ev.Fault.Resolved)
			}
		}
		return out
	}
	cfg := Config{Journal: j, Workspaces: workspace.Load("", "node")}
	r := newRig(t, cfg)
	r.run([]step{
		{facts: []Fact{owned(dir, Fact{ID: "a"}), owned(dir, Fact{ID: "b"})}},
		{tick: Interval, facts: []Fact{auth("a"), auth("b")}},
		{tick: Interval, facts: []Fact{auth("a"), auth("b")}},
	})
	if got := faults(r); !slices.Equal(got, []bool{false}) {
		t.Fatalf("before restart: faults = %v", got)
	}

	// Restarted within the window: the open fault is neither reported again
	// nor resolved while its units settle; it resolves once they recover.
	r2 := newRig(t, cfg, r.clock.wall.Add(2*time.Minute))
	r2.run([]step{
		{facts: []Fact{auth("a"), auth("b")}},
		{tick: Interval, facts: []Fact{auth("a"), auth("b")}},
		{tick: Interval, facts: []Fact{auth("a"), auth("b")}},
	})
	if got := faults(r2); len(got) != 0 {
		t.Errorf("after restart: faults = %v, want none", got)
	}
	r2.run([]step{{tick: Interval, facts: []Fact{owned(dir, Fact{ID: "a"}), owned(dir, Fact{ID: "b"})}}})
	if got := faults(r2); !slices.Equal(got, []bool{true}) {
		t.Errorf("after recovery: faults = %v, want one resolution", got)
	}
}

func TestIndex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "planner", "index.json")
	reg := workspace.Load("", "node")
	r := newRig(t, Config{IndexPath: path, Workspaces: reg})
	r.run([]step{{facts: []Fact{
		owned(dir, Fact{ID: "b", Running: true, Index: &IndexEntry{Key: "b", Name: "fix-ci", Title: "Fix CI", Result: strings.Repeat("x", 400)}}),
		owned(dir, Fact{ID: "a", Index: &IndexEntry{Key: "a", EventLog: "/x/a.log"}}),
		{ID: "cron:j", Kind: KindCron},
	}}})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f indexFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	ws, _ := reg.Lookup(dir)
	if f.V != IndexVersion || len(f.Sessions) != 2 {
		t.Fatalf("index = %s", data)
	}
	a, b := f.Sessions[0], f.Sessions[1]
	if a.Key != "a" || a.Health != Idle || a.Workspace != ws.ID || a.EventLog != "/x/a.log" || a.DispatchedByPlanner {
		t.Errorf("row a = %+v", a)
	}
	if b.Key != "b" || b.Health != Active || b.Name != "fix-ci" || b.Title != "Fix CI" || len(b.Result) >= 400 {
		t.Errorf("row b = %+v", b)
	}
}

// TestRemoteWorkspace: a remote session belongs to the workspace registered
// for its directory on its node; the same path on another node, or here, is
// someone else's, and an unregistered remote directory stays unowned.
func TestRemoteWorkspace(t *testing.T) {
	dir := t.TempDir()
	regPath := filepath.Join(dir, "workspaces.json")
	data, _ := json.Marshal(map[string]any{"v": workspace.Version, "workspaces": []workspace.Workspace{
		{ID: "ws_app00001", Name: "app", Paths: map[string][]string{"mini": {"/srv/app"}}},
	}})
	if err := os.WriteFile(regPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	reg := workspace.Load(regPath, "mac")
	idx := filepath.Join(dir, "index.json")
	r := newRig(t, Config{Workspaces: reg, IndexPath: idx})
	remote := func(node, key, dir string) Fact {
		return Fact{ID: node + "/" + key, Kind: KindRemote, Node: node, Dir: dir,
			Index: &IndexEntry{Key: key, Node: node}}
	}
	facts := []Fact{
		remote("mini", "a", "/srv/app/x"),
		remote("box", "a", "/srv/app/x"), // after mini's hit is cached
		remote("mini", "b", "/elsewhere"),
		{ID: "s", Kind: KindSession, Dir: "/srv/app/x"},
		// naozhi's own units belong to no workspace and are not unowned.
		{ID: "planner:main", Kind: KindSession},
		{ID: "sys:scout", Kind: KindSysession},
	}
	r.run([]step{{facts: facts}, {tick: Interval, facts: facts}})

	want := map[string]string{"mini/a": "ws_app00001", "box/a": "", "mini/b": "", "s": "", "planner:main": "", "sys:scout": ""}
	snap := r.m.Snapshot()
	for _, u := range snap.Units {
		if u.Workspace != want[u.ID] {
			t.Errorf("unit %s workspace = %q, want %q", u.ID, u.Workspace, want[u.ID])
		}
	}
	if snap.Unowned != 3 {
		t.Errorf("unowned = %d, want 3", snap.Unowned)
	}
	if n := len(reg.List()); n != 1 {
		t.Errorf("registry has %d workspaces, want 1 (remote paths are not registered)", n)
	}
	raw, err := os.ReadFile(idx)
	if err != nil {
		t.Fatal(err)
	}
	var f indexFile
	if err := json.Unmarshal(raw, &f); err != nil || len(f.Sessions) != 3 {
		t.Fatalf("index = %s (err %v)", raw, err)
	}
	if row := f.Sessions[1]; row.Node != "mini" || row.Key != "a" || row.Workspace != "ws_app00001" {
		t.Errorf("index row = %+v, want mini/a in ws_app00001", row)
	}

	// mini goes down and its sessions are purged: mini/a stays in app, in
	// the snapshot and in index.json, also after an index refresh.
	r.run([]step{{tick: Interval, facts: []Fact{{Node: "mini", NodeDown: true}}}})
	r.m.RefreshIndex()
	snap = r.m.Snapshot()
	if len(snap.Units) != 2 || snap.Units[0].ID != "mini/a" || snap.Units[0].Workspace != "ws_app00001" || snap.Unowned != 1 {
		t.Errorf("after mini went down: units %+v, unowned %d", snap.Units, snap.Unowned)
	}
	if raw, err = os.ReadFile(idx); err != nil {
		t.Fatal(err)
	}
	f = indexFile{}
	if err := json.Unmarshal(raw, &f); err != nil || len(f.Sessions) != 2 {
		t.Fatalf("index after mini went down = %s (err %v)", raw, err)
	}
	if row := f.Sessions[0]; row.Node != "mini" || row.Key != "a" || row.Workspace != "ws_app00001" {
		t.Errorf("index row after mini went down = %+v, want mini/a in ws_app00001", row)
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		text string
		want ErrorClass
	}{
		{"", ErrOther},
		{`API Error: 403 {"type":"error","error":{"type":"permission_error"}}`, ErrAuth},
		{"API Error: 401 invalid_api_key", ErrAuth},
		{"User: arn:aws:sts::1:assumed-role/x is not authorized to perform: bedrock:InvokeModel", ErrAuth},
		{`API Error: 400 Unsupported countries, regions, or territories`, ErrRegion},
		{"API Error: 429 rate_limit_error", ErrRateLimit},
		{"ThrottlingException: Too many requests", ErrRateLimit},
		{"dial tcp: lookup bedrock-runtime.us-east-1.amazonaws.com: no such host", ErrNetwork},
		{"dial tcp 10.0.0.5:4013: connect: connection refused", ErrNetwork},
		{"ExpiredTokenException: The security token included in the request is expired", ErrCredentialExpired},
		{"Midway credentials expired; run mwinit", ErrCredentialExpired},
		{"write /tmp/x: no space left on device", ErrDiskFull},
		{"system/model_refusal_no_fallback", ErrRefusal},
		{"API Error: 529 overloaded_error", ErrOther},
		{"exit status 1", ErrOther},
	}
	for _, tt := range tests {
		if got := Classify(tt.text); got != tt.want {
			t.Errorf("Classify(%q) = %q, want %q", tt.text, got, tt.want)
		}
	}
}
