package backend

import (
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/claudefs"
)

// Each backend names the file its resume reads; one without a cheap probe
// has no ResumeTarget.
func TestResumeTarget_PerBackendLayout(t *testing.T) {
	t.Parallel()
	claude, kiro, codex := claudeProfile(), kiroProfile(), codexProfile()
	if got, want := claude.ResumeTarget("/ignored", "/c", "/w", "s1"), claudefs.SessionJSONL("/c", "/w", "s1"); got != want {
		t.Errorf("claude ResumeTarget = %q, want %q", got, want)
	}
	for _, c := range []struct{ claudeDir, workspace string }{{"", "/w"}, {"/c", ""}} {
		if got := claude.ResumeTarget("", c.claudeDir, c.workspace, "s1"); got != "" {
			t.Errorf("claude ResumeTarget(claudeDir=%q, workspace=%q) = %q, want no probe", c.claudeDir, c.workspace, got)
		}
	}
	if got, want := kiro.ResumeTarget("/k", "/c", "/w", "s1"), filepath.Join("/k", "s1.json"); got != want {
		t.Errorf("kiro ResumeTarget = %q, want %q", got, want)
	}
	if got := kiro.ResumeTarget("", "/c", "/w", "s1"); got != "" {
		t.Errorf("kiro ResumeTarget with no dir = %q, want no probe", got)
	}
	if codex.ResumeTarget != nil {
		t.Error("codex has a ResumeTarget; its rollouts have no cheap probe")
	}
}
