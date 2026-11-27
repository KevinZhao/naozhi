package scout

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// errCred is the model call's failure of the 11-16 08:00 and 08:15 runs,
// woken before the Bedrock credential was back.
var errCred = errors.New("cron: turn failed (success): API Error: Could not load AWS credentials · Could not load credentials from any providers. Check or refresh your AWS credentials and try again")

// waitFixture is a scout whose session answers with err or reply, and whose
// Midway session expires at exp.
type waitFixture struct {
	root  string
	mu    sync.Mutex
	now   time.Time
	exp   time.Time
	err   error
	reply string
	calls int
	s     *Scout
}

func newWaitFixture(t *testing.T, noCred bool) *waitFixture {
	t.Helper()
	f := &waitFixture{root: t.TempDir(), reply: `{"findings":[],"cursors":{"email":"e1","slack":"","asana":""}}`}
	f.s = f.open(writeMCP(t, f.root, `{"mcpServers":{"slack-mcp":{"command":"s"},"aws-outlook-mcp":{"command":"o"}}}`), noCred)
	return f
}

func (f *waitFixture) open(mcp string, noCred bool) *Scout {
	cfg := Config{
		Layout:    datadir.FromRoot(f.root),
		MCPConfig: mcp,
		Session: func(context.Context, string, sessionview.AgentOpts, string) (string, []string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.calls++
			if f.err != nil {
				return "", nil, f.err
			}
			return f.reply, nil, nil
		},
		Now: func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now },
	}
	if !noCred {
		cfg.CredExpiry = func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.exp }
	}
	s := New(cfg)
	s.ctx = context.Background()
	return s
}

func (f *waitFixture) set(fn func()) { f.mu.Lock(); fn(); f.mu.Unlock() }

func (f *waitFixture) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

// A run whose model call finds the credential expired is not failed: it
// waits, and goes again once the Midway session is renewed; the retry that
// goes through ends the wait. A restart keeps waiting.
func TestCredentialWaitRetries(t *testing.T) {
	f := newWaitFixture(t, false)
	mon := time.Date(2026, 11, 16, 8, 0, 29, 0, time.Local) // a Monday
	f.set(func() { f.now, f.exp, f.err = mon, mon.Add(-time.Hour), errCred })
	id, err := f.s.start(TriggerSchedule)
	if err != nil {
		t.Fatal(err)
	}
	f.s.runs.Wait()
	st := f.s.Status()
	if st.Last == nil || st.Last.State != StateWaiting || !st.Last.Cred.Equal(mon.Add(-time.Hour)) {
		t.Fatalf("last run = %+v, want waiting with the expiry it saw", st.Last)
	}
	if rec, _ := f.s.st.lastRun(); rec.State != StateWaiting {
		t.Errorf("run record = %+v, want waiting", rec)
	}
	if c := f.s.st.cursors(); c.Email != "" {
		t.Errorf("cursors after a waiting run = %+v, want unchanged", c)
	}

	// Not renewed yet: nothing runs.
	f.set(func() { f.now = mon.Add(5 * time.Minute) })
	f.s.checkRetry(f.s.cfg.Now())
	f.s.runs.Wait()
	if n := f.count(); n != 1 {
		t.Fatalf("sessions before the renewal = %d, want 1", n)
	}

	// A restart before the renewal keeps the wait.
	f.s = f.open(filepath.Join(f.root, "mcp.json"), false)
	f.set(func() { f.now, f.exp, f.err = mon.Add(10*time.Minute), mon.Add(20*time.Hour), nil })
	f.s.checkRetry(f.s.cfg.Now())
	f.s.runs.Wait()
	st = f.s.Status()
	if n := f.count(); n != 2 || st.Last == nil || st.Last.State != StateOK || st.Last.Trigger != TriggerRetry || st.Last.RetryOf != id {
		t.Fatalf("after the renewal: sessions %d, last %+v; want a retry of %s that went through", n, st.Last, id)
	}
	if c := f.s.st.cursors(); c.Email != "e1" {
		t.Errorf("cursors after the retry = %+v", c)
	}
	f.s.checkRetry(mon.Add(11 * time.Minute))
	f.s.runs.Wait()
	if n := f.count(); n != 2 {
		t.Errorf("sessions after the retry went through = %d, want 2", n)
	}
	// The 08:15 slot is due as usual.
	if _, ok := due(mon.Add(15*time.Minute), f.s.started); !ok {
		t.Error("08:15 not due after the retry")
	}
}

