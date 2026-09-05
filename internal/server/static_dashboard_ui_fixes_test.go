// static_dashboard_ui_fixes_test.go — contract guards for the 2026-09 dashboard
// UI fix bundle (mobile header overflow, history sticky header layering,
// modal-above-split-drawer, settings badge aria-label, files_view attribute
// escaping / binary detection / empty-dir hint / error double-escape).
//
// Each guard pins a source-level shape that was observed broken in production
// (390px viewport: .detail-runstats painted off-screen and the git chip text
// overlapped "N 轮"; elementFromPoint on a history day header hit an item; a
// modal opened while a split drawer was front stacked under the drawer; a
// file named `a"b` navigated to the wrong path).
package server

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// cssRuleBody returns the declaration block of the FIRST rule whose selector
// list is exactly `selector` (after the preceding `}` or newline). Fails the
// test when the rule is missing so a selector rename surfaces loudly.
func cssRuleBody(t *testing.T, css, selector string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)(?:^|[}\n])\s*` + regexp.QuoteMeta(selector) + `\{([^}]*)\}`)
	m := re.FindStringSubmatch(css)
	if m == nil {
		t.Fatalf("css rule %q not found in dashboard.html", selector)
	}
	return m[1]
}

func readStaticAsset(t *testing.T, name string) string {
	t.Helper()
	var (
		data []byte
		err  error
	)
	switch name {
	case "dashboard.html":
		data, err = dashboardHTML.ReadFile("static/dashboard.html")
	case "dashboard.js":
		data, err = dashboardJS.ReadFile("static/dashboard.js")
	case "files_view.js":
		data, err = filesViewJS.ReadFile("static/files_view.js")
	default:
		t.Fatalf("unknown static asset %q", name)
	}
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// #1 — mobile header meta row must not overflow the viewport. .detail-left
// (cli + model label) needs min-width:0 so it can shrink inside the flex row,
// and overflow:hidden so the clipped text does not paint over its siblings;
// .detail-git already shrinks but without overflow:hidden its text spills
// into the runstats; runstats itself is dropped at phone width.
func TestDashboardHTML_MobileHeaderMetaRowNoOverflow(t *testing.T) {
	t.Parallel()
	html := readStaticAsset(t, "dashboard.html")

	left := cssRuleBody(t, html, ".main-header .detail-left")
	for _, want := range []string{"min-width:0", "overflow:hidden", "text-overflow:ellipsis"} {
		if !strings.Contains(left, want) {
			t.Errorf(".main-header .detail-left must declare %s (got %q) — without it the cli/model label cannot shrink and pushes the git chip + runstats off-screen at 390px", want, left)
		}
	}

	git := cssRuleBody(t, html, ".main-header .detail-git")
	if !strings.Contains(git, "overflow:hidden") {
		t.Errorf(".main-header .detail-git must declare overflow:hidden (got %q) — min-width:0 alone lets the chip text spill over the runstats", git)
	}

	// At ≤480px the run-history overview is dropped from the header: the row
	// cannot fit cli + model + git + "N 轮 · 共 X · 花费 $Y" in 390px and the
	// stats remain reachable in the collapsed run panel below.
	re480 := regexp.MustCompile(`(?s)@media\(max-width:480px\)\{.*?\n\}`)
	found := false
	for _, blk := range re480.FindAllString(html, -1) {
		if regexp.MustCompile(`\.main-header \.detail-runstats\{[^}]*display:none`).MatchString(blk) {
			found = true
			break
		}
	}
	if !found {
		t.Error("no @media(max-width:480px) rule hides .main-header .detail-runstats — the header row overflows the phone viewport")
	}
}

// #1b — model-name compression must strip every Bedrock inference-profile
// prefix, not just `global.`; the live default model is `us.anthropic.…` and
// the unstripped prefix is what pushed the label past the viewport.
func TestDashboardJS_ModelPrefixStripAllBedrockRegions(t *testing.T) {
	t.Parallel()
	js := readStaticAsset(t, "dashboard.js")
	if strings.Contains(js, `.replace(/^global\.anthropic\./, '')`) {
		t.Error("dashboard.js compactModel still strips only the `global.` prefix — us./eu./apac. inference profiles leak into the header label")
	}
	if !strings.Contains(js, `.replace(/^(global|us|eu|apac)\.anthropic\./, '')`) {
		t.Error("dashboard.js compactModel must strip /^(global|us|eu|apac)\\.anthropic\\./")
	}
}

// #2 — sticky day header in the history popover must sit above the items
// that scroll under it. Items are position:relative and painted later in DOM
// order, so a z-index-less sticky header loses the stacking race.
func TestDashboardHTML_HistoryDayHeaderStacksAboveItems(t *testing.T) {
	t.Parallel()
	html := readStaticAsset(t, "dashboard.html")
	body := cssRuleBody(t, html, ".hp-day-header")
	if !strings.Contains(body, "position:sticky") {
		t.Fatalf(".hp-day-header lost position:sticky (got %q)", body)
	}
	m := regexp.MustCompile(`z-index:(\d+)`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf(".hp-day-header has no z-index (got %q) — position:relative items painted after it cover the sticky header", body)
	}
	if v, _ := strconv.Atoi(m[1]); v < 1 {
		t.Errorf(".hp-day-header z-index=%d; want ≥1", v)
	}
}

// #3 — blocking modals (rename/confirm dialog, new-session command palette)
// must paint above the split-view front drawer. The front drawer lifts to a
// literal 202 (see static_split_view_test.go); the modals used to share
// --nz-z-drawer (200) and were therefore covered while a split was open.
func TestDashboardHTML_ModalTierAboveSplitFront(t *testing.T) {
	t.Parallel()
	html := readStaticAsset(t, "dashboard.html")

	tokens := map[string]int{}
	for _, m := range reZIndexToken.FindAllStringSubmatch(html, -1) {
		v, _ := strconv.Atoi(m[2])
		tokens[m[1]] = v
	}
	modal, ok := tokens["modal"]
	if !ok {
		t.Fatal("--nz-z-modal token not defined in :root — modal / cmd-palette have no tier above the split-front drawer (202)")
	}
	frontRe := regexp.MustCompile(`\.nz-split-front\{z-index:(\d+)`)
	fm := frontRe.FindStringSubmatch(html)
	if fm == nil {
		t.Fatal(".nz-split-front z-index literal not found")
	}
	front, _ := strconv.Atoi(fm[1])
	if modal <= front {
		t.Errorf("--nz-z-modal=%d must exceed the split-front drawer z-index %d", modal, front)
	}
	if toast, ok := tokens["toast"]; ok && modal >= toast {
		t.Errorf("--nz-z-modal=%d must stay below --nz-z-toast=%d so a toast still clears an open dialog", modal, toast)
	}

	for _, sel := range []string{".modal-overlay", ".cmd-palette-overlay"} {
		body := cssRuleBody(t, html, sel)
		if !strings.Contains(body, "z-index:var(--nz-z-modal)") {
			t.Errorf("%s must use z-index:var(--nz-z-modal) (got %q) — on --nz-z-drawer it is covered by the split-front pane", sel, body)
		}
	}
}

// #6 — the settings-tab badge mirrors the 系统 attention dot on mobile; its
// aria-label was copy-pasted from the 系统 badge and read "需关注的系统任务数"
// on the 设置 button, which is neither a count nor about settings.
func TestDashboardHTML_SettingsBadgeAriaLabel(t *testing.T) {
	t.Parallel()
	html := readStaticAsset(t, "dashboard.html")
	re := regexp.MustCompile(`id="abnav-settings-badge"[^>]*aria-label="([^"]*)"`)
	m := re.FindStringSubmatch(html)
	if m == nil {
		t.Fatal("#abnav-settings-badge with aria-label not found")
	}
	if m[1] == "需关注的系统任务数" {
		t.Errorf("#abnav-settings-badge aria-label %q is the 系统 badge copy — it must describe the 设置 entry", m[1])
	}
	if !strings.Contains(m[1], "设置") {
		t.Errorf("#abnav-settings-badge aria-label %q should mention 设置", m[1])
	}
}

