package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// Regression tests for #2429 (command palette P3 items 4-8). Behavioural
// tests run extracted pure functions under node (skip when node is absent);
// the static contracts always run.

// mockDateJS freezes `new Date()` / Date.now() at the given local-time
// constructor args while leaving every other Date constructor form intact.
const mockDateJS = `
const RealDate = Date;
const FIXED = new RealDate(%s).getTime();
class MockDate extends RealDate {
  constructor(...a) { if (a.length === 0) super(FIXED); else super(...a); }
  static now() { return FIXED; }
}
globalThis.Date = MockDate;
`

// extractJSAsyncFunction is extractJSFunction for `async function name(`.
func extractJSAsyncFunction(t *testing.T, js, name string) string {
	t.Helper()
	marker := "\nasync function " + name + "("
	i := strings.Index(js, marker)
	if i < 0 {
		t.Fatalf("dashboard.js: async function %s not found", name)
	}
	rest := js[i+1:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatalf("dashboard.js: async function %s has no column-0 closing brace", name)
	}
	return rest[:end+2] + "\n"
}

// Item 4: the palette's 「快速新建」 row must describe the selected node's
// default workspace, not the local defaultWorkspace path when a remote node
// is selected (the backend provides no per-node default workspace, so the
// remote hint is "<node> · 默认工作区").
func TestDashboardJS_QuickRow_FollowsSelectedNode(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	row := extractJSFunction(t, js, "buildQuickRow")
	if !strings.Contains(row, "quickRowHint(") {
		t.Errorf("buildQuickRow must derive its subtitle from quickRowHint(selectedNode), got:\n%s", row)
	}
	script := `
let defaultWorkspace = '/home/ec2-user/workspace/naozhi';
const nodesData = { n1: { display_name: 'GPU 盒子' }, n2: {} };
function shortPath(p) { return p.replace('/home/ec2-user', '~'); }
function getNodeDisplayName(id) {
  if (!id || id === 'local') return '本地';
  const nd = nodesData[id];
  return nd && nd.display_name ? nd.display_name : id;
}
` + extractJSFunction(t, js, "quickRowHint") + `
const out = { local: quickRowHint('local'), empty: quickRowHint(''), n1: quickRowHint('n1'), n2: quickRowHint('n2') };
defaultWorkspace = '';
out.localNoWs = quickRowHint('local');
process.stdout.write(JSON.stringify(out));
`
	var res map[string]string
	if err := json.Unmarshal([]byte(runNode(t, script)), &res); err != nil {
		t.Fatalf("decode node output: %v", err)
	}
	if res["local"] != "~/workspace/naozhi" || res["empty"] != "~/workspace/naozhi" {
		t.Errorf("local node must show the (shortened) local defaultWorkspace: %+v", res)
	}
	if res["localNoWs"] != "" {
		t.Errorf("local node with no defaultWorkspace must show nothing, got %q", res["localNoWs"])
	}
	for _, id := range []string{"n1", "n2"} {
		if strings.Contains(res[id], "naozhi") || strings.Contains(res[id], "~") {
			t.Errorf("remote node %s must not show the LOCAL default workspace: %q", id, res[id])
		}
		if !strings.Contains(res[id], "默认工作区") {
			t.Errorf("remote node %s hint must say 默认工作区: %q", id, res[id])
		}
	}
	if !strings.Contains(res["n1"], "GPU 盒子") {
		t.Errorf("remote hint must name the node via getNodeDisplayName: %q", res["n1"])
	}
	if !strings.Contains(res["n2"], "n2") {
		t.Errorf("remote hint falls back to the node id: %q", res["n2"])
	}
}

