// Package health is the gateway's session ledger and health state machine
// (docs/rfc/planner.md §3.8, §15.1). Every Interval it derives the table of
// execution units from the sources naozhi already has (sessions, remote node
// caches, terminal discovery, cron, sysession daemons, workflow boards),
// judges each unit's health by fixed rules and reports the transitions: to
// the Planner journal, to subscribers and in Snapshot. It holds no state of
// its own beyond the current table; nothing here calls a model.
//
// The rules are deliberately conservative: a state that would need a person
// is entered only after two reconciles agree, a unit without enough data is
// Unknown, and a machine sleep restarts every timer.
package health

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/monitor/beat"
	"github.com/naozhi/naozhi/internal/monitor/wake"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/workspace"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// Timing rules (§15.1).
const (
	// Interval is the reconcile period and the "health" loop's beat period.
	Interval = time.Minute
	// StallAfter is how long a running workflow or background task may go
	// unobserved before its unit is Stalled.
	StallAfter = 60 * time.Minute
	// WakeGrace is how long after a wake nothing is judged Stalled.
	WakeGrace = 5 * time.Minute
	// LostAfter is how long a remote node's polls must fail before its
	// sessions are Lost.
	LostAfter = time.Minute
	// FaultWindow is the window in which units failing with one error class
	// merge into a systemic fault; FaultMin is how many it takes.
	FaultWindow = 5 * time.Minute
	FaultMin    = 2
)

// seedLookback is how far back New reads the journal for each unit's last
// recorded state, so a restart does not report a state again.
const seedLookback = 7 * 24 * time.Hour

// indexDebounce coalesces the session-change bursts of one turn's end into
// one index.json refresh.
const indexDebounce = 2 * time.Second

// State is a unit's health. Values are wire-stable (journal, index.json).
type State string

const (
	Active  State = "active"
	Idle    State = "idle"
	Waiting State = "waiting" // an unanswered AskUserQuestion
	Stalled State = "stalled"
	Failed  State = "failed"
	Lost    State = "lost"
	Unknown State = "unknown"
)

// healthy states are entered at once; the others need two agreeing
// reconciles.
func (s State) healthy() bool { return s == Active || s == Idle }

// Kind is what sort of execution unit a unit is.
type Kind string

const (
	KindSession   Kind = "session"   // a local dashboard / IM / project session
	KindRemote    Kind = "remote"    // a session on a remote node
	KindExternal  Kind = "external"  // a CLI session found running in a terminal
	KindCron      Kind = "cron"      // a cron job
	KindSysession Kind = "sysession" // a background daemon
)

// Coverage is how much of a unit the gateway can see.
type Coverage string

const (
	CoverageFull     Coverage = "full"     // turn events, results, deaths, runs
	CoverageGateway  Coverage = "gateway"  // only what the gateway's own rules see
	CoverageSnapshot Coverage = "snapshot" // only a periodic snapshot
)

// Fact is what the sources observed about one unit at one reconcile. Facts
// with the same ID (a cron job and its cron: session) are merged.
type Fact struct {
	ID       string
	Kind     Kind
	Source   string
	Coverage Coverage
	// Dir is the unit's working directory ("" when it has none), on Node's
	// machine for a remote unit; Register lets it be registered as a new
	// workspace when no workspace holds it.
	Dir      string
	Register bool
	// Node is a remote unit's node; NodeDown is set while its polls fail.
	// A fact with no ID only reports its Node down: the node's sessions are
	// no longer listed (a reverse node is purged after its grace), and the
	// units it had are kept and judged Lost.
	Node     string
	NodeDown bool

	NoData  bool
	Running bool
	Asking  bool
	// Failed is the failure's evidence ("" when the unit has not failed);
	// ErrText the text to classify it by, never stored, unless the source
	// already knows the class (ErrClass).
	Failed   string
	ErrText  string
	ErrClass ErrorClass
	// Missed is when a cron job's last scheduled run should have happened
	// and did not; a miss before the last wake or start is the machine's
	// sleep, not the job's failure, and a job still running has not missed.
	Missed time.Time
	// Workflow names a running workflow or background task (a Bash
	// run_in_background, an Agent subtask); WorkflowSeen is its latest
	// observation.
	Workflow     string
	WorkflowSeen time.Time
	LastActive   time.Time

	// Index is the unit's index.json row, set for local and remote
	// sessions; the machine fills in Workspace and Health.
	Index *IndexEntry
}

