package wireup

import (
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

func TestSessionFactFailure(t *testing.T) {
	tests := []struct {
		name   string
		snap   session.SessionSnapshot
		failed bool
	}{
		{"idle", session.SessionSnapshot{State: "ready"}, false},
		{"reclaimed for idling", session.SessionSnapshot{State: "ready", DeathReason: session.DeathReasonIdleTimeout}, false},
		{"evicted", session.SessionSnapshot{State: "dead", DeathReason: session.DeathReasonEvicted}, false},
		{"clean CLI exit", session.SessionSnapshot{State: "dead", DeathReason: "cli_exited"}, false},
		{"killed by naozhi", session.SessionSnapshot{State: "dead", DeathReason: "killed"}, false},
		{"watchdog", session.SessionSnapshot{State: "ready", DeathReason: "no_output_timeout"}, true},
		{"kill -9", session.SessionSnapshot{State: "dead", DeathReason: "cli_exited_code_-1"}, true},
		{"startup failures", session.SessionSnapshot{State: "dead", StartupFailure: &sessionview.StartupFailureView{Class: "auth", Streak: 2}}, true},
		{"resume dropped only", session.SessionSnapshot{State: "dead", StartupFailure: &sessionview.StartupFailureView{NewSession: true}}, false},
		{"API error reply", session.SessionSnapshot{State: "ready", LastResponse: "API Error: 403 permission_error"}, true},
		{"prose about an API error", session.SessionSnapshot{State: "ready", LastResponse: "The API Error: 403 you saw means…"}, false},
		{"API error mid-turn", session.SessionSnapshot{State: "running", LastResponse: "API Error: 429 rate_limit"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.snap.Key = "dashboard:direct:x:general"
			f := sessionFact(tt.snap, nil, "")
			if got := f.Failed != ""; got != tt.failed {
				t.Errorf("failed = %v (%q), want %v", got, f.Failed, tt.failed)
			}
		})
	}
}

func TestAskPending(t *testing.T) {
	ev := func(types ...string) []clievent.EventEntry {
		out := make([]clievent.EventEntry, len(types))
		for i, ty := range types {
			out[i].Type = ty
		}
		return out
	}
	tests := []struct {
		name    string
		entries []clievent.EventEntry
		want    bool
	}{
		{"no question", ev(clievent.KindUser, clievent.KindText, clievent.KindResult), false},
		{"asked, auto-rejected, turn ended", ev(clievent.KindUser, clievent.KindAskQuestion, clievent.KindToolResult, clievent.KindText, clievent.KindResult), true},
		{"answered", ev(clievent.KindAskQuestion, clievent.KindResult, clievent.KindUser, clievent.KindText, clievent.KindResult), false},
	}
	for _, tt := range tests {
		if got := askPending(tt.entries); got != tt.want {
			t.Errorf("%s: askPending = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestOpenTask(t *testing.T) {
	attached := time.UnixMilli(1000)
	start := func(id, typ string, at int64) clievent.EventEntry {
		return clievent.EventEntry{Type: clievent.KindTaskStart, TaskID: id, TaskType: typ, Summary: id + " task", Time: at}
	}
	ev := func(kind, id, status string, at int64) clievent.EventEntry {
		return clievent.EventEntry{Type: kind, TaskID: id, Status: status, Time: at}
	}
	tests := []struct {
		name    string
		entries []clievent.EventEntry
		running bool
		want    string
		seen    int64
	}{
		{"none", []clievent.EventEntry{{Type: clievent.KindText, Time: 2000}}, true, "", 0},
		{"background bash in a turn", []clievent.EventEntry{start("b1", "local_bash", 2000), {Type: clievent.KindText, Time: 3000}}, true, "b1 task", 3000},
		// A server left running after the turn ended is not judged.
		{"background bash in an idle session", []clievent.EventEntry{start("b1", "local_bash", 2000), {Type: clievent.KindResult, Time: 3000}}, false, "", 0},
		{"agent with progress", []clievent.EventEntry{start("a1", "local_agent", 2000), ev(clievent.KindTaskProgress, "a1", "", 4000)}, false, "a1 task", 4000},
		{"ended", []clievent.EventEntry{start("b1", "local_bash", 2000), ev(clievent.KindTaskDone, "b1", "completed", 3000)}, true, "", 0},
		{"ended by a patch", []clievent.EventEntry{start("a1", "local_agent", 2000), ev(clievent.KindTaskProgress, "a1", "killed", 3000)}, false, "", 0},
		{"the first still running", []clievent.EventEntry{start("b1", "local_bash", 2000), start("b2", "local_bash", 2500), ev(clievent.KindTaskDone, "b1", "", 3000)}, true, "b2 task", 3000},
		{"replayed from before the process", []clievent.EventEntry{start("b0", "local_bash", 500), {Type: clievent.KindText, Time: 2000}}, true, "", 0},
		{"a workflow is the board's", []clievent.EventEntry{start("w1", clievent.TaskTypeWorkflow, 2000)}, true, "", 0},
	}
	for _, tt := range tests {
		name, seen := openTask(tt.entries, attached, tt.running)
		if name != tt.want || (tt.want != "" && seen.UnixMilli() != tt.seen) {
			t.Errorf("%s: openTask = %q at %d, want %q at %d", tt.name, name, seen.UnixMilli(), tt.want, tt.seen)
		}
	}
}

func TestCronFact(t *testing.T) {
	now := time.Date(2026, 11, 4, 10, 30, 0, 0, time.UTC)
	ranAt := now.Add(-210 * time.Minute) // three hourly ticks ago
	tests := []struct {
		name    string
		job     cron.Job
		running bool
		since   time.Time
		failed  bool
		missed  bool
	}{
		{"never failed", cron.Job{}, false, time.Time{}, false, false},
		{"manually paused", cron.Job{Paused: true, LastError: "x", LastErrorClass: cron.ErrClassTurnFailed}, false, time.Time{}, false, false},
		{"auto-paused", cron.Job{Paused: true, PausedReason: cron.PausedReasonAutoFailures}, false, time.Time{}, true, false},
		{"last run failed", cron.Job{LastError: "boom", LastErrorClass: cron.ErrClassTurnFailed}, false, time.Time{}, true, false},
		{"last run skipped", cron.Job{LastError: "busy", LastErrorClass: cron.ErrClassOverlapSkipped}, false, time.Time{}, false, false},
		{"ticks went by without a run", cron.Job{LastRunAt: ranAt}, false, time.Time{}, false, true},
		{"a long run is not a miss", cron.Job{LastRunAt: ranAt}, true, time.Time{}, false, false},
		{"ticks slept through are not a miss", cron.Job{LastRunAt: ranAt}, false, now.Add(-45 * time.Minute), false, false},
	}
	for _, tt := range tests {
		tt.job.ID, tt.job.Schedule = "j", "0 * * * *"
		f := cronFact(tt.job, tt.running, now, time.Time{}, tt.since)
		if got := f.Failed != ""; got != tt.failed {
			t.Errorf("%s: failed = %v (%q), want %v", tt.name, got, f.Failed, tt.failed)
		}
		if got := !f.Missed.IsZero(); got != tt.missed {
			t.Errorf("%s: missed = %v (%v), want %v", tt.name, got, f.Missed, tt.missed)
		}
		if f.Kind != health.KindCron || f.ID != "cron:j" {
			t.Errorf("%s: fact = %+v", tt.name, f)
		}
	}
}