// Item 5: session-key timestamps must use one clock. The old code glued a
// UTC date (toISOString) to a local time (toTimeString), so a key created at
// 00:30 Asia/Shanghai carried yesterday's date. localDateStamp() is the
// single local-time formatter shared by all three key builders.
func TestDashboardJS_LocalDateStamp_UsesLocalDate(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	if strings.Contains(js, "toISOString().slice(0,10)") || strings.Contains(js, "toISOString().slice(0, 10)") {
		t.Error("dashboard.js still builds session keys from toISOString().slice(0,10) (UTC date + local time) — use localDateStamp (#2429)")
	}
	if c := strings.Count(js, "localDateStamp(now)"); c < 3 {
		t.Errorf("all three session-key builders must call localDateStamp(now); found %d", c)
	}
	script := extractJSFunction(t, js, "localDateStamp") +
		extractJSFunction(t, js, "sanitizeKeySlug") +
		extractJSFunction(t, js, "buildDashboardSessionKey") + `
const d = new Date('2026-09-02T16:30:05Z'); // 2026-09-03 00:30:05 Asia/Shanghai
const stamp = localDateStamp(d);
process.stdout.write(JSON.stringify({ stamp, key: buildDashboardSessionKey(stamp + '-' + 7, 'naozhi', 'general') }));
`
	var res struct{ Stamp, Key string }
	if err := json.Unmarshal([]byte(runNodeEnv(t, script, []string{"TZ=Asia/Shanghai"})), &res); err != nil {
		t.Fatalf("decode node output: %v", err)
	}
	if res.Stamp != "2026-09-03-003005" {
		t.Errorf("localDateStamp at 00:30 local (UTC+8) = %q, want 2026-09-03-003005 (local date, not UTC date)", res.Stamp)
	}
	if err := session.ValidateSessionKey(res.Key); err != nil {
		t.Errorf("key %q rejected by server: %v", res.Key, err)
	}
	if want := "dashboard:direct:2026-09-03-003005-7-naozhi:general"; res.Key != want {
		t.Errorf("key = %q, want %q", res.Key, want)
	}
	// Sanity in UTC: same instant renders the UTC date.
	var utc struct{ Stamp string }
	if err := json.Unmarshal([]byte(runNodeEnv(t, script, []string{"TZ=UTC"})), &utc); err != nil {
		t.Fatalf("decode node output: %v", err)
	}
	if utc.Stamp != "2026-09-02-163005" {
		t.Errorf("localDateStamp under TZ=UTC = %q, want 2026-09-02-163005", utc.Stamp)
	}
}

// Item 6: a null / failed remote backends manifest must not be cached for
// 60s — the next call has to refetch so a transient proxy error does not pin
// the picker to "no backends" for a minute.
func TestDashboardJS_FetchCLIBackends_DoesNotCacheNullRemote(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	script := `
let cliBackends = null, cliBackendsFetchedAt = 0;
const cliBackendsByNode = {};
function applyFeatureGates() {}
const good = { backends: [{ id: 'claude' }, { id: 'codex' }], default: 'claude' };
let calls = 0;
const responses = [null, () => { throw new Error('502'); }, good, good];
async function fetchJSON() {
  calls++;
  const r = responses.shift();
  return typeof r === 'function' ? r() : r;
}
` + extractJSAsyncFunction(t, js, "fetchCLIBackends") + `
(async () => {
  const a = await fetchCLIBackends('n1');
  const b = await fetchCLIBackends('n1');
  const c = await fetchCLIBackends('n1');
  const d = await fetchCLIBackends('n1'); // cached
  process.stdout.write(JSON.stringify({ a, b, c, d, calls, cached: !!cliBackendsByNode.n1 }));
})();
`
	var res struct {
		A, B   json.RawMessage
		C, D   *struct{ Default string }
		Calls  int
		Cached bool
	}
	if err := json.Unmarshal([]byte(runNode(t, script)), &res); err != nil {
		t.Fatalf("decode node output: %v", err)
	}
	if string(res.A) != "null" || string(res.B) != "null" {
		t.Errorf("first two calls must surface the failures (null), got a=%s b=%s", res.A, res.B)
	}
	if res.C == nil || res.C.Default != "claude" {
		t.Errorf("third call must REFETCH after a null result instead of serving the cached null; got %+v (calls=%d)", res.C, res.Calls)
	}
	if res.Calls != 3 {
		t.Errorf("fetch count = %d, want 3 (null, error, good; fourth served from cache)", res.Calls)
	}
	if !res.Cached || res.D == nil {
		t.Errorf("a good manifest must still be cached per node (cached=%v d=%+v)", res.Cached, res.D)
	}
	// Static: the picker slot must offer a retry when a remote manifest fails.
	refresh := extractJSFunction(t, js, "refreshBackendPicker")
	if !strings.Contains(refresh, "renderBackendFetchFailed(") {
		t.Error("refreshBackendPicker must render renderBackendFetchFailed(...) (retry affordance) when a remote manifest is null (#2429)")
	}
}

