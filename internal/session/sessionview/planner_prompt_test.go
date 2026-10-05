package sessionview

import (
	"strings"
	"testing"
)

// TestSanitisePlannerPromptForSpawn_DirectFunction directly exercises
// the validator so a future caller (e.g. a third resolver branch) can
// reuse it without re-deriving the policy.
func TestSanitisePlannerPromptForSpawn_DirectFunction(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"normal", "hello", "hello"},
		{"oversize", strings.Repeat("a", MaxPlannerPromptBytesAtSpawn+1), ""},
		{"NUL", "x\x00y", ""},
		{"BEL", "x\x07y", ""},
		{"DEL", "x\x7fy", ""},
		{"ESC", "x\x1by", ""},
		{"C1 NEL", "x\u0085y", ""},
		{"invalid utf8", "\xc0", ""},
		{"bidi override", "x\u202ey", ""},
		{"tab + LF + CR allowed", "a\tb\nc\rd", "a\tb\nc\rd"},
		{"CJK allowed", "你好", "你好"},
	}
	for _, tc := range cases {
		got := SanitisePlannerPromptForSpawn(tc.in, "test")
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
