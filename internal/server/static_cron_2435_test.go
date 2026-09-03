package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// Regression tests for #2435 items 3 / 4 / 7 (cron_view.js):
//
//  3. humanizeCron must render "M * * * *" as 每小时 :MM instead of echoing
//     the raw expression (only "0 * * * *" was recognised as hourly).
//  4. the global ↑/↓ run-navigation keydown handler must not preventDefault
//     (nor switch an invisible run) when the cron view is not active, a
//     modal is open, or focus is in a <select>.
//  7. the abnav 需关注 red dot must not stay lit for manually paused jobs —
//     only failed / missed runs count for the rail badge.
//
// Behavioural tests run extracted pure functions under node (skip when node
// is absent); contract tests always run.

// extractCronJSFunction is extractJSFunction plus one-liner support: a
// `function name(...) { ... }` that closes on its own line (e.g. pad2) is
// returned as that single line instead of swallowing the next function.
func extractCronJSFunction(t *testing.T, js, name string) (string, bool) {
	t.Helper()
	marker := "\nfunction " + name + "("
	i := strings.Index(js, marker)
	if i < 0 {
		return "", false
	}
	rest := js[i+1:]
	if nl := strings.Index(rest, "\n"); nl >= 0 && strings.HasSuffix(strings.TrimRight(rest[:nl], " "), "}") {
		return rest[:nl] + "\n", true
	}
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatalf("cron_view.js: function %s has no column-0 closing brace", name)
	}
	return rest[:end+2] + "\n", true
}

func readCronViewJS2435(t *testing.T) string {
	t.Helper()
	data, err := cronViewJS.ReadFile("static/cron_view.js")
	if err != nil {
		t.Fatalf("read cron_view.js: %v", err)
	}
	return string(data)
}

// TestCronViewJS_HumanizeCronTable (#2435 item 3) — table-driven behavioural
// check of humanizeCron under node, covering the shapes operators actually
// write by hand plus the ones that must keep falling back to the raw expr.
func TestCronViewJS_HumanizeCronTable(t *testing.T) {
	t.Parallel()
	js := readCronViewJS2435(t)

	var src strings.Builder
	for _, name := range []string{
		"pad2", "parseDowField", "parseCronToFreq", "humanizeCron",
		"humanizeCronStepValue", "humanizeCronLegacyEvery", "humanizeCronMultiDow",
		"humanizeCronHourlyMinute",
	} {
		fn, ok := extractCronJSFunction(t, js, name)
		if !ok {
			t.Errorf("cron_view.js: function %s not found", name)
			continue
		}
		src.WriteString(fn)
	}

	cases := []struct {
		Expr string `json:"expr"`
		Want string `json:"-"`
	}{
		// item 3: minute-offset hourly shapes
		{"20 * * * *", "每小时 :20"},
		{"15 * * * *", "每小时 :15"},
		{"5 * * * *", "每小时 :05"},
		{"0 * * * *", "每小时"},
		// unchanged neighbours
		{"13 6 * * *", "每天 06:13"},
		{"*/15 * * * *", "每 15 分钟"},
		{"0 */6 * * *", "每 6 小时"},
		{"@every 30m", "每 30 分钟"},
		{"0 9 * * 1-5", "工作日 09:00"},
		{"0 9 * * 0,6", "周末 09:00"},
		{"30 8 1 * *", "每月 1 日 08:30"},
		// must still fall back to raw
		{"60 * * * *", "60 * * * *"},
		{"20 * 1 * *", "20 * 1 * *"},
		{"20 * * * 1", "20 * * * 1"},
		{"garbage", "garbage"},
	}
	exprs, _ := json.Marshal(cases)
	script := src.String() + `
const cases = ` + string(exprs) + `;
console.log(JSON.stringify(cases.map(c => humanizeCron(c.expr))));
`
	out := runNode(t, script)
	var got []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("parse node output %q: %v", out, err)
	}
	if len(got) != len(cases) {
		t.Fatalf("got %d results, want %d", len(got), len(cases))
	}
	for i, c := range cases {
		if got[i] != c.Want {
			t.Errorf("humanizeCron(%q) = %q, want %q", c.Expr, got[i], c.Want)
		}
	}
}

// extractCronArrowKeydown returns the source of the document-level ↑/↓
// keydown listener registered by cron_view.js.
func extractCronArrowKeydown(t *testing.T, js string) string {
	t.Helper()
	const open = "document.addEventListener('keydown', function(e) {"
	i := strings.Index(js, open)
	if i < 0 {
		t.Fatal("cron_view.js: document-level keydown listener not found")
	}
	rest := js[i:]
	end := strings.Index(rest, "\n});\n")
	if end < 0 {
		t.Fatal("cron_view.js: keydown listener has no `});` terminator")
	}
	return rest[:end+len("\n});\n")]
}