// Item 7: mermaid must pick its theme from the resolved dashboard theme
// (data-theme + prefers-color-scheme for 'auto'), not a hard-coded 'dark'.
func TestDashboardJS_MermaidTheme_FollowsDashboardTheme(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	if strings.Contains(js, "theme: 'dark'") {
		t.Error("dashboard.js still initialises mermaid with theme: 'dark' — use mermaidThemeName() (#2429)")
	}
	run := extractJSFunction(t, js, "runMermaid")
	if !strings.Contains(run, "mermaid.initialize(mermaidConfig())") {
		t.Error("runMermaid must re-initialise mermaid with mermaidConfig() before each run so new diagrams follow the current theme")
	}
	script := `
const doc = { documentElement: { dataset: {} } };
let prefersLight = false;
globalThis.document = doc;
globalThis.window = { matchMedia: q => ({ matches: q.indexOf('light') >= 0 ? prefersLight : !prefersLight }) };
` + extractJSFunction(t, js, "mermaidThemeName") + extractJSFunction(t, js, "mermaidConfig") + `
const out = {};
doc.documentElement.dataset.theme = 'light'; out.light = mermaidThemeName();
doc.documentElement.dataset.theme = 'dark'; out.dark = mermaidThemeName();
doc.documentElement.dataset.theme = 'auto'; prefersLight = true; out.autoLight = mermaidThemeName();
prefersLight = false; out.autoDark = mermaidThemeName();
delete doc.documentElement.dataset.theme; out.missing = mermaidThemeName();
doc.documentElement.dataset.theme = 'light';
const cfg = mermaidConfig(); out.cfgTheme = cfg.theme; out.cfgSec = cfg.securityLevel; out.cfgStart = cfg.startOnLoad;
process.stdout.write(JSON.stringify(out));
`
	var res struct {
		Light, Dark, AutoLight, AutoDark, Missing, CfgTheme, CfgSec string
		CfgStart                                                    bool
	}
	if err := json.Unmarshal([]byte(runNode(t, script)), &res); err != nil {
		t.Fatalf("decode node output: %v", err)
	}
	if res.Light != "default" || res.AutoLight != "default" {
		t.Errorf("light / auto+prefers-light must map to mermaid 'default', got light=%q autoLight=%q", res.Light, res.AutoLight)
	}
	if res.Dark != "dark" || res.AutoDark != "dark" || res.Missing != "dark" {
		t.Errorf("dark / auto+prefers-dark / unset must map to 'dark', got dark=%q autoDark=%q missing=%q", res.Dark, res.AutoDark, res.Missing)
	}
	if res.CfgTheme != "default" || res.CfgSec != "strict" || res.CfgStart {
		t.Errorf("mermaidConfig must keep securityLevel:'strict', startOnLoad:false and carry the resolved theme: %+v", res)
	}
}

