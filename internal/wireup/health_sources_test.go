package wireup

import (
	"testing"

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

func TestCronFact(t *testing.T) {
	tests := []struct {
		name   string
		job    cron.Job
		failed bool
	}{
		{"never failed", cron.Job{}, false},
		{"manually paused", cron.Job{Paused: true, LastError: "x", LastErrorClass: cron.ErrClassTurnFailed}, false},
		{"auto-paused", cron.Job{Paused: true, PausedReason: cron.PausedReasonAutoFailures}, true},
		{"last run failed", cron.Job{LastError: "boom", LastErrorClass: cron.ErrClassTurnFailed}, true},
		{"last run skipped", cron.Job{LastError: "busy", LastErrorClass: cron.ErrClassOverlapSkipped}, false},
	}
	for _, tt := range tests {
		tt.job.ID, tt.job.Schedule = "j", "@every 1h"
		f := cronFact(tt.job, false, tt.job.CreatedAt, tt.job.CreatedAt)
		if got := f.Failed != ""; got != tt.failed {
			t.Errorf("%s: failed = %v (%q), want %v", tt.name, got, f.Failed, tt.failed)
		}
		if f.Kind != health.KindCron || f.ID != "cron:j" {
			t.Errorf("%s: fact = %+v", tt.name, f)
		}
	}
}