// Unit is one unit's current health.
type Unit struct {
	ID         string     `json:"id"`
	Kind       Kind       `json:"kind"`
	Workspace  string     `json:"workspace,omitempty"` // workspace ID, "" when it belongs to none
	Source     string     `json:"source"`
	Coverage   Coverage   `json:"coverage"`
	State      State      `json:"state"`
	Evidence   string     `json:"evidence,omitempty"`
	ErrClass   ErrorClass `json:"error_class,omitempty"`
	Since      time.Time  `json:"since"`
	LastActive time.Time  `json:"last_active,omitzero"`
}

// Snapshot is the whole table at the last reconcile. Unowned counts the
// units without a workspace (§3.8: counted, not reported).
type Snapshot struct {
	At      time.Time `json:"at"`
	Units   []Unit    `json:"units"`
	Unowned int       `json:"unowned"`
}

// Event is one report to subscribers: a transition or a systemic fault.
type Event struct {
	At         time.Time
	Transition *Transition
	Fault      *Fault
}

// Transition is a unit entering a new state; Unit is the unit after it.
type Transition struct {
	From State
	Unit Unit
}

// Fault is FaultMin or more units failing with one error class within
// FaultWindow, reported once until no unit has that class (Resolved).
type Fault struct {
	Class    ErrorClass
	Units    []string
	Resolved bool
}

// Config wires a Machine. Collect is the only required field; a nil Journal
// or empty IndexPath turns that output off. Collect's since is when the
// machine's timers last restarted (its start or the last wake): a source
// judges lateness from it at the earliest.
type Config struct {
	Clock      wake.Clock
	Collect    func(since time.Time) []Fact
	Journal    *journal.Journal
	Workspaces *workspace.Registry
	IndexPath  string
	Beats      *beat.Registry
	// Watch runs on the loop after every reconcile: the push pipeline's
	// check on its heartbeat, which it cannot report itself once stopped.
	Watch func()
}

// Machine is the health state machine. Reconcile and RefreshIndex run on
// Run's goroutine; Snapshot and Subscribe are safe from any goroutine.
type Machine struct {
	cfg      Config
	detector *wake.Detector
	poke     chan struct{}

	// Run-goroutine state.
	units    map[string]*unit
	last     map[string]journaled     // the journal's last transition per unit, consumed on first sight
	nodeDown map[string]time.Duration // Mono when a node's polls began failing
	baseline time.Time                // wall time timers restart from (start, last wake)
	wokeAt   time.Duration            // Mono at the last wake
	woke     bool
	faults   map[ErrorClass]bool // classes with an open systemic fault
	wsCache  map[string]string   // dir → workspace ID, hits only
	lastIdx  []byte

	mu   sync.Mutex
	snap Snapshot
	subs map[int]func(Event)
	next int
}

type unit struct {
	Unit
	// first holds until the unit leaves the Unknown it starts in.
	first        bool
	pending      State
	pendingN     int
	pendingClass ErrorClass
	failedAt     time.Time // when ErrClass was confirmed, for fault merging
	node         string
	// dir and index are a remote unit's last listed directory and
	// index.json row, kept for when its node is down and lists it no more.
	dir   string
	index *IndexEntry
	// held is when a session failed before a restart, while it is still
	// held failed (holdFailure); zero otherwise.
	held         time.Time
	heldEvidence string
	heldClass    ErrorClass
}

// journaled is a unit's last journaled transition.
type journaled struct {
	State    State
	At       time.Time
	Evidence string
	ErrClass ErrorClass
}

// New returns a Machine whose timers start now, seeded with each unit's last
// journaled state and the systemic faults the journal left open.
func New(cfg Config) *Machine {
	if cfg.Clock == nil {
		cfg.Clock = wake.SystemClock()
	}
	now := cfg.Clock.Now()
	m := &Machine{
		cfg:      cfg,
		detector: wake.NewDetector(cfg.Clock),
		poke:     make(chan struct{}, 1),
		units:    map[string]*unit{},
		last:     map[string]journaled{},
		nodeDown: map[string]time.Duration{},
		baseline: now,
		faults:   map[ErrorClass]bool{},
		wsCache:  map[string]string{},
		subs:     map[int]func(Event){},
	}
	m.seed(now)
	return m
}

