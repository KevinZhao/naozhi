package server

import "testing"

// Verifies the workspace-basename fallback used by /api/sessions when a
// session's workspace is not registered with ProjectManager. The fallback
// replaces the legacy "Other" sidebar bucket with a folder-named group so
// quick sessions land somewhere meaningful.
func TestWorkspaceFallbackName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"root", "/", ""},
		{"dot", ".", ""},
		{"simple", "/home/alice/scratch", "scratch"},
		{"trailing slash", "/home/alice/scratch/", "scratch"},
		{"nested", "/tmp/a/b/c", "c"},
		{"relative", "scratch", "scratch"},
		{"dotdir", "/var/.cache", ".cache"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := workspaceFallbackName(tt.in); got != tt.want {
				t.Errorf("workspaceFallbackName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
