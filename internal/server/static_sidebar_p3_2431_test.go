package server

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// Regression tests for #2431 P3 items 4-8 (sidebar / mobile / history search):
//
//  4. a pending session card must group by workspace basename (mirroring the
//     server's workspaceFallbackName) instead of landing in 未分组 until the
//     first send promotes it;
//  5. the history-search count chip must decide "filtered" on the trimmed
//     query — a whitespace-only query is not a filter, so no "(N / N)";
//  6. discovered cards must be keyed by pid AND node — two nodes can run a
//     process with the same pid, and pid-only lookup picks/deletes the wrong one;
//  7. mobileEnterChat must not stack a history entry per session switch, and
//     mobileBack must pop the entry it owns;
//  8. selecting a discovered card from a non-chat view must switch back to
//     chat like the managed-session path does.
//
// Behavioural tests run extracted pure functions under node (skip without node).

// extractJSFunctionOpt is extractJSFunction that returns "" when name is absent.
func extractJSFunctionOpt(js, name string) string {
	marker := "\nfunction " + name + "("
	i := strings.Index(js, marker)
	if i < 0 {
		return ""
	}
	rest := js[i+1:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		return ""
	}
	return rest[:end+2] + "\n"
}

func TestDashboard2431_PendingCardWorkspaceFallback(t *testing.T) {
	js := readDashboardJS(t)

	// Contract: the pending-card push must stamp project + project_fallback
	// from the workspace basename when the workspace is not a registered
	// project — the same shape /api/sessions emits, so the card lands in the
	// same group before and after the first send.
	start := strings.Index(js, "const pendingKeys = Object.keys(sessionWorkspaces);")
	if start < 0 {
		t.Fatal("dashboard.js: pending-card merge block not found")
	}
	block := js[start:]
	if end := strings.Index(block, "renderSidebar(data);"); end > 0 {
		block = block[:end]
	}
	if !strings.Contains(block, "project_fallback:") {
		t.Error("pending-card push must set project_fallback (workspace basename group, #2431 item 4)")
	}
	if !strings.Contains(block, "workspaceFallbackName(") {
		t.Error("pending-card push must derive the group from workspaceFallbackName(workspace)")
	}

	fn := extractJSFunction(t, js, "workspaceFallbackName")
	// Same table as internal/dashboard/session/fallback_test.go so the
	// frontend and backend agree on the group label.
	script := fn + `
const cases = [["", ""], ["/", ""], [".", ""], ["/home/alice/scratch", "scratch"],
  ["/home/alice/scratch/", "scratch"], ["/tmp/a/b/c", "c"], ["scratch", "scratch"], ["/var/.cache", ".cache"]];
process.stdout.write(JSON.stringify(cases.map(c => [c[0], c[1], workspaceFallbackName(c[0])])));
`
	var got [][3]string
	if err := json.Unmarshal([]byte(runNode(t, script)), &got); err != nil {
		t.Fatalf("parse node output: %v", err)
	}
	for _, c := range got {
		if c[1] != c[2] {
			t.Errorf("workspaceFallbackName(%q) = %q, want %q", c[0], c[2], c[1])
		}
	}
}

func TestDashboard2431_HistoryCountIgnoresWhitespaceQuery(t *testing.T) {
	js := readDashboardJS(t)
	script := extractJSFunction(t, js, "filterHistoryEntries") +
		extractJSFunction(t, js, "applyHistoryFilter") + `
const els = { 'hp-items': { innerHTML: '' }, 'hp-count': { textContent: '' } };
const document = { getElementById: id => els[id] || null };
const out = {};
applyHistoryFilter([], '   ');
out.emptyList = els['hp-count'].textContent;
applyHistoryFilter([], '');
out.emptyQuery = els['hp-count'].textContent;
process.stdout.write(JSON.stringify(out));
`
	var got struct{ EmptyList, EmptyQuery string }
	if err := json.Unmarshal([]byte(runNode(t, script)), &got); err != nil {
		t.Fatalf("parse node output: %v", err)
	}
	if got.EmptyQuery != "(0)" {
		t.Errorf("empty query count = %q, want (0)", got.EmptyQuery)
	}
	if got.EmptyList != "(0)" {
		t.Errorf("whitespace-only query count = %q, want (0) — filterHistoryEntries trims, the chip must too (#2431 item 5)", got.EmptyList)
	}
}

