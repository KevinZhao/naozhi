package clievent

// Moved from internal/cli/process_extra_test.go with FormatToolInput and
// shortPath (#2649 G1-f). shortPath stays unexported; its only production callers
// are inside FormatToolInput, which is why it travelled with it.

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// FormatToolInput
// ---------------------------------------------------------------------------

func TestFormatToolInput(t *testing.T) {
	tests := []struct {
		tool, input, want string
	}{
		{"Read", `{"file_path":"/home/user/proj/main.go"}`, "Read ~/proj/main.go"},
		{"Write", `{"file_path":"/a/b/c.go"}`, "Write /a/b/c.go"},
		{"Edit", `{"file_path":"/a/b/c.go"}`, "Edit /a/b/c.go"},
		{"Glob", `{"pattern":"*.go"}`, "Glob *.go"},
		{"Grep", `{"pattern":"TODO","path":"/src"}`, "Grep TODO in /src"},
		{"Bash", `{"description":"run tests"}`, "Bash run tests"},
		{"Bash", `{"command":"go test ./..."}`, "Bash go test ./..."},
		{"Agent", `{"description":"review changes"}`, "Agent review changes"},
		{"UnknownTool", `{"description":"do it"}`, "UnknownTool do it"},
		// No matching key in known tool → fallback with truncated input
		{"Read", `{}`, "Read: {}"},
		// Empty input
		{"Read", `null`, "Read: null"},
	}
	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			got := FormatToolInput(tt.tool, json.RawMessage(tt.input))
			if got != tt.want {
				t.Errorf("FormatToolInput(%q, %q) = %q, want %q", tt.tool, tt.input, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// shortPath
// ---------------------------------------------------------------------------

func TestShortPath(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/home/alice/project/file.go", "~/project/file.go"},
		{"/home/alice/file.go", "~/file.go"},
		{"/var/log/app.log", "/var/log/app.log"},
		{strings.Repeat("a", 51), "..." + strings.Repeat("a", 47)},
	}
	for _, tt := range tests {
		if got := shortPath(tt.in); got != tt.want {
			t.Errorf("shortPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestShortPathRuneBoundary covers the non-/home long-path tail truncation
// branch with multi-byte (CJK) segments. The naive byte slice p[len(p)-47:]
// could begin mid-codepoint and emit invalid UTF-8; the fix snaps the tail
// start to a rune boundary. Regression for #1988 (R20260609-072532-LB-2).
func TestShortPathRuneBoundary(t *testing.T) {
	zhong := strings.Repeat("中", 20) // 60 bytes, no /home prefix, > 50
	tests := []struct {
		name string
		in   string
	}{
		// 3-byte runes: every potential cut point at len-47 lands mid-rune
		// for at least some of these depending on directory-name length.
		{"all_cjk", "/data/" + zhong + "/x.go"},
		{"mixed_cjk_tail", "/mnt/" + strings.Repeat("a", 30) + zhong + ".go"},
		{"opt_cjk", "/opt/" + strings.Repeat("世界", 15) + ".go"},
		{"malformed_home_no_slash", "/home" + zhong + zhong + ".go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shortPath(tt.in)
			if !utf8.ValidString(got) {
				t.Errorf("shortPath(%q) = %q produced invalid UTF-8", tt.in, got)
			}
			if !strings.HasPrefix(got, "...") {
				t.Errorf("shortPath(%q) = %q, expected \"...\" prefix for long path", tt.in, got)
			}
			// The rune-safe tail must remain a true suffix of the input.
			if !strings.HasSuffix(tt.in, strings.TrimPrefix(got, "...")) {
				t.Errorf("shortPath(%q) = %q tail is not a suffix of input", tt.in, got)
			}
		})
	}
}
