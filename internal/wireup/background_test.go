package wireup

import (
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/session"
)

// bgSession is a live session in r under key whose process naozhi
// attached at attached, its events entries.
func bgSession(r *session.Router, key string, attached time.Time, entries ...clievent.EventEntry) (*session.ManagedSession, *session.TestProcess) {
	p := session.NewTestProcess()
	p.StartedAtVal = attached
	for _, e := range entries {
		p.EventLog.Append(e)
	}
	return r.InjectSession(key, p), p
}

func taskStart(id, typ, summary string, at time.Time) clievent.EventEntry {
	return clievent.EventEntry{Type: clievent.KindTaskStart, TaskID: id, TaskType: typ, Summary: summary, Time: at.UnixMilli()}
}

func taskDone(id string, at time.Time) clievent.EventEntry {
	return clievent.EventEntry{Type: clievent.KindTaskDone, TaskID: id, Status: "completed", Time: at.UnixMilli()}
}

// TestBackgroundAfterTurnEnd is the regression the one judgment fixes: a
// turn ended with a background Bash still running. Custody's Settled, the
// Planner's receipt (action.Config.Background), the put-away and the
// health table all read it open: none takes the session for idle and done.
// Its end closes it at once, not after a cache's age.
func TestBackgroundAfterTurnEnd(t *testing.T) {
	r := newDigestRouter(t)
	now := time.Now()
	const key = "dashboard:direct:bg:general"
	s, p := bgSession(r, key, now.Add(-time.Hour),
		clievent.EventEntry{Type: clievent.KindToolUse, Tool: "Bash", Time: now.Add(-10 * time.Minute).UnixMilli()},
		taskStart("b1", "local_bash", "ssh bench ./run.sh", now.Add(-10*time.Minute)),
		clievent.EventEntry{Type: clievent.KindResult, Time: now.Add(-9 * time.Minute).UnixMilli()})
	cs := CustodySources{Router: r}
	if cs.Settled(key) {
		t.Error("Settled with a background Bash running: custody would take the turn for done")
	}
	if bg := cs.Background(key); bg.Count() != 1 || bg.Tasks[0].Summary != "ssh bench ./run.sh" {
		t.Errorf("Background = %+v, want the Bash", bg)
	}
	if rs, _ := (ReapSources{Router: r}).Session(key); !rs.Background {
		t.Error("reap.Session.Background = false")
	}
	f := sessionFact(s.Snapshot(), s, "")
	if !f.Background.Open() || f.Workflow != "" || f.Index.Background == nil || f.Index.Background.Count != 1 ||
		f.Index.Background.Tasks[0].Summary != "ssh bench ./run.sh" {
		t.Errorf("fact = %+v, index %+v; want the task open, not watched for a stall", f.Background, f.Index.Background)
	}
	p.EventLog.Append(taskDone("b1", now))
	if !cs.Settled(key) || cs.Background(key).Open() {
		t.Error("still background after its task_done")
	}
	if f := sessionFact(s.Snapshot(), s, ""); f.Background.Open() || f.Index.Background != nil {
		t.Errorf("fact after the end = %+v", f.Background)
	}
}

// A Monitor started at a long turn's head is still seen past the old
// 256-event window; a dead process has nothing open; a process started
// after the task (a /new, a --resume) does not inherit it.
func TestBackgroundBoundaries(t *testing.T) {
	r := newDigestRouter(t)
	now := time.Now()
	entries := []clievent.EventEntry{
		{Type: clievent.KindToolUse, Tool: "Monitor", Time: now.Add(-time.Hour).UnixMilli()},
		taskStart("m1", "local_bash", "watch CI", now.Add(-time.Hour)),
	}
	for i := range 400 {
		entries = append(entries, clievent.EventEntry{Type: clievent.KindToolUse, Tool: "Read", Time: now.Add(-time.Hour).UnixMilli() + int64(i) + 1})
	}
	cs := CustodySources{Router: r}

	bgSession(r, "dashboard:direct:mon:general", now.Add(-2*time.Hour), entries...)
	if bg := cs.Background("dashboard:direct:mon:general"); bg.Count() != 1 || bg.Tasks[0].ID != "m1" {
		t.Errorf("monitor: Background = %+v, want m1", bg)
	}

	_, p := bgSession(r, "dashboard:direct:dead:general", now.Add(-2*time.Hour), entries[:2]...)
	p.SetState(cli.StateDead)
	if cs.Background("dashboard:direct:dead:general").Open() {
		t.Error("dead: a task open in an exited process")
	}

	bgSession(r, "dashboard:direct:new:general", now.Add(-time.Minute), entries[:2]...)
	if cs.Background("dashboard:direct:new:general").Open() {
		t.Error("/new: the earlier process's task open in the new one")
	}

	// No process ever attached.
	r.InjectSession("dashboard:direct:none:general", nil)
	if cs.Background("dashboard:direct:none:general").Open() {
		t.Error("no process: a task open")
	}
}

