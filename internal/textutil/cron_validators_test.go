package textutil

import (
	"errors"
	"strings"
	"testing"
)

// TestValidateCronPromptStrict covers the shared cron-prompt policy migrated
// from internal/cron in R20260603140013-ARCH-1 (#1707).
func TestValidateCronPromptStrict(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "plain ascii", in: "check service status", wantErr: false},
		{name: "cjk", in: "检查服务状态", wantErr: false},
		{name: "tab newline cr allowed", in: "line1\n\tline2\r", wantErr: false},
		{name: "empty rejected", in: "", wantErr: true},
		{name: "oversize rejected", in: strings.Repeat("a", MaxCronPromptBytes+1), wantErr: true},
		{name: "at limit ok", in: strings.Repeat("a", MaxCronPromptBytes), wantErr: false},
		{name: "C0 control rejected", in: "bad\x01ctl", wantErr: true},
		{name: "DEL rejected", in: "bad\x7fctl", wantErr: true},
		{name: "invalid utf8 rejected", in: "bad\xffutf8", wantErr: true},
		{name: "bidi override rejected", in: "ok‮evil", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCronPromptStrict(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateCronPromptStrict(%q) err=%v, wantErr=%v", tt.in, err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidCronPrompt) {
				t.Fatalf("err %v is not ErrInvalidCronPrompt", err)
			}
		})
	}
}

// TestValidateCronScheduleChars covers the shared cron-schedule char policy.
func TestValidateCronScheduleChars(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "interval", in: "@every 30m", wantErr: false},
		{name: "cron expr", in: "0 9 * * 1-5", wantErr: false},
		{name: "empty rejected", in: "", wantErr: true},
		{name: "oversize rejected", in: strings.Repeat("*", MaxCronScheduleBytes+1), wantErr: true},
		{name: "at limit ok", in: strings.Repeat("*", MaxCronScheduleBytes), wantErr: false},
		{name: "tab rejected", in: "0 9\t* * *", wantErr: true},
		{name: "newline rejected", in: "0 9 * * *\n", wantErr: true},
		{name: "C0 control rejected", in: "0 9\x01* * *", wantErr: true},
		{name: "DEL rejected", in: "0 9\x7f* * *", wantErr: true},
		{name: "invalid utf8 rejected", in: "0 9\xff* * *", wantErr: true},
		{name: "bidi override rejected", in: "@every‮ 30m", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCronScheduleChars(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateCronScheduleChars(%q) err=%v, wantErr=%v", tt.in, err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidCronSchedule) {
				t.Fatalf("err %v is not ErrInvalidCronSchedule", err)
			}
		})
	}
}

// TestEscapeCronMarkdownPunct verifies link-syntax characters are swapped for
// full-width lookalikes while clean input is returned unchanged (aliased).
func TestEscapeCronMarkdownPunct(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "clean unchanged", in: "hello world 你好", want: "hello world 你好"},
		{name: "empty", in: "", want: ""},
		{name: "brackets", in: "[a]", want: "［a］"},
		{name: "parens", in: "(b)", want: "（b）"},
		{name: "full link smuggle", in: "click [here](http://evil)", want: "click ［here］（http://evil）"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EscapeCronMarkdownPunct(tt.in); got != tt.want {
				t.Fatalf("EscapeCronMarkdownPunct(%q)=%q, want %q", tt.in, got, tt.want)
			}
		})
	}
	// Idempotent: escaping an already-escaped string is a no-op.
	once := EscapeCronMarkdownPunct("[x](y)")
	if twice := EscapeCronMarkdownPunct(once); twice != once {
		t.Fatalf("not idempotent: %q -> %q", once, twice)
	}
}
