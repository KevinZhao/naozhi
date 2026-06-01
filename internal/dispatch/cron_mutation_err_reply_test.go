package dispatch

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/platform"
)

// TestCronMutationErrReply_ClassifiesError pins R20260531-ARCH-2: /cron
// del|pause|resume must surface a specific, action-appropriate reply derived
// from cron.ClassifyError rather than collapsing every failure into a single
// "请确认 ID 正确" string. In particular an ambiguous-prefix match must tell
// the user to type a longer ID, not that the ID is wrong.
func TestCronMutationErrReply_ClassifiesError(t *testing.T) {
	cases := []struct {
		name    string
		verb    string
		err     error
		wantSub string
		notWant string // a substring that the misleading collapsed reply had
	}{
		{
			name:    "ambiguous_prefix",
			verb:    "删除",
			err:     cron.ErrAmbiguousPrefix,
			wantSub: "更长的 ID",
			notWant: "请确认 ID 正确",
		},
		{
			name:    "already_paused",
			verb:    "暂停",
			err:     cron.ErrJobAlreadyPaused,
			wantSub: "已处于暂停状态",
		},
		{
			name:    "not_paused",
			verb:    "恢复",
			err:     cron.ErrJobNotPaused,
			wantSub: "未处于暂停状态",
		},
		{
			name:    "not_found",
			verb:    "删除",
			err:     cron.ErrJobNotFound,
			wantSub: "未找到",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cronMutationErrReply(tc.verb, tc.err)
			if !strings.Contains(got, tc.wantSub) {
				t.Fatalf("reply = %q, want substring %q", got, tc.wantSub)
			}
			if tc.notWant != "" && strings.Contains(got, tc.notWant) {
				t.Fatalf("reply = %q must not contain misleading %q", got, tc.notWant)
			}
		})
	}
}

// TestHandleCronMutations_AmbiguousPrefix_EndToEnd drives the slash-command
// handlers through a Dispatcher wired with a fake scheduler that returns
// ErrAmbiguousPrefix, asserting the user-facing reply is the disambiguation
// hint — not the old "请确认 ID 正确" text.
func TestHandleCronMutations_AmbiguousPrefix_EndToEnd(t *testing.T) {
	lg := slog.Default()
	msg := platform.IncomingMessage{Platform: "feishu", ChatID: "c1"}

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
			set:  func(f *fakeCronScheduler) { f.deleteJobErr = cron.ErrAmbiguousPrefix },
			call: func(d *Dispatcher, m platform.IncomingMessage, p []string, r func(string), l *slog.Logger) {
				d.handleCronDel(m, p, r, l)
			},
		},
		{
			name: "pause",
			sub:  "pause",
			set:  func(f *fakeCronScheduler) { f.pauseJobErr = cron.ErrAmbiguousPrefix },
			call: func(d *Dispatcher, m platform.IncomingMessage, p []string, r func(string), l *slog.Logger) {
				d.handleCronPause(m, p, r, l)
			},
		},
		{
			name: "resume",
			sub:  "resume",
			set:  func(f *fakeCronScheduler) { f.resumeJobErr = cron.ErrAmbiguousPrefix },
			call: func(d *Dispatcher, m platform.IncomingMessage, p []string, r func(string), l *slog.Logger) {
				d.handleCronResume(m, p, r, l)
			},
		},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			fake := &fakeCronScheduler{}
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