// The wait lasts until the next scheduled slot: that run reads the span.
func TestCredentialWaitEndsAtNextSlot(t *testing.T) {
	f := newWaitFixture(t, false)
	fri := time.Date(2026, 11, 13, 20, 0, 0, 0, time.Local) // a Friday, the day's last slot
	f.set(func() { f.now, f.exp, f.err = fri, fri.Add(-time.Hour), errCred })
	if _, err := f.s.start(TriggerSchedule); err != nil {
		t.Fatal(err)
	}
	f.s.runs.Wait()
	f.s.mu.Lock()
	until := f.s.wait.until
	f.s.mu.Unlock()
	if want := time.Date(2026, 11, 16, 8, 0, 0, 0, time.Local); !until.Equal(want) {
		t.Fatalf("wait until %s, want the next slot %s", until, want)
	}
	f.set(func() { f.exp, f.err = until.Add(time.Hour), nil })
	f.s.checkRetry(until)
	f.s.runs.Wait()
	if n := f.count(); n != 1 {
		t.Errorf("sessions = %d, want no retry at the next slot", n)
	}
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	if f.s.wait != nil {
		t.Errorf("wait = %+v at the next slot, want none", f.s.wait)
	}
}

// A credential renewing Midway does not fix is no wait run after run: the
// next run failing so with Midway not renewed since is failed, as the health
// table and the inbox see; one after a renewal waits again.
func TestCredentialWaitNotRenewedFails(t *testing.T) {
	f := newWaitFixture(t, false)
	mon := time.Date(2026, 11, 16, 8, 0, 29, 0, time.Local)
	valid := mon.Add(10 * time.Hour) // the cookie is fine; the credential is not
	f.set(func() { f.now, f.exp, f.err = mon, valid, errCred })
	run := func(at time.Time, trig string) *Run {
		t.Helper()
		f.set(func() { f.now = at })
		if _, err := f.s.start(trig); err != nil {
			t.Fatal(err)
		}
		f.s.runs.Wait()
		return f.s.Status().Last
	}
	if r := run(mon, TriggerSchedule); r == nil || r.State != StateWaiting {
		t.Fatalf("first run = %+v, want waiting", r)
	}
	f.s.checkRetry(mon.Add(15 * time.Minute)) // the wait ends at the next slot
	r := run(mon.Add(15*time.Minute), TriggerSchedule)
	if r == nil || r.State != StateFailed || !strings.Contains(r.Error, errCredStill) {
		t.Fatalf("second run, Midway not renewed = %+v, want failed", r)
	}
	f.s.mu.Lock()
	w := f.s.wait
	f.s.mu.Unlock()
	if w != nil {
		t.Errorf("wait after a failed run = %+v, want none", w)
	}
	if rec, _ := f.s.st.lastRun(); rec.State != StateFailed {
		t.Errorf("run record = %+v, want failed", rec)
	}

	// A failed run then a renewal: the next failure waits again.
	f.set(func() { f.exp = valid.Add(time.Hour) })
	if r := run(mon.Add(2*time.Hour), TriggerSchedule); r == nil || r.State != StateWaiting {
		t.Fatalf("after a failed run = %+v, want waiting", r)
	}
	// Renewed during the wait, the retry failing so waits again too.
	f.set(func() { f.exp = valid.Add(2 * time.Hour) })
	f.s.checkRetry(mon.Add(2*time.Hour + time.Minute))
	f.s.runs.Wait()
	if r := f.s.Status().Last; f.count() != 4 || r == nil || r.Trigger != TriggerRetry || r.State != StateWaiting {
		t.Fatalf("retry after a renewal: sessions %d, last %+v; want a waiting retry", f.count(), r)
	}
}

