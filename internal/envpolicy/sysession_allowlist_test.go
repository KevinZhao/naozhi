package envpolicy

import (
	"slices"
	"testing"
)

// The list is the one the two call sites in cmd/naozhi spelled out before it
// moved here; a change to it changes what every daemon's claude -p inherits.
func TestSysessionRunnerAllowlist(t *testing.T) {
	t.Parallel()
	want := []string{
		"ANTHROPIC_", "CLAUDE_", "AWS_",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "no_proxy",
	}
	got := SysessionRunnerAllowlist()
	if !slices.Equal(got, want) {
		t.Fatalf("SysessionRunnerAllowlist() = %v, want %v", got, want)
	}
	got[0] = "MUTATED"
	if SysessionRunnerAllowlist()[0] != "ANTHROPIC_" {
		t.Error("a caller's edit leaked into the next call; return a fresh slice")
	}
}