func TestDashboard2431_DiscoveredKeyIncludesNode(t *testing.T) {
	js := readDashboardJS(t)

	// Static contract: every discovered lookup/removal goes through the
	// node-aware helpers; no pid-only find/filter and only one literal key
	// concatenation (inside discoveredKey itself).
	for _, bad := range []string{
		"discoveredItems.find(x => x.pid === pid)",
		"discoveredItems.filter(x => x.pid !== pid)",
		"discoveredItems.filter(d => d.pid !== pid)",
		"discoveredItems.filter(d => d.pid !== pd.pid)",
		"pendingDiscovered.pid === pid)",
	} {
		if strings.Contains(js, bad) {
			t.Errorf("dashboard.js still keys discovered items by pid only: %q (#2431 item 6)", bad)
		}
	}
	if n := strings.Count(js, "'_discovered:' + "); n != 1 {
		t.Errorf("'_discovered:' literal concatenations = %d, want exactly 1 (discoveredKey helper)", n)
	}

	script := extractJSFunction(t, js, "discoveredKey") +
		extractJSFunction(t, js, "sameDiscovered") +
		extractJSFunction(t, js, "parseDiscoveredPid") +
		extractJSFunction(t, js, "findDiscovered") +
		extractJSFunction(t, js, "dropDiscovered") + `
let discoveredItems = [
  { pid: 4242, node: 'local', session_id: 'a' },
  { pid: 4242, node: 'remote1', session_id: 'b' },
  { pid: 7, session_id: 'c' },
];
const out = {};
out.keyLocal = discoveredKey(4242, 'local');
out.keyRemote = discoveredKey(4242, 'remote1');
out.keyDefault = discoveredKey(7, '');
out.pid = parseDiscoveredPid(out.keyRemote);
out.findRemote = (findDiscovered(4242, 'remote1') || {}).session_id || null;
out.findLocal = (findDiscovered(4242, 'local') || {}).session_id || null;
out.findDefault = (findDiscovered(7, undefined) || {}).session_id || null;
out.findMissing = findDiscovered(4242, 'nope');
dropDiscovered(4242, 'local');
out.afterDrop = discoveredItems.map(d => d.session_id);
process.stdout.write(JSON.stringify(out));
`
	var got struct {
		KeyLocal, KeyRemote, KeyDefault string
		Pid                             int
		FindRemote, FindLocal           string
		FindDefault                     string
		FindMissing                     interface{}
		AfterDrop                       []string
	}
	if err := json.Unmarshal([]byte(runNode(t, script)), &got); err != nil {
		t.Fatalf("parse node output: %v", err)
	}
	if got.KeyLocal == got.KeyRemote {
		t.Errorf("discoveredKey must differ across nodes for the same pid: %q", got.KeyLocal)
	}
	if got.KeyDefault != discoveredKeyWant(7, "local") {
		t.Errorf("discoveredKey(7, '') = %q, want %q", got.KeyDefault, discoveredKeyWant(7, "local"))
	}
	if got.Pid != 4242 {
		t.Errorf("parseDiscoveredPid = %d, want 4242", got.Pid)
	}
	if got.FindRemote != "b" || got.FindLocal != "a" || got.FindDefault != "c" {
		t.Errorf("findDiscovered picked wrong items: remote=%q local=%q default=%q", got.FindRemote, got.FindLocal, got.FindDefault)
	}
	if got.FindMissing != nil {
		t.Errorf("findDiscovered on unknown node = %v, want null", got.FindMissing)
	}
	if strings.Join(got.AfterDrop, ",") != "b,c" {
		t.Errorf("dropDiscovered(4242,'local') left %v, want [b c]", got.AfterDrop)
	}
}

func discoveredKeyWant(pid int, node string) string {
	return "_discovered:" + strconv.Itoa(pid) + ":" + node
}

func TestDashboard2431_MobileChatHistoryStack(t *testing.T) {
	js := readDashboardJS(t)
	script := extractJSFunction(t, js, "mobileEnterChat") +
		extractJSFunction(t, js, "mobileBack") +
		extractJSFunctionOpt(js, "mobileShowList") + `
const isMobile = () => true;
const history = {
  state: null, pushes: 0, replaces: 0, backs: 0,
  pushState(s) { this.pushes++; this.state = s; },
  replaceState(s) { this.replaces++; this.state = s; },
  back() { this.backs++; this.state = null; },
};
const cls = new Set();
const document = {
  body: { classList: { add: (...a) => a.forEach(c => cls.add(c)), remove: (...a) => a.forEach(c => cls.delete(c)), contains: c => cls.has(c) } },
  activeElement: null,
};
const out = {};
mobileEnterChat();
mobileEnterChat();
mobileEnterChat();
out.pushesAfterThreeEnters = history.pushes;
out.inChat = cls.has('mobile-chat-view');
mobileBack();
out.backs = history.backs;
out.inList = cls.has('mobile-list-view') && !cls.has('mobile-chat-view');
// Entering again after a real back must push (stack depth back to 1).
mobileEnterChat();
out.pushesAfterReenter = history.pushes;
process.stdout.write(JSON.stringify(out));
`
	var got struct {
		PushesAfterThreeEnters int
		InChat                 bool
		Backs                  int
		InList                 bool
		PushesAfterReenter     int
	}
	if err := json.Unmarshal([]byte(runNode(t, script)), &got); err != nil {
		t.Fatalf("parse node output: %v", err)
	}
	if !got.InChat {
		t.Error("mobileEnterChat must add mobile-chat-view")
	}
	if got.PushesAfterThreeEnters != 1 {
		t.Errorf("history.pushState calls after 3 mobileEnterChat = %d, want 1 (replaceState when already in chat, #2431 item 7)", got.PushesAfterThreeEnters)
	}
	if got.Backs != 1 {
		t.Errorf("mobileBack must history.back() the entry it owns; back calls = %d", got.Backs)
	}
	if !got.InList {
		t.Error("mobileBack must flip to mobile-list-view synchronously")
	}
	if got.PushesAfterReenter != 2 {
		t.Errorf("re-entering chat after back must push again; pushes = %d, want 2", got.PushesAfterReenter)
	}
}

func TestDashboard2431_SelectDiscoveredReturnsToChatView(t *testing.T) {
	js := readDashboardJS(t)
	fn := extractJSFunction(t, js, "selectSession")
	branch := strings.Index(fn, "previewDiscovered(")
	if branch < 0 {
		t.Fatal("selectSession: discovered branch (previewDiscovered call) not found")
	}
	view := strings.Index(fn, "setActivityView('chat')")
	if view < 0 {
		t.Fatal("selectSession: setActivityView('chat') not found")
	}
	if view > branch {
		t.Error("selectSession: the discovered branch returns before setActivityView('chat') — selecting a discovered card from assets/cron/settings leaves the view stuck (#2431 item 8)")
	}
}
