package osutil

import (
	"strings"
	"testing"
)

func TestRedactAbsolutePaths(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"no path", "context deadline exceeded", "context deadline exceeded"},
		{"posix path with reason", "open /home/u/proj/x: permission denied", "open <path>: permission denied"},
		{"two posix paths", "copy /tmp/a to /var/b done", "copy <path> to <path> done"},
		{"windows drive backslash", `open C:\Users\bob\f: denied`, "open <path>: denied"},
		{"windows drive slash", "open C:/Users/bob/f done", "open <path> done"},
		{"tilde home", "workspace ~/proj missing", "workspace <path> missing"},
		{"bare root passes through", "error: / not a dir", "error: / not a dir"},
		{"tilde not at boundary stays", "weight ~5kg ok", "weight ~5kg ok"},
		{"path at end of string", "cannot stat /etc/passwd", "cannot stat <path>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactAbsolutePaths(tc.in); got != tc.want {
				t.Errorf("RedactAbsolutePaths(%q)\n  got  = %q\n  want = %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRedactAbsolutePathsInto_ReusesBuilder(t *testing.T) {
	var b strings.Builder
	RedactAbsolutePathsInto(&b, "open /tmp/x: boom")
	if got := b.String(); got != "open <path>: boom" {
		t.Fatalf("Into got %q", got)
	}
	// A second use after Reset must not carry over residual bytes.
	b.Reset()
	RedactAbsolutePathsInto(&b, "no path here")
	if got := b.String(); got != "no path here" {
		t.Fatalf("Into after reset got %q", got)
	}
}

func TestHasNoPathTrigger(t *testing.T) {
	if !HasNoPathTrigger("plain text no triggers") {
		t.Error("expected true for trigger-free string")
	}
	for _, s := range []string{"a/b", `a\b`, "a~b"} {
		if HasNoPathTrigger(s) {
			t.Errorf("expected false for %q", s)
		}
	}
}