// journalData is a health entry's data.
type journalData struct {
	From     State      `json:"from,omitempty"`
	To       State      `json:"to,omitempty"`
	Kind     Kind       `json:"kind,omitempty"`
	Evidence string     `json:"evidence,omitempty"`
	ErrClass ErrorClass `json:"error_class,omitempty"`
	// Systemic-fault entries.
	Systemic bool     `json:"systemic,omitempty"`
	Units    []string `json:"units,omitempty"`
	Resolved bool     `json:"resolved,omitempty"`
}

func (m *Machine) seed(now time.Time) {
	if m.cfg.Journal == nil {
		return
	}
	entries, err := m.cfg.Journal.Read(now.Add(-seedLookback), now.Add(time.Minute))
	if err != nil {
		slog.Warn("health: read journal for last states", "err", err)
	}
	for _, e := range entries {
		var d journalData
		if e.Kind != journal.KindHealth || json.Unmarshal(e.Data, &d) != nil {
			continue
		}
		switch {
		case d.Systemic && d.ErrClass != "":
			m.faults[d.ErrClass] = !d.Resolved
		case !d.Systemic && d.To != "":
			m.last[e.Unit] = journaled{State: d.To, At: e.TS, Evidence: d.Evidence, ErrClass: d.ErrClass}
		}
	}
	maps.DeleteFunc(m.faults, func(_ ErrorClass, open bool) bool { return !open })
}

// Run reconciles every Interval until ctx ends, beating the "health" loop,
// and refreshes index.json shortly after each Poke.
func (m *Machine) Run(ctx context.Context) {
	hb := m.cfg.Beats.Register("health", Interval)
	m.safely("reconcile", m.Reconcile)
	hb.Beat()
	tick := time.NewTicker(Interval)
	defer tick.Stop()
	var debounce *time.Timer
	var refresh <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			if debounce != nil {
				debounce.Stop()
			}
			return
		case <-tick.C:
			m.safely("reconcile", m.Reconcile)
			hb.Beat()
			if m.cfg.Watch != nil {
				m.safely("watch", m.cfg.Watch)
			}
		case <-m.poke:
			if debounce == nil {
				debounce = time.NewTimer(indexDebounce)
				refresh = debounce.C
			}
		case <-refresh:
			debounce, refresh = nil, nil
			m.safely("index refresh", m.RefreshIndex)
		}
	}
}

// safely keeps a panicking source from ending the loop: the beat stops only
// if the loop itself does.
func (m *Machine) safely(what string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("health: "+what+" panicked", "panic", r)
		}
	}()
	fn()
}

// Poke asks for an index.json refresh (a turn ended). Never blocks.
func (m *Machine) Poke() {
	select {
	case m.poke <- struct{}{}:
	default:
	}
}

// Subscribe registers fn for every transition of a unit that belongs to a
// workspace and every systemic fault, and returns the func that ends the
// subscription. fn runs on the reconcile goroutine and must not block.
func (m *Machine) Subscribe(fn func(Event)) (cancel func()) {
	m.mu.Lock()
	id := m.next
	m.next++
	m.subs[id] = fn
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		delete(m.subs, id)
		m.mu.Unlock()
	}
}

// Snapshot returns the table as of the last reconcile, sorted by unit ID.
func (m *Machine) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.snap
	s.Units = slices.Clone(s.Units)
	return s
}