// After a restart the reattached process is the one that started the task
// (its shim's start predates the attach): the task comes back from the
// event log, unless the shim's replay holds its end. The judgment that
// counted from the attach saw nothing open, and custody took the session
// for idle.
func TestBackgroundAcrossRestart(t *testing.T) {
	r := newDigestRouter(t)
	now := time.Now()
	cliStart := now.Add(-3 * time.Hour)
	history := []clievent.EventEntry{
		taskStart("b1", "local_bash", "make bench", now.Add(-2*time.Hour)),
		taskStart("b2", "local_bash", "gh run watch", now.Add(-time.Hour)),
		{Type: clievent.KindResult, Time: now.Add(-time.Hour).UnixMilli()},
	}
	s, _ := bgSession(r, "task:aaaaaaaaaaaa", now, history...)
	cs := CustodySources{Router: r}
	if cs.Background("task:aaaaaaaaaaaa").Open() {
		t.Fatal("without the shim's start, a task from before the attach counted")
	}
	s.SetReattachedForTest(cliStart, []clievent.EventEntry{{Type: clievent.KindTaskDone, TaskID: "b1"}})
	backgrounds.mu.Lock()
	delete(backgrounds.entries, s)
	backgrounds.mu.Unlock()
	bg := cs.Background("task:aaaaaaaaaaaa")
	if bg.Count() != 1 || bg.Tasks[0].ID != "b2" {
		t.Fatalf("reattached: Background = %+v, want b2 only (b1 ended in the replay)", bg)
	}
	if cs.Settled("task:aaaaaaaaaaaa") {
		t.Error("reattached: Settled with b2 open")
	}
}

// The health table's unit and index.json carry the count, the first
// tasks and the latest progress; the unit stays idle (后台中 is idle with
// tasks open).
func TestBackgroundInHealth(t *testing.T) {
	r := newDigestRouter(t)
	now := time.Now()
	s, _ := bgSession(r, "dashboard:direct:h:general", now.Add(-time.Hour),
		taskStart("b1", "local_bash", "bench a", now.Add(-30*time.Minute)),
		taskStart("b2", "local_bash", "bench b", now.Add(-20*time.Minute)),
		taskStart("b3", "local_bash", "bench c", now.Add(-10*time.Minute)),
		clievent.EventEntry{Type: clievent.KindResult, Time: now.Add(-10 * time.Minute).UnixMilli()})
	f := sessionFact(s.Snapshot(), s, "")
	ib := f.Index.Background
	if ib == nil || ib.Count != 3 || len(ib.Tasks) != health.BackgroundShown || ib.Tasks[0].ID != "b1" ||
		ib.LastProgress.UnixMilli() != now.Add(-10*time.Minute).UnixMilli() {
		t.Fatalf("index background = %+v", ib)
	}
}

// fill appends n tool calls to p's event log, each a millisecond after the
// one before, from at.
func fill(p *session.TestProcess, n int, at time.Time) {
	for i := range n {
		p.EventLog.Append(clievent.EventEntry{Type: clievent.KindToolUse, Tool: "Read", Time: at.UnixMilli() + int64(i)})
	}
}

// A Monitor started more than a scan's window before the latest event is
// still open: the scans of one process carry it until its end is seen.
func TestBackgroundBeyondTheWindow(t *testing.T) {
	r := newDigestRouter(t)
	now := time.Now()
	const key = "dashboard:direct:long:general"
	_, p := bgSession(r, key, now.Add(-2*time.Hour),
		clievent.EventEntry{Type: clievent.KindToolUse, Tool: "Monitor", Time: now.Add(-time.Hour).UnixMilli()},
		taskStart("m1", "local_bash", "watch CI", now.Add(-time.Hour)))
	cs := CustodySources{Router: r}
	if bg := cs.Background(key); bg.Count() != 1 {
		t.Fatalf("Background = %+v, want m1", bg)
	}
	fill(p, taskScan+100, now.Add(-50*time.Minute))
	if bg := cs.Background(key); bg.Count() != 1 || bg.Tasks[0].ID != "m1" {
		t.Fatalf("after %d more events: Background = %+v, want m1 carried", taskScan+100, bg)
	}
	p.EventLog.Append(taskDone("m1", now.Add(-time.Minute)))
	if cs.Background(key).Open() {
		t.Fatal("still open after its end")
	}
	fill(p, taskScan+100, now.Add(-30*time.Second))
	if cs.Background(key).Open() {
		t.Fatal("open again once its end left the window")
	}
}

