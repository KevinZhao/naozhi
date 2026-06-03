package dispatch

import (
	"strings"
	"testing"
)

// TestParseCronAddArgs_EmptySchedule verifies that ParseCronAdd rejects a
// schedule consisting only of whitespace with a clear, dedicated error rather
// than letting it fall through to a confusing generic robfig parse error.
// (R20260603-GEN-4)
//
// These schedules are all-whitespace: they pass cron.ValidateScheduleChars
// (no forbidden characters) but are semantically empty, so the GEN-4 guard
// must reject them with the dedicated Chinese message. (Empty "" and tab are
// already rejected earlier by ValidateScheduleChars with different messages,
// so they are out of scope for this specific guard.)
func TestParseCronAddArgs_EmptySchedule(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args string
	}{
		{"single space", `" " run now`},
		{"multiple spaces", `"   " do thing`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			schedule, prompt, err := ParseCronAdd(tc.args)
			if err == nil {
				t.Fatalf("ParseCronAdd(%q) expected error for blank schedule, got schedule=%q prompt=%q", tc.args, schedule, prompt)
			}
			if !strings.Contains(err.Error(), "定时表达式不能为空") {
				t.Errorf("ParseCronAdd(%q) error = %q; want '定时表达式不能为空'", tc.args, err.Error())
			}
		})
	}
}

// TestParseCronAddArgs_NonBlankScheduleStillParses guards against the blank
// guard over-rejecting a legitimate schedule.
func TestParseCronAddArgs_NonBlankScheduleStillParses(t *testing.T) {
	t.Parallel()
	schedule, prompt, err := ParseCronAdd(`"@every 30m" ping the team`)
	if err != nil {
		t.Fatalf("ParseCronAdd valid input errored: %v", err)
	}
	if schedule != "@every 30m" {
		t.Errorf("schedule = %q, want %q", schedule, "@every 30m")
	}
	if prompt != "ping the team" {
		t.Errorf("prompt = %q, want %q", prompt, "ping the team")
	}
}
