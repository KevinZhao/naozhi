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
	cfg.Collect = func() []Fact { return slices.Clone(r.facts) }
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
			{tick: Interval, facts: []Fact{remote(true)}},
		}, Lost, nil}, // unowned: journaled, not delivered
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
		name  string
		steps []step
		want  []fault
	}{
		{"one unit is no fault", []step{
			{facts: []Fact{auth("a"), idle("b")}},
			{tick: Interval, facts: []Fact{auth("a"), idle("b")}},
		}, nil},
		{"two units with one class merge once", []step{
			{facts: []Fact{auth("a"), auth("b")}},
			{tick: Interval, facts: []Fact{auth("a"), auth("b")}},
			{tick: Interval, facts: []Fact{auth("a"), auth("b"), auth("c")}},
			{tick: Interval, facts: []Fact{auth("a"), auth("b"), auth("c")}},
		}, []fault{{ErrAuth, 2, false}}},
		{"failures further apart than the window do not merge", []step{
			{facts: []Fact{auth("a"), idle("b")}},
			{tick: Interval, facts: []Fact{auth("a"), idle("b")}},
			{tick: FaultWindow, facts: []Fact{auth("a"), auth("b")}},
			{tick: Interval, facts: []Fact{auth("a"), auth("b")}},
		}, nil},
		{"unclassified failures never merge", []step{
			{facts: []Fact{other("a"), other("b")}},
			{tick: Interval, facts: []Fact{other("a"), other("b")}},
		}, nil},
		{"resolves once every unit recovered", []step{
			{facts: []Fact{auth("a"), auth("b")}},
			{tick: Interval, facts: []Fact{auth("a"), auth("b")}},
			{tick: Interval, facts: []Fact{idle("a"), auth("b")}},
			{tick: Interval, facts: []Fact{idle("a"), idle("b")}},
			{tick: Interval, facts: []Fact{auth("a"), auth("b")}},
			{tick: Interval, facts: []Fact{auth("a"), auth("b")}},
		}, []fault{{ErrAuth, 2, false}, {ErrAuth, 0, true}, {ErrAuth, 2, false}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t, Config{Workspaces: workspace.Load("", "node")})
			r.run(tt.steps)
			var got []fault
			for _, ev := range r.events {
				if f := ev.Fault; f != nil {
					got = append(got, fault{f.Class, len(f.Units), f.Resolved})
				}
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("faults = %v, want %v", got, tt.want)
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
	if got, want := r.transitions(), []string{"s:unknown>failed"}; !slices.Equal(got, want) {
		t.Errorf("delivered = %q, want %q (an unowned unit is only journaled)", got, want)
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
		{facts: []Fact{failed, owned(dir, Fact{ID: "sys:x"})}},
		{tick: Interval, facts: []Fact{failed, owned(dir, Fact{ID: "sys:x"})}},
	})
	if got, want := r2.transitions(), []string{"sys:x:failed>idle"}; !slices.Equal(got, want) {
		t.Errorf("after restart = %q, want %q", got, want)
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