// Reconcile collects the facts, judges every unit and reports what changed.
func (m *Machine) Reconcile() {
	now, mono := m.cfg.Clock.Now(), m.cfg.Clock.Mono()
	if m.detector.Woke() {
		m.baseline, m.wokeAt, m.woke = now, mono, true
		for _, u := range m.units {
			u.pending, u.pendingN = "", 0
		}
		for n := range m.nodeDown {
			m.nodeDown[n] = mono
		}
	}
	facts, down := m.collect()
	m.trackNodes(down, mono)

	var events []Event
	seen := make(map[string]bool, len(facts))
	for _, f := range facts {
		seen[f.ID] = true
	}
	for i := range facts {
		f := &facts[i]
		u := m.units[f.ID]
		if u == nil {
			u = &unit{Unit: Unit{ID: f.ID, State: Unknown, Since: now}, first: true}
			if j := m.last[f.ID]; j.State == Failed && f.Kind == KindSession {
				u.held, u.heldEvidence, u.heldClass = j.At, j.Evidence, j.ErrClass
			}
			m.units[f.ID] = u
		}
		u.Kind, u.Source, u.Coverage, u.node = f.Kind, f.Source, f.Coverage, f.Node
		if f.Kind == KindRemote {
			u.dir, u.index = f.Dir, f.Index
		}
		u.Workspace = m.workspaceFor(f)
		u.LastActive = f.LastActive
		state, evidence, class := m.judge(f, now, mono)
		state, evidence, class = u.holdFailure(f, state, evidence, class)
		if ev, ok := m.step(u, state, evidence, class, now); ok {
			events = append(events, ev)
		}
	}
	for id := range m.units {
		if !seen[id] {
			delete(m.units, id)
		}
	}
	events = append(events, m.faultEvents(now)...)

	snap := Snapshot{At: now, Units: make([]Unit, 0, len(m.units))}
	for _, u := range m.units {
		snap.Units = append(snap.Units, u.Unit)
		// The Planner answers to no workspace by design (RFC §6.3).
		if u.Workspace == "" && u.ID != sessionkey.PlannerMainKey {
			snap.Unowned++
		}
	}
	slices.SortFunc(snap.Units, func(a, b Unit) int { return cmp.Compare(a.ID, b.ID) })
	m.mu.Lock()
	m.snap = snap
	subs := make([]func(Event), 0, len(m.subs))
	for _, fn := range m.subs {
		subs = append(subs, fn)
	}
	m.mu.Unlock()

	for _, ev := range events {
		m.record(ev)
		// A snapshot-only unit with no workspace (a remote node's or a
		// terminal's session) is only journaled; an unowned cron job,
		// daemon or the Planner still fails loudly (RFC §15.1).
		if t := ev.Transition; t != nil && t.Unit.Workspace == "" && (t.Unit.Kind == KindRemote || t.Unit.Kind == KindExternal) {
			continue
		}
		for _, fn := range subs {
			fn(ev)
		}
	}
	m.writeIndex(facts)
}

// RefreshIndex rewrites index.json from fresh facts with the health of the
// last reconcile.
func (m *Machine) RefreshIndex() {
	if m.cfg.IndexPath == "" {
		return
	}
	facts, _ := m.collect()
	m.writeIndex(facts)
}

// unlisted returns facts for a down node's sessions that it no longer lists
// (its sessions are purged once it deregisters): they are still its
// sessions, judged as unreachable, in the workspace and index.json row they
// were last listed with.
func (m *Machine) unlisted(facts []Fact, down map[string]bool) []Fact {
	listed := make(map[string]bool, len(facts))
	for _, f := range facts {
		listed[f.ID] = true
	}
	var out []Fact
	for id, u := range m.units {
		if !listed[id] && u.Kind == KindRemote && down[u.node] {
			out = append(out, Fact{ID: id, Kind: KindRemote, Source: u.Source, Coverage: u.Coverage,
				Dir: u.dir, Node: u.node, NodeDown: true, Running: u.State == Active,
				LastActive: u.LastActive, Index: u.index})
		}
	}
	return out
}

// collect gathers the units' facts, merged, with those of the down nodes'
// unlisted sessions, and the nodes reported down.
func (m *Machine) collect() ([]Fact, map[string]bool) {
	all := m.cfg.Collect(m.baseline)
	facts := make([]Fact, 0, len(all))
	down := map[string]bool{}
	for _, f := range all {
		if f.NodeDown {
			down[f.Node] = true
		}
		if f.ID != "" {
			facts = append(facts, f)
		}
	}
	facts = mergeFacts(facts)
	return append(facts, m.unlisted(facts, down)...), down
}

