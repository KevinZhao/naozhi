package textutil

import (
	"errors"
	"strings"
	"testing"
)

// TestValidateScheduleChars covers the shared schedule-char policy that the IM
// dispatch.ParseCronAdd edge and the dashboard validateCronScheduleChars edge
// delegate to. Migrated from internal/cron under #1707.
func TestValidateScheduleChars(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"every", "@every 30m", false},
		{"cron expr", "0 9 * * 1-5", false},
		{"non-ascii desc ok", "0 9 * * * 北京", false},
		{"empty rejected", "", true},
		{"too long", strings.Repeat("a", MaxScheduleBytes+1), true},
		{"null byte", "@every \x00 30m", true},
		{"newline", "0 9 * *\n1-5", true},
		{"tab rejected", "0 9\t* * *", true},
		{"del", "0 9 * * *\x7f", true},
		{"bidi override", "0 9 * * *‮", true},
		{"line separator", "0 9 * * * ", true},
		{"invalid utf8", "0 9 * * *\xff", true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateScheduleChars(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateScheduleChars(%q) err=%v, wantErr=%v", tt.in, err, tt.wantErr)
			}
			if tt.wantErr && err != nil && !errors.Is(err, ErrInvalidSchedule) {
				t.Errorf("ValidateScheduleChars(%q) error %v not wrapping ErrInvalidSchedule", tt.in, err)
			}
		})
	}
}

// TestValidatePromptStrict covers the shared prompt policy. Migrated from
// internal/cron under #1707.
func TestValidatePromptStrict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"plain", "run the daily report", false},
		{"multiline tab allowed", "step 1\n\tstep 2\r\n", false},
		{"unicode body", "生成今天的简报", false},
		{"empty rejected", "", true},
		{"too long", strings.Repeat("a", MaxPromptBytes+1), true},
		{"null byte", "do \x00 thing", true},
		{"del", "do \x7f thing", true},
		{"c0 control", "do \x01 thing", true},
		{"bidi override", "do ‮ thing", true},
		{"line separator", "do   thing", true},
		{"invalid utf8", "do \xff thing", true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidatePromptStrict(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidatePromptStrict(%q) err=%v, wantErr=%v", tt.in, err, tt.wantErr)
			}
			if tt.wantErr && err != nil && !errors.Is(err, ErrInvalidPrompt) {
				t.Errorf("ValidatePromptStrict(%q) error %v not wrapping ErrInvalidPrompt", tt.in, err)
			}
		})
	}
}

// TestEscapeMarkdownPunct pins the link-smuggling neutralisation: the four
// markdown punctuation bytes map to their full-width counterparts, and
// punct-free input is returned unchanged. Migrated from internal/cron (#1707).
func TestEscapeMarkdownPunct(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"clean unchanged", "hello world", "hello world"},
		{"empty", "", ""},
		{"link smuggle", "[click](http://evil)", "［click］（http://evil）"},
		{"only brackets", "[]", "［］"},
		{"only parens", "()", "（）"},
		{"mixed", "a[b(c)d]e", "a［b（c）d］e"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := EscapeMarkdownPunct(tt.in); got != tt.want {
				t.Errorf("EscapeMarkdownPunct(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestMaxIDLen pins the migrated bound so a future edit can't silently shrink
// the cron job-ID cap shared by the IM and dashboard edges (#1707).
func TestMaxIDLen(t *testing.T) {
	t.Parallel()
	if MaxIDLen != 64 {
		t.Errorf("MaxIDLen = %d, want 64", MaxIDLen)
	}
}
