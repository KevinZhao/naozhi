package cron

import (
	"errors"
	"strings"
	"testing"
)

// TestValidateJobFields_AcceptsClean confirms zero values and well-formed
// fields pass. Mirrors the dashboard create-flow expectation that optional
// WorkDir/Backend/Notify* are accepted when empty. R250-CR-8 (#1141).
func TestValidateJobFields_AcceptsClean(t *testing.T) {
	cases := []Job{
		{}, // all-empty
		{WorkDir: "/tmp/work", Backend: "claude"},
		{NotifyPlatform: "feishu", NotifyChatID: "oc_abc123"},
		{
			WorkDir:        "/srv/work",
			Backend:        "kiro",
			NotifyPlatform: "feishu",
			NotifyChatID:   "oc_xyz",
		},
	}
	for i, j := range cases {
		if err := validateJobFields(&j); err != nil {
			t.Errorf("case %d: clean job rejected: %v", i, err)
		}
	}
}

// TestValidateJobFields_RejectsOversize checks each field's byte cap rejects
// a oversize value. Covers the defence-in-depth contract that AddJob does
// not propagate multi-KB strings into cron_jobs.json. R250-CR-8 (#1141).
func TestValidateJobFields_RejectsOversize(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Job)
		wantSub string
	}{
		{
			name:    "WorkDir over cap",
			mutate:  func(j *Job) { j.WorkDir = strings.Repeat("a", MaxWorkDirLen+1) },
			wantSub: "work_dir too long",
		},
		{
			name:    "Backend over cap",
			mutate:  func(j *Job) { j.Backend = strings.Repeat("b", MaxBackendLen+1) },
			wantSub: "backend too long",
		},
		{
			name:    "NotifyPlatform over cap",
			mutate:  func(j *Job) { j.NotifyPlatform = strings.Repeat("p", MaxNotifyTargetLen+1) },
			wantSub: "notify_platform too long",
		},
		{
			name:    "NotifyChatID over cap",
			mutate:  func(j *Job) { j.NotifyChatID = strings.Repeat("c", MaxNotifyTargetLen+1) },
			wantSub: "notify_chat_id too long",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			j := &Job{}
			tc.mutate(j)
			err := validateJobFields(j)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestValidateJobFields_RejectsControlBytes ensures unsafe control bytes
// (NUL / DEL / bidi-override) cannot reach cron_jobs.json via AddJob even
// though loadJobs already drops them on the read path. The write-side guard
// keeps the in-memory map clean from the moment AddJob accepts the job.
// R250-CR-8 (#1141).
func TestValidateJobFields_RejectsControlBytes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Job)
	}{
		{"WorkDir NUL", func(j *Job) { j.WorkDir = "/tmp/a\x00b" }},
		{"Backend DEL", func(j *Job) { j.Backend = "claude\x7f" }},
		{"NotifyPlatform bidi", func(j *Job) { j.NotifyPlatform = "feishu‮" }},
		{"NotifyChatID C0", func(j *Job) { j.NotifyChatID = "oc_\x01abc" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			j := &Job{}
			tc.mutate(j)
			if err := validateJobFields(j); err == nil {
				t.Fatalf("expected control-byte rejection, got nil")
			}
		})
	}
}

// TestValidateJobFields_RejectsOverlongTitle confirms R20260607-ARCH-3 (#1927):
// Title rune-cap validation was folded into validateJobFields so a caller
// reaching it directly (not via AddJob's inline check) still gets the full
// policy. A Title exceeding MaxCronTitleLen runes must be rejected.
func TestValidateJobFields_RejectsOverlongTitle(t *testing.T) {
	j := &Job{Title: strings.Repeat("a", MaxCronTitleLen+1)}
	err := validateJobFields(j)
	if err == nil {
		t.Fatalf("expected title-too-long rejection, got nil")
	}
	if !strings.Contains(err.Error(), "title too long") {
		t.Errorf("error %q does not mention title too long", err.Error())
	}
	// Exactly cap runes is allowed (inclusive bound).
	if err := validateJobFields(&Job{Title: strings.Repeat("a", MaxCronTitleLen)}); err != nil {
		t.Errorf("title at exactly cap rejected: %v", err)
	}
}

// TestValidateJobFields_RejectsUnsafePrompt confirms R20260607-ARCH-3 (#1927):
// Prompt strict validation (R244-SEC-P2-5 / #889) was folded into
// validateJobFields. A non-empty prompt carrying log-injection / control
// bytes must be rejected, while an empty prompt (paused-with-empty-prompt
// dashboard state) stays allowed.
func TestValidateJobFields_RejectsUnsafePrompt(t *testing.T) {
	// Empty prompt is permitted (paused-with-empty-prompt create state).
	if err := validateJobFields(&Job{Prompt: ""}); err != nil {
		t.Errorf("empty prompt rejected: %v", err)
	}
	// A clean prompt passes.
	if err := validateJobFields(&Job{Prompt: "summarise the inbox"}); err != nil {
		t.Errorf("clean prompt rejected: %v", err)
	}
	// An oversized prompt is rejected by the strict validator.
	over := &Job{Prompt: strings.Repeat("x", MaxPromptBytes+1)}
	if err := validateJobFields(over); err == nil {
		t.Fatalf("expected oversize prompt rejection, got nil")
	} else if !errors.Is(err, ErrInvalidPrompt) {
		t.Errorf("oversize prompt error %v is not ErrInvalidPrompt", err)
	}
	// A control-byte (NUL) prompt is rejected.
	if err := validateJobFields(&Job{Prompt: "hi\x00there"}); err == nil {
		t.Fatalf("expected control-byte prompt rejection, got nil")
	}
}
