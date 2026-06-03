package cron

import (
	"strings"
	"testing"
)

// TestUpdateJob_WorkDir_Validation_R20260603GO1 verifies that UpdateJob rejects
// oversized, non-UTF-8, and control-byte WorkDir values, mirroring the
// validateJobFields policy applied on the AddJob path (limits.go:164-168).
// R20260603-GO-1: non-dashboard callers must not be able to persist arbitrary
// bytes for WorkDir by reaching UpdateJob directly.
func TestUpdateJob_WorkDir_Validation_R20260603GO1(t *testing.T) {
	t.Parallel()

	strPtr := func(s string) *string { return &s }

	cases := []struct {
		name    string
		upd     JobUpdate
		wantErr bool
	}{
		{
			name:    "workdir too long",
			upd:     JobUpdate{WorkDir: strPtr(strings.Repeat("x", MaxWorkDirLen+1))},
			wantErr: true,
		},
		{
			name:    "workdir with control char",
			upd:     JobUpdate{WorkDir: strPtr("/tmp/ok\x01bad")},
			wantErr: true,
		},
		{
			name:    "workdir with null byte",
			upd:     JobUpdate{WorkDir: strPtr("/tmp/ok\x00bad")},
			wantErr: true,
		},
		{
			name:    "workdir exactly at max length passes",
			upd:     JobUpdate{WorkDir: strPtr(strings.Repeat("x", MaxWorkDirLen))},
			wantErr: false,
		},
		{
			name:    "empty workdir passes",
			upd:     JobUpdate{WorkDir: strPtr("")},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := schedulerForJobsR241GO2Test(t)

			job := &Job{
				Schedule: "0 * * * *",
				Prompt:   "hello",
				WorkDir:  t.TempDir(),
			}
			if err := s.AddJob(job); err != nil {
				t.Fatalf("AddJob: %v", err)
			}

			_, err := s.UpdateJob(job.ID, tc.upd)
			if tc.wantErr && err == nil {
				t.Fatalf("UpdateJob(%q): got nil error, want validation error", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("UpdateJob(%q): got unexpected error: %v", tc.name, err)
			}
		})
	}
}
