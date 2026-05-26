package server

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestSanitisePlannerPromptForSpawn_ServerCanReachIt pins R215-SEC-P1-2
// (#535): handlePlannerRestart's legacy fallback (h.resolver==nil) used
// to feed EffectivePlannerPrompt straight into AgentOpts.ExtraArgs,
// bypassing the spawn-boundary sanitiser the resolver path enforces.
//
// The fix exports session.SanitisePlannerPromptForSpawn so the server
// fallback can re-use the same policy without rebuilding it. This test
// is the cross-package smoke test that the export is grep-able from
// the server package and rejects the same classes (oversize / control
// bytes / injection runes / invalid UTF-8) that the resolver path
// rejects.
func TestSanitisePlannerPromptForSpawn_ServerCanReachIt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty passes empty", "", ""},
		{"plain prompt passes", "be a helpful planner", "be a helpful planner"},
		{"tab and newline allowed (legitimate markdown)", "step 1\n\tdo this", "step 1\n\tdo this"},
		{"NUL rejected", "evil\x00prompt", ""},
		{"escape rejected", "evil\x1bprompt", ""},
		{"DEL rejected", "evil\x7fprompt", ""},
		{"C1 NEL rejected (injection rune)", "evilprompt", ""},
		{"oversize rejected (> 8 KB)", strings.Repeat("a", 8*1024+1), ""},
		{"invalid UTF-8 rejected", "\xc3\x28suffix", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := session.SanitisePlannerPromptForSpawn(c.in, "demo")
			if got != c.want {
				t.Errorf("Sanitise(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
