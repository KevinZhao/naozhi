package osutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpandHome(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir:", err)
	}
	cases := []struct{ in, want string }{
		{"/absolute/path", "/absolute/path"},
		{"relative/path", "relative/path"},
		{"", ""},
		// "~" alone (without slash) is NOT expanded per the HasPrefix("~/") guard.
		{"~", "~"},
		{"~/some/subdir", filepath.Join(home, "some/subdir")},
		// filepath.Join strips the trailing slash.
		{"~/", filepath.Join(home, "")},
	}
	for _, tc := range cases {
		if got := ExpandHome(tc.in); got != tc.want {
			t.Errorf("ExpandHome(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
