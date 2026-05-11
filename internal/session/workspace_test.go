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
	t.Parallel()
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
		// UTF-8-encoded control / bidi / invalid runes: byte-only scan
		// misses these entirely, so the pre-fix validator would accept
		// them. With IsLogInjectionRune + utf8.ValidString the boundary
		// is aligned with session.ValidateUserLabel (label.go:47) so a
		// workspace path cannot smuggle characters that corrupt slog
		// attrs / dashboard display / journalctl rendering.
		{"C1 NEL U+0085", "/home/\xc2\x85user/proj", true},
		{"bidi override LRE U+202A", "/home/\xe2\x80\xaauser/proj", true},
		{"bidi override RLO U+202E", "/home/\xe2\x80\xaeuser/proj", true},
		{"bidi isolate LRI U+2066", "/home/\xe2\x81\xa6user/proj", true},
		{"LS U+2028", "/home/\xe2\x80\xa8user/proj", true},
		{"PS U+2029", "/home/\xe2\x80\xa9user/proj", true},
		{"invalid utf-8", "/home/\xff\xfe/proj", true},
		{"legal CJK path passes", "/home/用户/项目", false},
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
