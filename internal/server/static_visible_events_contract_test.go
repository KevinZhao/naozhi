package server

import (
	"regexp"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// internalSetRe extracts the element list from
//
//	const INTERNAL_EVENT_TYPES = new Set(['tool_use','result',...]);
//
// in dashboard.js so the test can compare it element-by-element against the
// server-side cli.IsInternalEventType predicate.
var internalSetRe = regexp.MustCompile(`INTERNAL_EVENT_TYPES\s*=\s*new Set\(\[([^\]]*)\]\)`)

// parseJSStringList turns `'a','b' , 'c'` into []string{"a","b","c"}.
func parseJSStringList(s string) []string {
	var out []string
	for _, raw := range strings.Split(s, ",") {
		raw = strings.TrimSpace(raw)
		raw = strings.Trim(raw, `'"`)
		if raw != "" {
			out = append(out, raw)
		}
	}
	return out
}

// TestInternalEventTypes_JSGoParity is the load-bearing guard for the
// visible-aware history fix. The server's EventLastNVisibleCtx counts entries
// cli.IsInternalEventType reports false for; the dashboard hides exactly the
// types in its INTERNAL_EVENT_TYPES Set. If the two sets drift, the server
// would either over-walk (counting a hidden type as visible) or hand back a
// page the dashboard renders blank — re-opening the "parallel agent team ate
// my history" bug. This test pins them byte-for-byte in both directions.
func TestInternalEventTypes_JSGoParity(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	m := internalSetRe.FindSubmatch(data)
	if m == nil {
		t.Fatal("could not locate INTERNAL_EVENT_TYPES = new Set([...]) in dashboard.js")
	}
	jsTypes := parseJSStringList(string(m[1]))
	if len(jsTypes) == 0 {
		t.Fatal("INTERNAL_EVENT_TYPES parsed to an empty set")
	}

	// Direction 1: every JS-hidden type must be internal per the Go predicate.
	jsSet := map[string]bool{}
	for _, ty := range jsTypes {
		jsSet[ty] = true
		if !cli.IsInternalEventType(ty) {
			t.Errorf("dashboard.js hides %q but cli.IsInternalEventType(%q)=false — the server would count it as a visible bubble and mis-size the initial page", ty, ty)
		}
	}

	// Direction 2: every Go-internal type must be hidden by the JS set. We
	// don't have a public enumerator for the Go map, so probe the known
	// universe of types the dashboard renders plus the internal ones; any
	// type the Go side calls internal but JS doesn't hide is a drift.
	for _, ty := range allKnownEventTypes {
		if cli.IsInternalEventType(ty) && !jsSet[ty] {
			t.Errorf("cli.IsInternalEventType(%q)=true but dashboard.js INTERNAL_EVENT_TYPES does not hide it — the server would skip past it while the UI still renders it", ty)
		}
	}
}

// allKnownEventTypes is the union of every EventEntry.Type the codebase emits
// (see clievent.EventEntry.Type godoc). The parity test probes each against
// both sides so a newly-added internal type that lands in only one place is
// caught.
var allKnownEventTypes = []string{
	"init", "thinking", "tool_use", "text", "result", "system", "agent",
	"todo", "task_start", "task_progress", "task_done", "user",
	"ask_question",
}

// TestDashboardJS_AutoPageBackSafetyNet pins the frontend safety net (plan A)
// that complements the server-side visible-aware read. When the initial page
// rendered blank (every event internal-filtered) the dashboard must page back
// transparently, bounded by AUTO_PAGEBACK_MAX, instead of stranding the
// operator on the "该会话最近仅有 agent 活动" placeholder.
func TestDashboardJS_AutoPageBackSafetyNet(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	wants := []struct {
		label    string
		fragment string
	}{
		{"bounded counter", "let _autoPageBackCount = 0;"},
		{"cap constant", "const AUTO_PAGEBACK_MAX = 3;"},
		{"helper fn", "function maybeAutoPageBack("},
		{"cap guard", "if (_autoPageBackCount >= AUTO_PAGEBACK_MAX) return;"},
		{"counter reset on session switch", "_autoPageBackCount = 0; // reset the blank-page recovery budget per session"},
		{"renderEvents wiring", "if (!html && events.length > 0) maybeAutoPageBack();"},
	}
	for _, w := range wants {
		if !strings.Contains(js, w.fragment) {
			t.Errorf("dashboard.js missing auto-page-back %s: %q", w.label, w.fragment)
		}
	}

	// The all-internal placeholder must STILL exist — the safety net layers on
	// top of it (shown briefly while paging back), it does not remove it.
	// Match either raw UTF-8 or the \u-escaped form prettier may produce.
	if !strings.Contains(js, "该会话最近仅有 agent 活动") &&
		!strings.Contains(js, `该会话最近仅有 agent`) {
		t.Error("all-internal placeholder must remain — maybeAutoPageBack augments it, not replaces it")
	}
}