// step applies one judged state to u and returns the transition it causes.
// Healthy states take effect at once; the others once two reconciles in a
// row agree, except Lost, which is judged only once two reconciles LostAfter
// apart saw the node down, and a unit's first state when it is the one the
// journal last recorded for it. A unit's first state counts from that
// record, so a restart reports only what changed meanwhile; with no record, settling into a healthy state is not news. A
// failure a unit already had when first seen is dated by its last activity,
// so old failures do not merge into a systemic fault.
func (m *Machine) step(u *unit, state State, evidence string, class ErrorClass, now time.Time) (Event, bool) {
	if state == u.State {
		u.pending, u.pendingN = "", 0
		u.Evidence = evidence
		if class != u.ErrClass {
			u.ErrClass, u.failedAt = class, now
		}
		return Event{}, false
	}
	// A unit first seen in the state the journal last recorded for it is
	// not news: it was confirmed before the restart.
	settled := u.first && m.last[u.ID].State == state
	if !state.healthy() && state != Lost && !settled {
		if u.pending != state {
			u.pending, u.pendingN = state, 0
		}
		u.pendingN++
		u.pendingClass = class
		if u.pendingN < 2 {
			return Event{}, false
		}
	}
	u.pending, u.pendingN = "", 0
	from, first := u.State, u.first
	u.failedAt = now
	if first {
		from = cmp.Or(m.last[u.ID].State, Unknown)
		delete(m.last, u.ID)
		u.first = false
		u.failedAt = u.LastActive
	}
	u.State, u.Evidence, u.ErrClass, u.Since = state, evidence, class, now
	if first && from == state && !u.held.IsZero() {
		u.Since = u.held // the failure the journal recorded, not a new one
	}
	if from == state || first && from == Unknown && state.healthy() {
		return Event{}, false
	}
	return Event{At: now, Transition: &Transition{From: from, Unit: u.Unit}}, true
}

// judge applies the §15.1 table to one unit's facts.
func (m *Machine) judge(f *Fact, now time.Time, mono time.Duration) (State, string, ErrorClass) {
	switch {
	case f.NodeDown && mono-m.nodeDown[f.Node] >= LostAfter:
		return Lost, fmt.Sprintf("node %s unreachable", f.Node), ErrNone
	case f.Failed != "":
		return Failed, f.Failed, cmp.Or(f.ErrClass, Classify(f.ErrText))
	case !f.Missed.IsZero() && f.Missed.After(m.baseline) && !f.Running:
		return Failed, "missed the run due " + f.Missed.Format(time.RFC3339), ErrOther
	case f.Asking:
		return Waiting, "unanswered AskUserQuestion", ErrNone
	case f.Workflow != "":
		seen := f.WorkflowSeen
		if seen.Before(m.baseline) {
			seen = m.baseline
		}
		quiet := now.Sub(seen)
		if quiet >= StallAfter && !(m.woke && mono-m.wokeAt < WakeGrace) {
			return Stalled, fmt.Sprintf("task %q: no observation for %s", f.Workflow, quiet.Round(time.Minute)), ErrNone
		}
		return Active, fmt.Sprintf("task %q running", f.Workflow), ErrNone
	case f.Running:
		return Active, "turn in progress", ErrNone
	case f.NoData:
		return Unknown, "no data", ErrNone
	}
	return Idle, "", ErrNone
}

// holdFailure keeps a session that failed before a restart failed while
// nothing happened to it since: its death reason does not survive the
// restart, so it would read idle. Anything else judged, or any activity
// after the failure, lets it go for good.
func (u *unit) holdFailure(f *Fact, state State, evidence string, class ErrorClass) (State, string, ErrorClass) {
	if u.held.IsZero() {
		return state, evidence, class
	}
	if state == Idle && !f.LastActive.After(u.held) {
		return Failed, u.heldEvidence, u.heldClass
	}
	u.held = time.Time{}
	return state, evidence, class
}

// trackNodes records when each down node's polls began failing.
func (m *Machine) trackNodes(down map[string]bool, mono time.Duration) {
	for n := range m.nodeDown {
		if !down[n] {
			delete(m.nodeDown, n)
		}
	}
	for n := range down {
		if _, ok := m.nodeDown[n]; !ok {
			m.nodeDown[n] = mono
		}
	}
}