// #7 — files_view.js builds data-name / data-dir / data-project / title
// attributes by string concatenation. nz_util's esc() deliberately leaves
// quotes alone, so a file named `a"b` truncated the attribute and the click
// handler navigated to the wrong path. Attribute contexts must use escAttr.
func TestFilesViewJS_AttributeContextsUseEscAttr(t *testing.T) {
	t.Parallel()
	js := readStaticAsset(t, "files_view.js")

	if !strings.Contains(js, "function escAttr(") {
		t.Fatal("files_view.js has no escAttr helper (should delegate to nz.util.escAttr)")
	}
	// Any `attr="' + esc(` is an attribute value built with the non-quote
	// escaping esc(); list the offenders so the fix is targeted.
	bad := regexp.MustCompile(`(data-name|data-dir|data-project|title)="' \+ esc\(`)
	for _, m := range bad.FindAllString(js, -1) {
		t.Errorf("files_view.js attribute built with esc() instead of escAttr(): %q", m)
	}
	// Positive: the attributes that were broken in production.
	for _, want := range []string{
		`data-name="' + escAttr(e.name)`,
		`data-dir="' + escAttr(acc)`,
		`data-project="' + escAttr(c.name)`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("files_view.js missing %q", want)
		}
	}
}

// #8 — the preview endpoint reports a non-text file as {"content":"",
// "binary":true}; `res.content == null` was never true for "" so the
// "please download" hint never showed and an empty <pre> rendered instead.
func TestFilesViewJS_BinaryPreviewUsesBinaryFlag(t *testing.T) {
	t.Parallel()
	js := readStaticAsset(t, "files_view.js")
	if strings.Contains(js, "res.content == null") {
		t.Error("files_view.js still gates the binary hint on `res.content == null` — the server sends content:\"\" so this is never true")
	}
	if !regexp.MustCompile(`if \(res && res\.binary\)`).MatchString(js) {
		t.Error("files_view.js must branch on `res && res.binary` for the not-previewable hint")
	}
}

