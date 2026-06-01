package cron

import (
	"strings"
	"testing"
)

// TestUpdateJob_NotifyChatID_Validation_R171023_SEC_10 verifies that
// UpdateJob rejects oversized and control-byte NotifyChatID/NotifyPlatform
// values, mirroring the validateJobFields policy applied by AddJob.
// R171023-SEC-10: non-dashboard callers must not be able to persist
// arbitrary bytes for these fields by bypassing the HTTP edge validators.
func TestUpdateJob_NotifyChatID_Validation_R171023_SEC_10(t *testing.T) {
	t.Parallel()

	strPtr := func(s string) *string { return &s }

	cases := []struct {
		name string
		upd  JobUpdate
	}{
		{
			name: "NotifyChatID too long",
			upd:  JobUpdate{NotifyChatID: strPtr(strings.Repeat("x", MaxNotifyTargetLen+1))},
		},
		{
			name: "NotifyChatID control byte",
			upd:  JobUpdate{NotifyChatID: strPtr("ok\x01bad")},
		},
		{
			name: "NotifyPlatform too long",
			upd:  JobUpdate{NotifyPlatform: strPtr(strings.Repeat("p", MaxNotifyTargetLen+1))},
		},
		{
			name: "NotifyPlatform control byte",
			upd:  JobUpdate{NotifyPlatform: strPtr("ok\x02bad")},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := schedulerForJobsR241GO2Test(t)

			// Add a valid job first so UpdateJob has something to patch.
			job := &Job{
				Schedule: "0 * * * *",
				Prompt:   "hello",
				WorkDir:  t.TempDir(),
			}
			if err := s.AddJob(job); err != nil {
				t.Fatalf("AddJob: %v", err)
			}

			_, err := s.UpdateJob(job.ID, tc.upd)
			if err == nil {
				t.Fatalf("UpdateJob(%q) with invalid notify field: got nil error, want validation error", tc.name)
			}
		})
	}
}
