package claudefs

// Session-ID validation and the project-directory encoding, moved with the
// implementation out of internal/discovery (#2643).

import "testing"

func TestIsValidSessionID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"valid lowercase", "550e8400-e29b-41d4-a716-446655440000", true},
		{"valid v4 style", "00000000-0000-4000-8000-000000000000", true},
		{"empty", "", false},
		{"no hyphens", "550e8400e29b41d4a716446655440000", false},
		{"too short", "550e8400-e29b-41d4-a716-44665544000", false},
		{"uppercase", "550E8400-E29B-41D4-A716-446655440000", false},
		{"extra char", "550e8400-e29b-41d4-a716-4466554400001", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := IsValidSessionID(tc.input)
			if got != tc.want {
				t.Errorf("IsValidSessionID(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestProjectSlug(t *testing.T) {
	t.Parallel()
	tests := []struct {
		cwd  string
		want string
	}{
		{"/home/user/workspace/foo", "-home-user-workspace-foo"},
		{"/tmp", "-tmp"},
		{"", ""},
		// Folded in when internal/session's one-line wrapper was deleted: the
		// wrapper had no production callers left and its two tests only compared
		// it to this function. These inputs were the part worth keeping.
		{"/", "-"},
		{"/home/user/", "-home-user-"},
		{"relative/path", "relative-path"},
		{"//double//slash//", "--double--slash--"},
		{"/with spaces/in path", "-with-spaces-in-path"},
	}
	for _, tc := range tests {
		t.Run(tc.cwd, func(t *testing.T) {
			t.Parallel()
			got := ProjectSlug(tc.cwd)
			if got != tc.want {
				t.Errorf("ProjectSlug(%q) = %q, want %q", tc.cwd, got, tc.want)
			}
		})
	}
}
