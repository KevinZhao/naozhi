package envpolicy

import (
	"strings"
	"testing"
)

// TestIsSafeProfileValue covers the allow/deny boundary for AWS profile names
// (RFC §4). R20260603000023-SEC-1 (#1617).
func TestIsSafeProfileValue(t *testing.T) {
	valid := []string{
		"default",
		"my-profile",
		"my_profile_1",
		"ALLCAPS",
		"a",
		"abcdefghijklmnopqrstuvwxyz0123456789-_ABCDE", // 44 chars
		strings.Repeat("a", 64),                       // boundary: exactly 64
	}
	for _, v := range valid {
		if !IsSafeProfileValue(v) {
			t.Errorf("IsSafeProfileValue(%q) = false, want true", v)
		}
	}

	invalid := []string{
		"",
		"has space",
		"semi;colon",
		"new\nline",
		"../../etc/passwd",
		"$(cmd)",
		"profile|pipe",
		"profile`backtick`",
		"slash/path",
		strings.Repeat("a", 65), // boundary: too long
	}
	for _, v := range invalid {
		if IsSafeProfileValue(v) {
			t.Errorf("IsSafeProfileValue(%q) = true, want false", v)
		}
	}
}
