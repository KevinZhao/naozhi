package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_AgentViewModuleLoaded pins the RFC v4 agent-team-ui
// Phase 2.5 invariants:
//
//  1. dashboard.html references /static/agent_view.js AFTER dashboard.js
//     (the module depends on globals dashboard.js defines).
//  2. agent_view.js is served via embed.FS and non-empty.
//  3. The 5 banner helpers moved out of dashboard.js live in agent_view.js
//     so Phase 3 additions have a stable home to grow into.
//  4. dashboard.js no longer defines those 5 functions (duplicate names
//     in two scripts would have the later-loaded copy silently clobber
//     the earlier — acceptable today but a footgun worth banning).
func TestDashboardJS_AgentViewModuleLoaded(t *testing.T) {
	t.Parallel()

	html, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	htmlStr := string(html)

	dashIdx := strings.Index(htmlStr, `src="/static/dashboard.js"`)
	agentIdx := strings.Index(htmlStr, `src="/static/agent_view.js"`)
	if dashIdx < 0 {
		t.Fatal("dashboard.html missing <script src=\"/static/dashboard.js\">")
	}
	if agentIdx < 0 {
		t.Fatal("dashboard.html missing <script src=\"/static/agent_view.js\">")
	}
	if agentIdx < dashIdx {
		t.Errorf("agent_view.js must load AFTER dashboard.js (dashIdx=%d agentIdx=%d)",
			dashIdx, agentIdx)
	}

	av, err := agentViewJS.ReadFile("static/agent_view.js")
	if err != nil {
		t.Fatalf("read agent_view.js: %v", err)
	}
	if len(av) == 0 {
		t.Fatal("agent_view.js empty")
	}
	avStr := string(av)
	for _, name := range []string{
		"renderAgentRows",
		"agentRowHtml",
		"findAgentByToolUseId",
		"findAgentByTaskId",
		"initAgentsFromSession",
	} {
		if !strings.Contains(avStr, "function "+name+"(") {
			t.Errorf("agent_view.js missing function %s", name)
		}
	}

	// dashboard.js must NOT re-define these (duplicate-definition footgun).
	djs, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	djsStr := string(djs)
	for _, name := range []string{
		"renderAgentRows",
		"agentRowHtml",
		"findAgentByToolUseId",
		"findAgentByTaskId",
		"initAgentsFromSession",
	} {
		if strings.Contains(djsStr, "function "+name+"(") {
			t.Errorf("dashboard.js still defines %s — should be in agent_view.js only", name)
		}
	}

	// AgentView namespace export — Phase 3 callers will land here.
	if !strings.Contains(avStr, "window.AgentView") {
		t.Error("agent_view.js missing window.AgentView namespace export")
	}

	// agent_view.js must consume the shared bubble renderers via their
	// actual global names. A previous revision referenced window.renderEvent
	// which dashboard.js never defined, silently falling back to a plain-
	// text stub and dropping markdown / tool_result folding / images in the
	// sub-agent panel. Pin the contract at both ends.
	for _, sym := range []string{"window.eventHtml", "window.renderEventsWithDividers"} {
		if !strings.Contains(djsStr, sym+" = ") {
			t.Errorf("dashboard.js missing export %s — agent_view.js depends on it", sym)
		}
		if !strings.Contains(avStr, sym) {
			t.Errorf("agent_view.js must reference %s (shared bubble renderer)", sym)
		}
	}
	// Reject bare `window.renderEvent(` (the old stub name) but allow
	// `window.renderEventsWithDividers(` which is a legitimate successor.
	if strings.Contains(avStr, "window.renderEvent(") {
		t.Error("agent_view.js still calls window.renderEvent() — use window.eventHtml() instead")
	}
}