// The renewal baseline is the Midway expiry before the session started: a
// renewal while the failing session still runs is one the retry sees, for
// a run and for a pre-meeting brief.
func TestCredentialRenewedMidSession(t *testing.T) {
	f := newWaitFixture(t, false)
	mon := time.Date(2026, 11, 16, 8, 0, 0, 0, time.Local)
	f.set(func() { f.now, f.exp = mon, mon.Add(-time.Hour) })
	session := f.s.cfg.Session
	failing := true
	f.s.cfg.Session = func(ctx context.Context, key string, o sessionview.AgentOpts, p string) (string, []string, error) {
		if failing {
			failing = false
			// mwinit while the failing call is still on its way back.
			f.set(func() { f.exp, f.err = mon.Add(12*time.Hour), errCred })
			defer f.set(func() { f.err = nil })
		}
		return session(ctx, key, o, p)
	}
	id, err := f.s.start(TriggerSchedule)
	if err != nil {
		t.Fatal(err)
	}
	f.s.runs.Wait()
	if r := f.s.Status().Last; r == nil || r.State != StateWaiting || !r.Cred.Equal(mon.Add(-time.Hour)) {
		t.Fatalf("run = %+v, want waiting on the expiry before the session", r)
	}
	f.s.checkRetry(mon.Add(time.Minute))
	f.s.runs.Wait()
	if r := f.s.Status().Last; f.count() != 2 || r == nil || r.RetryOf != id || r.State != StateOK {
		t.Fatalf("sessions %d, last %+v; want the retry of %s at the next tick", f.count(), r, id)
	}

	start := time.Date(2026, 11, 16, 10, 0, 0, 0, time.Local)
	cal := []Event{{ID: "m1", Title: "iso 评审", Start: start, End: start.Add(time.Hour), Organizer: "a@x", Attendees: []string{"b@x"}, Prep: true}}
	if _, err := f.s.st.saveCalendar(&cal, start.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.s.cfg.Brief = func(func(int, error), ...string) error { return nil }
	f.set(func() {
		f.exp = start.Add(-time.Hour)
		f.reply = `{"why":"定方案","last":"","waiting":"","prepare":"","related":[]}`
	})
	failing = true
	check := func(at time.Time) { f.set(func() { f.now = at }); f.s.checkBrief(at); f.s.runs.Wait() }
	check(start.Add(-15 * time.Minute))
	if len(f.s.st.briefed()) != 0 {
		t.Fatalf("briefed %+v, want the brief waiting", f.s.st.briefed())
	}
	check(start.Add(-14 * time.Minute))
	if b := f.s.st.briefed(); f.count() != 4 || len(b) != 1 || b[0].State != StateOK {
		t.Fatalf("sessions %d, briefed %+v; want the brief run again at the next tick", f.count(), b)
	}
}

// Without a Midway session to watch, or for another failure, the run fails
// as before.
func TestCredentialWaitNeedsMidway(t *testing.T) {
	cases := []struct {
		name   string
		noCred bool
		exp    time.Duration // from now; 0 leaves no cookie
		err    error
	}{
		{"no CredExpiry", true, -time.Hour, errCred},
		{"no Midway cookie", false, 0, errCred},
		{"another failure", false, -time.Hour, errors.New("API Error: 500 internal server error")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newWaitFixture(t, c.noCred)
			now := time.Date(2026, 11, 16, 8, 0, 0, 0, time.Local)
			f.set(func() {
				f.now, f.err = now, c.err
				if c.exp != 0 {
					f.exp = now.Add(c.exp)
				}
			})
			if _, err := f.s.start(TriggerSchedule); err != nil {
				t.Fatal(err)
			}
			f.s.runs.Wait()
			if st := f.s.Status(); st.Last == nil || st.Last.State != StateFailed {
				t.Errorf("last run = %+v, want failed", st.Last)
			}
		})
	}
}

// A run its time limit cut short records what the session was doing: the
// tool calls in flight and how long each ran, and how long the run went.
func TestTimeoutRecorded(t *testing.T) {
	f := newWaitFixture(t, false)
	now := time.Date(2026, 11, 16, 10, 0, 29, 0, time.Local)
	te := &TimeoutError{
		Timeout: Timeout{Tools: []TimeoutTool{{Tool: "mcp__aws-outlook-mcp__calendar_view", ElapsedSec: 404}, {Tool: "mcp__slack-mcp__get_unread_counts", ElapsedSec: 404}}, QuietSec: 404},
		Err:     context.DeadlineExceeded,
	}
	f.set(func() { f.now = now })
	f.s.cfg.Session = func(context.Context, string, sessionview.AgentOpts, string) (string, []string, error) {
		f.set(func() { f.now = now.Add(runTimeout) })
		return "", []string{"mcp__aws-outlook-mcp__calendar_view"}, fmt.Errorf("cron: %w", te)
	}
	if _, err := f.s.start(TriggerSchedule); err != nil {
		t.Fatal(err)
	}
	f.s.runs.Wait()
	r := f.s.Status().Last
	if r == nil || r.State != StateFailed || r.Timeout == nil {
		t.Fatalf("last run = %+v, want failed with its timeout", r)
	}
	if r.Timeout.ElapsedSec != 600 || len(r.Timeout.Tools) != 2 || r.Timeout.Tools[0].ElapsedSec != 404 {
		t.Errorf("timeout = %+v", r.Timeout)
	}
	for _, want := range []string{"已用 10 分 0 秒", "aws-outlook-mcp__calendar_view（6 分 44 秒）", "deadline exceeded"} {
		if !strings.Contains(r.Error, want) {
			t.Errorf("error %q lacks %q", r.Error, want)
		}
	}
	// The model's own wait: no tool in flight.
	if got := (Timeout{ElapsedSec: 600, QuietSec: 75}).describe(); got != "超时（已用 10 分 0 秒）：没有工具在运行，模型 1 分 15 秒 没有输出" {
		t.Errorf("describe = %q", got)
	}
}