// TestCronViewJS_ArrowKeydownGuards (#2435 item 4) — the ↑/↓ handler must be
// inert (no preventDefault, no run switch) outside the cron view, while a
// modal is open, or when focus sits in a form control including <select>.
func TestCronViewJS_ArrowKeydownGuards(t *testing.T) {
	t.Parallel()
	js := readCronViewJS2435(t)
	handler := extractCronArrowKeydown(t, js)

	// Static contract: pin the guards so a refactor can't drop them.
	for _, want := range []string{
		"activeView !== 'cron'",
		"document.querySelector('.modal-overlay",
		"tag === 'select'",
	} {
		if !strings.Contains(handler, want) {
			t.Errorf("cron ↑/↓ keydown handler missing guard %q", want)
		}
	}

	type kase struct {
		Name     string `json:"name"`
		View     string `json:"view"`
		Modal    bool   `json:"modal"`
		Tag      string `json:"tag"`
		Editable bool   `json:"editable"`
		Key      string `json:"key"`
		WantPD   bool   `json:"-"`
		WantNav  string `json:"-"`
	}
	cases := []kase{
		{Name: "chat view", View: "chat", Tag: "DIV", Key: "ArrowDown"},
		{Name: "settings view", View: "settings", Tag: "DIV", Key: "ArrowUp"},
		{Name: "modal open", View: "cron", Modal: true, Tag: "DIV", Key: "ArrowDown"},
		{Name: "select focused", View: "cron", Tag: "SELECT", Key: "ArrowDown"},
		{Name: "input focused", View: "cron", Tag: "INPUT", Key: "ArrowDown"},
		{Name: "contenteditable", View: "cron", Tag: "DIV", Editable: true, Key: "ArrowDown"},
		{Name: "cron down", View: "cron", Tag: "DIV", Key: "ArrowDown", WantPD: true, WantNav: "next"},
		{Name: "cron up", View: "cron", Tag: "TR", Key: "ArrowUp", WantPD: true, WantNav: "prev"},
	}
	casesJSON, _ := json.Marshal(cases)
	script := `
let activeView = 'chat';
let modalOpen = false;
let handler = null;
const navs = [];
const cronExpandedRunId = { jobId: 'j1', runId: 'r1' };
function navigateExpandedRun(d) { navs.push(d); }
const document = {
  addEventListener(type, fn) { if (type === 'keydown') handler = fn; },
  querySelector(sel) { return modalOpen ? {} : null; },
};
` + handler + `
if (!handler) { console.log(JSON.stringify({ error: 'no handler' })); process.exit(0); }
const cases = ` + string(casesJSON) + `;
const results = cases.map(c => {
  activeView = c.view; modalOpen = c.modal; navs.length = 0;
  let prevented = false;
  handler({
    key: c.key, metaKey: false, ctrlKey: false, altKey: false,
    target: { tagName: c.tag, isContentEditable: c.editable },
    preventDefault() { prevented = true; },
  });
  return { prevented, nav: navs.join(',') };
});
console.log(JSON.stringify(results));
`
	out := runNode(t, script)
	var got []struct {
		Prevented bool   `json:"prevented"`
		Nav       string `json:"nav"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("parse node output %q: %v", out, err)
	}
	if len(got) != len(cases) {
		t.Fatalf("got %d results, want %d (%s)", len(got), len(cases), out)
	}
	for i, c := range cases {
		if got[i].Prevented != c.WantPD || got[i].Nav != c.WantNav {
			t.Errorf("%s: prevented=%v nav=%q, want prevented=%v nav=%q",
				c.Name, got[i].Prevented, got[i].Nav, c.WantPD, c.WantNav)
		}
	}
}

// TestCronViewJS_RailBadgeExcludesPaused (#2435 item 7) — the abnav red dot
// counts only failed / missed jobs. Paused jobs remain in the in-view 需关注
// filter and header chip, but must not light the rail badge indefinitely.
func TestCronViewJS_RailBadgeExcludesPaused(t *testing.T) {
	t.Parallel()
	js := readCronViewJS2435(t)

	idx := strings.Index(js, "async function fetchCronJobs()")
	if idx < 0 {
		t.Fatal("fetchCronJobs not found")
	}
	body, ok := sliceFunctionBody(js, idx)
	if !ok {
		t.Fatal("fetchCronJobs body boundary not found")
	}
	const want = "const attention = cronJobs.filter(j => j.last_error || j.missed).length;"
	if !strings.Contains(body, want) {
		t.Errorf("fetchCronJobs must compute rail attention without paused: want %s", want)
	}
	if strings.Contains(body, "j.paused ||") {
		t.Error("fetchCronJobs rail badge must not count paused jobs (#2435 item 7)")
	}
	if !strings.Contains(body, "railBadge.hidden = attention === 0") {
		t.Error("railBadge must still be driven by the shared `attention` const")
	}

	// The in-view 需关注 semantics (filter chip + header count) keep paused —
	// only the cross-view red dot changes.
	if !strings.Contains(js, "s === 'attention' && !(j.paused || j.last_error || j.missed)") {
		t.Error("filterCronJobs 'attention' arm must keep paused || last_error || missed")
	}
	if !strings.Contains(js, "const attentionCount = cronJobs.filter(j => j.paused || j.last_error || j.missed).length;") {
		t.Error("header attentionCount must keep paused || last_error || missed")
	}
}