// faultEvents opens a systemic fault for each error class FaultMin units
// entered within FaultWindow, and resolves an open one once no unit has or
// is about to confirm its class. ErrOther never merges: two unrelated
// failures share it.
func (m *Machine) faultEvents(now time.Time) []Event {
	recent := map[ErrorClass][]string{}
	current := map[ErrorClass]bool{}
	for _, u := range m.units {
		if u.State != Failed {
			if u.pending == Failed {
				current[u.pendingClass] = true
			}
			continue
		}
		if u.ErrClass == ErrNone || u.ErrClass == ErrOther {
			continue
		}
		current[u.ErrClass] = true
		if !u.failedAt.IsZero() && now.Sub(u.failedAt) <= FaultWindow {
			recent[u.ErrClass] = append(recent[u.ErrClass], u.ID)
		}
	}
	var out []Event
	for c, ids := range recent {
		if len(ids) >= FaultMin && !m.faults[c] {
			m.faults[c] = true
			slices.Sort(ids)
			out = append(out, Event{At: now, Fault: &Fault{Class: c, Units: ids}})
		}
	}
	for c := range m.faults {
		if !current[c] {
			delete(m.faults, c)
			out = append(out, Event{At: now, Fault: &Fault{Class: c, Resolved: true}})
		}
	}
	slices.SortFunc(out, func(a, b Event) int { return cmp.Compare(a.Fault.Class, b.Fault.Class) })
	return out
}

// record writes ev to the journal.
func (m *Machine) record(ev Event) {
	if m.cfg.Journal == nil {
		return
	}
	e := journal.Entry{TS: ev.At, Kind: journal.KindHealth}
	var d journalData
	if t := ev.Transition; t != nil {
		e.WS, e.Unit = t.Unit.Workspace, t.Unit.ID
		d = journalData{From: t.From, To: t.Unit.State, Kind: t.Unit.Kind, Evidence: t.Unit.Evidence, ErrClass: t.Unit.ErrClass}
	} else {
		e.Unit = "systemic:" + string(ev.Fault.Class)
		d = journalData{Systemic: true, ErrClass: ev.Fault.Class, Units: ev.Fault.Units, Resolved: ev.Fault.Resolved}
	}
	data, err := json.Marshal(d)
	if err == nil {
		e.Data = data
		err = m.cfg.Journal.Append(e)
	}
	if err != nil {
		slog.Warn("health: journal append failed", "unit", e.Unit, "err", err)
	}
}

// workspaceFor returns the ID of the workspace holding f's directory,
// registering the directory when f allows it. A remote unit's directory is
// looked up among its node's paths only, and never registered.
func (m *Machine) workspaceFor(f *Fact) string {
	if f.Dir == "" || m.cfg.Workspaces == nil {
		return ""
	}
	// Two machines may each have the same path, holding different work.
	ck := f.Dir
	if f.Node != "" {
		ck = f.Node + " " + f.Dir
	}
	if id, ok := m.wsCache[ck]; ok {
		return id
	}
	var w workspace.Workspace
	var ok bool
	switch {
	case f.Node != "":
		w, ok = m.cfg.Workspaces.LookupOn(f.Node, f.Dir)
	case f.Register:
		w, ok = m.cfg.Workspaces.Ensure(f.Dir)
	default:
		w, ok = m.cfg.Workspaces.Lookup(f.Dir)
	}
	if !ok {
		return ""
	}
	m.wsCache[ck] = w.ID
	return w.ID
}

// mergeFacts folds facts sharing an ID into the first one, keeping its kind
// and source and adding what the later ones saw.
func mergeFacts(in []Fact) []Fact {
	out := make([]Fact, 0, len(in))
	at := make(map[string]int, len(in))
	for _, f := range in {
		i, ok := at[f.ID]
		if !ok {
			at[f.ID] = len(out)
			out = append(out, f)
			continue
		}
		o := &out[i]
		o.Dir = cmp.Or(o.Dir, f.Dir)
		o.Register = o.Register || f.Register
		o.Running = o.Running || f.Running
		o.Asking = o.Asking || f.Asking
		o.NoData = o.NoData && f.NoData
		if o.Failed == "" {
			o.Failed, o.ErrText, o.ErrClass = f.Failed, f.ErrText, f.ErrClass
		}
		if o.Missed.IsZero() {
			o.Missed = f.Missed
		}
		if o.Workflow == "" {
			o.Workflow, o.WorkflowSeen = f.Workflow, f.WorkflowSeen
		}
		if f.LastActive.After(o.LastActive) {
			o.LastActive = f.LastActive
		}
		if o.Index == nil {
			o.Index = f.Index
		}
	}
	return out
}