// Item 8a: formatTimeShort's "within a week → weekday" branch must count
// local calendar days, not floor(ms/24h): an event 6d23.5h ago is the same
// weekday as today and must not be labelled with today's weekday name.
func TestDashboardJS_FormatTimeShort_WeekBoundaryByCalendarDay(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	// now = Wed 2026-09-09 00:30 local.
	script := strings.Replace(mockDateJS, "%s", "2026, 8, 9, 0, 30", 1) +
		extractJSFunction(t, js, "formatTimeShort") + `
const t = (...a) => new Date(...a).getTime();
const out = {
  today: formatTimeShort(t(2026, 8, 9, 0, 5)),
  yesterday: formatTimeShort(t(2026, 8, 8, 23, 50)),
  sixDays: formatTimeShort(t(2026, 8, 3, 23, 0)),      // Thu, 6 calendar days ago
  lastWed: formatTimeShort(t(2026, 8, 2, 1, 0)),       // Wed, 7 calendar days (6d23.5h) ago
  lastWedLate: formatTimeShort(t(2026, 8, 2, 23, 59)), // Wed, 7 calendar days (6d0.5h) ago
  lastYear: formatTimeShort(t(2025, 11, 31, 8, 0)),
};
process.stdout.write(JSON.stringify(out));
`
	var res map[string]string
	if err := json.Unmarshal([]byte(runNodeEnv(t, script, []string{"TZ=Asia/Shanghai"})), &res); err != nil {
		t.Fatalf("decode node output: %v", err)
	}
	if res["today"] != "00:05" {
		t.Errorf("today = %q, want 00:05", res["today"])
	}
	if res["yesterday"] != "昨天 23:50" {
		t.Errorf("yesterday = %q, want 昨天 23:50", res["yesterday"])
	}
	if res["sixDays"] != "周四 23:00" {
		t.Errorf("6 calendar days ago = %q, want 周四 23:00", res["sixDays"])
	}
	for _, k := range []string{"lastWed", "lastWedLate"} {
		if strings.Contains(res[k], "周") {
			t.Errorf("%s (7 calendar days ago, same weekday as today) = %q — must fall through to M-D, not a weekday name", k, res[k])
		}
		if !strings.HasPrefix(res[k], "9-2 ") {
			t.Errorf("%s = %q, want 9-2 HH:MM", k, res[k])
		}
	}
	if res["lastYear"] != "2025-12-31 08:00" {
		t.Errorf("lastYear = %q, want 2025-12-31 08:00", res["lastYear"])
	}
}

// Item 8b: historyDayLabel must include the year for dates outside the
// current year.
func TestDashboardJS_HistoryDayLabel_YearOutsideCurrentYear(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	script := strings.Replace(mockDateJS, "%s", "2026, 8, 9, 0, 30", 1) +
		extractJSFunction(t, js, "historyDayLabel") + `
const out = {
  today: historyDayLabel(new Date(2026, 8, 9, 0, 1)),
  yesterday: historyDayLabel(new Date(2026, 8, 8, 23, 59)),
  thisYear: historyDayLabel(new Date(2026, 3, 29, 12, 0)),
  lastYear: historyDayLabel(new Date(2025, 11, 31, 12, 0)),
};
process.stdout.write(JSON.stringify(out));
`
	var res map[string]string
	if err := json.Unmarshal([]byte(runNodeEnv(t, script, []string{"TZ=Asia/Shanghai"})), &res); err != nil {
		t.Fatalf("decode node output: %v", err)
	}
	if res["today"] != "今天" || res["yesterday"] != "昨天" {
		t.Errorf("today/yesterday = %q/%q", res["today"], res["yesterday"])
	}
	if strings.Contains(res["thisYear"], "2026") {
		t.Errorf("current-year label must stay short (no year): %q", res["thisYear"])
	}
	if !strings.Contains(res["lastYear"], "2025") {
		t.Errorf("label for a date in another year must include the year: %q", res["lastYear"])
	}
}

// Item 8c: the health strip used 「运行」 both for the running-session count
// and for uptime; the two must read differently.
func TestDashboardJS_HomeHealthLine_DistinguishesRunningFromUptime(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	script := `
const cliBackends = null;
` + extractJSFunction(t, js, "buildHomeHealthLines") + `
const lines = buildHomeHealthLines({ running: 2, ready: 1, total: 3, uptime: '3h2m' });
process.stdout.write(JSON.stringify(lines[0].text));
`
	var line string
	if err := json.Unmarshal([]byte(runNode(t, script)), &line); err != nil {
		t.Fatalf("decode node output: %v", err)
	}
	if !strings.Contains(line, "运行中 2") {
		t.Errorf("health line 1 must label the running-session count 「运行中 N」: %q", line)
	}
	if !strings.Contains(line, "已运行 3h2m") {
		t.Errorf("health line 1 must label uptime 「已运行 X」: %q", line)
	}
	if strings.HasPrefix(line, "运行 ") || strings.Contains(line, "· 运行 ") {
		t.Errorf("bare 「运行 」 is ambiguous (count vs uptime), got %q", line)
	}
}
