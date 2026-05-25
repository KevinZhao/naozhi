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

// TestAgentView_NoInlineOnClickJSEscape pins R247-SEC-4: agent_view.js must
// not emit `onclick="...switchTo('" + escAttr(...) + "')"`. escAttr is HTML-
// attr escape; switchTo's argument is a JS string literal — wrong sink. The
// fix moved the click handler to a delegated `addEventListener('click', ...)`
// that reads dataset.task, so taskId never crosses a JS-parse boundary
// inside an HTML attribute.
//
// Even though the server validates taskId via `agentTaskIDRe ^[a-z0-9]{1,32}$`
// (so today no character can break out), the wrong sink at a security
// boundary is the regression-prone shape we want to ban for good.
func TestAgentView_NoInlineOnClickJSEscape(t *testing.T) {
	t.Parallel()

	av, err := agentViewJS.ReadFile("static/agent_view.js")
	if err != nil {
		t.Fatalf("read agent_view.js: %v", err)
	}
	avStr := string(av)

	// Forbid the exact escAttr-into-JS-string-literal shape that triggered
	// the finding. Either the literal substring or any `escAttr(` reference
	// inside an `onclick=` attribute would re-introduce the same bug.
	for _, bad := range []string{
		"AgentView.switchTo('", // inline JS-literal in attr
		"switchTo(\\'",         // escaped variant
		"onclick=\"window.AgentView.switchTo",
	} {
		if strings.Contains(avStr, bad) {
			t.Errorf("agent_view.js must not contain inline onclick JS-literal pattern %q "+
				"(R247-SEC-4: use addEventListener + dataset.task instead)", bad)
		}
	}

	// Positive contract: the delegated click listener that replaces the
	// inline onclick must be present, and it must read dataset.task.
	if !strings.Contains(avStr, "addEventListener('click'") {
		t.Error("agent_view.js missing delegated `addEventListener('click', ...)` — " +
			"R247-SEC-4 fix requires event delegation, not inline onclick")
	}
	if !strings.Contains(avStr, ".rb-agent-row[data-task]") {
		t.Error("agent_view.js delegated click handler must select " +
			".rb-agent-row[data-task] (the rows with a real taskId)")
	}
	if !strings.Contains(avStr, "dataset") {
		t.Error("agent_view.js delegated click handler must read taskId from dataset, " +
			"not parse it back out of the onclick attribute")
	}
}
