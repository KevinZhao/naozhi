package subagent

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// With no resolvable home, claudeProjectsRoot is "" and the containment check
// used to degrade to HasPrefix(path, "/") — every absolute path accepted, so a
// tampered events/*.log could point agent_events streaming at any readable
// file (#2971). Before 448a73e0 the root was the relative ".claude/projects"
// and the same probe was refused; this pins the fail-closed behaviour back.
func TestLinker_SeedFromHistory_NoHome_RefusesEveryPath(t *testing.T) {
	// Not Parallel — t.Setenv redirects HOME for this test only. os.UserHomeDir
	// errors on an empty $HOME on every platform naozhi ships to.
	t.Setenv("HOME", "")
	if got := claudeProjectsRoot(); got != "" {
		t.Skipf("claudeProjectsRoot resolved to %q despite empty HOME; probe needs an empty root", got)
	}

	l := NewLinker()
	l.SeedFromHistory([]clievent.EventEntry{
		{Type: "task_start", TaskID: "t1", ToolUseID: "toolu_1", InternalAgentID: "a1", JSONLPath: "/etc/passwd", Subagent: "x"},
		{Type: "task_start", TaskID: "t2", InternalAgentID: "a2", JSONLPath: "/home/nobody/.claude/projects/-p/s/subagents/agent-a2.jsonl"},
	})

	for _, id := range []string{"t1", "t2"} {
		if info, ok := l.Query(id); ok {
			t.Errorf("Query(%q) accepted %q with an unresolvable projects root", id, info.JSONLPath)
		}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if len(l.byTaskID) != 0 || len(l.byToolUseID) != 0 {
		t.Errorf("indexes populated despite the refused seed: tasks=%d tool_uses=%d", len(l.byTaskID), len(l.byToolUseID))
	}
}
