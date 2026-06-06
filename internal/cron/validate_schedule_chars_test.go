package cron

import (
	"errors"
	"strings"
	"testing"
)

// TestValidateScheduleChars covers the shared schedule-char policy that both
// the IM dispatch.ParseCronAdd edge and the dashboard validateCronScheduleChars
// edge now delegate to. R20260527122801-ARCH-3 (#1315).
func TestValidateScheduleChars(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"every", "@every 30m", false},
		{"cron expr", "0 9 * * 1-5", false},
		{"empty rejected", "", true}, // R20260531-QUAL-2: empty schedule is invalid, aligns with godoc
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

// TestValidateScheduleChars_EmptyRejected pins R20260531-QUAL-2: the godoc
// promises "Empty schedule is rejected here", so an empty string must return a
// wrapped ErrInvalidSchedule rather than passing every char check and returning
// nil.
func TestValidateScheduleChars_EmptyRejected(t *testing.T) {
	t.Parallel()
	err := ValidateScheduleChars("")
	if err == nil {
		t.Fatal("ValidateScheduleChars(\"\") = nil, want ErrInvalidSchedule")
	}
	if !errors.Is(err, ErrInvalidSchedule) {
		t.Errorf("ValidateScheduleChars(\"\") error %v not wrapping ErrInvalidSchedule", err)
	}
}