// A task the shim's replay started and the event log ended after the
// reattach stays ended once that end leaves the window: the replay is
// kept for the process's life and would start it again.
func TestBackgroundReplayEndedInTheLog(t *testing.T) {
	r := newDigestRouter(t)
	now := time.Now()
	const key = "task:bbbbbbbbbbbb"
	s, p := bgSession(r, key, now.Add(-time.Hour))
	s.SetReattachedForTest(now.Add(-3*time.Hour), []clievent.EventEntry{{Type: clievent.KindTaskStart, TaskID: "x1", TaskType: "local_bash", Summary: "bench"}})
	cs := CustodySources{Router: r}
	bg := cs.Background(key)
	if bg.Count() != 1 || bg.Tasks[0].ID != "x1" || !bg.LastProgress.Equal(now.Add(-time.Hour)) {
		t.Fatalf("Background = %+v, want x1 from the replay, its progress the attach", bg)
	}
	p.EventLog.Append(taskDone("x1", now.Add(-30*time.Minute)))
	if cs.Background(key).Open() {
		t.Fatal("open after its end in the log")
	}
	fill(p, taskScan+100, now.Add(-20*time.Minute))
	if bg := cs.Background(key); bg.Open() {
		t.Fatalf("its end left the window and the replay started it again: %+v", bg)
	}
	if !cs.Settled(key) {
		t.Error("not Settled with nothing open")
	}
}

// A foreground Bash the turn waits on is the turn running, not background
// work: it is not counted until the model writes after its start.
func TestBackgroundForegroundBash(t *testing.T) {
	r := newDigestRouter(t)
	now := time.Now()
	const key = "dashboard:direct:fg:general"
	_, p := bgSession(r, key, now.Add(-time.Hour),
		clievent.EventEntry{Type: clievent.KindToolUse, Tool: "Bash", Time: now.Add(-time.Minute).UnixMilli()},
		taskStart("b1", "local_bash", "go test ./...", now.Add(-time.Minute)))
	cs := CustodySources{Router: r}
	if bg := cs.Background(key); bg.Open() {
		t.Fatalf("a foreground Bash counted: %+v", bg)
	}
	p.EventLog.Append(taskDone("b1", now.Add(-30*time.Second)))
	p.EventLog.Append(clievent.EventEntry{Type: clievent.KindText, Time: now.Add(-20 * time.Second).UnixMilli()})
	if bg := cs.Background(key); bg.Open() {
		t.Fatalf("an ended Bash counted: %+v", bg)
	}
}

// The idle expiry leaves the CLI of a session whose background Bash still
// runs (bgtask's judgment through BackgroundOpen): 11-08 and 11-09 a trn2
// job died at about 40 minutes with the 30-minute idle expiry. A session
// whose Bash ended expires as before. The events are replayed so that
// they stamp no live activity.
func TestIdleExpiryKeepsBackgroundBash(t *testing.T) {
	RegisterCLIBackends()
	r := session.NewRouter(session.RouterConfig{TTL: time.Minute})
	t.Cleanup(r.Shutdown)
	r.SetBackgroundCheck(BackgroundOpen)
	old := time.Now().Add(-2 * time.Hour)
	idle := func(key string, entries ...clievent.EventEntry) *session.TestProcess {
		p := session.NewTestProcess()
		p.StartedAtVal = old
		p.EventLog.AppendBatchReplay(append([]clievent.EventEntry{
			{Type: clievent.KindToolUse, Tool: "Bash", Time: old.UnixMilli()},
			taskStart("b1", "local_bash", "ssh trn2 ./bench.sh", old),
			{Type: clievent.KindResult, Time: old.Add(time.Second).UnixMilli()},
		}, entries...))
		r.InjectSession(key, p).SetLastTurnAtForTest(old)
		if !p.LastEventAt().IsZero() {
			t.Fatal("the replay stamped live activity")
		}
		return p
	}
	running := idle("dashboard:direct:bg:general")
	ended := idle("dashboard:direct:done:general", taskDone("b1", old.Add(time.Minute)))
	r.Cleanup()
	if !running.Alive() {
		t.Error("the idle expiry closed a CLI whose background Bash still runs")
	}
	if ended.Alive() {
		t.Error("an idle session whose background Bash ended was not expired")
	}
}