// #9 — the "空目录" hint was gated on !state.dir, so an empty SUBdirectory
// rendered nothing but the ↑ 上级目录 row.
func TestFilesViewJS_EmptySubdirShowsHint(t *testing.T) {
	t.Parallel()
	js := readStaticAsset(t, "files_view.js")
	if strings.Contains(js, "if (!entries.length && !state.dir)") {
		t.Error("files_view.js still gates the 空目录 hint on the root dir — empty subdirectories render blank")
	}
	if !strings.Contains(js, "if (!entries.length)") {
		t.Error("files_view.js renderList must show the 空目录 hint whenever entries is empty")
	}
}

// #10 — renderEmpty() escapes its argument; callers that pre-escape
// e.message double-encode (`&amp;lt;` shows up literally in the hint).
func TestFilesViewJS_RenderEmptyNotDoubleEscaped(t *testing.T) {
	t.Parallel()
	js := readStaticAsset(t, "files_view.js")
	re := regexp.MustCompile(`renderEmpty\([^)]*esc\(`)
	for _, m := range re.FindAllString(js, -1) {
		t.Errorf("files_view.js double-escapes a renderEmpty argument: %q (renderEmpty escapes internally)", m)
	}
}

// Matches `--nz-z-NAME:VALUE;` token definitions in :root (moved from the
// deleted static_zindex_scale_test.go, #2533 A2b).
var reZIndexToken = regexp.MustCompile(`--nz-z-([a-z]+):(\d+)`)