// A pre-meeting brief whose model call finds the credential expired waits
// for the renewal and runs then; one still waiting five minutes before its
// meeting gives up, recorded and journaled.
func TestBriefCredentialWait(t *testing.T) {
	f := newWaitFixture(t, false)
	start := time.Date(2026, 11, 16, 10, 0, 0, 0, time.Local)
	cal := []Event{{ID: "m1", Title: "iso 评审", Start: start, End: start.Add(time.Hour), Organizer: "a@x", Attendees: []string{"b@x"}, Prep: true}}
	if _, err := f.s.st.saveCalendar(&cal, start.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	j, err := journal.Open(filepath.Join(f.root, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	var blocks []string
	f.s.cfg.Journal = j
	f.s.cfg.Brief = func(_ func(int, error), b ...string) error { blocks = append(blocks, b...); return nil }
	check := func(at time.Time) { f.set(func() { f.now = at }); f.s.checkBrief(at); f.s.runs.Wait() }

	f.set(func() { f.exp, f.err = start.Add(-2*time.Hour), errCred })
	check(start.Add(-15 * time.Minute))
	check(start.Add(-14 * time.Minute))
	if n := f.count(); n != 1 || len(f.s.st.briefed()) != 0 {
		t.Fatalf("before the renewal: sessions %d, briefed %+v; want one try, no record", n, f.s.st.briefed())
	}
	f.set(func() {
		f.exp, f.err = start.Add(20*time.Hour), nil
		f.reply = `{"why":"定方案","last":"","waiting":"","prepare":"","related":[]}`
	})
	check(start.Add(-12 * time.Minute))
	if b := f.s.st.briefed(); f.count() != 2 || len(blocks) != 1 || len(b) != 1 || b[0].State != StateOK {
		t.Fatalf("after the renewal: sessions %d, blocks %d, briefed %+v", f.count(), len(blocks), b)
	}

	// Another meeting, the credential never renewed.
	start2 := start.Add(2 * time.Hour)
	cal = []Event{{ID: "m2", Title: "iso 周会", Start: start2, End: start2.Add(time.Hour), Organizer: "a@x", Attendees: []string{"b@x"}, Prep: true}}
	if _, err := f.s.st.saveCalendar(&cal, start2.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.set(func() { f.exp, f.err = start2.Add(-time.Hour), errCred })
	check(start2.Add(-15 * time.Minute))
	check(start2.Add(-6 * time.Minute))
	if n := f.count(); n != 3 || len(f.s.st.briefed()) != 1 {
		t.Fatalf("waiting: sessions %d, briefed %+v", n, f.s.st.briefed())
	}
	check(start2.Add(-5 * time.Minute))
	b := f.s.st.briefed()
	if f.count() != 3 || len(b) != 2 || b[1].State != StateFailed || b[1].Error != errBriefCred {
		t.Fatalf("given up: sessions %d, briefed %+v", f.count(), b)
	}
	check(start2.Add(-4 * time.Minute))
	if n := len(f.s.st.briefed()); n != 2 || f.count() != 3 {
		t.Errorf("after giving up: briefed %d, sessions %d", n, f.count())
	}

	// Asleep from the wait to past the meeting's start: given up all the same.
	start3 := start2.Add(2 * time.Hour)
	cal = []Event{{ID: "m3", Title: "iso 复盘", Start: start3, End: start3.Add(time.Hour), Organizer: "a@x", Attendees: []string{"b@x"}, Prep: true}}
	if _, err := f.s.st.saveCalendar(&cal, start3.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	check(start3.Add(-15 * time.Minute))
	check(start3.Add(time.Minute))
	if b := f.s.st.briefed(); f.count() != 4 || len(b) != 3 || b[2].ID != "m3" || b[2].Error != errBriefCred {
		t.Fatalf("slept past: sessions %d, briefed %+v", f.count(), b)
	}
	entries, err := j.Read(time.Time{}, start3.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if strings.Contains(string(e.Data), errBriefCred) {
			n++
		}
	}
	if n != 2 {
		t.Errorf("journal holds %d give-ups, want 2: %+v", n, entries)
	}
}

func TestNextSlot(t *testing.T) {
	at := func(day, h, m int) time.Time { return time.Date(2026, 11, day, h, m, 0, 0, time.Local) } // 11-16 is a Monday
	for _, c := range []struct{ t, want time.Time }{
		{at(16, 8, 0), at(16, 8, 15)},
		{at(16, 8, 15), at(16, 10, 0)},
		{at(16, 7, 0), at(16, 8, 0)},
		{at(16, 20, 0), at(17, 8, 0)},
		{at(20, 20, 30), at(23, 8, 0)}, // Friday night: Monday
		{at(21, 9, 0), at(23, 8, 0)},
	} {
		if got := nextSlot(c.t); !got.Equal(c.want) {
			t.Errorf("nextSlot(%s) = %s, want %s", c.t, got, c.want)
		}
	}
}
