package session

import (
	"strings"
	"testing"
)

// TestValidateRemoteWorkspacePath locks down R68-SEC-M2: connector's
// reverse-RPC `send`/`takeover` must reject traversal, control bytes,
// relative paths, and oversized inputs BEFORE filepath.Clean silently
// folds `/home/../etc` into `/etc`.
func TestValidateRemoteWorkspacePath(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"empty", "", false},
		{"absolute", "/home/user/project", false},
		{"absolute with dot segment", "/home/./user", false},
		{"traversal in middle", "/home/../etc", true},
		{"traversal leading", "../etc/passwd", true},
		{"relative", "home/user/project", true},
		{"nul byte", "/home/user\x00proj", true},
		{"control byte LF", "/home/user\nproj", true},
		{"control byte ESC", "/home/user\x1bproj", true},
		{"control byte TAB", "/home/user\tproj", true},
		{"DEL byte", "/home/user\x7fproj", true},
		{"too long", "/" + strings.Repeat("a", MaxRemoteWorkspacePath), true},
		{"at length cap", "/" + strings.Repeat("a", MaxRemoteWorkspacePath-1), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRemoteWorkspacePath(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateRemoteWorkspacePath(%q) err=%v wantErr=%v", tc.input, err, tc.wantErr)
			}
		})
	}
}
