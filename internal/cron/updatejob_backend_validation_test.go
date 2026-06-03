package cron

import (
	"strings"
	"testing"
)

// TestUpdateJob_Backend_Validation_R20260603CR2 verifies that UpdateJob rejects
// oversized and invalid-character Backend values, mirroring the validateJobFields
// policy applied on the AddJob path (limits.go:170-173).
// R20260603-CR-2: non-dashboard callers must not be able to persist arbitrary
// bytes for Backend by reaching UpdateJob directly.
func TestUpdateJob_Backend_Validation_R20260603CR2(t *testing.T) {
	t.Parallel()

	strPtr := func(s string) *string { return &s }

	cases := []struct {
		name    string
		upd     JobUpdate
		wantErr bool
	}{
		{
			name:    "backend too long",
			upd:     JobUpdate{Backend: strPtr(strings.Repeat("x", MaxBackendLen+1))},
			wantErr: true,
		},
		{
			name:    "backend with control char",
			upd:     JobUpdate{Backend: strPtr("ok\x01bad")},
			wantErr: true,
		},
		{
			name:    "backend with null byte",
			upd:     JobUpdate{Backend: strPtr("ok\x00bad")},
			wantErr: true,
		},
		{
			name:    "valid backend passes",
			upd:     JobUpdate{Backend: strPtr("claude")},
			wantErr: false,
		},
		{
			name:    "valid backend with hyphens passes",
			upd:     JobUpdate{Backend: strPtr("my-backend_v2")},
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
