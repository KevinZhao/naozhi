package dispatch

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
)

// TestHandleCronMutations_AmbiguousPrefix_EndToEnd drives the slash-command
// handlers through a Dispatcher wired with a fake scheduler whose mutation
// errors classify as CronCodeAmbiguousPrefix, asserting the user-facing
// reply is the disambiguation hint — not the old "请确认 ID 正确" text.
func TestHandleCronMutations_AmbiguousPrefix_EndToEnd(t *testing.T) {
	lg := slog.Default()
	msg := platform.IncomingMessage{Platform: "feishu", ChatID: "c1"}
	ambiguous := errors.New("ambiguous prefix")

	type handler func(*Dispatcher, platform.IncomingMessage, []string, func(string), *slog.Logger)
	steps := []struct {
		name string
		sub  string
		set  func(*fakeCronScheduler)
		call handler
	}{
		{
			name: "del",
			sub:  "del",
			set:  func(f *fakeCronScheduler) { f.deleteJobErr = ambiguous },
			call: func(d *Dispatcher, m platform.IncomingMessage, p []string, r func(string), l *slog.Logger) {
				d.handleCronDel(m, p, r, l)
			},
		},
		{
			name: "pause",
			sub:  "pause",
			set:  func(f *fakeCronScheduler) { f.pauseJobErr = ambiguous },
			call: func(d *Dispatcher, m platform.IncomingMessage, p []string, r func(string), l *slog.Logger) {
				d.handleCronPause(m, p, r, l)
			},
		},
		{
			name: "resume",
			sub:  "resume",
			set:  func(f *fakeCronScheduler) { f.resumeJobErr = ambiguous },
			call: func(d *Dispatcher, m platform.IncomingMessage, p []string, r func(string), l *slog.Logger) {
				d.handleCronResume(m, p, r, l)
			},
		},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			fake := &fakeCronScheduler{classifyResult: CronCodeAmbiguousPrefix}
			st.set(fake)
			d := &Dispatcher{scheduler: fake}
			var reply string
			st.call(d, msg, []string{"/cron", st.sub, "ab"}, func(s string) { reply = s }, lg)
			if !strings.Contains(reply, "更长的 ID") {
				t.Fatalf("%s ambiguous reply = %q, want disambiguation hint", st.name, reply)
			}
			if strings.Contains(reply, "请确认 ID 正确") {
				t.Fatalf("%s reply leaked misleading 'ID 正确' text: %q", st.name, reply)
			}
		})
	}
}
