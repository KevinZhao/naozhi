package cron

import (
	"errors"
	"strings"
	"testing"
)

// TestUpdateJob_FieldValidation pins that UpdateJob guards every patchable
// string field itself. It does not go through validateJobFields (AddJob's
// path), so each field needs its own row here: a field dropped from UpdateJob's
// checks would let a non-dashboard caller persist arbitrary bytes
// (R20260603-GO-1 work_dir, R20260603-CR-2 backend, R171023-SEC-10 notify
// target, #889 prompt).
func TestUpdateJob_FieldValidation(t *testing.T) {
	t.Parallel()
	str := func(s string) *string { return &s }
	cases := []struct {
		name    string
		upd     JobUpdate
		wantErr bool
		// wantIs / wantMsg, when set, further pin the rejection.
		wantIs  error
		wantMsg string
	}{
		{name: "workdir too long", upd: JobUpdate{WorkDir: str(strings.Repeat("x", MaxWorkDirLen+1))}, wantErr: true},
		{name: "workdir control char", upd: JobUpdate{WorkDir: str("/tmp/ok\x01bad")}, wantErr: true},
		{name: "workdir null byte", upd: JobUpdate{WorkDir: str("/tmp/ok\x00bad")}, wantErr: true},
		{name: "workdir at max length", upd: JobUpdate{WorkDir: str(strings.Repeat("x", MaxWorkDirLen))}},
		{name: "workdir empty", upd: JobUpdate{WorkDir: str("")}},
		{name: "backend too long", upd: JobUpdate{Backend: str(strings.Repeat("x", MaxBackendLen+1))}, wantErr: true},
		{name: "backend control char", upd: JobUpdate{Backend: str("ok\x01bad")}, wantErr: true},
		{name: "backend null byte", upd: JobUpdate{Backend: str("ok\x00bad")}, wantErr: true},
		{name: "backend plain", upd: JobUpdate{Backend: str("claude")}},
		{name: "backend with hyphen and underscore", upd: JobUpdate{Backend: str("my-backend_v2")}},
		{name: "notify chat id too long", upd: JobUpdate{NotifyChatID: str(strings.Repeat("x", MaxNotifyTargetLen+1))}, wantErr: true},
		{name: "notify chat id control char", upd: JobUpdate{NotifyChatID: str("ok\x01bad")}, wantErr: true},
		{name: "notify platform too long", upd: JobUpdate{NotifyPlatform: str(strings.Repeat("p", MaxNotifyTargetLen+1))}, wantErr: true},
		{name: "notify platform control char", upd: JobUpdate{NotifyPlatform: str("ok\x02bad")}, wantErr: true},
		{name: "title too long", upd: JobUpdate{Title: str(strings.Repeat("a", MaxCronTitleLen+1))}, wantErr: true, wantMsg: "title too long"},
		{name: "title control char", upd: JobUpdate{Title: str("ok\x01bad")}, wantErr: true},
		{name: "prompt too long", upd: JobUpdate{Prompt: str(strings.Repeat("b", MaxPromptBytes+1))}, wantErr: true, wantIs: ErrInvalidPrompt},
		{name: "prompt control char", upd: JobUpdate{Prompt: str("ok\x01bad")}, wantErr: true, wantIs: ErrInvalidPrompt},
		{name: "prompt empty", upd: JobUpdate{Prompt: str("")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := schedulerForJobsR241GO2Test(t)
			job := &Job{Schedule: "0 * * * *", Prompt: "hello", WorkDir: t.TempDir()}
			if err := s.AddJob(job); err != nil {
				t.Fatalf("AddJob: %v", err)
			}
			_, err := s.UpdateJob(job.ID, tc.upd)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("UpdateJob: unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("UpdateJob: got nil error, want a validation error")
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("err = %v, want errors.Is(%v)", err, tc.wantIs)
			}
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("err = %v, want it to mention %q", err, tc.wantMsg)
			}
		})
	}
}

// TestAddJob_FieldValidationWired pins that AddJob runs validateJobFields
// (whose per-field policy validate_job_fields_test covers) before persisting.
func TestAddJob_FieldValidationWired(t *testing.T) {
	t.Parallel()
	s := schedulerForJobsR241GO2Test(t)
	if err := s.AddJob(&Job{Schedule: "@every 30m", Prompt: "test", Title: strings.Repeat("a", MaxCronTitleLen+1)}); err == nil || !strings.Contains(err.Error(), "title too long") {
		t.Errorf("over-long title: err = %v, want a title-length rejection", err)
	}
	if err := s.AddJob(&Job{Schedule: "@every 30m", Prompt: strings.Repeat("a", MaxPromptBytes+1)}); !errors.Is(err, ErrInvalidPrompt) {
		t.Errorf("over-long prompt: err = %v, want errors.Is(ErrInvalidPrompt)", err)
	}
	if n := len(s.mapKeysForTest()); n != 0 {
		t.Errorf("%d jobs persisted after rejected adds, want 0", n)
	}
}
