package dispatch

import (
	"log/slog"
	"strings"
	"testing"
)

// A leading --keep-context (or --keep) opts the job into a kept context. The
// dash glyphs and capitalization phone keyboards substitute are accepted, the
// flag combines with smart-quoted schedules, and only a token in front of the
// schedule counts: the same text inside the prompt is prompt text.
func TestParseCronAdd_KeepContextFlag(t *testing.T) {
	cases := []struct {
		name       string
		args       string
		wantKeep   bool
		wantPrompt string
	}{
		{"no flag", `"@every 30m" check`, false, "check"},
		{"long flag", `--keep-context "@every 30m" check`, true, "check"},
		{"short flag", `--keep "@every 30m" check`, true, "check"},
		{"em dash", "—keep-context \"@every 30m\" check", true, "check"},
		{"en dash", "–keep \"@every 30m\" check", true, "check"},
		{"full-width hyphen", "－－keep-context \"@every 30m\" check", true, "check"},
		{"minus sign", "\u2212\u2212keep \"@every 30m\" check", true, "check"},
		{"leading space, flag", ` --keep "@every 30m" check`, true, "check"},
		{"leading space, no flag", ` "@every 30m" check`, false, "check"},
		{"capitalized", `--Keep-Context "@every 30m" check`, true, "check"},
		{"smart quotes", "--keep-context “@every 30m” check", true, "check"},
		{"glued to quote", `--keep-context"@every 30m" check`, true, "check"},
		{"ideographic space", "--keep-context　\"@every 30m\" check", true, "check"},
		{"flag text inside prompt", `"@every 30m" --keep-context is prompt text`, false, "--keep-context is prompt text"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseCronAdd(c.args)
			if err != nil {
				t.Fatalf("ParseCronAdd(%q): %v", c.args, err)
			}
			if got.KeepContext != c.wantKeep {
				t.Errorf("KeepContext = %v, want %v", got.KeepContext, c.wantKeep)
			}
			if got.Schedule != "@every 30m" {
				t.Errorf("Schedule = %q, want %q", got.Schedule, "@every 30m")
			}
			if got.Prompt != c.wantPrompt {
				t.Errorf("Prompt = %q, want %q", got.Prompt, c.wantPrompt)
			}
		})
	}
}

// Any other dash-led token is reported as an unknown option instead of the
// misleading "schedule must be quoted", and a bare flag still needs a schedule.
func TestParseCronAdd_RejectsUnknownOrBareFlag(t *testing.T) {
	for _, args := range []string{
		`--fresh "@every 30m" check`,
		`-k "@every 30m" check`,
		`--keep-contexts "@every 30m" check`,
	} {
		_, err := ParseCronAdd(args)
		if err == nil || !strings.Contains(err.Error(), "未知选项") {
			t.Errorf("ParseCronAdd(%q) err = %v, want an unknown-option error", args, err)
		}
	}
	if _, err := ParseCronAdd(`--keep-context`); err == nil || !strings.Contains(err.Error(), "quoted") {
		t.Errorf("bare flag err = %v, want the quoted-schedule error", err)
	}
}

// IM jobs default to a fresh context; --keep-context flips the request, and
// the success reply names the mode the job actually got.
func TestHandleCronAdd_ContextMode(t *testing.T) {
	cases := []struct {
		args      string
		wantFresh bool
		wantNote  string
	}{
		{`"@every 30m" x`, true, "需要延续上次的上下文，请创建时加 --keep-context"},
		{`--keep-context "@every 30m" x`, false, "每次执行延续同一会话的上下文"},
	}
	for _, c := range cases {
		fake := &fakeCronScheduler{}
		d := newTestDispatcher(&fakePlatform{})
		d.scheduler = fake
		var got string
		d.handleCronAdd(incomingMsg("/cron add "+c.args),
			[]string{"/cron", "add", c.args}, func(s string) { got = s }, slog.Default())
		if fake.addJobCalls != 1 {
			t.Fatalf("%q: AddJob calls = %d, want 1 (reply %q)", c.args, fake.addJobCalls, got)
		}
		if fake.lastAddReq.FreshContext != c.wantFresh {
			t.Errorf("%q: request FreshContext = %v, want %v", c.args, fake.lastAddReq.FreshContext, c.wantFresh)
		}
		if !strings.Contains(got, c.wantNote) {
			t.Errorf("%q: reply = %q, want substring %q", c.args, got, c.wantNote)
		}
	}
}

// /cron list marks kept-context jobs, so a chat can tell which of its jobs
// carry a growing conversation; fresh jobs stay unmarked.
func TestHandleCronList_MarksKeptContext(t *testing.T) {
	d := newTestDispatcher(&fakePlatform{})
	d.scheduler = &fakeCronScheduler{listJobsResult: []CronJob{
		{ID: "fresh01", Schedule: "@hourly", Prompt: "a", FreshContext: true},
		{ID: "kept002", Schedule: "@hourly", Prompt: "b"},
		{ID: "kept003", Schedule: "@hourly", Prompt: "c", Paused: true},
	}}
	var got string
	d.handleCronList(incomingMsg("/cron list"), func(s string) { got = s })

	lines := map[string]string{}
	for _, l := range strings.Split(got, "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			lines[f[0]] = l
		}
	}
	if l := lines["fresh01"]; l == "" || strings.Contains(l, "[保留上下文]") {
		t.Errorf("fresh job line = %q, want present and unmarked", l)
	}
	if l := lines["kept002"]; !strings.HasSuffix(l, " [保留上下文]") {
		t.Errorf("kept job line = %q, want the [保留上下文] mark", l)
	}
	if l := lines["kept003"]; !strings.HasSuffix(l, " [保留上下文] [暂停]") {
		t.Errorf("paused kept job line = %q, want both marks", l)
	}
}
