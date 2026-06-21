package cron

import (
	"path/filepath"
	"testing"
)

// TestStateSubtree pins #2175: stateSubtree returns "" when persistence is
// disabled, and otherwise produces exactly the legacy
// filepath.Join(filepath.Dir(storePath), parts...) for each of the five
// open-coded call shapes it replaced (golden equivalence).
func TestStateSubtree(t *testing.T) {
	t.Parallel()

	const storePath = "/var/lib/naozhi/cron_jobs.json"
	storeDir := filepath.Dir(storePath)

	tests := []struct {
		name      string
		storePath string
		parts     []string
		want      string
	}{
		{
			name:      "storeless returns empty",
			storePath: "",
			parts:     []string{"sandboxpending"},
			want:      "",
		},
		{
			name:      "storeless empty with multi-part",
			storePath: "",
			parts:     []string{"sandboxevents", "0123456789abcdef"},
			want:      "",
		},
		{
			name:      "single part: sandboxpending",
			storePath: storePath,
			parts:     []string{"sandboxpending"},
			want:      filepath.Join(storeDir, "sandboxpending"),
		},
		{
			name:      "single part: sandboxattention",
			storePath: storePath,
			parts:     []string{"sandboxattention"},
			want:      filepath.Join(storeDir, "sandboxattention"),
		},
		{
			name:      "single part: runsnapshots",
			storePath: storePath,
			parts:     []string{"runsnapshots"},
			want:      filepath.Join(storeDir, "runsnapshots"),
		},
		{
			name:      "multi part: sandboxevents/<jobID>",
			storePath: storePath,
			parts:     []string{"sandboxevents", "0123456789abcdef"},
			want:      filepath.Join(storeDir, "sandboxevents", "0123456789abcdef"),
		},
		{
			name:      "multi part: sandboxevents/<jobID>/<runID>.ndjson",
			storePath: storePath,
			parts:     []string{"sandboxevents", "0123456789abcdef", "feedfacefeedface.ndjson"},
			want:      filepath.Join(storeDir, "sandboxevents", "0123456789abcdef", "feedfacefeedface.ndjson"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Scheduler{storePath: tc.storePath}
			got := s.stateSubtree(tc.parts...)
			if got != tc.want {
				t.Fatalf("stateSubtree(%v) = %q, want %q", tc.parts, got, tc.want)
			}
			// Golden equivalence to the legacy open-coded form (skip the
			// storeless case, whose legacy form was the early-return "").
			if tc.storePath != "" {
				legacy := filepath.Join(append([]string{filepath.Dir(tc.storePath)}, tc.parts...)...)
				if got != legacy {
					t.Fatalf("stateSubtree drift from legacy filepath.Join: got %q, legacy %q", got, legacy)
				}
			}
		})
	}
}
