package server

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestDashboardJS_NoPromptIsLocalized pins the R110-P1 UX fix that replaced
// the raw English placeholder "(no prompt)" with a Chinese label in the
// user-facing surfaces (history popover + session card). The cron-card's
// "(no prompt — tap to set)" was later localized in Round 122 — this test
// only guards the two originally-scoped surfaces; the cron placeholder has
// its own contract in TestDashboardJS_R122_CronEmptyPromptLocalized.
func TestDashboardJS_NoPromptIsLocalized(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)
	// Expected new strings present
	for _, want := range []string{
		`>未命名</div>`,
		`? '新会话' : '未命名'`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("dashboard.js missing R110-P1 localized string %q", want)
		}
	}
	// "(no prompt)" must not remain as a user-visible label in the two
	// surfaces. Scope the forbidden check to the exact phrase so the cron
	// placeholder "(no prompt — tap to set)" is not caught.
	forbidden := "'(no prompt)'"
	if strings.Contains(js, forbidden) {
		t.Errorf("dashboard.js still contains legacy English placeholder %q", forbidden)
	}
	if strings.Contains(js, ">(no prompt)<") {
		t.Errorf("dashboard.js still renders legacy English placeholder >(no prompt)<")
	}
}

// TestDashboardJS_CronBadgeAlertClass pins the R110-P1 fix that the cron
// header badge toggles the .is-alert red variant when any jobs need attention
// (paused or last_error). The history badge is intentionally left neutral
// because it is a cumulative count, not an unread/failure signal — so this
// test also asserts history-badge's classList is NOT being mutated toward
// is-alert in the same render path.
func TestDashboardJS_CronBadgeAlertClass(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)
	if !strings.Contains(js, "cronBadge.classList.toggle('is-alert', attention > 0)") {
		t.Error("dashboard.js: cron badge must toggle 'is-alert' based on attention count")
	}
	// History badge block (id 'history-badge') must not grow an is-alert
	// toggle. Search within the surrounding neighborhood to keep the
	// assertion robust against unrelated later references to the class.
	hIdx := strings.Index(js, "history-badge")
	if hIdx < 0 {
		t.Fatal("history-badge reference not found in dashboard.js")
	}
	// Slice ~400 chars around the history-badge render site — that is the
	// full block that writes textContent + style.display.
	end := hIdx + 800
	if end > len(js) {
		end = len(js)
	}
	window := js[hIdx:end]
	if strings.Contains(window, "hBadge.classList") && strings.Contains(window, "is-alert") {
		t.Error("history-badge must stay neutral grey (no is-alert toggle); it is a cumulative count, not an alert")
	}
}

// TestDashboardJS_FormatAbsTimeHoverTitles pins the R110-P3 time-format-unify
// fix: relative labels ("3m ago", "next 2h") stay compact in the UI, but the
// three surfaces that render them — history popover / session card / cron
// card — attach an absolute-time title attribute for hover. The helper
// formatAbsTime is the single source of truth for that string; its call sites
// are what the test actually cares about.
func TestDashboardJS_FormatAbsTimeHoverTitles(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)
	// 1. Helper exists. Without this all three call sites are noise.
	if !strings.Contains(js, "function formatAbsTime(ms)") {
		t.Error("dashboard.js: formatAbsTime helper must be defined")
	}
	// 2. History popover wires it into hp-meta ago span title.
	if !strings.Contains(js, "const abs = s.last_active ? formatAbsTime(s.last_active) : '';") {
		t.Error("history popover must compute absolute time for the ago span title")
	}
	if !strings.Contains(js, "(ago ? '<span' + (abs ? ' title=\"' + escAttr(abs) + '\"' : '') + '>' + ago + '</span>' : '')") {
		t.Error("history popover ago span must attach title with formatAbsTime output")
	}
	// 3. Session card sc-time span gets the same treatment.
	if !strings.Contains(js, "const absTime = s.last_active ? formatAbsTime(s.last_active) : '';") {
		t.Error("session card must compute absolute time for sc-time title")
	}
	if !strings.Contains(js, "class=\"sc-time\"' + (absTime ? ' title=\"' + escAttr(absTime)") {
		t.Error("session card sc-time must attach title with formatAbsTime output")
	}
	// 4. Cron card last/next run meta spans carry absolute-time tooltips.
	if !strings.Contains(js, "const nextAbs = j.next_run ? formatAbsTime(j.next_run) : '';") {
		t.Error("cron card must compute next_run absolute time")
	}
	if !strings.Contains(js, "const lastAbs = j.last_run_at ? formatAbsTime(j.last_run_at) : '';") {
		t.Error("cron card must compute last_run_at absolute time")
	}
	if !strings.Contains(js, `title="last run: ' + escAttr(lastAbs)`) {
		t.Error("cron card 'ran X' span must attach last run absolute time title")
	}
	if !strings.Contains(js, `title="next run: ' + escAttr(nextAbs)`) {
		t.Error("cron card 'next X' span must attach next run absolute time title")
	}
}

// TestDashboardJS_EmptyStateCTAs pins the R110-P2 empty-state enrichment. The
// legacy 'no sessions' and 'no history' strings stay in the DOM so the
// existing E2E assertions (test/e2e/dashboard.test.js toContain) keep passing,
// but each empty state now surfaces a visible CTA / hint so first-time users
// aren't left staring at dead panels. The cron empty state was already
// CTA-rich before this round and is not touched here.
func TestDashboardJS_EmptyStateCTAs(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)
	// Sidebar: legacy string present (protects E2E) AND new CTA button.
	if !strings.Contains(js, `class="no-sessions">no sessions`) {
		t.Error("dashboard.js: legacy 'no sessions' substring must remain for E2E toContain assertions")
	}
	if !strings.Contains(js, `class="no-sessions-cta" onclick="createNewSession()"`) {
		t.Error("dashboard.js: no-sessions empty state must expose a CTA wired to createNewSession()")
	}
	if !strings.Contains(js, "开启你的第一个会话") {
		t.Error("dashboard.js: no-sessions CTA must carry the Chinese guidance label")
	}
	// History popover: legacy 'no history' substring + hint.
	if !strings.Contains(js, `class="history-popover-empty">no history`) {
		t.Error("dashboard.js: legacy 'no history' substring must remain in empty popover")
	}
	if !strings.Contains(js, `class="hp-empty-hint"`) {
		t.Error("dashboard.js: history empty popover must expose the hp-empty-hint element")
	}
	if !strings.Contains(js, "发起对话后，历史记录会出现在这里") {
		t.Error("dashboard.js: history empty popover must carry the Chinese hint")
	}
}

// TestDashboardHTML_SectionHeaderNotAllCaps pins the R110-P2 project-group
// header style tweak: removes CSS text-transform:uppercase and bumps font
// size 11px → 12px so user-visible project names render in their natural
// case (Title / Mixed / CJK) without losing density. textContent of the
// .section-header elements is unchanged, so E2E assertions like
// `expect(headers).toContain('myproject')` still pass.
func TestDashboardHTML_SectionHeaderNotAllCaps(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)
	// Must contain the new rule (not uppercase, 12px, no letter-spacing).
	if !strings.Contains(html, "text-transform:none") {
		t.Error("section-header must drop text-transform:uppercase — project names render in natural case now")
	}
	if !strings.Contains(html, ".section-header{padding:6px 12px 6px 16px;font-size:12px;") {
		t.Error("section-header must use font-size:12px on desktop")
	}
	// Forbid the legacy rule that still includes uppercase + letter-spacing,
	// to catch accidental reverts that paste the old block back in full.
	legacy := ".section-header{padding:6px 12px 6px 16px;font-size:11px;font-weight:600;color:var(--nz-text-mute);text-transform:uppercase"
	if strings.Contains(html, legacy) {
		t.Error("section-header legacy uppercase rule must not reappear; drop text-transform:uppercase")
	}
}

// TestDashboardHTML_BodyFontStackSupportsCJK pins the R110-P2 font-stack
// fix. The legacy body rule ended in `monospace` — Chromium on Linux
// resolves that to DejaVu Sans Mono, a fixed-width latin face whose CJK
// glyphs come from uneven fallback metrics, giving the whole UI a
// typewriter feel. The new stack adds the common CJK system fonts before
// falling through to sans-serif so Chinese readers get native glyph
// metrics while Apple/Windows users keep San Francisco / Segoe UI.
// Code paths (`.md-code`, `.md-pre code`, `.fv-body pre`) already set
// their own monospace stack inline and should stay that way.
func TestDashboardHTML_BodyFontStackSupportsCJK(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)
	// Every body font-family rule must include the CJK anchor AND terminate
	// in sans-serif (not monospace).
	wantSubstrings := []string{
		"'PingFang SC'",
		"'Noto Sans CJK SC'",
		"sans-serif;background:var(--nz-bg-0)",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(html, s) {
			t.Errorf("dashboard.html body font stack missing %q", s)
		}
	}
	// Forbid the legacy stack so an accidental revert is caught.
	legacy := "body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',monospace;"
	if strings.Contains(html, legacy) {
		t.Errorf("dashboard.html still uses legacy mono-terminated body font stack")
	}
	// Sanity: code-oriented selectors still use monospace internally so this
	// rollback doesn't accidentally de-fix code rendering.
	if !strings.Contains(html, ".md-code{background:var(--nz-border);color:#e6edf3;padding:1px 5px;border-radius:4px;font-size:13px;font-family:'SF Mono'") {
		t.Error("dashboard.html inline code `.md-code` must keep its monospace stack")
	}
}

// TestDashboardJS_CheatsheetEntriesCoverCoreShortcuts pins the R110-P2 Help
// modal expansion. The entries array is the single source of truth for the
// rendered modal; any edit must include these three keyboard shortcuts that
// actually exist in handleKey (Enter/Shift+Enter/double-Esc). Guarding the
// data source rather than the rendered HTML lets us trust the render path
// separately — renderCheatsheetHTML is already tested by the auth/focus trap
// suite.
func TestDashboardJS_CheatsheetEntriesCoverCoreShortcuts(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)
	for _, want := range []string{
		`{ keys: ['Enter'], desc: '发送消息' }`,
		`{ keys: ['Shift', 'Enter'], desc: '输入框内换行' }`,
		`{ keys: ['Esc', 'Esc'], desc: '双击 Esc 打断当前运行中的回复' }`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("CHEATSHEET_ENTRIES missing expected entry: %s", want)
		}
	}
	// The sections array must still read 会话 / 消息 / 斜杠命令 / 上传 / 帮助 —
	// missing a section header breaks the CSS grid layout (ks-section spans
	// the full row) and leaves operators unable to discover non-keyboard
	// features (slash commands, upload) via the Help panel.
	for _, want := range []string{
		`{ section: '会话' }`,
		`{ section: '消息' }`,
		`{ section: '斜杠命令' }`,
		`{ section: '上传' }`,
		`{ section: '帮助' }`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("CHEATSHEET_ENTRIES missing section header: %s", want)
		}
	}
}

// TestDashboardJS_CheatsheetDocumentsSlashCommands pins R110-P2 Help
// content expansion: every slash command actually routed by
// internal/dispatch/commands.go must appear in the cheatsheet so IM
// operators can discover them via the `?` panel instead of reading the
// source. The command set here mirrors the handler's `case` arms.
//
// Deliberately not listing: agent commands (e.g. `/review`) are
// config-driven (`agent_commands` map in config.yaml) and vary per
// deployment; the cheatsheet shows the generic `/new <agent>` wrapper
// instead, which works in every deployment.
func TestDashboardJS_CheatsheetDocumentsSlashCommands(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)
	for _, cmd := range []string{"/new", "/cd", "/pwd", "/project", "/cron", "/help"} {
		want := "keys: ['" + cmd + "']"
		if !strings.Contains(js, want) {
			t.Errorf("cheatsheet must document slash command %q (look for %q)", cmd, want)
		}
	}
	// Upload section must carry both the paperclip icon row and the
	// drag-drop row — both features are present in the UI (see
	// `openFilePicker()` and the `#input-area` dragover/drop listeners)
	// but neither had any discoverability surface until this round.
	for _, want := range []string{
		`keys: ['📎']`,
		`keys: ['拖拽']`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("upload section missing expected row: %s", want)
		}
	}
}

// TestDashboardJS_SectionHeaderHasNewButton pins that every project section
// header carries a compact `+` button that invokes newSessionInProject, so
// groups with existing sessions — and empty favorite groups where the
// full-width "New session in X" CTA used to live — can still create from the
// header without using the top-right `+` (which opens a generic modal
// requiring re-typing the workspace).
func TestDashboardJS_SectionHeaderHasNewButton(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)
	// The new button must carry the sh-new class so CSS scoping works, must
	// be inside sectionHeaderHtml (not elsewhere), and must wire to
	// newSessionInProject.
	if !strings.Contains(js, `class="sh-btn sh-new"`) {
		t.Error("section header must include sh-btn.sh-new button")
	}
	if !strings.Contains(js, `onclick="event.stopPropagation();newSessionInProject(this.dataset.name,this.dataset.node)"`) {
		t.Error("sh-new button must wire onclick to newSessionInProject via stopPropagation")
	}
	// The legacy sectionEmptyHtml CTA was removed — guard against regressions:
	// empty favorite groups now rely on the header sh-new `+` button alone,
	// and a redundant full-width "New session in X" row directly below would
	// just duplicate the affordance.
	if strings.Contains(js, "function sectionEmptyHtml") {
		t.Error("sectionEmptyHtml was removed; empty favorite groups rely on the header sh-new button alone")
	}
	if strings.Contains(js, `class="section-empty"`) {
		t.Error("section-empty row was removed; header sh-new button is the sole per-project create CTA")
	}
}

// TestDashboardJS_AuthModalHintsAtConfig pins the R110-P3 login brand hint:
// first-time operators see a concise pointer to dashboard_token in
// config.yaml so the modal isn't a dead end. The hint sits between <h3> and
// <input> so focus order / trapFocus still work.
func TestDashboardJS_AuthModalHintsAtConfig(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)
	if !strings.Contains(js, `class="auth-hint"`) {
		t.Error("auth modal must include .auth-hint element")
	}
	if !strings.Contains(js, `<code>config.yaml</code>`) ||
		!strings.Contains(js, `<code>dashboard_token</code>`) {
		t.Error("auth hint must mention both config.yaml and dashboard_token in inline code")
	}
	data2, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data2)
	if !strings.Contains(html, ".modal .auth-hint{") {
		t.Error("dashboard.html must style .auth-hint")
	}
	if !strings.Contains(html, ".modal .auth-hint code{") {
		t.Error("dashboard.html must style .auth-hint code pill")
	}
}

// TestDashboardJS_CronEmptyStateSub pins the R110-P2 cron-panel empty state
// enrichment: legacy English lines stay (E2E
// test/e2e/dashboard.test.js:988-989 toContain both), plus a Chinese
// sub-hint explains the feature for unfamiliar operators.
func TestDashboardJS_CronEmptyStateSub(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)
	// Legacy lines must remain for E2E.
	for _, want := range []string{
		`class="cron-empty-hint">No cron jobs yet</div>`,
		`class="cron-empty-cta" onclick="createNewCronJob()">Create your first cron job</button>`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("cron empty state missing legacy string for E2E compatibility: %s", want)
		}
	}
	// New sub-hint.
	if !strings.Contains(js, `class="cron-empty-sub">让 naozhi 按计划自动在某个工作目录下运行 prompt</div>`) {
		t.Error("cron empty state must include the Chinese sub-hint")
	}
	data2, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data2)
	if !strings.Contains(html, ".cron-empty-sub{") {
		t.Error("dashboard.html must style .cron-empty-sub")
	}
}

// TestDashboardJS_LoginRetryCountdown pins the R110-P2 WS auth rate-limit
// countdown front-end fix. The old saveToken catch-all branch treated a 429
// response the same as 401, rendering "invalid token — try again" even
// while the server was still locking the IP out — users retried
// immediately and racked up more 429s. The new branch reads Retry-After,
// disables the input + save button, and counts down via a per-input
// setInterval (timer id tracked on dataset.countdownId so re-entering
// clears the prior timer).
func TestDashboardJS_LoginRetryCountdown(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)
	// The 429 branch must exist in saveToken.
	if !strings.Contains(js, "} else if (r.status === 429) {") {
		t.Error("saveToken must branch on 429 before the generic else")
	}
	// Retry-After parsing with default must be present.
	if !strings.Contains(js, `r.headers.get('Retry-After')`) {
		t.Error("saveToken must read Retry-After header from 429 response")
	}
	// Helper function must exist and guard against re-entry.
	if !strings.Contains(js, "function startLoginRetryCountdown(seconds)") {
		t.Error("startLoginRetryCountdown helper must be defined")
	}
	if !strings.Contains(js, `input.dataset.countdownId`) {
		t.Error("startLoginRetryCountdown must track timer id on dataset.countdownId to prevent double-scheduling")
	}
	// Human-readable placeholder must include the remaining seconds.
	if !strings.Contains(js, "'登录尝试过多，请在 ' + remaining + 's 后重试'") {
		t.Error("countdown placeholder must surface the remaining-seconds hint")
	}
	// Input + save button must both toggle disabled so users can't keep
	// hammering the save click during the window.
	if !strings.Contains(js, "input.disabled = true;") ||
		!strings.Contains(js, "saveBtn.disabled = true") {
		t.Error("countdown must disable both the input and the primary save button")
	}
	// On zero, input must re-enable + refocus + placeholder reset.
	// Round 155 localized the default placeholder to Chinese; keep the
	// structural assertion but adapt the literal.
	if !strings.Contains(js, "input.disabled = false;") ||
		!strings.Contains(js, "input.focus();") ||
		!strings.Contains(js, `input.placeholder = '请输入 dashboard token…'`) {
		t.Error("countdown teardown must re-enable the input, refocus, and restore the default placeholder")
	}
}

// TestDashboardJS_RecentProjectsPaletteOrdering pins the R110-P3 recent
// projects feature. Three invariants worth protecting:
//  1. doCreateInProject must push the (name,node) pair so project
//     creations are the only events that feed the recent list — custom
//     workspaces have no stable identifier.
//  2. loadRecentProjects must tolerate localStorage corruption / empty
//     / Safari-private errors by returning []; the palette must not
//     crash if the JSON is manually edited.
//  3. renderPaletteList's q=="" branch must consult the recent list
//     and apply it via a rank map. The search branch (q!="") must
//     stay untouched — recent projects mustn't jump matches.
func TestDashboardJS_RecentProjectsPaletteOrdering(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// Constants must be defined so callers can't accidentally grow them
	// to unsafe sizes — the stored blob is surfaced to every tab.
	for _, want := range []string{
		"const RECENT_PROJECTS_KEY = 'naozhi_recent_projects';",
		"const RECENT_PROJECTS_MAX = 10;",
		"const RECENT_PROJECTS_SHOW = 5;",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("missing constant: %s", want)
		}
	}

	// Loader must exist and swallow errors (try/catch with return []).
	if !strings.Contains(js, "function loadRecentProjects()") {
		t.Error("loadRecentProjects helper must be defined")
	}
	// The error-silent contract: the only way the palette stays robust
	// to private-browsing setItem throws is a try/catch that returns [].
	if !strings.Contains(js, "} catch (_) {") {
		t.Error("loadRecentProjects must catch JSON/localStorage errors and fall back to []")
	}

	// Writer must exist and be invoked from doCreateInProject.
	if !strings.Contains(js, "function pushRecentProject(name, node)") {
		t.Error("pushRecentProject helper must be defined")
	}
	if !strings.Contains(js, "pushRecentProject(projectName, nodeId || 'local');") {
		t.Error("doCreateInProject must invoke pushRecentProject for every successful creation")
	}

	// Render branch: q==="" must call loadRecentProjects and build a
	// rank map. Search branch must remain its standalone sort. The rank
	// map was renamed rankMap → recentRank in the R110-P3 three-tier sort
	// (favorite + recent + rest); accept the current name so the
	// refactor doesn't break this older test — it's an orthogonal
	// invariant about the recent-projects signal still being consulted.
	if !strings.Contains(js, "const recents = loadRecentProjects();") {
		t.Error("renderPaletteList q='' branch must consult loadRecentProjects")
	}
	if !strings.Contains(js, "recentRank.set(e.name + '|' + (e.node || 'local'), i);") {
		t.Error("renderPaletteList must compose rank keys as name|node for partition (look for recentRank.set after the R110-P3 three-tier sort refactor)")
	}
	if !strings.Contains(js, "if (q) {\n    scored.sort((a, b) => b.score - a.score);") {
		t.Error("search branch (q!='') must keep its standalone sort untouched")
	}
}

// TestDashboardJS_CmdKOpensPalette pins the R110-P3 Cmd/Ctrl+K
// keybinding. The listener must:
//   - fire on the literal key 'k' (not 'K' — case varies between
//     browsers / OSes, but Chromium consistently lowercases when a
//     modifier is held)
//   - require metaKey || ctrlKey, ignoring bare 'k'
//   - bail when another modal/palette is already open to avoid
//     stacking overlays on repeated hits
//   - invoke createNewSession() — the same entry point the header
//     `+` button uses, so the recent-projects ordering is reused.
//
// Additionally, CHEATSHEET_ENTRIES must surface the shortcut so users
// can discover it via the `?` modal; missing from the cheatsheet is
// the main reason hidden keybindings go unused.
func TestDashboardJS_CmdKOpensPalette(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)
	if !strings.Contains(js, "if (!(e.metaKey || e.ctrlKey) || e.key !== 'k') return;") {
		t.Error("Cmd/Ctrl+K handler must gate on meta||ctrl AND e.key === 'k'")
	}
	if !strings.Contains(js, "if (document.querySelector('.modal-overlay, .cmd-palette-overlay')) return;\n  e.preventDefault();\n  createNewSession();") {
		t.Error("Cmd/Ctrl+K handler must no-op when a modal is open and otherwise call createNewSession()")
	}
	// Cheatsheet surfacing so the shortcut is discoverable.
	if !strings.Contains(js, "{ keys: ['Cmd/Ctrl', 'K'], desc: '打开新建会话面板（最近使用置顶）' }") {
		t.Error("CHEATSHEET_ENTRIES must document Cmd/Ctrl+K")
	}
}

// TestDashboardJS_WSReconnectUX pins the WebSocket recovery UX after the
// noise-reduction pass that routed connection-state signalling into the
// sidebar status line (and removed the reconnect/reconnected toasts that
// covered the mobile header).
//
// Invariants:
//  1. setState must capture the previous state BEFORE mutating to
//     detect a non-CONNECTED → CONNECTED transition. _everConnected
//     stays on wsm as a discriminator, but no toast is emitted on
//     reconnection — the sidebar status row (amber→green dot, outage
//     row removal) carries the signal instead.
//  2. No reconnect-state toast regressions: neither "已重新连接" nor
//     the manual-reconnect "正在重连…" may come back — they were the
//     exact strings the UX sweep excised.
//  3. The sidebar-status row gains a "重连" button only when the
//     status resolves to 'disconnected' (the stable HTTP-fallback
//     state, backoff > 8s). Short reconnect windows stay
//     button-free because the auto-retry is imminent.
//  4. reconnectNow helper: cancels any pending timer, resets backoff
//     to 1000ms, and calls wsm.connect(). The previous transient
//     toast was removed because the sidebar already reflects the
//     CONNECTING transition.
func TestDashboardJS_WSReconnectUX(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)
	// 1. _everConnected flag declared on wsm (kept for state differentiation,
	// not for toast gating any more).
	if !strings.Contains(js, "_everConnected: false,") {
		t.Error("wsm must declare _everConnected: false as the first-handshake discriminator")
	}
	// 2. setState captures prev BEFORE mutation.
	if !strings.Contains(js, "const prev = this.state;\n    this.state = s;") {
		t.Error("setState must capture previous state before mutating this.state")
	}
	// 3. Negative guard: the reconnect toast must NOT come back. If a
	//    future change re-introduces it, this test fires.
	if strings.Contains(js, "showToast('已重新连接'") {
		t.Error("reconnect-success toast must stay removed — sidebar status dot + outage row removal already signal recovery")
	}
	// 3b. Negative guard: manual reconnect must NOT toast either.
	if strings.Contains(js, "showToast('正在重连") {
		t.Error("manual reconnectNow() must not emit a toast — sidebar status row already flips to connecting...")
	}
	// 4. Reconnect button surfaces only at stable-disconnect state.
	if !strings.Contains(js, "const showReconnect = statusKey === 'disconnected';") {
		t.Error("updateStatusBar must compute showReconnect from the stable disconnected statusKey")
	}
	if !strings.Contains(js, `class="status-reconnect" onclick="reconnectNow()"`) {
		t.Error("reconnect button must wire onclick to reconnectNow()")
	}
	// 5. reconnectNow helper semantics.
	if !strings.Contains(js, "function reconnectNow() {") {
		t.Error("reconnectNow helper must be defined")
	}
	if !strings.Contains(js, "clearTimeout(wsm.reconnectTimer);") {
		t.Error("reconnectNow must clear the pending reconnectTimer so backoff doesn't re-arm after manual reconnect")
	}
	if !strings.Contains(js, "wsm.backoff = 1000;") {
		t.Error("reconnectNow must reset backoff to the 1000ms floor")
	}
	if !strings.Contains(js, "wsm.connect();") {
		t.Error("reconnectNow must invoke wsm.connect() to kick a new attempt")
	}
}

// TestDashboardHTML_NoiseReductionStyles pins the CSS hooks that back the
// toast-to-inline migration. The JS side renders these class names via
// innerHTML into elements inside dashboard.html, so the corresponding
// style rules must live in the <style> block — a silent CSS rename would
// leave the signals functional but invisible.
//
//   - .status-authwait backs the auth rate-limit countdown that used to
//     live in a top-of-screen toast. The old toast covered the mobile
//     header and flickered every second; moving the countdown into the
//     sidebar status line preserves the signal without the noise.
//   - .msg-queued-chip attaches to the optimistic user bubble when
//     send_ack returns status=queued. Replaces the "消息已排队" toast so
//     the signal binds to the bubble (it disappears naturally when the
//     real "user" event replaces the optimistic bubble).
func TestDashboardHTML_NoiseReductionStyles(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	css := string(data)
	if !strings.Contains(css, ".sidebar-status .status-authwait{") {
		t.Error("dashboard.html must define .status-authwait — backs the in-status-row rendering of the auth retry countdown")
	}
	if !strings.Contains(css, ".event.user.optimistic-msg .msg-queued-chip{") {
		t.Error("dashboard.html must define .msg-queued-chip on .event.user.optimistic-msg — backs the inline queued indicator that replaces the '消息已排队' toast")
	}
}

// TestDashboardHTML_ReconnectButtonStyled pins the CSS presence of the
// .status-reconnect button. The button is injected by dashboard.js via
// innerHTML into the sidebar-status container, so the style rules must
// live in dashboard.html's <style> block. Both the base rule and the
// hover rule are required for the button to be discoverable (base =
// neutral low-chrome, hover = accent signal).
func TestDashboardHTML_ReconnectButtonStyled(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)
	if !strings.Contains(html, ".sidebar-status .status-reconnect{") {
		t.Error("dashboard.html must define .status-reconnect base rule")
	}
	if !strings.Contains(html, ".sidebar-status .status-reconnect:hover{") {
		t.Error("dashboard.html must define .status-reconnect:hover rule for affordance")
	}
	// Hover must use --nz-accent to match the rest of the app's
	// "interactive primary" semantics.
	if !strings.Contains(html, ".sidebar-status .status-reconnect:hover{color:var(--nz-accent);border-color:var(--nz-accent);") {
		t.Error(".status-reconnect:hover must use --nz-accent for color AND border to match in-app primary treatment")
	}
}

// TestDashboardJS_MarkdownExport pins the UX-P2 session Markdown export
// feature. The export pipeline is split into three pieces that must stay
// wired: a button in the main header, an ignore set that mirrors the UI
// render filter, and formatSessionMarkdown that produces the document
// body. Each lives behind a grep-checkable contract so a future refactor
// has to touch the test when it reshapes any of them.
func TestDashboardJS_MarkdownExport(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// 1. Main header emits the download button gated on selectedKey only
	//    so both managed and discovered sessions can export.
	if !strings.Contains(js, `class="btn-rename btn-download" onclick="downloadSessionMarkdown()"`) {
		t.Error("main header must emit the btn-download button wired to downloadSessionMarkdown()")
	}

	// 2. Ignore set mirrors the UI filter (tool_use/result/agent/etc.),
	//    plus `thinking` which the UI also hides. Drift between this
	//    set and INTERNAL_EVENT_TYPES would cause the export to contain
	//    content the operator never saw.
	if !strings.Contains(js, "const MARKDOWN_EXPORT_IGNORE = new Set(['tool_use', 'result', 'agent', 'task_start', 'task_progress', 'task_done', 'thinking']);") {
		t.Error("MARKDOWN_EXPORT_IGNORE must list exactly the UI-hidden types (tool_use/result/agent/task_*/thinking)")
	}

	// 3. Formatter signature + key behaviours.
	if !strings.Contains(js, "function formatSessionMarkdown(meta, events) {") {
		t.Error("formatSessionMarkdown helper must be defined as a pure function")
	}
	// Metadata anchors at the top. Round 149 localized the five label
	// strings (Session/Node/Workspace/Cost/Exported at → 会话/节点/工作目录/
	// 花费/导出时间) to parity with the already-Chinese `## 用户 / ## 助手`
	// bubble headings. CLI stays English: it is a proper noun used in the
	// UI `.cli-label` chip too, so localizing it here would create
	// *more* inconsistency, not less.
	for _, want := range []string{
		`lines.push('# ' + (meta.title || '未命名会话'));`,
		"lines.push('- **会话**: `' + meta.key + '`');",
		"lines.push('- **CLI**: ' + meta.cli);",
		"lines.push('- **花费**: $' + (meta.cost.toFixed ? meta.cost.toFixed(4) : meta.cost));",
		"lines.push('- **导出时间**: ' + new Date().toISOString());",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("formatSessionMarkdown must emit header line: %s", want)
		}
	}
	// Reverse-assert: the old English labels must be gone from the
	// formatter. This guards against a future "localization revert" that
	// silently flips individual lines back — the whole block needs to
	// stay consistent.
	for _, stale := range []string{
		"'- **Session**: `'",
		"'- **Node**: `'",
		"'- **Workspace**: `'",
		"'- **Cost**: $'",
		"'- **Exported at**: '",
	} {
		if strings.Contains(js, stale) {
			t.Errorf("formatSessionMarkdown must not emit legacy English label: %s", stale)
		}
	}
	// Bubble headings keep Chinese labels (UI uses them too).
	if !strings.Contains(js, `lines.push('## 用户' + (ts ? ' · ' + ts : ''));`) {
		t.Error("formatSessionMarkdown user bubble must use '## 用户' heading for parity with UI")
	}
	if !strings.Contains(js, `lines.push('## 助手' + (ts ? ' · ' + ts : ''));`) {
		t.Error("formatSessionMarkdown assistant bubble must use '## 助手' heading")
	}
	// Claude system XML injected as user messages must be filtered in
	// the export the same way eventHtml filters them from the UI —
	// otherwise exports become twice as noisy as the live view.
	if !strings.Contains(js, "/^<(task-notification|system-reminder|local-command|command-name|available-deferred-tools)[\\s>]/.test(raw)") {
		t.Error("formatSessionMarkdown must filter the same Claude system XML as the UI eventHtml path")
	}

	// 4. Download path: Blob → objectURL → anchor click. The revoke
	//    must defer through setTimeout because some browsers race the
	//    click handler against immediate revocation.
	for _, want := range []string{
		"async function downloadSessionMarkdown() {",
		"const blob = new Blob([md], { type: 'text/markdown;charset=utf-8' });",
		"a.download = sessionMarkdownFilename(title, Date.now());",
		"setTimeout(() => URL.revokeObjectURL(href), 60000);",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("downloadSessionMarkdown must include: %s", want)
		}
	}

	// 5. Filename sanitizer strips filesystem-hostile chars. Regex as a
	//    string so the test tolerates Go/JS escaping asymmetry.
	if !strings.Contains(js, "/[\\\\\\/:*?\"<>|\\x00-\\x1f]+/g") {
		t.Error("sessionMarkdownFilename must strip filesystem-hostile characters via the documented regex")
	}
	if !strings.Contains(js, "return 'naozhi-' + safe + '-' + stamp + '.md';") {
		t.Error("sessionMarkdownFilename must return the 'naozhi-<title>-<date>.md' shape")
	}
}

// TestDashboardJS_BubbleActionsHoverOnly pins the unified display contract
// for the two bubble-footer actions (复制 / ↗ 追问): both must gate on the
// SAME "long message" predicate (cleanRaw.length > 500) via a single
// `isLong` flag, and both must always carry `.hover-only` so the buttons
// only surface on .event hover / keyboard focus. Short bubbles render
// neither button; long bubbles render both, and both stay hidden until
// hover — keeping the default view uncluttered and preventing the two
// actions from drifting apart again.
func TestDashboardJS_BubbleActionsHoverOnly(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)
	// Shared predicate — one source of truth for "is this bubble long
	// enough to deserve a toolbar?".
	if !strings.Contains(js, "const isLong = !!cleanRaw && cleanRaw.length > 500;") {
		t.Error("copy + ask buttons must share a single `isLong` gate (cleanRaw.length > 500)")
	}
	// Copy button: gated on isLong + text/user type, always `.hover-only`.
	if !strings.Contains(js, "const copyBtn = isLong && (e.type === 'text' || e.type === 'user')\n    ? '<button class=\"event-copy-btn hover-only\"") {
		t.Error("copy button must gate on isLong AND always carry the .hover-only class")
	}
	// Ask button: gated on isLong + AI text type only, always `.hover-only`.
	if !strings.Contains(js, "const askBtn = isLong && e.type === 'text'\n    ? '<button class=\"event-ask-btn hover-only\"") {
		t.Error("ask button must gate on isLong AND always carry the .hover-only class")
	}
	// Accessibility contract — title + aria-label survive. Round 154
	// localized the aria-label from "Copy message" → "复制消息" to match
	// the otherwise-Chinese a11y surface; the title was already "复制".
	if !strings.Contains(js, `title="复制" aria-label="复制消息"`) {
		t.Error("event-copy-btn must carry title='复制' + aria-label='复制消息' for a11y")
	}
	if strings.Contains(js, `aria-label="Copy message"`) {
		t.Error("legacy English event-copy-btn aria-label \"Copy message\" must be removed after Round 154")
	}
	if !strings.Contains(js, `title="基于此内容追问"`) {
		t.Error("event-ask-btn must carry title='基于此内容追问' for hover tooltip")
	}
	// Regression guard — the legacy divergent gates must not return:
	//   - copy button rendered on every non-empty bubble
	//   - ask button with its own separate `length > 10` threshold
	if strings.Contains(js, "(e.type === 'text' || e.type === 'user') && cleanRaw\n    ?") {
		t.Error("legacy universal copy-button gate must not return — short bubbles should render no button")
	}
	if strings.Contains(js, "cleanRaw.length > 10") {
		t.Error("ask button must not reintroduce its own >10 threshold; it must share isLong with copy")
	}
}

// TestDashboardHTML_CopyButtonHoverOnly pins the hover-reveal CSS for the
// short-bubble variant. Three invariants: base `.event-copy-btn` class
// still renders (for the permanently-visible long bubbles), `.hover-only`
// sets opacity:0, and `.event:hover` as well as `:focus-visible` both
// bring the button back — the focus-visible branch is specifically for
// keyboard users who never trigger mouse hover.
func TestDashboardHTML_CopyButtonHoverOnly(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)
	if !strings.Contains(html, ".event-copy-btn.hover-only{opacity:0}") {
		t.Error(".hover-only variant must default to opacity:0")
	}
	if !strings.Contains(html, ".event:hover .event-copy-btn.hover-only,.event-copy-btn.hover-only:focus-visible{opacity:1}") {
		t.Error("hover + focus-visible rules must lift the opacity so both mouse and keyboard users can reach the button")
	}
	// The `.copied` feedback state must still override opacity so the
	// green "copied" confirmation stays visible long enough for the
	// operator to register it, regardless of hover state.
	if !strings.Contains(html, ".event-copy-btn.copied{color:var(--nz-green);border-color:var(--nz-green);opacity:1}") {
		t.Error(".copied state must force opacity:1 so the success tick persists after the hover leaves")
	}
}

// TestDashboardHTML_AskButtonHoverOnly mirrors the copy-button hover CSS
// for the ↗ 追问 button. The two actions share a single display contract
// (see TestDashboardJS_BubbleActionsHoverOnly), so the CSS must give the
// ask button identical hover/focus-visible reveal rules. If these rules
// drift, long bubbles would show one button but not the other.
func TestDashboardHTML_AskButtonHoverOnly(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)
	if !strings.Contains(html, ".event-ask-btn.hover-only{opacity:0}") {
		t.Error(".event-ask-btn.hover-only must default to opacity:0 so short-bubble callers stay invisible until hover")
	}
	if !strings.Contains(html, ".event:hover .event-ask-btn.hover-only,.event-ask-btn.hover-only:focus-visible{opacity:1}") {
		t.Error("hover + focus-visible rules must lift the ask-button opacity so both mouse and keyboard users can reach it")
	}
}

// TestDashboardHTML_CheatsheetPrimaryIsBlue pins the R110-P2 fix that the
// cheatsheet modal's confirm button ("好的") uses --nz-blue instead of the
// green #238636 used for submit/save actions elsewhere. Scoped selector
// ensures other .primary buttons (create session, save cron, save token) keep
// their existing green to preserve submit-action mental models.
func TestDashboardHTML_CheatsheetPrimaryIsBlue(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)
	want := ".modal.cheatsheet .modal-btns button.primary{background:var(--nz-blue);border-color:var(--nz-blue)}"
	if !strings.Contains(html, want) {
		t.Errorf("dashboard.html missing cheatsheet primary override: %q", want)
	}
	// Sanity: other modals' .primary still uses the shared green rule —
	// if the base rule gets rewritten, the scoped override above loses its
	// meaning and this test flags it.
	base := ".modal .modal-btns button.primary{background:#238636;border-color:#238636;color:#fff}"
	if !strings.Contains(html, base) {
		t.Errorf("dashboard.html must keep shared green .primary rule for submit actions: %q", base)
	}
}

// TestDashboardJS_UX1_UnifiedErrorHelpers pins UX1 — the switch from ad-hoc
// `showToast('send error: ' + e.message)` leakage to a centralized
// localizeAPIError / showAPIError / showNetworkError trio. Without these
// helpers, operators saw raw fetch/HTTP jargon like `send failed: 502` with
// no retry guidance and mixed English/Chinese copy.
//
// The contract has three arms:
//
//  1. The three helpers (localizeAPIError, showAPIError, showNetworkError)
//     must exist in dashboard.js — if they vanish, every replacement call
//     site ends up calling an undefined function, silently failing.
//  2. localizeAPIError must cover the critical status classes (401/403, 429,
//     5xx) and network-failure (status=0) with Chinese copy. This guards
//     against a well-meaning refactor that collapses all of them into a
//     single "HTTP X failed" string.
//  3. Past technical leakage patterns ("send error:", "send failed:",
//     "takeover failed:", etc.) must not reappear as bare showToast calls.
//     We're OK with the strings existing inside the helpers, but they must
//     not be concatenated with `e.message`/`r.status` raw.
func TestDashboardJS_UX1_UnifiedErrorHelpers(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// Arm 1: helper definitions must exist.
	for _, want := range []string{
		"function localizeAPIError(status, raw)",
		"function showAPIError(action, status, raw, duration)",
		"function showNetworkError(action, err, duration)",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("dashboard.js missing UX1 helper definition: %q", want)
		}
	}

	// Arm 2: localizeAPIError must classify the critical status classes
	// with Chinese copy. Each literal is a substring inside the helper's
	// switch/if cascade; the test catches accidental deletions.
	for _, want := range []string{
		"'网络错误'",          // status 0 / connection-refused
		"'鉴权失败，请重新登录'",    // 401 / 403
		"'资源不存在'",         // 404
		"'请求过于频繁，请稍后重试'",  // 429
		"'服务暂时不可用，请稍后重试'", // 502/503/504
	} {
		if !strings.Contains(js, want) {
			t.Errorf("localizeAPIError must classify Chinese copy %q", want)
		}
	}

	// Arm 3: legacy leakage patterns must be gone. We scan for the bare
	// `showToast('<verb> error: ' + e.message)` and
	// `showToast('<verb> failed: ' + r.status)` idioms that the helpers
	// replace. Surviving instances would mean the migration missed a
	// call site and the UX regresses silently.
	forbidden := []string{
		"showToast('send error: ' + e.message",
		"showToast('send failed: ' +",
		"showToast('takeover error: ' + e.message",
		"showToast('takeover failed: ' +",
		"showToast('resume error: ' + e.message",
		"showToast('resume failed')",
		"showToast('close error: ' + e.message",
		"showToast('close failed: ' +",
		"showToast('remove error: ' + e.message",
		"showToast('remove failed: ' +",
		"showToast('create failed: ' +",
		"showToast('save failed: ' +",
		"showToast('delete failed')",
		"showToast('pause failed')",
		"showToast('preview error: ' + e.message",
		"showToast('error: ' + e.message)",
		"showToast('network error'",
	}
	for _, bad := range forbidden {
		if strings.Contains(js, bad) {
			t.Errorf("dashboard.js still contains legacy technical toast %q — replace with showAPIError/showNetworkError", bad)
		}
	}
}

// TestDashboardJS_UX2_ConfirmDialogHelper pins UX2 — the replacement of
// native window.confirm() with a themed confirmDialog() that returns a
// Promise<boolean>. Three arms:
//
//  1. Helper definition must exist, return a Promise, and default focus
//     to the cancel button (safer than defaulting to a destructive
//     primary — an accidental Enter should not delete a session).
//  2. dismissSession must NOT show any confirmation dialog — per operator
//     preference the × button deletes immediately. The swipe-gesture
//     caller still passes `{skipConfirm: true}` so the public signature
//     stays compatible, but no confirmDialog call may exist inside the
//     function body.
//  3. cronDelete must use confirmDialog (not the native confirm), and
//     the legacy English confirm('Delete cron job ...') string must be
//     gone. Surviving `confirm('Delete cron job ...')` in the file would
//     be the clearest regression indicator.
func TestDashboardJS_UX2_ConfirmDialogHelper(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// Arm 1: helper exists with the expected shape.
	for _, want := range []string{
		"function confirmDialog(opts)",
		"return new Promise((resolve) => {",
		// Default focus goes to cancel — protects against stray Enter.
		".confirm-cancel').focus()",
		// Backdrop click cancels (resolve false). The exact `finish(false)`
		// call site moves with refactors; assert the guard comment stays
		// to keep the invariant visible.
		"e.target === overlay",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("confirmDialog helper missing expected structure %q", want)
		}
	}

	// Arm 2: dismissSession must NOT prompt for confirmation. Operator
	// preference is to delete immediately on × click; accidental deletes
	// are recoverable by re-entering the prompt (pending) or reopening
	// the CLI (remote/discovered). Pinning the absence of the legacy
	// confirm titles prevents regressions that would re-add the modal.
	if !strings.Contains(js, "async function dismissSession(key, node") {
		t.Error("dismissSession must keep its (key, node, opts) signature for swipe-gesture callers")
	}
	forbiddenDismiss := []string{
		// Legacy confirm titles for the × button — must not return.
		"'删除会话？'",
		"'关闭外部会话？'",
	}
	for _, bad := range forbiddenDismiss {
		if strings.Contains(js, bad) {
			t.Errorf("dismissSession must not show a confirm dialog: found %q", bad)
		}
	}

	// Arm 3: cronDelete uses confirmDialog, not window.confirm.
	if !strings.Contains(js, "title: '删除定时任务？'") {
		t.Error("cronDelete must use confirmDialog with Chinese title")
	}
	forbidden := []string{
		// Legacy English native confirm on cron delete.
		`confirm('Delete cron job '`,
		`confirm("Delete cron job "`,
	}
	for _, bad := range forbidden {
		if strings.Contains(js, bad) {
			t.Errorf("dashboard.js still uses native confirm() for cron delete: %q", bad)
		}
	}
}

// TestDashboardHTML_UX2_DangerButtonStyle pins the themed .danger button
// variant that confirmDialog relies on. Without this rule the destructive
// button would fall back to the neutral --nz-bg-2 color and be visually
// indistinguishable from the cancel button — undermining the whole point
// of the confirm dialog.
func TestDashboardHTML_UX2_DangerButtonStyle(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)
	want := ".modal .modal-btns button.danger{background:var(--nz-red);border-color:var(--nz-red);color:#fff}"
	if !strings.Contains(html, want) {
		t.Errorf("dashboard.html missing danger button variant: %q", want)
	}
	// The dialog's detail label (mono-spaced key / cron id preview) must
	// have a mute color so it doesn't fight with the primary message.
	detailSel := ".modal.confirm-dialog .confirm-detail"
	if !strings.Contains(html, detailSel) {
		t.Errorf("dashboard.html missing confirm-detail style: %q", detailSel)
	}
}

// TestDashboardJS_R110A11y_IconButtonLabels pins R110-P2 — every icon-only
// button that the operator can click must carry both a non-empty `title`
// (tooltip) and `aria-label` (screen-reader hook). The audit found 6
// buttons that were missing one or the other; this test guards against
// future drift by locating each button via a unique substring sentinel
// and asserting both attributes are present and non-empty.
//
// Literals are intentionally *form-agnostic*: the static bundle may store
// CJK copy as raw UTF-8 or as `\uXXXX` escapes depending on which hook
// formatted the file last, and we don't want the test to whiplash when
// prettier changes its mind. The check is structural: "title=\"" not
// immediately followed by a closing quote means the attribute carries
// SOME value. A regex would catch all forms cleanly.
func TestDashboardJS_R110A11y_IconButtonLabels(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	cases := []struct {
		name, anchor string
	}{
		{"mobile-back", `onclick="mobileBack()"`},
		{"nav-prev", `id="nav-prev"`},
		{"nav-next", `id="nav-next"`},
		{"hold-talk", `id="btn-hold-talk"`},
	}
	titleAttr := regexp.MustCompile(`title="[^"]+"`)
	ariaAttr := regexp.MustCompile(`aria-label="[^"]+"`)
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Find every occurrence of the anchor (mobile-back and nav
			// buttons appear twice — once in the managed session path
			// and once in the discovered-session preview path) and
			// assert each surrounding button tag has both attributes.
			// If any copy is forgotten on one path, that site fails.
			rest := js
			found := false
			for {
				idx := strings.Index(rest, tc.anchor)
				if idx < 0 {
					break
				}
				found = true
				// Locate the enclosing `<button ... >` by scanning
				// backward to '<button' and forward to '>'.
				tagStart := strings.LastIndex(rest[:idx], "<button")
				if tagStart < 0 {
					t.Fatalf("button %s: no <button preceding anchor", tc.name)
				}
				tagEnd := strings.Index(rest[idx:], ">")
				if tagEnd < 0 {
					t.Fatalf("button %s: no '>' after anchor", tc.name)
				}
				tag := rest[tagStart : idx+tagEnd+1]
				if !titleAttr.MatchString(tag) {
					t.Errorf("button %s missing non-empty title=\"...\" in %q", tc.name, tag)
				}
				if !ariaAttr.MatchString(tag) {
					t.Errorf("button %s missing non-empty aria-label=\"...\" in %q", tc.name, tag)
				}
				rest = rest[idx+tagEnd+1:]
			}
			if !found {
				t.Fatalf("anchor %q not found — test wiring stale", tc.anchor)
			}
		})
	}

	// Separate smoke for file-remove — it uses a generated onclick
	// (`removeFile(N)`) so we locate by the class name instead.
	{
		anchor := `<button class="remove"`
		idx := strings.Index(js, anchor)
		if idx < 0 {
			t.Fatalf("file-remove button not found")
		}
		end := strings.Index(js[idx:], ">")
		if end < 0 {
			t.Fatalf("file-remove: no '>' after anchor")
		}
		tag := js[idx : idx+end+1]
		if !titleAttr.MatchString(tag) || !ariaAttr.MatchString(tag) {
			t.Errorf("file-remove button missing title/aria-label: %q", tag)
		}
	}
}

// TestDashboardJS_R110P3_AuthBrandLockup pins R110-P3 — the login modal
// carries a brand lockup (mark + wordmark) so first-time operators
// recognize they're on the right service. Tests the three salient pieces:
//   - mark element (`.ab-mark`) present and decorative (aria-hidden)
//   - Chinese wordmark "脑汁 Naozhi" visible
//   - tagline present (so the brand isn't just the service name)
//
// We assert the CSS hooks exist too so a refactor that rips the brand
// container but leaves stray CSS would still flag.
func TestDashboardJS_R110P3_AuthBrandLockup(t *testing.T) {
	t.Parallel()
	js, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	html, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	jsSrc := string(js)
	htmlSrc := string(html)
	for _, want := range []string{
		`<div class="auth-brand">`,
		`<div class="ab-mark" aria-hidden="true">`,
		`<span class="ab-name">脑汁 Naozhi</span>`,
		`<span class="ab-tag">Claude Code on IM</span>`,
	} {
		if !strings.Contains(jsSrc, want) {
			t.Errorf("auth modal missing brand lockup fragment %q", want)
		}
	}
	for _, want := range []string{
		".modal .auth-brand",
		".modal .auth-brand .ab-mark",
		".modal .auth-brand .ab-wordmark .ab-name",
	} {
		if !strings.Contains(htmlSrc, want) {
			t.Errorf("dashboard.html missing brand CSS selector %q", want)
		}
	}
}

// TestDashboardJS_R117_MainEmptyStateHelper pins the consolidation of the
// three inline `<div class="empty-state">select a session</div>` markup
// strings that followed session dismiss/remove paths. After Round 117 all
// three call sites route through `mainEmptyHtml()`, a helper that returns
// the same Chinese-localized, CTA-bearing empty state the cold-start HTML
// renders. Two arms:
//
//  1. The legacy raw English string must be gone — surviving instances
//     would mean a new dismiss path forgot to call the helper.
//  2. The helper must contain a create-session CTA (the whole point of
//     the enrichment) and use the Chinese lead line to match the rest
//     of the dashboard's post-R110 empty-state pattern.
func TestDashboardJS_R117_MainEmptyStateHelper(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// Arm 1: legacy English empty state must not be re-introduced.
	// Intentionally search for the bare `>select a session<` fragment so
	// comments and test-facing docs (e.g. a future grep in this very
	// test) don't cause false positives.
	forbidden := `<div class="empty-state">select a session</div>`
	if strings.Contains(js, forbidden) {
		t.Errorf("dashboard.js still has raw English empty state %q — route it through mainEmptyHtml()", forbidden)
	}

	// Arm 2: helper exists and carries the expected pieces. The empty state
	// was redesigned post-Round 141 from a "+ 新建会话" button to a
	// "问点什么？" quick-ask textarea — both the lead line and the textarea
	// element must appear in the helper output.
	for _, want := range []string{
		"function mainEmptyHtml()",
		"问点什么？",
		`id="quick-ask-input"`,
		"submitQuickAsk",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("mainEmptyHtml() missing expected fragment %q", want)
		}
	}

	// Also assert the three call sites are present (regression signal if
	// someone accidentally inlines raw HTML again).
	count := strings.Count(js, "mainEmptyHtml()")
	// 3 call sites + 1 definition + possible doc references → >= 4.
	if count < 4 {
		t.Errorf("expected ≥4 mainEmptyHtml references (3 call sites + 1 def); got %d", count)
	}

	// Arm 3: every dismiss path that calls mainEmptyHtml() MUST also call
	// wireQuickAskInput() to rebind the fresh textarea's Enter / auto-grow
	// handlers. Without the rebind, a dismiss-repaint leaves the quick-ask
	// textarea inert — Enter falls through to form submit, refreshing the
	// page. 3 dismiss call sites + 1 function definition + 1 cold-start
	// bootstrap = ≥5 occurrences. The bootstrap intentionally passes
	// autofocus=true; dismiss paths should NOT (autofocus theft on
	// mid-interaction dismiss). We don't assert arg polarity here because
	// the argumentless vs argumented split is a behaviour concern, not a
	// shape concern — a separate test would gate it if it ever regresses.
	wireCount := strings.Count(js, "wireQuickAskInput(")
	if wireCount < 5 {
		t.Errorf("expected ≥5 wireQuickAskInput(...) references (3 dismiss + 1 def + 1 bootstrap); got %d", wireCount)
	}
}

// TestDashboardHTML_R122_FontMonoToken pins the R110-P2 font-stack
// normalization: `--nz-font-mono` is defined in :root so new code can
// consume `var(--nz-font-mono)` instead of copy-pasting the full stack.
// The bare `font-family:monospace` on `.rec-badge` (recording badge)
// migrates to the token so operators don't see an inconsistent, browser-
// default monospace glyph in the mic overlay's timer.
//
// The existing `'SF Mono',ui-monospace,monospace` usages in `.fv-*` /
// `.md-*` / `.auth-hint code` etc. are left alone for now — they're
// visually equivalent to the token and the piecemeal migration is safe
// once we have a contract asserting the token exists.
func TestDashboardHTML_R122_FontMonoToken(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)
	// Token must be defined in :root so every rule can reach it.
	if !strings.Contains(html, "--nz-font-mono:") {
		t.Error("dashboard.html missing --nz-font-mono CSS token in :root")
	}
	// .rec-badge must consume the token (no bare monospace).
	badgeIdx := strings.Index(html, ".rec-badge{")
	if badgeIdx < 0 {
		t.Fatal(".rec-badge rule not found")
	}
	// Read the whole rule body (short — ends at the next `}`).
	end := strings.Index(html[badgeIdx:], "}")
	if end < 0 {
		t.Fatal(".rec-badge rule unterminated")
	}
	rule := html[badgeIdx : badgeIdx+end+1]
	if !strings.Contains(rule, "var(--nz-font-mono)") {
		t.Errorf(".rec-badge must use var(--nz-font-mono); got %q", rule)
	}
	if strings.Contains(rule, "font-family:monospace") {
		t.Errorf(".rec-badge still has bare `font-family:monospace`; got %q", rule)
	}
}

// TestDashboardJS_R122_CronEmptyPromptLocalized pins the cron card
// placeholder localization: the English "(no prompt — tap to set)"
// implied the whole card was clickable to edit, but the card actually
// routes to openCronSession; only the `edit` button triggers editCronJob.
// Chinese copy + accurate affordance.
func TestDashboardJS_R122_CronEmptyPromptLocalized(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)
	// Legacy English placeholder must be gone.
	if strings.Contains(js, "(no prompt — tap to set)") {
		t.Error("cron placeholder still uses misleading English CTA; should direct to edit button")
	}
	// Chinese replacement must be present and point at the edit button.
	want := "未设置 prompt（点右侧 edit 按钮配置）"
	if !strings.Contains(js, want) {
		t.Errorf("cron placeholder missing Chinese copy %q", want)
	}
}

// TestDashboardJS_R123_GithubButtonCTA pins the R110-P2 tooltip fix on
// the project-header GitHub icon. The old title was `"GitHub: " + url`
// which left the click-to-open affordance implicit. New title leads with
// the Chinese verb "在 GitHub 打开仓库：" so the CTA is explicit, and
// aria-label mirrors the verb so screen readers announce the action, not
// just the link target.
//
// Deliberately not asserting url placement: the url is appended after the
// colon in both cases and passes through escAttr; the interesting invariant
// is the presence of the verb phrase.
func TestDashboardJS_R123_GithubButtonCTA(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// Verb-led tooltip must be present — the title literal and the
	// aria-label literal both start with "在 GitHub 打开仓库".
	for _, want := range []string{
		`title="在 GitHub 打开仓库：`,
		`aria-label="在 GitHub 打开仓库 `,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("GitHub button missing CTA fragment %q", want)
		}
	}
	// The old "Show GitHub remote" aria-label must be gone — if it
	// survives, that's an unreachable render branch leaking implicit
	// English to screen readers.
	forbidden := `aria-label="Show GitHub remote"`
	if strings.Contains(js, forbidden) {
		t.Errorf("dashboard.js still has legacy aria-label %q — replace with verb-led Chinese", forbidden)
	}
}

// TestDashboardJS_UX1_WSAuthFailRateLimit pins the finer-grained
// classification inside the WS `auth_fail` branch. The server emits
// "too many attempts" when the login rate limiter trips; that is a
// warn-level wait, not a "invalid token" fail. Collapsing both into
// a single error toast mislead operators into retrying immediately
// and racking up more 429s during the lockout window.
func TestDashboardJS_UX1_WSAuthFailRateLimit(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)
	// The rate-limit arm must (a) still classify via msg.error "too many"
	// substring match, and (b) now route into startWSAuthRetryCountdown
	// (UX-P2) instead of the previous one-shot warn toast. The token-
	// invalid arm must still fall through to showAPIError so operators
	// see the unified error treatment.
	//
	// The countdown helper itself is pinned in detail by
	// TestDashboardJS_UXP2_WSAuthRetryAfterCountdown; here we only verify
	// that the auth_fail branch routes through it instead of the legacy
	// toast copy — that split keeps each test focused on one concern.
	for _, want := range []string{
		"too many",
		"startWSAuthRetryCountdown(retryAfter)",
		"showAPIError('WebSocket 鉴权'",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("WS auth_fail branch must contain %q so rate-limit vs token-invalid diverge", want)
		}
	}
}

// TestDashboardHTML_R110StatusDotPalette pins the Round 126 refactor of
// the session-card status-dot visuals. The side-bar uses three dot
// classes — .dot-running / .dot-ready / .dot-new — and before this
// round running was solid green, ready was solid blue. At 6px the two
// hues were hard to distinguish at a glance in the session list.
//
// The fix introduces three semantic CSS variables
// (--nz-status-running/ready/new) so future state additions (error,
// idle) land in one place instead of hex leaking into rules, and it
// wires .dot-running to the global @keyframes pulse so an actively
// working session is obvious. Ready stays static green. The site-wide
// prefers-reduced-motion guard at the top of the stylesheet disables
// the animation for users with that preference — we assert the guard
// line still exists so the pulse doesn't become an accessibility
// regression if somebody removes it.
func TestDashboardHTML_R110StatusDotPalette(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	css := string(data)

	// Token declarations must be defined (so rules below consume them).
	for _, want := range []string{
		"--nz-status-running:var(--nz-amber)",
		"--nz-status-ready:var(--nz-green)",
		"--nz-status-new:var(--nz-blue)",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("dashboard.html missing status-dot token %q", want)
		}
	}
	// The three dot rules must consume the tokens — if somebody drops a
	// raw hex back in, this test fails so the review catches it.
	for _, want := range []string{
		".dot-running{background:var(--nz-status-running);animation:pulse 1.5s ease-in-out infinite}",
		".dot-ready{background:var(--nz-status-ready)}",
		// dot-new keeps the dashed accent border so it still reads as
		// "not yet started" even with the token migration.
		".dot-new{background:var(--nz-status-new);border:1px dashed var(--nz-accent);width:5px;height:5px}",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("dashboard.html missing dot rule %q", want)
		}
	}
	// @keyframes pulse must still exist (dot-running references it).
	if !strings.Contains(css, "@keyframes pulse{") {
		t.Error("dashboard.html must define @keyframes pulse for .dot-running animation")
	}
	// Site-wide reduced-motion guard must remain — the dot-running
	// animation piggybacks on it instead of replicating a local guard.
	if !strings.Contains(css, "@media (prefers-reduced-motion: reduce)") {
		t.Error("dashboard.html must preserve the global prefers-reduced-motion guard so .dot-running respects it")
	}
	// Legacy green/blue solid-fill rules must be gone — catches a
	// partial revert that would re-introduce the weak contrast.
	for _, forbidden := range []string{
		".dot-running{background:var(--nz-green)}",
		".dot-ready{background:var(--nz-blue)}",
	} {
		if strings.Contains(css, forbidden) {
			t.Errorf("dashboard.html still has legacy rule %q — must route through --nz-status-* tokens", forbidden)
		}
	}
}

// TestDashboardHTML_R110HeaderBadgePalette pins the Round 127 dedup of
// the header badge visual contract. Before this round two rules at
// ~L460 and ~L800 competed for the same .hdr-badge selector: the
// earlier rule hard-coded --nz-red as the default fill, and a later
// "Track D" rule silently overrode it to neutral grey. The two-rules
// pattern made future refactors brittle — a prettier pass or
// single-file cleanup could drop the later rule and the red badge
// would come back to life as an "unread" signal even though the JS
// only sets a cumulative history count.
//
// The fix consolidates the canonical visual definition at the single
// .hdr-badge base rule (neutral grey via --nz-badge-info) and keeps
// only the opt-in .is-alert / .is-warn variants in the Track D block.
// Operators who want red must explicitly add the .is-alert class —
// dashboard.js does this for the cron attention badge only.
func TestDashboardHTML_R110HeaderBadgePalette(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	css := string(data)

	// Token set must exist (Track D tokens).
	for _, want := range []string{
		"--nz-badge-info:#30363d",
		"--nz-badge-info-text:#c9d1d9",
		"--nz-badge-warn:#d29922",
		"--nz-badge-danger:#da3633",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("dashboard.html missing badge token %q", want)
		}
	}
	// Canonical base rule uses the neutral token — catches a revert to
	// the pre-Round-127 red default.
	if !strings.Contains(css, "background:var(--nz-badge-info);color:var(--nz-badge-info-text);border:1px solid var(--nz-border)") {
		t.Error("dashboard.html .hdr-badge base must route through --nz-badge-info (neutral) and border token")
	}
	// Alert / warn variants must still exist so opt-in paths work.
	for _, want := range []string{
		".hdr-badge.is-alert{background:var(--nz-badge-danger);color:#fff;border-color:transparent}",
		".hdr-badge.is-warn{background:var(--nz-badge-warn);color:#1b1b1b;border-color:transparent}",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("dashboard.html missing Track D variant %q", want)
		}
	}
	// The pre-dedup default-red rule must be gone. If this comes back,
	// the whole point of the consolidation is undone.
	legacy := ".hdr-badge{position:absolute;top:-5px;right:-5px;background:var(--nz-red)"
	if strings.Contains(css, legacy) {
		t.Errorf("dashboard.html still contains pre-Round-127 red default %q — must use --nz-badge-info", legacy)
	}
	// There must be EXACTLY one .hdr-badge{ base-rule declaration
	// (the other two are the .is-alert / .is-warn variants). Two
	// sibling `.hdr-badge{` entries without a class qualifier means
	// somebody re-introduced the shadowed pair.
	count := strings.Count(css, ".hdr-badge{")
	if count != 1 {
		t.Errorf("dashboard.html should have exactly one `.hdr-badge{` base rule, got %d — the duplicate was the source of the pre-Round-127 confusion", count)
	}
}

// TestDashboardJS_R110HistoryBadgeIsNeutral pins the JS side of the
// header badge contract. The history badge is a *cumulative* count of
// filesystem-stored sessions that are not in the live workspace — it
// is not an unread / failure / alert signal. Red would misread as
// "something needs attention" when the number has been sitting there
// for months. The rendering path must:
//
//  1. Set textContent + display, but NEVER add the .is-alert class.
//  2. The only `classList.toggle('is-alert', ...)` call in dashboard.js
//     must be the cron attention one — the history badge must not
//     acquire a second such site in the future.
func TestDashboardJS_R110HistoryBadgeIsNeutral(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// The only is-alert toggle in the file should be the cron one.
	// If somebody adds a `hBadge.classList.toggle('is-alert', ...)`
	// or similar, count goes to ≥2 and the test flags it.
	toggleCount := strings.Count(js, "classList.toggle('is-alert'")
	if toggleCount != 1 {
		t.Errorf("dashboard.js has %d `classList.toggle('is-alert'...)` sites — expected exactly 1 (cron attention). The history badge must stay neutral.", toggleCount)
	}
	// The single toggle must be on cronBadge, not on the history
	// badge. If the target variable ever changes to hBadge the
	// single-count check above would still pass, so pin the variable
	// name directly.
	if !strings.Contains(js, "cronBadge.classList.toggle('is-alert'") {
		t.Error("dashboard.js is-alert toggle must target cronBadge (cron attention), not the history badge")
	}
	// Conversely, the history-badge pipeline must still be present
	// and must not touch is-alert.
	hIdx := strings.Index(js, "getElementById('history-badge')")
	if hIdx < 0 {
		t.Fatal("dashboard.js missing history-badge pipeline")
	}
	// Window the next ~400 chars to ensure is-alert does not show
	// up in the history badge update block. This keeps the neutral
	// invariant local to the rendering site.
	end := hIdx + 600
	if end > len(js) {
		end = len(js)
	}
	window := js[hIdx:end]
	if strings.Contains(window, "is-alert") {
		t.Error("dashboard.js history-badge render block must not touch .is-alert — badge is a cumulative count, not an alert")
	}
}

// TestDashboardHTML_R110ModalOverlayScrim pins the Round 128 fix of
// the modal scrim transparency. The R110-P2 UX review flagged that
// the 0.6 overlay let the main content bleed through at 40%
// opacity, competing for attention with the dialog it was supposed
// to focus. We moved to 0.75 to match the macOS / Material spec for
// system-level modals — high enough to suppress background detail,
// low enough that the dialog still reads as "temporary overlay"
// rather than "page has crashed".
//
// The regex tolerates whitespace changes from future prettier
// passes but pins the actual alpha value so a drive-by revert to
// 0.5 or 0.6 fails this test. We also assert the modal body itself
// stays opaque (--nz-bg-1), because "transparent scrim + opaque
// body" and "opaque scrim + opaque body" are both fine, but "opaque
// scrim + translucent body" would put text on top of text.
func TestDashboardHTML_R110ModalOverlayScrim(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	css := string(data)

	// Pin the exact scrim value — regex-based so whitespace/property
	// ordering in future reformats doesn't false-positive.
	re := regexp.MustCompile(`\.modal-overlay\s*\{[^}]*background\s*:\s*rgba\(0\s*,\s*0\s*,\s*0\s*,\s*(0?\.\d+)\)`)
	m := re.FindStringSubmatch(css)
	if m == nil {
		t.Fatal("dashboard.html .modal-overlay must set a rgba(0,0,0,X) background")
	}
	// Minimum threshold: 0.7 — the UX review specifically flagged
	// anything below this as insufficient scrim coverage. Parse to
	// float64 because ASCII string-compare on ".75" vs "0.7" sorts
	// wrong (".75" < "0.7" because '.' < '0').
	alpha, parseErr := strconv.ParseFloat(m[1], 64)
	if parseErr != nil {
		t.Fatalf(".modal-overlay scrim alpha %q failed to parse as float: %v", m[1], parseErr)
	}
	if alpha < 0.7 || alpha > 0.80 {
		t.Errorf(".modal-overlay scrim alpha = %v; want 0.7..0.80 (macOS / Material spec; pre-Round-128 was 0.6)", alpha)
	}

	// Modal body opacity: must be the opaque --nz-bg-1 token so text
	// never overlaps. Plain substring match — the declaration is a
	// single stable line in the minified CSS.
	if !strings.Contains(css, ".modal{background:var(--nz-bg-1)") {
		t.Error("dashboard.html .modal body must keep opaque --nz-bg-1 background so a darker scrim doesn't leak through translucent card")
	}

	// Cheatsheet primary button must already use --nz-blue (this was
	// implicitly true since a prior round but TODO description was
	// stale — lock it so it can't regress back to green).
	if !strings.Contains(css, ".modal.cheatsheet .modal-btns button.primary{background:var(--nz-blue);border-color:var(--nz-blue)}") {
		t.Error("dashboard.html cheatsheet primary button must route through --nz-blue token — green conflicts with the global accent")
	}
}

// TestDashboard_R110HistoryDrawerTimeFormat pins the Round 129 fix to
// the history drawer's day-group formatting. The screenshots that
// drove R110 showed all-caps Latin day headers ("WED, APR 29")
// clashing against 中文 empty-state hints below — the result of a
// global `text-transform: uppercase` that did nothing for 中文
// weekdays ("周三") but shouted at English ones.
//
// The fix is two-fold:
//  1. Drop the uppercase on .hp-day-header + .history-popover-header
//     so the browser-locale formatted date reads naturally in both
//     locales.
//  2. Introduce a historyDayLabel(Date) helper that returns 中文
//     "今天" / "昨天" for the two most common buckets (collapsing the
//     top-of-list noise) and falls back to the browser-locale date
//     for older entries.
//
// Both surfaces are covered here so a future "just restore the
// uppercase" or "drop the today/yesterday shortcut for simplicity"
// change fails CI with a specific message rather than a vague test
// break.
func TestDashboard_R110HistoryDrawerTimeFormat(t *testing.T) {
	t.Parallel()
	cssData, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	css := string(cssData)

	// The two header rules must NOT carry text-transform:uppercase
	// anymore. The regex pins each rule body and flags uppercase.
	for _, selector := range []string{
		`.hp-day-header`,
		`.history-popover-header`,
	} {
		re := regexp.MustCompile(regexp.QuoteMeta(selector) + `\s*\{[^}]*\}`)
		rule := re.FindString(css)
		if rule == "" {
			t.Errorf("dashboard.html missing rule for %s", selector)
			continue
		}
		if strings.Contains(rule, "text-transform:uppercase") {
			t.Errorf("%s rule still has text-transform:uppercase — Round 129 dropped it to avoid WED/APR shouting; rule=%q", selector, rule)
		}
	}

	jsData, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(jsData)

	// historyDayLabel helper must exist at module scope and return
	// the 中文 short-circuits. Prettier rewrites CJK literals as
	// \uXXXX escapes on save (consistent with earlier rounds'
	// observation), so the regex accepts both the raw glyphs and
	// escaped forms — either one preserves the runtime string.
	if !strings.Contains(js, "function historyDayLabel(d)") {
		t.Error("dashboard.js missing historyDayLabel(d) helper — required by Round 129 day-group fix")
	}
	// 今天 = U+4ECA U+5929, 昨天 = U+6628 U+5929.
	todayRe := regexp.MustCompile(`return '(?:今天|\\u4eca\\u5929)'`)
	yesterdayRe := regexp.MustCompile(`return '(?:昨天|\\u6628\\u5929)'`)
	if !todayRe.MatchString(js) {
		t.Error("dashboard.js historyDayLabel missing `return '今天'` short-circuit (raw or unicode-escaped form)")
	}
	if !yesterdayRe.MatchString(js) {
		t.Error("dashboard.js historyDayLabel missing `return '昨天'` short-circuit (raw or unicode-escaped form)")
	}

	// The history popover render path must consume historyDayLabel
	// rather than inlining toLocaleDateString directly (which would
	// bypass the today/yesterday collapse). Pin the call site.
	if !strings.Contains(js, "const dayStr = historyDayLabel(d)") {
		t.Error("dashboard.js history popover must call historyDayLabel(d) — Round 129 routes day-group labels through the helper")
	}

	// Older entries still fall back to the browser-locale date —
	// the toLocaleDateString call must live *inside* historyDayLabel,
	// not at the old inline site. Assert exactly one occurrence in
	// the file (was at the inline site pre-Round-129).
	count := strings.Count(js, "d.toLocaleDateString(undefined, { month: 'short', day: 'numeric', weekday: 'short' })")
	if count != 1 {
		t.Errorf("toLocaleDateString with the weekday+month+day args should occur exactly 1 time (inside historyDayLabel); got %d — an inline caller may have resurfaced", count)
	}
}

// TestDashboardJS_UXP3_UploadReorderHelper pins the UX-P3 "drag-to-reorder
// + keyboard arrow fallback" affordance on upload thumbnail previews.
// Previously users could only delete a picked image; changing the order
// required removing + re-uploading from scratch. This fix adds:
//   - reorderPendingFile(from,to) pure helper that mutates pendingFiles
//   - HTML5 drag-drop gesture on 'ready' thumbnails (uploading/error
//     thumbnails are intentionally pinned so in-flight completion doesn't
//     race with index shifts)
//   - ArrowLeft/ArrowRight keyboard a11y fallback when a thumb is focused
//
// The test is structural, not behavioral — Go can't drive DOM events, but
// the helper existence + render-site wiring is the canonical lever anyway.
func TestDashboardJS_UXP3_UploadReorderHelper(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// 1) Pure helper must exist and be module-scoped (not a closure inside
	//    another function — that would prevent direct unit testing later).
	if !strings.Contains(js, "function reorderPendingFile(from, to)") {
		t.Error("dashboard.js missing reorderPendingFile(from, to) pure helper — UX-P3 reorder needs a testable pure mutator")
	}

	// 2) The helper must bound-check `to` and no-op on equal indices
	//    (asserting presence of the early-return + clamp logic by the
	//    canonical snippet keeps future refactors honest about the
	//    invariants we care about).
	for _, needed := range []string{
		"if (from === to) return false",
		"if (to < 0) to = 0",
		"if (to > pendingFiles.length - 1) to = pendingFiles.length - 1",
	} {
		if !strings.Contains(js, needed) {
			t.Errorf("reorderPendingFile missing guard clause %q — required to keep move semantics safe", needed)
		}
	}

	// 3) The five HTML5 drag handlers + keyboard handler must be defined.
	for _, fn := range []string{
		"function onThumbDragStart(ev, idx)",
		"function onThumbDragOver(ev)",
		"function onThumbDragLeave(ev)",
		"function onThumbDrop(ev, idx)",
		"function onThumbDragEnd()",
		"function onThumbKeyDown(ev, idx)",
	} {
		if !strings.Contains(js, fn) {
			t.Errorf("dashboard.js missing drag/key handler %q", fn)
		}
	}

	// 4) 'uploading' / 'error' thumbs must NOT be draggable. Pin the
	//    render-site guard so a future refactor can't accidentally
	//    make an in-flight upload's index mutate under uploadEntry's feet.
	if !strings.Contains(js, "const draggable = entry.status === 'ready'") {
		t.Error("renderFilePreviews must gate draggable on entry.status==='ready' so uploading/error thumbs keep stable indices")
	}

	// 5) dragstart must also guard status in case ev originates from an
	//    unrelated inline override — defense in depth.
	if !strings.Contains(js, "if (!entry || entry.status !== 'ready') { ev.preventDefault(); return; }") {
		t.Error("onThumbDragStart must early-exit for non-ready thumbnails to prevent re-entrant drags")
	}

	// 6) The keyboard fallback must only react to ArrowLeft/ArrowRight so
	//    it doesn't swallow Tab / Enter / Space (which already mean other
	//    things on the thumb's surrounding buttons).
	if !strings.Contains(js, "if (ev.key !== 'ArrowLeft' && ev.key !== 'ArrowRight') return") {
		t.Error("onThumbKeyDown must restrict its activation to ArrowLeft/ArrowRight")
	}
}

// TestDashboardJS_R110P1_CronStepValueHumanize pins the R110-P1 fix that
// lets humanizeCron recognize "step-value" cron expressions (e.g.
// "*\/15 * * * *" every 15 minutes, "0 *\/6 * * *" every 6 hours on the
// hour). The frequency picker intentionally doesn't round-trip those
// shapes — it emits "@every Nm"/"@every Nh" — but operators commonly
// hand-write step-value cron from crontab manpages or AI-generated
// configs, then the cron card showed the raw "0 *\/6 * * *" instead of
// a friendly "每 6 小时" label.
//
// The label-only hook humanizeCronStepValue keeps the picker's
// round-trip contract intact: it's consulted ONLY in the fallback
// arm of humanizeCron (when parseCronToFreq returns null), and it
// NEVER wires into buildFreqSchedule.
func TestDashboardJS_R110P1_CronStepValueHumanize(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// 1) The label-only helper must exist at module scope — not a
	//    closure inside humanizeCron — so a unit-test harness can
	//    exercise it directly.
	if !strings.Contains(js, "function humanizeCronStepValue(expr)") {
		t.Error("dashboard.js missing humanizeCronStepValue helper — step-value cron won't humanize without it")
	}

	// 2) humanizeCron must consult the label-only hook in its fallback
	//    arm, BEFORE echoing the raw expression. Pin the exact shape so
	//    a refactor can't silently drop it.
	if !strings.Contains(js, "const step = humanizeCronStepValue(expr);") {
		t.Error("humanizeCron fallback arm must call humanizeCronStepValue")
	}
	if !strings.Contains(js, "if (step) return step;") {
		t.Error("humanizeCron must return the step-value label when the hook matches")
	}

	// 3) Two regex shapes must live inside the helper:
	//    - minute step: "*/N" in mm field, "*" in hh
	//    - hour step: "0" in mm field, "*/N" in hh
	//    The string "mm.match" + "hh.match" presence is the anchor;
	//    changing either direction (e.g. supporting "/N" without the "*"
	//    prefix) would quietly accept invalid cron at the label layer.
	if !strings.Contains(js, "mm.match(/^\\*\\/(\\d+)$/)") {
		t.Error("humanizeCronStepValue must regex-match /^\\*\\/(\\d+)$/ against the minute field for `*/N * * * *`")
	}
	if !strings.Contains(js, "hh.match(/^\\*\\/(\\d+)$/)") {
		t.Error("humanizeCronStepValue must regex-match /^\\*\\/(\\d+)$/ against the hour field for `0 */N * * *`")
	}

	// 4) Bounds: the helper must reject N outside reasonable cron ranges
	//    so a malformed expression like "*/99 * * * *" doesn't produce a
	//    misleading "每 99 分钟" label.
	if !strings.Contains(js, "if (n >= 2 && n <= 59) return '每 ' + n + ' 分钟';") {
		t.Error("humanizeCronStepValue minute branch must bound N to [2,59] and emit Chinese label")
	}
	if !strings.Contains(js, "if (n >= 2 && n <= 23) return '每 ' + n + ' 小时';") {
		t.Error("humanizeCronStepValue hour branch must bound N to [2,23] and emit Chinese label")
	}

	// 5) The helper MUST NOT be wired into buildFreqSchedule. Picker
	//    round-trip emits "@every Nm/Nh", not "*/N", and the helper is
	//    label-only. Reverse-assert the helper name doesn't appear in
	//    the build/schedule path so a future refactor can't route it
	//    back through.
	buildIdx := strings.Index(js, "function buildFreqSchedule(")
	if buildIdx < 0 {
		t.Fatal("buildFreqSchedule not found")
	}
	buildEnd := strings.Index(js[buildIdx+40:], "\nfunction ")
	if buildEnd < 0 {
		t.Fatal("couldn't delimit buildFreqSchedule body")
	}
	buildBody := js[buildIdx : buildIdx+40+buildEnd]
	if strings.Contains(buildBody, "humanizeCronStepValue") {
		t.Error("buildFreqSchedule must not consume humanizeCronStepValue — label-only helper would break the picker's round-trip contract")
	}
}

// TestDashboardJS_R110P3_CostTooltipHelper pins the R110-P3 cost-detail
// hover MVP. The session header's `$N.NN` chip previously had no
// tooltip — operators couldn't see when the spend accumulated, which
// session ID it belonged to, or for how long the session had been open.
// The full TODO ask (input/output/cache token breakdown) requires
// backend schema extension and is tracked as residual scope; this MVP
// surfaces the four data points the front-end already has (precise
// cost, firstSeen, lastActive, session_id tail).
//
// The helper is pure so a future unit-test harness can exercise it
// without DOM setup, and — importantly — it MUST return plain text
// (never HTML). Native `title` attributes render as text, so an XSS
// payload in s.user_label that somehow reached this helper would be
// harmless. Pinning the "return lines.join('\\n')" shape locks that
// invariant.
func TestDashboardJS_R110P3_CostTooltipHelper(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// 1) Pure helper at module scope. Not a closure inside updateHeaderCost
	//    so a contract test can reach it via Node/JSDOM harness later.
	if !strings.Contains(js, "function formatHeaderCostTooltip(s, selKey, selNode)") {
		t.Error("dashboard.js missing formatHeaderCostTooltip helper — R110-P3 cost hover needs a pure text builder")
	}

	// 2) Guard: zero cost + no session_id returns ''. Prevents an empty
	//    chip from having a phantom "$0 · …" tooltip that distracts
	//    operators on a freshly-created pending session.
	if !strings.Contains(js, "if (cost <= 0 && !s.session_id) return '';") {
		t.Error("formatHeaderCostTooltip must short-circuit when there's nothing meaningful to show — zero cost + empty session_id should return ''")
	}

	// 3) Four data-point labels in Chinese. Pinning the literal copy so
	//    an i18n refactor touches this line with intent.
	for _, label := range []string{
		"'累计花费: $'",
		"'首次打开: '",
		"'最后活动: '",
		"'会话 ID: …'",
	} {
		if !strings.Contains(js, label) {
			t.Errorf("formatHeaderCostTooltip missing label %q — cost hover MVP requires all four data-point labels", label)
		}
	}

	// 4) session_id must use the last 8 chars (common shim for CLI
	//    --resume handoffs). If somebody changes to first 8, operators
	//    would paste a prefix that doesn't match anything.
	if !strings.Contains(js, "s.session_id.slice(-8)") {
		t.Error("formatHeaderCostTooltip must show the LAST 8 chars of session_id (last is what operators paste for --resume)")
	}

	// 5) Plain-text return contract: must use \n joiner (native `title`
	//    renders line breaks), NOT <br> or similar HTML. If somebody
	//    later switches to a custom tooltip component they must update
	//    this test together with the HTML escaping story.
	if !strings.Contains(js, "return lines.join('\\n');") {
		t.Error("formatHeaderCostTooltip must return plain text joined with '\\n' — using HTML markers would require an explicit escape story, not yet in place")
	}

	// 6) Live-update integration: updateHeaderCost must refresh the
	//    title on every cost tick so hover content doesn't go stale
	//    when result events arrive mid-session.
	if !strings.Contains(js, "el.title = formatHeaderCostTooltip(s, selectedKey, selectedNode);") {
		t.Error("updateHeaderCost must refresh el.title via formatHeaderCostTooltip on each tick — otherwise the tooltip drifts behind live cost updates")
	}

	// 7) First-render integration: renderMainShell must seed the title
	//    via costTitleAttr so the very first hover after selection has
	//    the detail, not an empty popup that waits for updateHeaderCost.
	if !strings.Contains(js, "const costTooltip = formatHeaderCostTooltip(s, selectedKey, selectedNode);") {
		t.Error("renderMainShell must compute costTooltip at render time so initial hover isn't empty")
	}
	if !strings.Contains(js, "' title=\"' + escAttr(costTooltip) + '\"'") {
		t.Error("renderMainShell must emit the title attribute with escAttr so any user-controlled characters in s fields can't break out of the attribute")
	}
}

// TestDashboardJS_R110P3_PaletteFavoriteSort pins the R110-P3 palette
// ordering fix. Before this change, the empty-query "idle" palette only
// boosted localStorage recents; user-favorited projects (starred via the
// sidebar ⭐ button, persisted via /api/projects/favorite) had no
// palette-side affordance at all. The fix introduces a three-tier sort:
//
//	Tier 0 — favorites (all of them, regardless of recent-ness)
//	Tier 1 — recents top-N not already in favorites
//	Tier 2 — everything else (original projectsData order)
//
// Reusing the backend `favorite` field (instead of inventing a new
// "palette-pin" concept in localStorage) keeps one source of truth and
// means pinning from the sidebar immediately affects palette order.
func TestDashboardJS_R110P3_PaletteFavoriteSort(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// 1) Tier-0 gate: favorite trumps everything. Pinning the exact
	//    comparator line catches a subtle regression where someone
	//    might reorder tiers (e.g. put recents first) and break the
	//    user expectation that "I starred this, why isn't it on top".
	if !strings.Contains(js, "const fa = pa.favorite ? 0 : 1;") {
		t.Error("palette empty-query sort missing favorite-first gate — `pa.favorite ? 0 : 1` is the tier-0 boost")
	}
	if !strings.Contains(js, "const fb = pb.favorite ? 0 : 1;") {
		t.Error("palette empty-query sort missing tier-0 gate for pb (both sides of comparator must test favorite)")
	}
	if !strings.Contains(js, "if (fa !== fb) return fa - fb;") {
		t.Error("palette empty-query sort must return early when favorite tiers differ — otherwise tier-1 recents could override tier-0 favorites")
	}

	// 2) Tier-1: recents still work for non-favorite projects.
	//    Verify the Map lookup + Infinity fallback — the exact shape
	//    is load-bearing because changing Infinity to a large number
	//    would silently re-rank non-recents.
	if !strings.Contains(js, "const ra = recentRank.has(ka) ? recentRank.get(ka) : Infinity;") {
		t.Error("palette tier-1 recent-boost must use Infinity sentinel so non-recents sort stable on input index")
	}
	if !strings.Contains(js, "if (ra !== rb) return ra - rb;") {
		t.Error("palette tier-1 gate must early-return on differing recent ranks")
	}

	// 3) Tier-2: stable tail on input index. Preserving projectsData
	//    order means alpha + backend favorite-first continues to work
	//    as the implicit "rest" ordering.
	if !strings.Contains(js, "return a.i - b.i;") {
		t.Error("palette tier-2 stable fallback must sort by input index so projectsData order is preserved for the rest bucket")
	}

	// 4) Visual indicator: favorite rows get the ★ glyph instead of the
	//    default ▸ arrow. Locking the conditional ensures a future
	//    refactor can't silently drop the star while keeping the sort.
	if !strings.Contains(js, "p.favorite ? ' is-favorite' : ''") {
		t.Error("buildProjectRow must toggle .is-favorite class on favorite rows for potential CSS targeting")
	}
	if !strings.Contains(js, `'<span class="cp-icon cp-icon-fav" title="已收藏" aria-label="已收藏">★</span>'`) {
		t.Error("buildProjectRow must render the ★ glyph with title+aria-label for favorite projects — the sort change without visual signal is a silent UX regression")
	}
}

// TestDashboardHTML_R110P3_PaletteFavoriteStyles pins the CSS hook that
// gives the favorite star its golden tint. The sort + glyph would still
// function without it (the row would render a default-color star), but
// it's a visual-parity regression against the sidebar ⭐ button.
func TestDashboardHTML_R110P3_PaletteFavoriteStyles(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	css := string(data)

	// The .cp-icon-fav rule must exist and use the same #e3b341 gold the
	// sidebar's .sh-btn.star-on already uses — the two surfaces should
	// read as "the same starred state".
	if !strings.Contains(css, ".cmd-palette-item .cp-icon-fav{color:#e3b341}") {
		t.Error("dashboard.html missing .cp-icon-fav color rule — palette favorite star loses its gold tint")
	}
	// Assert the sidebar reference still uses #e3b341 so the link in
	// the rule comment above remains valid. If the sidebar color ever
	// changes, THIS is where we notice and update both sides together.
	if !strings.Contains(css, ".section-header .sh-btn.star-on{color:#e3b341}") {
		t.Error("sidebar .sh-btn.star-on color changed — update .cp-icon-fav (palette favorite) to match or justify the divergence")
	}
}

// TestDashboardJS_UXP3_SidebarSearchHelper pins the UX-P3 sidebar-search
// surface. Operators with many sessions couldn't quickly locate one —
// the sidebar had no filter box. The fix adds a toggle-revealed input
// in .sidebar-header, a pure filter helper, and a `/` keyboard shortcut
// mirroring the `?` help-modal pattern. The renderer reads the live
// query on every repaint so sessions_update deltas don't clobber the
// filter. Local re-render on keystroke (against _lastSidebarData) avoids
// per-keystroke /api/sessions hits.
func TestDashboardJS_UXP3_SidebarSearchHelper(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// 1) Pure helpers must exist at module scope.
	for _, need := range []string{
		"function filterSessionsByQuery(items, query)",
		"function readSidebarSearchQuery()",
		"function toggleSidebarSearch()",
		"function closeSidebarSearch()",
		"function initSidebarSearch()",
	} {
		if !strings.Contains(js, need) {
			t.Errorf("dashboard.js missing %q — UX-P3 sidebar-search needs this module-scope helper", need)
		}
	}

	// 2) Match surface: substring, case-insensitive, hitting these six
	//    fields. session_id intentionally excluded (opaque hash; matching
	//    on `key` covers the "paste a key slice" workflow without forcing
	//    every test session to carry a human-shaped session_id).
	if !strings.Contains(js, "s.user_label, s.summary, s.last_prompt,") {
		t.Error("filterSessionsByQuery must match user_label/summary/last_prompt as the primary prompt-derived fields")
	}
	if !strings.Contains(js, "s.project, s.cli_name, s.key,") {
		t.Error("filterSessionsByQuery must extend the match surface to project/cli_name/key")
	}
	if !strings.Contains(js, "typeof f === 'string' && f.toLowerCase().indexOf(q) !== -1") {
		t.Error("filterSessionsByQuery must do case-insensitive substring match, guarded by typeof check (some fields may be absent)")
	}

	// 3) Render integration: renderSidebar must read the live query via
	//    readSidebarSearchQuery and skip grouping when filter is active.
	//    Pinning the conditional shape catches a refactor that might
	//    accidentally call the filter twice or run grouping+filter
	//    concurrently.
	if !strings.Contains(js, "const filterQuery = readSidebarSearchQuery();") {
		t.Error("renderSidebar must consult readSidebarSearchQuery() so each repaint re-applies the current filter")
	}
	if !strings.Contains(js, "const filterActive = !!filterQuery;") {
		t.Error("renderSidebar must derive filterActive from the query so both the render branch and the empty-state branch agree")
	}
	if !strings.Contains(js, "if (!filterActive) {") {
		t.Error("renderSidebar must gate grouping behind !filterActive so filter mode skips project grouping")
	}

	// 4) Local re-render on keystroke: the oninput handler must target
	//    _lastSidebarData, NOT debouncedFetchSessions — the latter would
	//    issue one /api/sessions per keystroke. The fallback to
	//    debouncedFetchSessions when the cache is empty is intentional
	//    for the first-paint corner case.
	if !strings.Contains(js, "if (_lastSidebarData) {") {
		t.Error("sidebar-search input handler must short-circuit via _lastSidebarData cache to avoid server DoS on keystroke")
	}
	if !strings.Contains(js, "renderSidebar(_lastSidebarData);") {
		t.Error("sidebar-search oninput must re-render locally via renderSidebar(_lastSidebarData) on cache hit")
	}

	// 5) Keyboard shortcut: `/` opens search unless user is already in
	//    an input. Mirror of the `?` help-modal pattern, so pin both the
	//    key + the tagname skip rules.
	if !strings.Contains(js, "if (e.key !== '/') return;") {
		t.Error("initSidebarSearch must register a `/` global shortcut")
	}
	if !strings.Contains(js, "tgt.tagName === 'INPUT' || tgt.tagName === 'TEXTAREA' || tgt.isContentEditable") {
		t.Error("`/` shortcut must skip when the focus target is an input/textarea/contenteditable")
	}
	// Esc inside the search input must close the pane, not bubble to
	// other Esc handlers (like the lightbox).
	if !strings.Contains(js, "if (e.key === 'Escape') { e.preventDefault(); closeSidebarSearch(); }") {
		t.Error("sidebar-search input must intercept Esc and route to closeSidebarSearch")
	}

	// 6) Filter-mode empty state: distinct from the "no sessions" global
	//    empty state. Pin the filter-specific copy so future i18n passes
	//    keep the two messages separate.
	if !strings.Contains(js, "没有匹配的会话") {
		t.Error("renderSidebar must emit a filter-specific empty-state line (没有匹配的会话); the 'no sessions' CTA would mislead into thinking zero sessions exist")
	}
	// The legacy "no sessions" CTA must NOT be emitted in filter mode —
	// assert the gate condition.
	if !strings.Contains(js, "if (!html && !filterActive)") {
		t.Error("renderSidebar must gate the 'no sessions' CTA behind !filterActive so filter mode doesn't render a misleading 'no sessions' shell")
	}
}

// TestDashboardHTML_UXP3_SidebarSearchUI pins the HTML + CSS the JS
// helper relies on. Missing hooks would leave the pane functionally
// intact but visually broken (e.g. input with no styling, no hover on
// the clear button), which is a silent UX regression. Also locks the
// sidebar-search position OUTSIDE #session-list so sessions_update
// re-renders don't blow away the input value.
func TestDashboardHTML_UXP3_SidebarSearchUI(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)

	// Toggle button in the header-row btns: clicking fires toggleSidebarSearch.
	if !strings.Contains(html, `id="btn-sidebar-search" onclick="toggleSidebarSearch()"`) {
		t.Error("dashboard.html must expose the btn-sidebar-search trigger with onclick=toggleSidebarSearch()")
	}

	// The search pane must exist inside .sidebar-header and OUTSIDE
	// #session-list. Verify by checking the ordering of the two ids.
	paneIdx := strings.Index(html, `id="sidebar-search"`)
	listIdx := strings.Index(html, `id="session-list"`)
	if paneIdx <= 0 {
		t.Fatal("sidebar-search id not found in dashboard.html")
	}
	if listIdx <= 0 {
		t.Fatal("session-list id not found in dashboard.html")
	}
	if paneIdx > listIdx {
		t.Error("sidebar-search must appear BEFORE session-list in dashboard.html so it lives inside .sidebar-header, not inside #session-list (which gets innerHTML-replaced on every sessions_update)")
	}

	// CSS rules the JS handler + layout depend on.
	for _, rule := range []string{
		`.sidebar-search{`,
		`.sidebar-search-input{`,
		`.sidebar-search-input:focus{`,
		`.sidebar-search-clear{`,
		`.session-list-filter-empty{`,
		`.hdr-btn.active{`,
	} {
		if !strings.Contains(html, rule) {
			t.Errorf("dashboard.html missing CSS rule %q required by UX-P3 sidebar search", rule)
		}
	}
}

// TestDashboardJS_R110P1_HistoryDrawerSearch pins the R110-P1 fix that
// adds a search input to the history popover. Before this change, an
// operator with 100+ history entries had to eyeball-scroll to locate a
// specific prompt or project. The search input does case-insensitive
// substring match against (prompt, project) and updates a count chip
// that surfaces "N / total" so the denominator stays visible.
//
// The filter function is pulled out as a pure helper (filterHistoryEntries)
// so it's easy to reason about and — importantly — so a refactor can't
// silently break the match surface. The render-time integration
// (applyHistoryFilter) wires the input handler and the empty-result copy.
func TestDashboardJS_R110P1_HistoryDrawerSearch(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// 1) Pure helper must exist at module scope and take (merged, query).
	//    Pure-function contract lets future tests drive it from a JS
	//    runtime harness; module scope (not a closure inside
	//    toggleHistory) prevents a refactor from moving it somewhere
	//    non-testable.
	if !strings.Contains(js, "function filterHistoryEntries(merged, query)") {
		t.Error("dashboard.js missing filterHistoryEntries(merged, query) pure helper — required for R110-P1 history search")
	}

	// 2) Match surface: case-insensitive substring on prompt AND project.
	//    Session_id not in the surface by design — it's a long opaque
	//    hash operators don't type. Pin both match sites so a future
	//    refactor can't silently drop one.
	for _, anchor := range []string{
		`const p = (s.prompt || '').toLowerCase();`,
		`if (p.indexOf(q) !== -1) return true;`,
		`const proj = (s.project || '').toLowerCase();`,
		`if (proj.indexOf(q) !== -1) return true;`,
	} {
		if !strings.Contains(js, anchor) {
			t.Errorf("filterHistoryEntries missing match anchor %q", anchor)
		}
	}
	// Empty-query short-circuit: MUST return the full list, not filter
	// against empty string (which would match everything, fine, but
	// wastes an O(N) allocation on every keystroke the user clears the
	// input).
	if !strings.Contains(js, "if (!q) return merged;") {
		t.Error("filterHistoryEntries must short-circuit on empty query to avoid re-scanning on trivial clears")
	}

	// 3) Render integration: applyHistoryFilter must exist and route
	//    through filterHistoryEntries. The counter chip text change is
	//    the observable UX signal — operators learn "3 / 47" means
	//    their filter is working, not a staleness bug.
	if !strings.Contains(js, "function applyHistoryFilter(merged, query)") {
		t.Error("dashboard.js missing applyHistoryFilter(merged, query) render step")
	}
	if !strings.Contains(js, "const filtered = filterHistoryEntries(merged, query);") {
		t.Error("applyHistoryFilter must call filterHistoryEntries (single source of truth for match logic)")
	}
	if !strings.Contains(js, "'(' + filtered.length + ' / ' + merged.length + ')'") {
		t.Error("applyHistoryFilter must render the 'N / total' count chip so the denominator stays visible during filter")
	}

	// 4) Input wiring: toggleHistory must inject #hp-search and bind its
	//    input event to applyHistoryFilter. Pin the element id and the
	//    handler so a rename breaks the test (forces a matching CSS
	//    update).
	if !strings.Contains(js, `id="hp-search"`) {
		t.Error("toggleHistory must render the search input with id=hp-search so the handler and CSS selectors have a stable anchor")
	}
	if !strings.Contains(js, "searchInput.addEventListener('input', e => applyHistoryFilter(merged, e.target.value))") {
		t.Error("toggleHistory must wire input event to applyHistoryFilter with (merged, value) — any other shape drops the closure over `merged`")
	}

	// 5) Empty-result branch copy: distinct from the "no history" copy.
	//    Operators seeing "no matches" after typing understand their
	//    filter is the reason; the hint tells them how to recover.
	if !strings.Contains(js, "没有匹配的历史") {
		t.Error("applyHistoryFilter must emit a filter-specific empty-state line (没有匹配的历史) — distinguished from the 'no history' global empty state")
	}
	if !strings.Contains(js, "调整关键词") {
		t.Error("filter-empty hint must include a recovery tip mentioning adjusting keywords")
	}

	// 6) Ensure the legacy single-branch render path is gone: the old
	//    toggleHistory had an inlined `merged.map(...)` right inside the
	//    function. After the refactor that map lives in applyHistoryFilter.
	//    Surface the anti-assertion so a future merge doesn't resurrect
	//    both paths at once (which would silently double-render).
	toggleIdx := strings.Index(js, "function toggleHistory()")
	if toggleIdx < 0 {
		t.Fatal("toggleHistory() not found")
	}
	// Scope the forbidden check to the toggleHistory body (~2500 chars
	// is well over what the current function occupies; trim to the next
	// top-level `function ` so we don't catch applyHistoryFilter's map).
	bodyEnd := strings.Index(js[toggleIdx+50:], "\nfunction ")
	if bodyEnd < 0 {
		t.Fatal("couldn't delimit toggleHistory body")
	}
	body := js[toggleIdx : toggleIdx+50+bodyEnd]
	if strings.Contains(body, "merged.map(s => {") {
		t.Error("toggleHistory must delegate rendering to applyHistoryFilter; a `merged.map` inside toggleHistory means two render paths exist")
	}
}

// TestDashboardHTML_R110P1_HistoryDrawerSearchStyles pins the CSS hooks
// the search input relies on. Missing any of these leaves the input
// functionally intact but visually broken, which is a silent UX
// regression. Pinning surfaces it at build time.
func TestDashboardHTML_R110P1_HistoryDrawerSearchStyles(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	css := string(data)

	for _, rule := range []string{
		`.history-popover-search{`,
		`.hp-search-input{`,
		`.hp-search-input:focus{`,
		`.history-popover-header .hp-count{`,
	} {
		if !strings.Contains(css, rule) {
			t.Errorf("dashboard.html missing CSS rule %q used by R110-P1 history drawer search — input would render unstyled", rule)
		}
	}

	// The search input block must NOT be position:sticky: .hp-day-header
	// already uses position:sticky top:0 relative to the popover scroll
	// container, and adding a second sticky at the same anchor causes
	// them to fight for the top slot. This assertion guards the decision
	// recorded in the source comment.
	searchBlock := rfindCSSBlock(css, ".history-popover-search")
	if strings.Contains(searchBlock, "position:sticky") {
		t.Error(".history-popover-search must not be position:sticky; .hp-day-header already occupies top:0 in the popover scroll container — see source comment for rationale")
	}
}

// rfindCSSBlock returns the `{...}` body of the first rule matching the
// selector. Tiny helper so the sticky-check doesn't depend on a full
// CSS parser — the file is ours and selectors are unique.
func rfindCSSBlock(css, selector string) string {
	i := strings.Index(css, selector+"{")
	if i < 0 {
		return ""
	}
	open := i + len(selector)
	depth := 0
	for j := open; j < len(css); j++ {
		switch css[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return css[open : j+1]
			}
		}
	}
	return ""
}

// TestDashboardJS_R110P3_OriginBadgeHelper pins the R110-P3 "IM origin
// indicator" surface. Session keys sourced from a real IM platform
// (feishu / slack / discord / weixin) get a small chip on the sidebar
// card meta line AND on the main-header detail line so operators can
// eyeball which sessions are genuine IM threads vs dashboard-local /
// cron / scratch / planner conversations. The residual "jump back to
// feishu group X" affordance remains deferred pending backend schema
// work — this test only locks the MVP (platform + 私聊/群 label).
//
// Scope: the chip is a pure function of the session key prefix, so the
// tests stay structural — no DOM, no fetch. PLATFORM_ORIGINS is the single
// source of truth for platform → Chinese label; adding a new platform
// means extending PLATFORM_ORIGINS plus the matching .sc-origin.kind-*
// CSS variant, and both sides are pinned here.
func TestDashboardJS_R110P3_OriginBadgeHelper(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// 1) PLATFORM_ORIGINS map must exist and cover the four currently-
	//    wired IM platforms. Asserting the literal field names protects
	//    against a refactor that silently drops one platform.
	if !strings.Contains(js, "const PLATFORM_ORIGINS = {") {
		t.Error("dashboard.js missing PLATFORM_ORIGINS constant — R110-P3 origin badge requires a single source of truth for platform labels")
	}
	for _, need := range []string{
		"feishu:  { name: '飞书',",
		"slack:   { name: 'Slack',",
		"discord: { name: 'Discord',",
		"weixin:  { name: '微信',",
	} {
		if !strings.Contains(js, need) {
			t.Errorf("PLATFORM_ORIGINS missing entry %q — extending the map is the expansion path for new IM platforms", need)
		}
	}

	// 2) Helper existence — tests cover the two layers:
	//    (a) originBadgeInfo: pure; returns {label, kind} or null.
	//    (b) originBadgeHtml: template; returns '' when info is null.
	if !strings.Contains(js, "function originBadgeInfo(key)") {
		t.Error("dashboard.js missing originBadgeInfo(key) pure helper")
	}
	if !strings.Contains(js, "function originBadgeHtml(key)") {
		t.Error("dashboard.js missing originBadgeHtml(key) renderer")
	}

	// 3) Guard clauses: null for non-string, empty, and keys without a
	//    platform prefix. These are the top-of-function early returns in
	//    originBadgeInfo; if a refactor drops one a dashboard.js key like
	//    "cron:abc" could grow a phantom origin chip.
	if !strings.Contains(js, "if (typeof key !== 'string' || !key) return null;") {
		t.Error("originBadgeInfo must guard against non-string/empty key")
	}
	if !strings.Contains(js, "if (colon <= 0) return null;") {
		t.Error("originBadgeInfo must reject keys without a platform:<rest> separator")
	}

	// 4) chatType → Chinese label: only 'group' maps to 群; everything
	//    else falls through to 私聊. This matches how the Go platform
	//    layer emits chatType (direct / group / mpim-merged-to-group).
	//    Pin the ternary shape so a future "if === 'direct'" flip can't
	//    silently relabel mpim as "私聊" when it should be "群".
	if !strings.Contains(js, "const chatLabel = chatType === 'group' ? '群' : '私聊';") {
		t.Error("originBadgeInfo must use 'group → 群, else 私聊' mapping to match Go platform chatType emission")
	}

	// 5) Render-site wiring: both the sidebar meta line and the main
	//    header must pull the chip from originBadgeHtml. Asserting the
	//    canonical variable names catches copy-paste typos like
	//    "originBadge" / "headerOriginBadge" drifting apart.
	if !strings.Contains(js, "const originBadge = originBadgeHtml(s.key)") {
		t.Error("sessionCardHtml must read the origin chip via originBadgeHtml(s.key) so the sidebar card shows IM origin for real IM sessions")
	}
	if !strings.Contains(js, "const headerOriginBadge = originBadgeHtml(selectedKey)") {
		t.Error("renderMainShell must read the origin chip via originBadgeHtml(selectedKey) so the session header shows IM origin")
	}

	// 6) The chip className must be the kebab `sc-origin kind-<platform>`
	//    pair the CSS rules hang off of. Assert the shape so a JS-side
	//    rename would immediately fail the test together with the unused
	//    CSS rule (otherwise the chip would render without brand color).
	if !strings.Contains(js, "'<span class=\"sc-origin kind-' + esc(info.kind)") {
		t.Error("originBadgeHtml must emit <span class=\"sc-origin kind-X\"> to match the CSS selectors in dashboard.html")
	}
}

// TestDashboardHTML_R110P3_OriginBadgeStyles pins the CSS hooks
// originBadgeHtml relies on: the base `.sc-origin` structure rule plus one
// variant per IM platform (.sc-origin.kind-feishu / slack / discord /
// weixin). Without the variant, the chip still renders via the muted
// fallback, but platform hue distinguishability — the whole point of the
// chip — is lost. Pinning prevents a CSS cleanup from silently flattening
// all four into one hue.
func TestDashboardHTML_R110P3_OriginBadgeStyles(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	css := string(data)
	for _, rule := range []string{
		`.sc-origin{`,
		`.sc-origin.kind-feishu`,
		`.sc-origin.kind-slack`,
		`.sc-origin.kind-discord`,
		`.sc-origin.kind-weixin`,
	} {
		if !strings.Contains(css, rule) {
			t.Errorf("dashboard.html missing CSS rule %q used by R110-P3 origin badge — chip would render with fallback hue", rule)
		}
	}
}

// TestDashboardJS_UXP2_WSAuthRetryAfterCountdown pins the UX-P2
// "WS auth 限流倒计时" fix. The front-end catches the server's
// auth_fail("too many attempts") reply driven by `retry_after`, blocks
// wsm.connect() during the window, and auto-reconnects on expiry.
//
// Originally the countdown fired a top-of-screen toast; that was later
// moved into the sidebar status row because on mobile the toast covered
// the header AND the 1Hz re-emit caused visible flicker. This test now
// pins the inline-status variant: the countdown copy lives in
// updateStatusBar (rendered as .status-authwait), the toast reference is
// gone, and the gate/auto-recover plumbing is unchanged.
func TestDashboardJS_UXP2_WSAuthRetryAfterCountdown(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// 1) The auth_fail rate-limit branch must parse msg.retry_after and
	//    route into startWSAuthRetryCountdown. The legacy 5s showToast
	//    path was the previous behavior — explicitly asserting its
	//    absence so a future merge doesn't silently resurrect it.
	if !strings.Contains(js, "let retryAfter = parseInt(msg.retry_after, 10)") {
		t.Error("dashboard.js auth_fail branch must read msg.retry_after so the countdown honours the server-side window")
	}
	if !strings.Contains(js, "startWSAuthRetryCountdown(retryAfter)") {
		t.Error("dashboard.js auth_fail rate-limit branch must call startWSAuthRetryCountdown")
	}
	// Assert the legacy one-shot 5s toast is gone from the rate-limit branch.
	// Scope the check to the auth_fail neighborhood so unrelated warning
	// toasts elsewhere don't false-positive.
	authIdx := strings.Index(js, "case 'auth_fail':")
	if authIdx < 0 {
		t.Fatal("case 'auth_fail': branch not found")
	}
	end := authIdx + 1200
	if end > len(js) {
		end = len(js)
	}
	window := js[authIdx:end]
	if strings.Contains(window, "showToast('登录尝试过于频繁，请稍后重试', 'warning', 5000)") {
		t.Error("legacy 5-second warn toast must not remain in auth_fail rate-limit branch — UX-P2 replaces it with a countdown")
	}

	// 2) Helper contract: startWSAuthRetryCountdown must exist, arm
	//    wsm._authBlockUntil, and re-trigger wsm.connect() when the
	//    countdown hits zero. Each assertion targets a distinct behavior
	//    so a future refactor that breaks just one is caught.
	if !strings.Contains(js, "function startWSAuthRetryCountdown(seconds)") {
		t.Error("dashboard.js missing startWSAuthRetryCountdown(seconds) helper")
	}
	if !strings.Contains(js, "wsm._authBlockUntil = Date.now() + seconds * 1000") {
		t.Error("startWSAuthRetryCountdown must arm wsm._authBlockUntil so scheduleReconnect honours the window")
	}
	if !strings.Contains(js, "wsm._authBlockUntil = 0") {
		t.Error("startWSAuthRetryCountdown must clear wsm._authBlockUntil on expiry so the gate disarms")
	}
	// After the countdown elapses the helper must force a fresh connect:
	// the surrounding reconnect timer (scheduled by the socket close) is
	// not guaranteed to fire right at the deadline, so we assert the
	// immediate wsm.connect() call.
	if !strings.Contains(js, "wsm.connect();") {
		t.Error("startWSAuthRetryCountdown must invoke wsm.connect() on expiry so recovery is automatic")
	}

	// 3) Connection-gate wiring: wsm.connect() must early-return while
	//    the block is active, and wsm.scheduleReconnect() must push the
	//    next dial to the deadline (not just the exponential backoff).
	if !strings.Contains(js, "if (this._authBlockUntil > 0 && Date.now() < this._authBlockUntil)") {
		t.Error("wsm.connect() must skip dialling while inside the auth rate-limit window")
	}
	if !strings.Contains(js, "const authGap = Math.max(0, this._authBlockUntil - now)") {
		t.Error("wsm.scheduleReconnect() must compute the auth-block gap to clamp its backoff")
	}

	// 4) The Chinese copy ("鉴权过于频繁") distinguishes the WS lockout
	//    from the login modal's own countdown ("登录尝试过多"). Pin it so
	//    future i18n work doesn't accidentally merge the two.
	if !strings.Contains(js, "'鉴权过于频繁，'") {
		t.Error("auth retry countdown must use the WS-specific Chinese copy so it's distinguishable from the login-modal countdown")
	}

	// 5) Noise-reduction invariant: the countdown must NOT emit a toast.
	//    It now renders into the sidebar status row via updateStatusBar.
	//    Previously the helper called showToast('鉴权过于频繁，...') every
	//    second, which on mobile covered the header and caused flicker.
	if strings.Contains(js, "showToast('鉴权过于频繁") {
		t.Error("countdown must render into the sidebar status line, not a toast — the toast form covered mobile headers and flickered every second")
	}
	// The inline rendering lives in updateStatusBar: assert the class
	// name and copy are both present so the CSS rule
	// (.sidebar-status .status-authwait) stays reachable from JS.
	if !strings.Contains(js, `<div class="status-authwait">`) {
		t.Error("updateStatusBar must render .status-authwait when _authBlockUntil is armed — replaces the old rate-limit toast")
	}
	if !strings.Contains(js, "'鉴权过于频繁，' + authWaitSecs + 's 后自动重连'") {
		t.Error("updateStatusBar must compose the countdown copy from wsm._authBlockUntil so the sidebar ticks in sync with the gate")
	}
}

// TestDashboardHTML_UXP3_UploadReorderStyles pins the CSS hooks the JS
// handler relies on: the .dragging opacity dim, the .drop-target accent
// border, and the grab/grabbing cursors. Changing any of these class
// names silently would leave the drag handlers functional but invisible.
func TestDashboardHTML_UXP3_UploadReorderStyles(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	css := string(data)

	for _, rule := range []string{
		`.file-thumb[draggable="true"]`,
		`.file-thumb.dragging`,
		`.file-thumb.drop-target`,
		`cursor:grab`,
		`cursor:grabbing`,
	} {
		if !strings.Contains(css, rule) {
			t.Errorf("dashboard.html missing CSS hook %q used by UX-P3 drag-reorder — handler would have no visual feedback", rule)
		}
	}

	// Ensure focus-visible outline exists so keyboard users can see which
	// thumb is selected before pressing Arrow keys.
	if !strings.Contains(css, `.file-thumb[draggable="true"]:focus-visible`) {
		t.Error("dashboard.html missing :focus-visible outline for draggable thumbs — keyboard reorder needs a visible focus ring")
	}
}

// TestDashboardJS_R110P2_CronPanelFilter pins the Round 139 contract for the
// cron panel filter. Three invariants that together make the filter correct
// and non-regression-able:
//
//  1. filterCronJobs(pure) has the expected match surface (prompt / work_dir /
//     schedule / id) + status gate ('all' / 'active' / 'attention' where
//     'attention' == paused OR last_error). If a future refactor narrows the
//     surface silently, operators lose the ability to search by their job's
//     schedule expression, which is one of the most useful keys.
//  2. renderCronList is shell-preserving: renderCronPanel detects a mounted
//     shell via document.getElementById('cron-list-items') and short-circuits
//     to renderCronList, so typing in the search box doesn't wipe the <input>
//     value/focus.
//  3. The search input wires oninput → onCronSearchInput (not a direct
//     renderCronPanel call), confirming the module-state path. And the chip
//     buttons carry data-status attributes matching the status filter domain.
func TestDashboardJS_R110P2_CronPanelFilter(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// Invariant 1: filterCronJobs exists and its match surface covers the
	// documented fields. cron-v2-polish §3.1 Increment A 扩展了 title 字段，
	// 放在 fields 数组的第一位（最高匹配优先），其余字段保持不变。使用
	// substring 精确锚定 fields 字面量避免重构漂移。
	if !strings.Contains(js, "function filterCronJobs(jobs, query, status)") {
		t.Fatal("dashboard.js missing filterCronJobs(jobs, query, status) — R110-P2 cron filter predicate")
	}
	if !strings.Contains(js, "[j.title, j.prompt, j.work_dir, j.schedule, j.id]") {
		t.Error("filterCronJobs match surface must include title, prompt, work_dir, schedule, id — operators search by any of these; title 是 cron-v2-polish §3.1 引入的人类可读名称")
	}
	// Status gate: both 'active' and 'attention' arms must exist + use the
	// same attention definition as the cron-badge (paused OR last_error),
	// so the "what needs my eyeballs" filter and the header badge count
	// stay in lockstep.
	if !strings.Contains(js, "s === 'active' && j.paused") {
		t.Error("filterCronJobs 'active' arm must exclude paused jobs")
	}
	// cron-v2-polish §3.3 Increment C 将 missed（进程重启空窗期跳过）纳入
	// attention。断言扩展为 paused || last_error || missed；cronBadge 计数
	// 同步更新以保持"filter 和徽章同源"的反漂移不变式。
	if !strings.Contains(js, "s === 'attention' && !(j.paused || j.last_error || j.missed)") {
		t.Error("filterCronJobs 'attention' arm must match paused OR last_error OR missed — cron-v2-polish §3.3 引入 missed 并与 cronBadge 计数保持同源")
	}

	// Invariant 2: renderCronPanel short-circuits to renderCronList when the
	// shell is already mounted. The probe element id is 'cron-list-items'.
	// Without this branch, each keystroke (via the oninput handler)
	// rebuilds the shell, blowing away <input> value/focus.
	if !strings.Contains(js, "if (document.getElementById('cron-list-items')) {") {
		t.Error("renderCronPanel must shell-preserve by short-circuiting to renderCronList when #cron-list-items already exists")
	}
	// The short-circuit body must actually call renderCronList (not just
	// return). Pin a 200-byte window around the probe to assert the call.
	idx := strings.Index(js, "if (document.getElementById('cron-list-items')) {")
	if idx >= 0 {
		window := js[idx:min2(idx+200, len(js))]
		if !strings.Contains(window, "renderCronList();") {
			t.Error("renderCronPanel shell-preserve branch must invoke renderCronList()")
		}
	}

	// Invariant 3: the search input's oninput handler routes through the
	// module-state helper, not directly through renderCronPanel (which
	// would rebuild the shell). Also pin the three chip data-status values
	// so adding/removing a chip requires updating this test — the
	// filterCronJobs status domain and the UI surface MUST stay in sync.
	if !strings.Contains(js, `oninput="onCronSearchInput()"`) {
		t.Error("cron search input must wire oninput to onCronSearchInput — direct renderCronPanel would wipe the input")
	}
	if !strings.Contains(js, "function onCronSearchInput()") {
		t.Error("dashboard.js missing onCronSearchInput handler")
	}
	if !strings.Contains(js, "function setCronStatusFilter(status)") {
		t.Error("dashboard.js missing setCronStatusFilter handler")
	}
	for _, chip := range []string{
		`data-status="all"`,
		`data-status="active"`,
		`data-status="attention"`,
	} {
		if !strings.Contains(js, chip) {
			t.Errorf("dashboard.js missing cron status chip %s", chip)
		}
	}

	// Filter-specific empty state: if the filter matches zero jobs the
	// panel MUST paint a filter-specific hint rather than the cold-start
	// CTA (which would mislead operators into thinking they deleted all
	// their jobs). The dual-empty-state invariant mirrors the sidebar
	// search's two-branch hint.
	if !strings.Contains(js, "cron-filter-empty") {
		t.Error("dashboard.js missing .cron-filter-empty branch — filter-no-match must not render the cold-start CTA")
	}
	if !strings.Contains(js, "没有匹配的定时任务") {
		t.Error("cron filter empty state must use the Chinese hint '没有匹配的定时任务'")
	}
}

// TestDashboardJS_R110P2_CronFilterBoundary pins the reverse invariant:
// renderCronPanel must NOT duplicate the card rendering code — it must
// delegate to cronJobCardHtml(j). If a future refactor inlines the card
// markup back into renderCronPanel, the filter + list-only-repaint path
// would regress silently because the shell-preserving branch would paint
// a different template than the initial shell paint.
func TestDashboardJS_R110P2_CronFilterBoundary(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	if !strings.Contains(js, "function cronJobCardHtml(j)") {
		t.Fatal("dashboard.js missing cronJobCardHtml(j) — card markup must be extracted so renderCronList and the shell-init path share one template")
	}

	// Both renderCronList and the initial renderCronPanel path paint cards
	// through cronJobCardHtml — count the call sites. Exactly ONE .map
	// callsite expected (inside renderCronList), because renderCronPanel
	// delegates to renderCronList after mounting the shell.
	mapCalls := strings.Count(js, "sorted.map(cronJobCardHtml)")
	if mapCalls != 1 {
		t.Errorf("expected exactly 1 sorted.map(cronJobCardHtml) call site, got %d — if renderCronPanel also maps the cards directly, the shell-preserve branch will render a different template", mapCalls)
	}

	// Module-level filter state variables exist (not function-local); this
	// is what makes the shell-preserve branch able to re-read the user's
	// intent without re-querying the DOM for the input value every paint.
	for _, decl := range []string{
		"let cronFilterQuery = '';",
		"let cronFilterStatus = 'all';",
	} {
		if !strings.Contains(js, decl) {
			t.Errorf("dashboard.js missing module-level declaration %q — state must survive across repaints", decl)
		}
	}
}

// TestDashboardHTML_R110P2_CronFilterStyles pins the CSS hooks the cron
// filter JS relies on. Matches the pattern used by
// TestDashboardHTML_UXP3_UploadReorderStyles — if a future CSS cleanup
// renames these class names, the handler would still run but the filter
// bar would be unstyled (transparent input, invisible chips).
func TestDashboardHTML_R110P2_CronFilterStyles(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	css := string(data)

	for _, rule := range []string{
		".cron-filter-bar",
		".cron-search-input",
		".cron-status-chip",
		".cron-status-chip.active",
		".cron-filter-empty",
	} {
		if !strings.Contains(css, rule) {
			t.Errorf("dashboard.html missing CSS rule %q used by R110-P2 cron filter", rule)
		}
	}

	// The .active chip variant uses the same blue token family as the
	// sidebar-search-input:focus border so the filter bar reads as part
	// of the same design system. Use a regex to accept either the
	// --nz-blue token or its rgba-alpha-tinted background, without
	// pinning the exact rule order.
	if ok, _ := regexp.MatchString(`\.cron-status-chip\.active\{[^}]*--nz-blue`, css); !ok {
		t.Error(".cron-status-chip.active must reference --nz-blue so the active chip matches the dashboard's primary accent")
	}
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestDashboardJS_R110P2_SectionHeaderNewButtonConsistency pins two
// invariants around the per-project "+" create affordance:
//
//  1. The `sh-new` compact + button is emitted by sectionHeaderHtml
//     unconditionally — no `if (g.items.length === 0)` or similar gate
//     that would bring back the original regression where groups with
//     existing sessions had no per-project create affordance. The header
//     `+` is now the sole per-project create surface (the old full-width
//     "New session in X" row below empty favorite groups was removed as
//     redundant), so regressing emission would leave empty favorite
//     groups with no CTA at all.
//  2. The per-project sh-new button's title + aria-label are localized
//     to Chinese ("在 X 中新建会话") — the top-right header `+` already
//     carries Chinese copy elsewhere; keeping the per-project one English
//     would be a mixed-locale inconsistency.
func TestDashboardJS_R110P2_SectionHeaderNewButtonConsistency(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// Invariant 1: sh-new button is emitted unconditionally. Locate
	// sectionHeaderHtml's body and assert there's no `if` before the
	// `const newBtn = ...` declaration — emission must not be gated on
	// items.length or similar. We scan a bounded window after the
	// function declaration.
	fnIdx := strings.Index(js, "function sectionHeaderHtml(p) {")
	if fnIdx < 0 {
		t.Fatal("dashboard.js missing sectionHeaderHtml function")
	}
	body := js[fnIdx:minInt(fnIdx+4000, len(js))]
	newBtnIdx := strings.Index(body, "const newBtn =")
	if newBtnIdx < 0 {
		t.Fatal("sectionHeaderHtml body missing `const newBtn = ...` declaration")
	}
	// The chunk between the function start and the newBtn declaration
	// must not include a conditional that would gate emission. Conditions
	// that appear before `const newBtn` (like the github URL match for
	// ghBtn, or the favorite-starBtn ternary) are fine; we only check
	// that the newBtn declaration itself is unconditional (no `if` line
	// starts on the same indentation level guarding the declaration).
	// A cheaper structural test: the return statement must reference newBtn
	// without any ternary that could drop it.
	ret := body[newBtnIdx:]
	retStart := strings.Index(ret, "return '<div")
	if retStart < 0 {
		t.Fatal("sectionHeaderHtml missing return '<div... statement")
	}
	returnStmt := ret[retStart:minInt(retStart+1000, len(ret))]
	// newBtn must be concatenated directly (not inside a conditional arm).
	// If someone introduced `(cond ? newBtn : '')`, this simple substring
	// check on `+ newBtn +` would still match, so we also forbid the
	// conditional wrapping pattern.
	if !strings.Contains(returnStmt, "+\n    newBtn +") && !strings.Contains(returnStmt, "+ newBtn +") {
		t.Error("sectionHeaderHtml return must include `newBtn` unconditionally; a conditional gate would regress the R110-P2 '+ New session in X' consistency fix")
	}
	if strings.Contains(returnStmt, "? newBtn :") || strings.Contains(returnStmt, ": newBtn)") {
		t.Error("sectionHeaderHtml must not wrap newBtn in a ternary — that would gate emission and regress the consistency fix")
	}

	// The sh-new data-driven call site must still exist. After the removal
	// of sectionEmptyHtml this is the only caller using this exact arg
	// shape; if it disappears the header `+` button is broken.
	shNewCallPattern := `newSessionInProject(this.dataset.name,this.dataset.node)`
	if strings.Count(js, shNewCallPattern) < 1 {
		t.Errorf("expected at least 1 `newSessionInProject(this.dataset.name,this.dataset.node)` call site (sh-new header button), got 0")
	}

	// Invariant 2: the sh-new button's title + aria-label are localized.
	// Scope the check to the newBtn declaration to avoid false positives
	// from the header `+` button's English "New Session" title at the
	// top-right of the dashboard.
	newBtnDecl := body[newBtnIdx:minInt(newBtnIdx+800, len(body))]
	if !strings.Contains(newBtnDecl, `title="在 `) {
		t.Error("sh-new button's title must be localized to Chinese (在 X 中新建会话)")
	}
	if !strings.Contains(newBtnDecl, `aria-label="在 `) {
		t.Error("sh-new button's aria-label must be localized to Chinese (在 X 中新建会话)")
	}
	if !strings.Contains(newBtnDecl, `中新建会话`) {
		t.Error("sh-new button must end its localized label with '中新建会话'")
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestDashboardJS_R110P1_WSOutageDurationHint pins the Round 142 contract
// for the WS-outage duration hint. Previous rounds landed the reconnect
// button (backoff > 8s) and the "已重新连接" success toast; what remained
// was the TODO's core complaint: a pulsing "connecting..." dot with no
// indication of HOW LONG the connection has been dead, leaving operators
// unable to tell "just blipped 2s ago" from "been dead for 10 minutes".
//
// Four structural invariants:
//
//  1. formatOutageDuration is a pure helper (no DOM, no wsm access) with
//     the documented rounding behaviour: <5s suppressed to ”, 5-89s as
//     seconds, 90s-59min as minutes, >=1h as hours(+ optional minutes).
//  2. wsm carries a _disconnectedSince module field + setState updates it
//     on CONNECTED (clear to 0) / first non-CONNECTED transition (arm
//     Date.now if still 0 — guard against mid-outage restamp during
//     connecting→auth→connecting cycles).
//  3. updateStatusBar reads the stamp + renders a .status-outage row
//     only when the gate (!wsUp && stamp > 0) allows AND format returns
//     non-empty, so transient reconnects don't spawn a noisy hint.
//  4. _updateStatusTick starts a 1s interval while disconnected and
//     clears it on CONNECTED, so the "已断开 N 秒" label advances
//     without waiting for the next WS state transition. The timer
//     handle lives at module scope (not inside updateStatusBar) so
//     multiple repaint paths don't spawn leaking intervals.
func TestDashboardJS_R110P1_WSOutageDurationHint(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// Invariant 1: formatOutageDuration defined + covers the four branches.
	if !strings.Contains(js, "function formatOutageDuration(elapsedMs)") {
		t.Fatal("dashboard.js missing formatOutageDuration(elapsedMs) helper")
	}
	for _, fragment := range []string{
		"if (ms < 5000) return '';",
		"if (s < 90) return '已断开 ' + s + ' 秒';",
		"if (m < 60) return '已断开 ' + m + ' 分';",
		"'已断开 ' + h + ' 小时'",
	} {
		if !strings.Contains(js, fragment) {
			t.Errorf("formatOutageDuration body missing branch %q — the four-tier duration rounding contract must stay intact", fragment)
		}
	}

	// Invariant 2: wsm has the _disconnectedSince field + setState maintains
	// it. Detect the "declared as 0 module initializer" + both arm/clear
	// sites inside setState.
	if !strings.Contains(js, "_disconnectedSince: 0,") {
		t.Error("wsm must declare _disconnectedSince: 0 module-level field")
	}
	if !strings.Contains(js, "this._disconnectedSince = 0;") {
		t.Error("setState's CONNECTED arm must clear _disconnectedSince to 0")
	}
	if !strings.Contains(js, "this._disconnectedSince === 0 && s !== WS_STATES.OFF") {
		t.Error("setState's arm branch must guard with `=== 0` so connecting→auth cycles don't restamp the outage clock")
	}
	if !strings.Contains(js, "prev === WS_STATES.CONNECTED && this._disconnectedSince === 0") {
		t.Error("setState must stamp on the first CONNECTED→non-CONNECTED transition when the field is still zero")
	}

	// Invariant 3: updateStatusBar renders .status-outage gated on both the
	// state (!wsUp) and the non-zero timestamp. The render call must use
	// esc() so Chinese label stays safe even if a future refactor pipes
	// user-controlled text through the same path.
	if !strings.Contains(js, "const outageLabel = (!wsUp && wsm._disconnectedSince > 0)") {
		t.Error("updateStatusBar must gate outage hint on (!wsUp && wsm._disconnectedSince > 0)")
	}
	if !strings.Contains(js, `'<div class="status-outage">' + esc(outageLabel)`) {
		t.Error("updateStatusBar must render .status-outage row via esc() for XSS safety")
	}

	// Invariant 4: _statusTickTimer module handle + _updateStatusTick
	// start/stop contract. The timer must be declared at module scope
	// (not redeclared inside a function) so the start/stop logic references
	// a single shared handle.
	if !strings.Contains(js, "let _statusTickTimer = null;") {
		t.Error("_statusTickTimer must be declared at module scope so start/stop reference one handle")
	}
	if !strings.Contains(js, "function _updateStatusTick(state)") {
		t.Fatal("dashboard.js missing _updateStatusTick(state) helper")
	}
	if !strings.Contains(js, "if (state === WS_STATES.CONNECTED) {") {
		t.Error("_updateStatusTick must clear the timer on CONNECTED")
	}
	if !strings.Contains(js, "_statusTickTimer = setInterval(updateStatusBar, 1000);") {
		t.Error("_updateStatusTick must set a 1s setInterval on non-CONNECTED states")
	}
	// setState must call _updateStatusTick so the timer maintenance is
	// wired into every state change — otherwise the tick never arms.
	if !strings.Contains(js, "_updateStatusTick(s);") {
		t.Error("setState must invoke _updateStatusTick(s) so the 1s tick timer tracks WS state changes")
	}
}

// TestDashboardHTML_R110P1_WSOutageHintStyle locks the CSS hook the JS
// renderer relies on. Matches the pattern used by the sidebar-search /
// upload-reorder style tests: a missing CSS rule would leave the hint
// text unstyled (default paragraph tone) and the outage feedback visually
// indistinguishable from the "status-sys" sub-line.
func TestDashboardHTML_R110P1_WSOutageHintStyle(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	css := string(data)

	if !strings.Contains(css, ".sidebar-status .status-outage") {
		t.Error("dashboard.html missing .sidebar-status .status-outage CSS rule — outage hint would be unstyled")
	}
	// Amber token ties the hint color to the existing .connecting dot
	// pulse; future hue changes that stray from --nz-amber / --nz-red
	// should consciously update this test.
	if ok, _ := regexp.MatchString(`\.sidebar-status \.status-outage\{[^}]*--nz-amber`, css); !ok {
		t.Error(".status-outage color must reference --nz-amber so it visually ties to the connecting dot pulse")
	}
}

// .sc-agent 模型家族 chip 在 Round R110-P1 作为"侧栏每卡显示模型家族"的
// MVP 引入，后期因与 section header 的项目名视觉重复被移除——侧栏 chip
// 的价值抵不过重复造成的噪声，且 dashboard 直创 session 的 key 末段是
// folderName 会被当成"未知 agent"错误展示。保留 .sc-agents（复数，子
// agent 计数）。删除后对应契约测试一并移除。

// TestDashboardHTML_R141_ColdStartEmptyStateMatchesHelper pins the invariant
// that the cold-start `<main id="main">` empty state in dashboard.html and
// the mainEmptyHtml() helper in dashboard.js stay in lockstep. Prior to
// Round 141 the helper had been localized (Round 117) but the cold-start
// HTML still read "Ready when you are" / "+ New session" / "or pick one
// from the sidebar" in English — so a fresh page load landed on English,
// while dismissing a session (which repaints via the helper) flipped the
// region to Chinese. That language flicker is a surprising UX regression.
//
// The empty state was redesigned post-Round 141: instead of a "+ 新建会话"
// button pointing at the project palette, the region now renders a
// "问点什么？" quick-ask textarea that — on Enter — creates a general-agent
// session in the default workspace and ships the message in one shot. The
// contract still holds: cold-start HTML and the dismiss-path helper must
// render the same markup shape so there is no flicker when dismissing a
// session.
//
// Three structural invariants (updated for the quick-ask redesign):
//  1. Both files carry the "问点什么？" lead line.
//  2. Both files carry the quick-ask textarea element (id="quick-ask-input").
//  3. The old English strings are gone from dashboard.html so a future
//     revert of localization would fail this test.
func TestDashboardHTML_R141_ColdStartEmptyStateMatchesHelper(t *testing.T) {
	t.Parallel()
	htmlData, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	jsData, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	html := string(htmlData)
	js := string(jsData)

	// Scope the HTML check to the <main id="main"> cold-start body so the
	// assertions don't accidentally match one of the Chinese dismiss-path
	// strings elsewhere in the file (e.g. a tooltip or onboarding modal).
	mainIdx := strings.Index(html, `<main class="main" id="main"`)
	if mainIdx < 0 {
		t.Fatal(`dashboard.html missing <main id="main"> — cold-start host element`)
	}
	mainEnd := strings.Index(html[mainIdx:], "</main>")
	if mainEnd < 0 {
		t.Fatal("dashboard.html missing </main> close tag")
	}
	mainBody := html[mainIdx : mainIdx+mainEnd]

	// Invariant 1 + 2: quick-ask lead line and textarea present in BOTH files.
	for _, want := range []string{
		`问点什么？`,
		`id="quick-ask-input"`,
	} {
		if !strings.Contains(mainBody, want) {
			t.Errorf("dashboard.html cold-start empty state missing %q — must match mainEmptyHtml() helper copy", want)
		}
		if !strings.Contains(js, want) {
			t.Errorf("mainEmptyHtml() in dashboard.js missing %q — cold-start HTML would diverge from dismiss-path repaint", want)
		}
	}

	// Invariant 3: legacy English strings banished from dashboard.html.
	// Keeping them in place (even commented out) risks a copy-paste revert.
	for _, forbidden := range []string{
		`Ready when you are`,
		`>+ New session<`,
		`or pick one from the sidebar`,
	} {
		if strings.Contains(mainBody, forbidden) {
			t.Errorf("dashboard.html cold-start still contains legacy English %q — language flickers between cold-start and dismiss-path repaint", forbidden)
		}
	}
}

// TestDashboardHTML_R144_TopNavA11yLabelsLocalized pins the Round 144
// localization sweep for the main-nav a11y attributes that Round 116
// left in English. Mirrors the pattern of Round 141 / 116:
//
//  1. Two target elements have Chinese aria-label values now:
//     - #resizer → "拖动调整侧栏宽度"
//     - <main id="main"> → "会话内容"
//  2. Their former English strings ("Resize sidebar" / "Session content")
//     are gone so a future revert would trip the check.
//  3. CRITICAL preservation: the header `+` button's
//     `title="New Session"` and `aria-label="Create new session"` MUST
//     stay English — 6+ E2E tests and a screenshot script select by
//     those exact strings:
//     test/e2e/{dashboard,mobile,take-screenshots}.js
//     scripts/dashboard-screenshots.js
//     Localizing those would break a stable selector API with no gain,
//     so this test locks them in place as a deliberate carve-out.
//
// 历史注解：本测试原先还校验侧栏底部 .sf-help（？）按钮的中英文切换，
// 该按钮连同整个 sidebar-footer 在后续"底部信息整块让位给 session 列表"
// 的 UX 迭代中被删除，对应断言随之移除。
func TestDashboardHTML_R144_TopNavA11yLabelsLocalized(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)

	// Invariant 1: Chinese anchors present.
	wantChinese := []struct {
		label    string
		fragment string
	}{
		{"resizer aria-label", `aria-label="拖动调整侧栏宽度"`},
		{"main aria-label", `aria-label="会话内容"`},
	}
	for _, w := range wantChinese {
		if !strings.Contains(html, w.fragment) {
			t.Errorf("dashboard.html missing localized %s: %q", w.label, w.fragment)
		}
	}

	// Invariant 2: former English strings gone.
	forbiddenEnglish := []struct {
		label    string
		fragment string
	}{
		{"resizer aria-label", `aria-label="Resize sidebar"`},
		{"main aria-label", `aria-label="Session content"`},
	}
	for _, f := range forbiddenEnglish {
		if strings.Contains(html, f.fragment) {
			t.Errorf("dashboard.html still contains legacy English %s: %q — future revert would flicker labels back to English", f.label, f.fragment)
		}
	}

	// Invariant 3: E2E-selector carve-out MUST remain untouched. These
	// exact strings are selectors in production test suites; localizing
	// them would cascade breakage.
	mustStayEnglish := []struct {
		label    string
		fragment string
	}{
		{"hdr-btn `+` title", `title="New Session"`},
		{"hdr-btn `+` aria-label", `aria-label="Create new session"`},
	}
	for _, m := range mustStayEnglish {
		if !strings.Contains(html, m.fragment) {
			t.Errorf("dashboard.html LOST the E2E-selector-critical %s: %q — this breaks test/e2e/dashboard.test.js selectors; DO NOT localize without coordinating E2E updates", m.label, m.fragment)
		}
	}
}

// TestDashboardHTML_R149_HeaderIconA11yLocalized pins the Round 149
// follow-on to Round 144: the hdr-btn icons `btn-history` and `btn-cron`
// had `title="History"` / `"Cron Jobs"` + `aria-label="Show session
// history"` / `"Cron jobs"` left in English, which clashed with the
// otherwise-Chinese a11y surface (Round 116 did btn-mobile-back/nav-prev/
// nav-next/btn-hold-talk; Round 144 did sf-help/resizer/main). The two
// invisible badges nested inside (history-badge, cron-badge) also had
// English aria-labels. This round localizes all four plus the nav's
// top-level `aria-label="Sessions"`, while the `+` New Session button
// stays English (hard E2E contract — see Round 144 test).
//
// The history popover header text "History (N)" → "历史 (N)" is covered
// by the sibling TestDashboardJS_R149_HistoryPopoverHeaderLocalized so the
// JS and HTML contracts can fail independently.
func TestDashboardHTML_R149_HeaderIconA11yLocalized(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)

	// Invariant 1: localized anchors present on both header icons and the
	// two badge spans + the sidebar landmark label.
	wantChinese := []struct {
		label    string
		fragment string
	}{
		{"sidebar nav aria-label", `<nav class="sidebar" aria-label="会话列表">`},
		{"btn-history title", `title="历史会话"`},
		{"btn-history aria-label", `aria-label="查看会话历史"`},
		{"history-badge aria-label", `aria-label="历史记录数"`},
		{"btn-cron title", `title="定时任务"`},
		{"btn-cron aria-label", `aria-label="定时任务面板"`},
		{"cron-badge aria-label", `aria-label="定时任务数"`},
	}
	for _, w := range wantChinese {
		if !strings.Contains(html, w.fragment) {
			t.Errorf("dashboard.html missing R149 localized %s: %q", w.label, w.fragment)
		}
	}

	// Invariant 2: legacy English strings gone. A revert that flips any
	// single label back to English would silently regress the already-
	// Chinese header row; this locks the whole block together.
	forbiddenEnglish := []struct {
		label    string
		fragment string
	}{
		{"sidebar nav legacy", `aria-label="Sessions"`},
		{"btn-history title legacy", `title="History"`},
		{"btn-history aria-label legacy", `aria-label="Show session history"`},
		{"history-badge legacy", `aria-label="unread history count"`},
		{"btn-cron title legacy", `title="Cron Jobs"`},
		{"btn-cron aria-label legacy", `aria-label="Cron jobs"`},
		{"cron-badge legacy", `aria-label="cron job count"`},
	}
	for _, f := range forbiddenEnglish {
		if strings.Contains(html, f.fragment) {
			t.Errorf("dashboard.html still contains legacy English %s: %q — single-label revert flickers the row", f.label, f.fragment)
		}
	}

	// Invariant 3: the New Session button stays English — Round 144
	// carved this out as an E2E-selector contract, and this test must
	// keep reinforcing it so a future batch-localization cannot cascade
	// into the E2E surface unnoticed.
	for _, m := range []string{`title="New Session"`, `aria-label="Create new session"`} {
		if !strings.Contains(html, m) {
			t.Errorf("dashboard.html LOST the E2E-selector-critical New Session label %q — this would break test/e2e/dashboard.test.js", m)
		}
	}
}

// TestDashboard_R155_AuxLabelsLocalized pins the Round 155 localization
// pass for the remaining scattered input/aria-label strings that
// Round 154 didn't reach:
//
//  1. Auth-modal token input: initial placeholder + countdown teardown
//     reset — both must use the same "请输入 dashboard token…" literal
//     so the UX doesn't flicker between English and Chinese on retry.
//  2. Discovered-session takeover flow: the "taking over session..."
//     placeholder flashed during the auto-takeover handshake.
//  3. Command palette "Open custom workspace" row (2 variants: with
//     query echo and without) — shown at the bottom of the project list.
//  4. btn-mic aria-label (2 variants) — the title already said "切换
//     键盘/切换语音" but aria-label stayed English.
//  5. Cron notify override inputs: aria-label for Platform + Chat ID.
//
// E2E contract carve-out: `test/e2e/dashboard.test.js:644` asserts
// `#token-input.placeholder` contains literal "invalid token" on auth
// failure — the 'invalid token — try again' string stays English. The
// other placeholders (send a message.../send a message to take over...)
// were already locked by Round 153.
func TestDashboard_R155_AuxLabelsLocalized(t *testing.T) {
	t.Parallel()
	jsBytes, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(jsBytes)

	// Invariant 1: token input placeholder. Both initial and countdown
	// reset must use the same string — inconsistency would surface on
	// rate-limit retries as "flicker back to old label".
	wantTokenPH := `请输入 dashboard token…`
	if c := strings.Count(js, wantTokenPH); c < 2 {
		t.Errorf("token placeholder %q must appear at both initial modal emit and countdown reset, got %d", wantTokenPH, c)
	}
	if strings.Contains(js, `enter dashboard token...`) {
		t.Error("legacy English token placeholder \"enter dashboard token...\" must be removed")
	}
	// E2E-locked invalid-branch placeholder must remain (dashboard.test.js:644).
	if !strings.Contains(js, `'invalid token — try again'`) {
		t.Error("E2E contract: 'invalid token — try again' placeholder must remain English — dashboard.test.js:644 asserts toContain('invalid token')")
	}

	// Invariant 2: discovered takeover placeholder.
	if !strings.Contains(js, `input.dataset.placeholder = '正在接管会话…';`) {
		t.Error("discovered takeover placeholder must be \"正在接管会话…\"")
	}
	if strings.Contains(js, `input.dataset.placeholder = 'taking over session...';`) {
		t.Error("legacy \"taking over session...\" must be removed")
	}

	// Invariant 3: custom-workspace palette row. Keep the prefix match
	// anchored on the Chinese label so both the query-echo and no-query
	// variants localize together.
	for _, want := range []string{
		`'打开自定义工作目录：<span style="color:#79c0ff">'`,
		`: '打开自定义工作目录…';`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("cmd palette custom-row missing localized fragment: %q", want)
		}
	}
	for _, legacy := range []string{
		`'Open custom workspace: <span`,
		`'Open custom workspace…'`,
	} {
		if strings.Contains(js, legacy) {
			t.Errorf("legacy English palette fragment %q must be removed", legacy)
		}
	}

	// Invariant 4: mic button aria-label. Two variants inside a single
	// ternary. Prettier re-encodes CJK inside this HTML-literal line as
	// \u-escape on save, so we accept either raw UTF-8 or the escape form.
	micUtf8 := "'切换到键盘输入' : '切换到语音输入'"
	micEsc := `'\u5207\u6362\u5230\u952e\u76d8\u8f93\u5165' : '\u5207\u6362\u5230\u8bed\u97f3\u8f93\u5165'`
	if !strings.Contains(js, micUtf8) && !strings.Contains(js, micEsc) {
		t.Errorf("btn-mic aria-label ternary missing — neither UTF-8 %q nor \\u-escape %q present", micUtf8, micEsc)
	}
	if strings.Contains(js, `'Switch to keyboard input' : 'Switch to voice input'`) {
		t.Error("legacy English btn-mic aria-label ternary must be removed")
	}

	// Invariant 5: cron notify override aria-labels. IM platform names
	// (feishu) and chat_id are technical tokens kept as placeholders;
	// only aria-label localizes.
	for _, want := range []string{
		`aria-label="IM 平台"`,
		`aria-label="群/会话 ID"`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("cron notify aria-label missing: %q", want)
		}
	}
	for _, legacy := range []string{
		`aria-label="Platform"`,
		`aria-label="Chat ID"`,
	} {
		if strings.Contains(js, legacy) {
			t.Errorf("legacy English cron notify aria-label %q must be removed", legacy)
		}
	}
}

// TestDashboard_R154_ModalsAndSectionsLocalized pins the Round 154
// localization batch for session-new modal + custom-workspace modal +
// cmd-palette + cron-create modal + cron-edit modal + scattered
// icon-button aria-labels that were English residue from earlier rounds.
//
// E2E hard-lock carve-outs preserved (re-asserted inline):
//   - .modal h3 for new-session modal: "New Session" (dashboard.test.js:717)
//   - .modal h3 for auth modal:        "Dashboard API Token" (L589)
//   - .modal h3 for new-cron modal:    "New Cron Job" (L904)
//
// Everything else (aria-label, button textContent, section labels,
// palette footer hints, etc.) was safe to localize because E2E selectors
// are all id-based or class-based.
func TestDashboard_R154_ModalsAndSectionsLocalized(t *testing.T) {
	t.Parallel()
	jsBytes, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(jsBytes)

	// Invariant 1: session-new modal + custom-workspace modal. The
	// <h3>New Session</h3> is hard-locked by E2E so we both assert it
	// stays English AND assert aria-label/buttons/label localize.
	if !strings.Contains(js, `<h3>New Session</h3>`) {
		t.Error("E2E contract: <h3>New Session</h3> must remain English — test/e2e/dashboard.test.js:717 asserts literal equality")
	}
	for _, want := range []string{
		`aria-label="新建会话">`,
		`for="new-workspace">工作目录</label>`,
		`.closest(\'.modal-overlay\').remove()">取消</button>`,
		`onclick="doCreateSession()">创建</button>`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("new-session modal missing localized fragment: %q", want)
		}
	}

	// Custom-workspace modal (spawned from palette's custom-path row).
	// No E2E contract on this h3; fully localized.
	if !strings.Contains(js, `<h3>自定义工作目录</h3>`) {
		t.Error("custom-workspace modal h3 must be \"自定义工作目录\"")
	}
	if strings.Contains(js, `<h3>Custom Workspace</h3>`) {
		t.Error("legacy <h3>Custom Workspace</h3> must be removed")
	}

	// Invariant 2: command palette placeholder + footer hints + aria.
	for _, want := range []string{
		`aria-label="新建会话">`,
		`placeholder="搜索项目或输入路径…">`,
		`<kbd>↑</kbd><kbd>↓</kbd> 切换`,
		`<kbd>Enter</kbd> 打开`,
		`<kbd>Esc</kbd> 关闭`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("cmd palette missing localized fragment: %q", want)
		}
	}
	for _, legacy := range []string{
		`placeholder="Search projects or type a path…"`,
		`<kbd>↑</kbd><kbd>↓</kbd> navigate`,
		`<kbd>Enter</kbd> open`,
		`<kbd>Esc</kbd> close`,
	} {
		if strings.Contains(js, legacy) {
			t.Errorf("legacy English cmd palette fragment %q must be removed", legacy)
		}
	}

	// Invariant 3: cron-create modal. Two-column layout uses .cf-label
	// field headers ("做什么 / 什么时候 / 在哪里 / 其他设置") instead of the
	// legacy .modal-section-label stacked sections. Title is E2E-locked to
	// the Chinese string "新建定时任务" (dashboard.test.js:904).
	if !strings.Contains(js, `<h3>新建定时任务</h3>`) {
		t.Error("E2E contract: <h3>新建定时任务</h3> must remain — dashboard.test.js:904")
	}
	for _, want := range []string{
		`aria-label="新建定时任务">`,
		`<div class="cf-label">做什么</div>`,
		`<div class="cf-label">什么时候</div>`,
		`<div class="cf-label">在哪里</div>`,
		`<div class="cf-label">其他设置</div>`,
		`aria-label="提示词">`,
		`aria-label="工作目录">`,
		`<span class="pp-custom-icon">+</span> 自定义路径`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("cron-create modal missing localized fragment: %q", want)
		}
	}
	for _, legacy := range []string{
		`<h3>New Cron Job</h3>`,
		`<div class="modal-section-label">Schedule</div>`,
		`<div class="modal-section-label">Prompt</div>`,
		`<div class="modal-section-label">Workspace (optional)</div>`,
		`placeholder="what should this job do?"`,
		`aria-label="Prompt"`,
		`aria-label="Workspace"`,
		`aria-label="Workspace path"`,
		`<span class="pp-custom-icon">+</span> Custom path`,
	} {
		if strings.Contains(js, legacy) {
			t.Errorf("legacy English cron fragment %q must be removed", legacy)
		}
	}

	// Invariant 4: cron-edit modal (h3 not E2E-locked, so fully localize).
	for _, want := range []string{
		`aria-label="编辑定时任务">`,
		`<h3>编辑定时任务</h3>`,
		`doEditCronJob(`,
		`">保存</button>`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("cron-edit modal missing localized fragment: %q", want)
		}
	}
	// The save button is also used by the auth modal; both sites must
	// have been localized in this round, so expect ≥ 2 occurrences.
	if c := strings.Count(js, `">保存</button>`); c < 2 {
		t.Errorf("`保存</button>` must appear at both auth-modal and cron-edit-modal sites, got %d", c)
	}
	for _, legacy := range []string{
		`<h3>Edit Cron Job</h3>`,
		`>save</button>`,
		`留空则使用默认 workspace`,
	} {
		if strings.Contains(js, legacy) {
			t.Errorf("legacy English cron-edit fragment %q must be removed", legacy)
		}
	}

	// Invariant 5: scattered icon-button aria-labels (Rename session,
	// Reconnect now, Upload image, Remove image). Each has Chinese title
	// already, but aria-label was English — Round 154 aligns them. The
	// "remove image" row is embedded in a wider HTML literal that
	// prettier re-encodes as \u....-escape on save, so match both forms.
	for _, want := range []string{
		`title="立即重连" aria-label="立即重连"`,
		`title="重命名会话" aria-label="重命名会话"`,
		// Upload button title was broadened from "上传图片" to
		// "上传图片或 PDF" when PDF attachment support landed (see
		// docs/rfc/pdf-attachment.md).
		`title="上传图片或 PDF" aria-label="上传图片或 PDF"`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("icon-button title+aria-label missing: %q", want)
		}
	}
	// Remove label: generalised from "移除图片" to plain "移除" because the
	// same button renders on image thumbs AND PDF chips since
	// docs/rfc/pdf-attachment.md. 移 U+79FB 除 U+9664 — match raw UTF-8
	// or \u-escape form (Go literal vs prettier-reformatted JS literal
	// may differ).
	removeUtf8 := `title="移除" aria-label="移除"`
	removeEsc := `title="\u79fb\u9664" aria-label="\u79fb\u9664"`
	if !strings.Contains(js, removeUtf8) && !strings.Contains(js, removeEsc) {
		t.Errorf("remove icon-button missing localized title+aria-label — neither UTF-8 %q nor \\u-escape %q present", removeUtf8, removeEsc)
	}
	for _, legacy := range []string{
		`aria-label="Reconnect now"`,
		`aria-label="Rename session"`,
		`aria-label="Upload image"`,
		`aria-label="Remove image"`,
		`title="upload image"`,
	} {
		if strings.Contains(js, legacy) {
			t.Errorf("legacy English icon-button label %q must be removed", legacy)
		}
	}
}

// TestDashboard_R153_InputAreaAndTakeoverLocalized pins the Round 153
// localization for the msg-input / btn-send / discovered-preview empty
// state / takeover-button 3-state strings.
//
// E2E contract carve-out: `test/e2e/dashboard.test.js:1225` hard-asserts
// `msg-input.dataset.placeholder === 'send a message...'`, so the
// `data-placeholder` attribute MUST stay English in the main-shell
// emit path. We only localize the `aria-label` (not read by the E2E
// test) and the `btn-send` title/aria-label which the E2E test does
// not check either. The "send a message to take over..." takeover-
// placeholder variant is also preserved unchanged to match the same
// E2E scope even though no test touches it — a single-placeholder
// form keeps both paths visually aligned until i18n lands.
//
// Call-site parity: the main-shell emitter and the discovered-preview
// emitter use the same four labels (msg-input aria-label, btn-send
// title, btn-send aria-label), so the test asserts both forms exist
// at the exact emit-site granularity via strings.Count >= 2.
func TestDashboard_R153_InputAreaAndTakeoverLocalized(t *testing.T) {
	t.Parallel()
	jsBytes, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(jsBytes)

	// Invariant 1: both emit paths use the localized msg-input aria-label.
	wantAriaLabel := `aria-label="消息输入框"`
	if c := strings.Count(js, wantAriaLabel); c < 2 {
		t.Errorf("msg-input aria-label %q must appear at both emit sites (main-shell + discovered-preview), got %d", wantAriaLabel, c)
	}
	if strings.Contains(js, `aria-label="Message input"`) {
		t.Error("legacy English msg-input aria-label \"Message input\" must be removed")
	}

	// Invariant 2: btn-send title + aria-label localized (both emit
	// paths). Exact tag shape so a decoupling of title/aria-label into
	// separate tokens would fail this test.
	wantSend := `title="发送" aria-label="发送消息"`
	if c := strings.Count(js, wantSend); c < 2 {
		t.Errorf("btn-send %q must appear at both emit sites, got %d", wantSend, c)
	}
	if strings.Contains(js, `title="send" aria-label="Send message"`) {
		t.Error("legacy English btn-send title/aria-label must be removed")
	}

	// Invariant 3: discovered-preview empty state localized.
	if !strings.Contains(js, `'<div class="empty-state">暂无会话历史</div>'`) {
		t.Error("discovered preview empty state must say \"暂无会话历史\"")
	}
	if strings.Contains(js, `'<div class="empty-state">no conversation history</div>'`) {
		t.Error("legacy English empty state \"no conversation history\" must be removed")
	}

	// Invariant 4: takeover button 3-state labels.
	if !strings.Contains(js, `btn.textContent = '接管中...';`) {
		t.Error("takeover progress text \"接管中...\" must be present")
	}
	if c := strings.Count(js, `btn.textContent = '接管';`); c < 2 {
		t.Errorf("takeover idle-label reset \"接管\" must appear at both failure-recovery branches, got %d", c)
	}
	for _, legacy := range []string{
		`btn.textContent = 'taking over...';`,
		`btn.textContent = 'takeover';`,
	} {
		if strings.Contains(js, legacy) {
			t.Errorf("legacy English takeover label %q must be removed", legacy)
		}
	}

	// Invariant 5: E2E-selector carve-outs preserved. The placeholder
	// strings are the hard contract; localizing them would cascade into
	// test/e2e/dashboard.test.js:1225 literal-equality assertion.
	for _, keep := range []string{
		`data-placeholder="send a message..."`,
		`data-placeholder="send a message to take over..."`,
	} {
		if !strings.Contains(js, keep) {
			t.Errorf("E2E-selector-critical placeholder %q must remain — localizing this breaks test/e2e/dashboard.test.js:1225", keep)
		}
	}
}

// TestDashboard_R151_EventStreamAndBannerLocalized pins the Round 151
// localization pass for three related UI surfaces:
//
//  1. Sidebar header title "Dashboard" → "脑汁" (matches the project's
//     brand name used throughout CLAUDE.md / README / auth-modal brand
//     lockup).
//  2. Event-stream "Load earlier" pagination button (4 states) + the
//     empty-state "no events yet" placeholder.
//  3. Running-banner tool-verb map (9 verbs) + the 3 non-tool states
//     (Thinking / Writing / Working) + the "Using X" fallback for
//     unknown tools.
//
// All three surfaces are pure textContent writes — JS reads nothing
// back from them. Tool keys (Read/Edit/Bash/…) are Claude protocol
// identifiers and MUST remain as English map keys; only the display
// verbs localize. Agent label stays English because it is already a
// product-level proper noun used throughout the UI ("Agent rows",
// ".sc-agents", etc.).
//
// E2E carve-out: `test/e2e/*` uses id selectors on #earlier-events-btn
// and #tool-activity and does not assert their text content.
func TestDashboard_R151_EventStreamAndBannerLocalized(t *testing.T) {
	t.Parallel()
	htmlBytes, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	jsBytes, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	html := string(htmlBytes)
	js := string(jsBytes)

	// Invariant 1: sidebar header brand.
	if !strings.Contains(html, `<h1>脑汁</h1>`) {
		t.Error("sidebar header must show `<h1>脑汁</h1>` (was `Dashboard`)")
	}
	if strings.Contains(html, `<h1>Dashboard</h1>`) {
		t.Error("sidebar header legacy `<h1>Dashboard</h1>` must be removed")
	}

	// Invariant 2: Load earlier button 4 states + empty state.
	wantJSStrings := []struct {
		label    string
		fragment string
	}{
		{"btn initial text #1", `btn.textContent = '加载更早的事件';`},
		{"btn loading state", `btn.textContent = '加载中…';`},
		{"btn done state", `btn.textContent = '没有更早的事件';`},
		{"btn error state", `btn.textContent = '加载失败 — 点击重试';`},
		{"empty-state events", `'<div class="empty-state">暂无事件</div>'`},
	}
	for _, w := range wantJSStrings {
		if !strings.Contains(js, w.fragment) {
			t.Errorf("dashboard.js missing R151 %s: %q", w.label, w.fragment)
		}
	}
	// Additional empty-state / cold-state strings covered by this round.
	// Prettier/lint passes re-encode CJK inside HTML-embedded string
	// literals as \u-escape (observed on this codebase after save), so we
	// match either the raw UTF-8 form or the \u-escape form. codepoints
	// are documented inline so a reviewer can sanity-check without a
	// Unicode lookup table.
	//
	//   正 U+6B63  在 U+5728  加 U+52A0  载 U+8F7D  事 U+4E8B  件 U+4EF6
	//   暂 U+6682  无 U+65E0  处 U+5904  理 U+7406  中 U+4E2D
	//   … U+2026
	type dualForm struct {
		label string
		utf8  string
		esc   string
	}
	for _, want := range []dualForm{
		{
			"updateState loadingEl replacement",
			`loadingEl.innerHTML = '暂无事件';`,
			`loadingEl.innerHTML = '\u6682\u65e0\u4e8b\u4ef6';`,
		},
		{
			"renderMainShell running loading-indicator",
			`'<div class="empty-state loading-indicator">正在加载事件…</div>'`,
			`'<div class="empty-state loading-indicator">\u6b63\u5728\u52a0\u8f7d\u4e8b\u4ef6\u2026</div>'`,
		},
		{
			"events innerHTML empty fallback",
			`'<div class="empty-state">暂无事件</div>'`,
			`'<div class="empty-state">\u6682\u65e0\u4e8b\u4ef6</div>'`,
		},
		{
			"tool-activity cold state",
			`<span id="tool-activity">处理中...</span>`,
			`<span id="tool-activity">\u5904\u7406\u4e2d...</span>`,
		},
		{
			"discovered shell loading",
			`<div class="empty-state">加载中...</div>`,
			`<div class="empty-state">\u52a0\u8f7d\u4e2d...</div>`,
		},
	} {
		if !strings.Contains(js, want.utf8) && !strings.Contains(js, want.esc) {
			t.Errorf("dashboard.js missing R151 cold-state %s — neither UTF-8 form %q nor \\u-escape form %q present", want.label, want.utf8, want.esc)
		}
	}
	legacyJSStrings := []string{
		`btn.textContent = 'Load earlier';`,
		`btn.textContent = 'Loading…';`,
		`btn.textContent = 'No earlier events';`,
		`btn.textContent = 'Failed — retry';`,
		`'<div class="empty-state">no events yet</div>'`,
		`loadingEl.innerHTML = 'no events yet';`,
		`'<div class="empty-state loading-indicator">loading events…</div>'`,
		`<span id="tool-activity">Working...</span>`,
		`<div class="empty-state">loading...</div>`,
	}
	for _, f := range legacyJSStrings {
		if strings.Contains(js, f) {
			t.Errorf("dashboard.js still contains legacy English: %q", f)
		}
	}

	// Invariant 3: tool verb map — strict whitelist of keys with
	// localized verbs, plus Agent staying English as a proper noun.
	wantVerbMap := `const toolVerbs = {
  Read: '读取', Edit: '编辑', Write: '写入', Bash: '执行',
  Grep: '搜索', Glob: '查找文件', Agent: 'Agent',
  Notebook: '编辑 Notebook', WebFetch: '抓取'
};`
	if !strings.Contains(js, wantVerbMap) {
		t.Error("toolVerbs map must match the Round 151 localized shape exactly (Read/Edit/Write/Bash/Grep/Glob/Agent/Notebook/WebFetch with Chinese verbs + Agent kept English)")
	}
	if !strings.Contains(js, `const verb = toolVerbs[tool] || ('使用 ' + tool);`) {
		t.Error("toolVerb fallback must be '使用 ' prefix for unknown tools (was 'Using ')")
	}
	if strings.Contains(js, `toolVerbs[tool] || ('Using ' + tool)`) {
		t.Error("toolVerb fallback legacy 'Using ' + tool must be removed")
	}

	// Banner 3 non-tool states.
	for _, want := range []string{
		`actEl.textContent = '思考中...';`,
		`actEl.textContent = '输出中...';`,
		`actEl.textContent = '处理中...';`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("refreshBanner non-tool state missing: %s", want)
		}
	}
	for _, legacy := range []string{
		`actEl.textContent = 'Thinking...';`,
		`actEl.textContent = 'Writing...';`,
		`actEl.textContent = 'Working...';`,
	} {
		if strings.Contains(js, legacy) {
			t.Errorf("refreshBanner legacy English still present: %s", legacy)
		}
	}

	// Invariant 4: id anchors untouched so JS / E2E keep working. These
	// three ids (events-scroll / tool-activity / running-banner) are
	// emitted by renderMainShell and renderDiscoveredShell in JS, not in
	// the static HTML, so grep on the JS string.
	for _, id := range []string{`id="events-scroll"`, `id="tool-activity"`, `id="running-banner"`} {
		if !strings.Contains(js, id) {
			t.Errorf("dashboard.js LOST id anchor %q — JS/E2E lookups break", id)
		}
	}
	if !strings.Contains(js, `btn.id = 'earlier-events-btn';`) {
		t.Error("earlier-events-btn id assignment must remain so stylesheet + JS locators stay wired")
	}
}

// TestDashboardHTML_R150_FilePreviewA11yLocalized pins the Round 150
// localization pass for the file-preview drawer + footer version tooltip
// + voice-overlay landmark. These were missed by Round 116/144/149 because
// they live outside the top-nav and the history/cron hot path.
//
// E2E carve-outs verified: `test/e2e` uses `#voice-overlay` and
// `#fv-drawer` id-based selectors only (grep passed). There is no text-
// based assertion on "File preview" / "Voice recording" / "Copy path" /
// "Download" / "Close preview" / "naozhi version". The `file` / `copy` /
// `download` visible button/placeholder strings are cold-state copy that
// JS overwrites on open — see openFilePreview / _codeBlockFilename paths.
func TestDashboardHTML_R150_FilePreviewA11yLocalized(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)

	// Invariant 1: Chinese anchors present.
	wantChinese := []struct {
		label    string
		fragment string
	}{
		{"fv-drawer aria-label", `aria-label="文件预览"`},
		{"fv-title placeholder", `<span class="fv-title" id="fv-title">文件</span>`},
		{"fv-btn-copy title", `id="fv-btn-copy" title="复制路径" aria-label="复制路径">复制<`},
		{"fv-btn-download title", `id="fv-btn-download" title="下载文件" aria-label="下载文件">下载<`},
		{"fv-btn-close title", `id="fv-btn-close" title="关闭" aria-label="关闭预览">&times;<`},
		{"voice-overlay aria-label", `aria-label="语音录制"`},
	}
	for _, w := range wantChinese {
		if !strings.Contains(html, w.fragment) {
			t.Errorf("dashboard.html missing R150 localized %s: %q", w.label, w.fragment)
		}
	}

	// Invariant 2: legacy English strings gone. Locking the whole group
	// so a partial revert can't quietly flicker individual labels back.
	forbiddenEnglish := []struct {
		label    string
		fragment string
	}{
		{"fv-drawer legacy", `aria-label="File preview"`},
		{"fv-title placeholder legacy", `<span class="fv-title" id="fv-title">file</span>`},
		{"fv-btn-copy legacy", `title="Copy path" aria-label="Copy path">copy<`},
		{"fv-btn-download legacy", `title="Download" aria-label="Download">download<`},
		{"fv-btn-close legacy", `title="Close" aria-label="Close preview">&times;<`},
		{"voice-overlay legacy", `aria-label="Voice recording"`},
	}
	for _, f := range forbiddenEnglish {
		if strings.Contains(html, f.fragment) {
			t.Errorf("dashboard.html still contains legacy English %s: %q", f.label, f.fragment)
		}
	}

	// Invariant 3: id-based selector anchors are untouched. JS in
	// openFilePreview / openInlineCodePreview grabs #fv-drawer / #fv-title
	// / #fv-btn-copy etc. by id; a rename here would break the drawer
	// silently without any a11y signal.
	for _, id := range []string{`id="fv-drawer"`, `id="fv-title"`, `id="fv-btn-copy"`, `id="fv-btn-download"`, `id="fv-btn-close"`, `id="voice-overlay"`} {
		if !strings.Contains(html, id) {
			t.Errorf("dashboard.html LOST the id anchor %q — JS lookups will return null and break the feature", id)
		}
	}
}

// TestDashboardJS_R149_HistoryPopoverHeaderLocalized pins the history
// popover header text "History (N)" → "历史 (N)". The popover is opened
// via #btn-history (id-selector) so E2E does not depend on the visible
// text. Localizing here closes the parity gap with the Chinese search
// input placeholder ("搜索提示词或项目…") and the Chinese day-group
// labels ("今天" / "昨天") already shipped in Round 129.
func TestDashboardJS_R149_HistoryPopoverHeaderLocalized(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// Invariant 1: Chinese header present. The hp-count span structure is
	// preserved (renders `(N)` or `(x / y)` depending on filter state); we
	// only touch the leading label word.
	want := `'<span>历史 <span class="hp-count" id="hp-count">('`
	if !strings.Contains(js, want) {
		t.Errorf("dashboard.js missing localized history popover header %q", want)
	}

	// Invariant 2: legacy English prefix gone. Match the full surrounding
	// fragment so an unrelated comment mentioning `History` (the word
	// appears in source comments) cannot cause false positive / negative.
	forbidden := `'<span>History <span class="hp-count"`
	if strings.Contains(js, forbidden) {
		t.Errorf("dashboard.js still emits legacy English history header %q", forbidden)
	}

	// Invariant 3: hp-count id and class stay stable — applyHistoryFilter
	// mutates the count via getElementById('hp-count'). A rename here
	// would break the filter's live count chip without any visual error.
	if !strings.Contains(js, `document.getElementById('hp-count')`) {
		t.Error("applyHistoryFilter must continue to look up the count chip by id='hp-count'")
	}
	if !strings.Contains(js, `class="hp-count" id="hp-count"`) {
		t.Error("history popover header must keep the hp-count chip with class+id both set so CSS and JS hooks stay wired")
	}
}

// TestDashboardJS_R110P1_HomePanelHealth pins the Round 148 contract for
// the Home-panel health strip — the bottom meta row that surfaces service
// health sourced from /api/sessions `stats` (active/running/ready/uptime/
// cli/watchdog). Round 146 landed the panel, Round 147 added the top stats
// condensed into 2 headline metrics; this round fills the bottom strip.
//
// Three structural invariants:
//
//  1. buildHomeHealthLines is a pure helper with documented input/output:
//     nil / non-object snapshot returns []; always emits line-1 counts
//     (running/ready/total); CLI line only when stats.cli_name present;
//     watchdog line only when total_kills > 0 and is tagged kind='warn'.
//  2. lastStatsSnapshot module field exists and is written by fetchSessions
//     so the render hook has a cache to read without a second HTTP call.
//  3. renderRecentSessionsPanel renders the health strip AFTER the session
//     list (below-the-fold for brevity) and gates on non-empty helper
//     output so cold-start (no snapshot yet) stays clean. Chinese copy
//     "服务健康" aria-label + "运行" / "就绪" / "总" / "运行" prefixes must
//     appear.
func TestDashboardJS_R110P1_HomePanelHealth(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// Invariant 1: buildHomeHealthLines exists + covers the 3 branches.
	if !strings.Contains(js, "function buildHomeHealthLines(stats)") {
		t.Fatal("dashboard.js missing buildHomeHealthLines(stats) helper")
	}
	for _, fragment := range []string{
		`if (!stats || typeof stats !== 'object') return [];`,
		`'运行 ' + running + ' · 就绪 ' + ready + ' · 总 ' + total`,
		`if (stats.uptime) line1 += ' · 运行 ' + stats.uptime;`,
		`if (stats.cli_name) {`,
		`if (totalKills > 0) {`,
		`kind: 'warn',`,
	} {
		if !strings.Contains(js, fragment) {
			t.Errorf("buildHomeHealthLines body missing contract fragment %q", fragment)
		}
	}

	// Invariant 2: lastStatsSnapshot module-level field + fetchSessions
	// write-through.
	if !strings.Contains(js, "let lastStatsSnapshot = null;") {
		t.Error("dashboard.js must declare `let lastStatsSnapshot = null;` at module scope so the health strip reads a fresh cache")
	}
	if !strings.Contains(js, "lastStatsSnapshot = data.stats;") {
		t.Error("fetchSessions must write `lastStatsSnapshot = data.stats` so the Home panel's health strip stays in sync with each poll")
	}

	// Invariant 3: renderRecentSessionsPanel wires the health strip.
	// Scan within the helper body for positional ordering (strip AFTER list).
	rpIdx := strings.Index(js, "function renderRecentSessionsPanel()")
	if rpIdx < 0 {
		t.Fatal("dashboard.js missing renderRecentSessionsPanel()")
	}
	bodyEnd := rpIdx + 8000
	if bodyEnd > len(js) {
		bodyEnd = len(js)
	}
	body := js[rpIdx:bodyEnd]
	if !strings.Contains(body, "const healthLines = buildHomeHealthLines(lastStatsSnapshot);") {
		t.Error("renderRecentSessionsPanel must derive healthLines from buildHomeHealthLines(lastStatsSnapshot)")
	}
	if !strings.Contains(body, "healthLines.length === 0") {
		t.Error("renderRecentSessionsPanel must gate the health strip on healthLines.length so cold-start without stats stays clean")
	}
	if !strings.Contains(body, `aria-label="服务健康"`) {
		t.Error("health strip must carry aria-label=\"服务健康\" for screen readers")
	}
	// Positional check: health strip HTML must be emitted AFTER list HTML.
	listHtmlIdx := strings.Index(body, `'<div class="recent-panel-list"`)
	healthIdx := strings.Index(body, "healthHtml +")
	if listHtmlIdx < 0 {
		t.Error("renderRecentSessionsPanel must still emit <div class=\"recent-panel-list\">")
	}
	if healthIdx < 0 {
		t.Error("renderRecentSessionsPanel must concat healthHtml into the final innerHTML")
	}
	if listHtmlIdx > 0 && healthIdx > 0 && healthIdx < listHtmlIdx {
		t.Error("health strip must render AFTER the session list, not before — operators scan the list first, health is a below-the-fold meta row")
	}
}

// TestDashboardHTML_R110P1_HomePanelHealthStyles pins the CSS hooks for
// the health strip. Warn kind must use --nz-amber so watchdog incidents
// visually align with the sidebar .connecting-dot pulse (both are
// "something isn't quite right" signals).
func TestDashboardHTML_R110P1_HomePanelHealthStyles(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	css := string(data)

	for _, rule := range []string{
		".recent-panel-health",
		".recent-health-line",
		".recent-health-line.warn",
	} {
		if !strings.Contains(css, rule) {
			t.Errorf("dashboard.html missing CSS rule %q used by R110-P1 home panel health strip", rule)
		}
	}

	// .warn must reference --nz-amber so watchdog alerts reuse the
	// same hue as the WS-outage hint (Round 142) and the .connecting
	// dot pulse — one palette, three surfaces.
	if ok, _ := regexp.MatchString(`\.recent-health-line\.warn\{[^}]*--nz-amber`, css); !ok {
		t.Error(".recent-health-line.warn must reference --nz-amber so the watchdog alert matches the existing amber-warn palette")
	}
	// The strip gets a border-top separator so it reads as a distinct
	// meta region below the list — otherwise it blends with the last row.
	if ok, _ := regexp.MatchString(`\.recent-panel-health\{[^}]*border-top:1px solid var\(--nz-border\)`, css); !ok {
		t.Error(".recent-panel-health must declare `border-top:1px solid var(--nz-border)` to visually separate health meta from the session list")
	}
}

// TestDashboardJS_R110P1_HomePanelStats pins the Round 147 contract for
// the Home-panel stats strip. Round 146 landed the "最近会话" list; this
// round adds two aggregate metrics (today active + total cost) computed
// pure-client-side from allSessionsCache. Prompts and tokens are deferred
// because they need backend aggregation.
//
// Three structural invariants:
//
//  1. computeHomeStats is a pure helper with the documented boundary:
//     today = items whose last_active >= local-midnight of the NOW arg;
//     totalCost sums total_cost across all cached sessions (no today
//     filter — cron-heavy workspaces accumulate cost overnight).
//     Input shape tolerant: missing fields contribute zero / are skipped.
//  2. formatHomeCost keeps sub-cent precision (4 decimals) for tiny
//     values, 2 decimals once cost is measurable — mirrors the session
//     card header-cost chip behavior.
//  3. renderRecentSessionsPanel emits the stats strip above the session
//     list AND passes Date.now() into computeHomeStats (not a hardcoded
//     constant — obvious bug bait if a future refactor captures a stale
//     timestamp at page-load and never refreshes).
func TestDashboardJS_R110P1_HomePanelStats(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// Invariant 1: computeHomeStats defined + carries the day-start
	// boundary computation + tolerant field checks.
	if !strings.Contains(js, "function computeHomeStats(items, nowMs)") {
		t.Fatal("dashboard.js missing computeHomeStats(items, nowMs) helper")
	}
	for _, fragment := range []string{
		`new Date(d.getFullYear(), d.getMonth(), d.getDate(), 0, 0, 0, 0).getTime()`,
		`typeof s.last_active === 'number' && s.last_active >= dayStart`,
		`typeof s.total_cost === 'number' && isFinite(s.total_cost)`,
		`return { todayActive: todayActive, totalCost: totalCost };`,
	} {
		if !strings.Contains(js, fragment) {
			t.Errorf("computeHomeStats body missing contract fragment %q", fragment)
		}
	}

	// Invariant 2: formatHomeCost defined + dual-precision branching.
	if !strings.Contains(js, "function formatHomeCost(cost)") {
		t.Fatal("dashboard.js missing formatHomeCost(cost) helper")
	}
	for _, fragment := range []string{
		`if (c >= 0.01) return '$' + c.toFixed(2);`,
		`if (c > 0) return '$' + c.toFixed(4);`,
		`return '$0.00';`,
	} {
		if !strings.Contains(js, fragment) {
			t.Errorf("formatHomeCost body missing branch %q", fragment)
		}
	}

	// Invariant 3: renderRecentSessionsPanel wires the stats strip in
	// correctly. Must call computeHomeStats with Date.now() (not a
	// captured constant) and emit the stats container BEFORE the list.
	rpIdx := strings.Index(js, "function renderRecentSessionsPanel()")
	if rpIdx < 0 {
		t.Fatal("dashboard.js missing renderRecentSessionsPanel()")
	}
	bodyEnd := rpIdx + 6000
	if bodyEnd > len(js) {
		bodyEnd = len(js)
	}
	body := js[rpIdx:bodyEnd]
	if !strings.Contains(body, "const stats = computeHomeStats(items, Date.now());") {
		t.Error("renderRecentSessionsPanel must compute stats via computeHomeStats(items, Date.now()) — a hardcoded timestamp would make the 'today active' count stale")
	}
	// stats must render before the list — scan positions inside the
	// body slice.
	statsHtmlIdx := strings.Index(body, `'<div class="recent-panel-stats"`)
	listHtmlIdx := strings.Index(body, `'<div class="recent-panel-list"`)
	if statsHtmlIdx < 0 {
		t.Error("renderRecentSessionsPanel must emit <div class=\"recent-panel-stats\">")
	}
	if listHtmlIdx < 0 {
		t.Error("renderRecentSessionsPanel must emit <div class=\"recent-panel-list\">")
	}
	if statsHtmlIdx > 0 && listHtmlIdx > 0 && statsHtmlIdx > listHtmlIdx {
		t.Error("stats strip must render BEFORE the session list — reverse order would push the list below the fold on short viewports")
	}
	// Chinese labels — operators should see Chinese copy.
	for _, want := range []string{"今日活跃会话", "累计花费"} {
		if !strings.Contains(body, want) {
			t.Errorf("renderRecentSessionsPanel stats strip missing Chinese label %q", want)
		}
	}
}

// TestDashboardHTML_R110P1_HomePanelStatsStyles pins the CSS hooks the
// stats strip JS relies on. Matches the per-round style test pattern:
// if any of these classes is renamed, the stats container would render
// unstyled (no grid layout, no visual separation from the list).
func TestDashboardHTML_R110P1_HomePanelStatsStyles(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	css := string(data)

	for _, rule := range []string{
		".recent-panel-stats",
		".recent-stat",
		".recent-stat-value",
		".recent-stat-label",
	} {
		if !strings.Contains(css, rule) {
			t.Errorf("dashboard.html missing CSS rule %q used by R110-P1 home panel stats strip", rule)
		}
	}

	// 2-column grid is the intended layout (today active + cost). Anchor
	// the declaration inside the .recent-panel-stats rule so a future
	// single-column reflow would trip the check.
	if ok, _ := regexp.MatchString(`\.recent-panel-stats\{[^}]*grid-template-columns:1fr 1fr`, css); !ok {
		t.Error(".recent-panel-stats must be a 2-column grid (today active + total cost)")
	}
	// Tabular-nums on the stat value so numbers align optically when
	// count of active sessions crosses digit boundaries (1 → 10).
	if ok, _ := regexp.MatchString(`\.recent-stat-value\{[^}]*font-variant-numeric:tabular-nums`, css); !ok {
		t.Error(".recent-stat-value must set font-variant-numeric:tabular-nums so digit widths line up")
	}
}

// TestDashboardJS_R110P1_HomePanelMVP pins the Round 146 contract for the
// idle "Home" panel — an MVP slice of the bigger "R110-P1 空闲态 Home 仪表"
// TODO. Top stats cards + bottom health row need backend aggregation
// (/api/stats/aggregate), so this round lands ONLY the middle section:
// a "最近会话" compact list rendered from data already in allSessionsCache.
//
// Three structural invariants:
//
//  1. renderRecentSessionsPanel is defined, gated by selectedKey (so it
//     doesn't paint when the main shell owns the view), and writes into
//     the #recent-sessions-panel DOM slot rather than returning HTML.
//  2. mainEmptyHtml carries the #recent-sessions-panel placeholder so
//     dismiss/remove repaints also host the home panel. Round 117's
//     existing `select a session` forbidden check must not regress.
//  3. renderSidebar calls renderRecentSessionsPanel after each repaint,
//     so the home panel's data stays in sync with the sidebar snapshot.
//     Without that call the panel would freeze on stale data.
func TestDashboardJS_R110P1_HomePanelMVP(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// Invariant 1: helper + gate + DOM write path.
	if !strings.Contains(js, "function renderRecentSessionsPanel()") {
		t.Fatal("dashboard.js missing renderRecentSessionsPanel() helper")
	}
	if !strings.Contains(js, "if (selectedKey) return; // active session rendered by renderMainShell") {
		t.Error("renderRecentSessionsPanel must early-return when selectedKey is set — active session main shell owns the view")
	}
	if !strings.Contains(js, `document.getElementById('recent-sessions-panel')`) {
		t.Error("renderRecentSessionsPanel must target the #recent-sessions-panel DOM slot")
	}
	// Zero-sessions branch must render empty innerHTML, not a "nothing to
	// show" hint — the cold-start CTA is already the empty state UX.
	if !strings.Contains(js, "if (items.length === 0) { host.innerHTML = ''; return; }") {
		t.Error("renderRecentSessionsPanel must short-circuit to empty innerHTML when allSessionsCache is empty — keeps cold-start minimal")
	}
	// Top-5 sort by last_active desc is the MVP spec.
	if !strings.Contains(js, "sort((a, b) => (b.last_active || 0) - (a.last_active || 0)).slice(0, 5)") {
		t.Error("renderRecentSessionsPanel must pick top-5 by last_active descending")
	}

	// Invariant 2: mainEmptyHtml carries the placeholder div so dismiss
	// flows get the same home panel slot. Match the full attribute
	// substring rather than loose fragments so a future refactor can't
	// silently drop the id.
	if !strings.Contains(js, `'<div id="recent-sessions-panel" class="recent-panel-wrap"></div>'`) {
		t.Error("mainEmptyHtml must carry the <div id=\"recent-sessions-panel\" class=\"recent-panel-wrap\"></div> slot")
	}

	// Invariant 3: renderSidebar invokes renderRecentSessionsPanel after
	// repaint. Scope: search within the renderSidebar body.
	rsIdx := strings.Index(js, "function renderSidebar(data)")
	if rsIdx < 0 {
		t.Fatal("dashboard.js missing renderSidebar(data) function — cannot verify panel refresh hook")
	}
	// Delimit the function body by the next "\nfunction " declaration rather
	// than a magic byte budget — the body has grown twice in as many rounds
	// (R110 home panel, multi-node filter) and each growth silently shifted
	// the renderRecentSessionsPanel() call out of a fixed window. Matches
	// the self-adjusting pattern used elsewhere in this file.
	nextFn := strings.Index(js[rsIdx+len("function renderSidebar(data)"):], "\nfunction ")
	var renderSidebarBody string
	if nextFn < 0 {
		renderSidebarBody = js[rsIdx:]
	} else {
		renderSidebarBody = js[rsIdx : rsIdx+len("function renderSidebar(data)")+nextFn]
	}
	if !strings.Contains(renderSidebarBody, "renderRecentSessionsPanel();") {
		t.Error("renderSidebar must call renderRecentSessionsPanel() after repaint so home panel mirrors the sidebar snapshot")
	}
}

// TestDashboardHTML_R110P1_HomePanelStyles pins the CSS hooks the home
// panel JS relies on. Matches the pattern of previous Round-specific
// style tests: if a future CSS cleanup renames any of these classes the
// panel would render unstyled (no border, no hover target, no layout).
func TestDashboardHTML_R110P1_HomePanelStyles(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	css := string(data)

	for _, rule := range []string{
		".recent-panel-wrap",
		".recent-panel",
		".recent-panel-title",
		".recent-panel-list",
		".recent-row",
		".recent-row:hover",
		".recent-dot",
		".recent-label",
		".recent-time",
	} {
		if !strings.Contains(css, rule) {
			t.Errorf("dashboard.html missing CSS rule %q used by R110-P1 home panel", rule)
		}
	}

	// The cold-start HTML must carry the same placeholder div as
	// mainEmptyHtml() so a fresh page load renders the panel the first
	// time fetchSessions lands (without waiting for a dismiss/repaint).
	if !strings.Contains(css, `<div id="recent-sessions-panel" class="recent-panel-wrap"></div>`) {
		t.Error("dashboard.html cold-start empty-state missing <div id=\"recent-sessions-panel\" class=\"recent-panel-wrap\"></div> — first load would skip the home panel until the next repaint")
	}

	// Reuse the existing status-dot tokens; tie the .recent-dot palette
	// to the same --nz-status-* vars so future palette shifts stay in
	// sync (cf. Round 126 status-dot palette tokenization).
	if ok, _ := regexp.MatchString(`\.recent-dot\.dot-running\{[^}]*--nz-status-running`, css); !ok {
		t.Error(".recent-dot.dot-running must use --nz-status-running so the home panel matches the sidebar's running-dot color")
	}
}

// TestDashboardHTML_R110P1_HdrBtnCoarsePointerSize pins Round 145: on
// touch devices, the three header icons (history / + / cron) live in a
// tight 390px-wide top-right cluster. The prior 40px target + 4px gap
// put tap centers only 44px apart, landing under the WCAG 2.5.5 AAA
// 44x44 recommendation and inside the "fuzzy" 8px inter-target zone
// that makes mis-taps feel random.
//
// Three structural invariants:
//
//  1. Inside the `(pointer: coarse)` block, `.hdr-btn` uses min-width
//     AND min-height of 44px (not 40). Using regex so a future
//     reformatter can rearrange rule order without breaking the test.
//  2. Inside the same block, `.hdr-btns` carries `gap:8px` so adjacent
//     44px tap targets have an 8px interstitial buffer (matches the
//     Material Touch Guidance "8dp minimum" spec).
//  3. Desktop (non-coarse) sizing is unchanged: the base `.hdr-btn`
//     rule outside the media query must still be 32x32, and the
//     `@media(max-width:480px)` override must still read 36x36 — mouse
//     users on narrow desktop windows keep the compact look. A test
//     here catches accidental "I'll just bump them all" refactors.
func TestDashboardHTML_R110P1_HdrBtnCoarsePointerSize(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	css := string(data)

	// Locate the `@media (pointer: coarse)` block and scan only within
	// its braces — otherwise a coincidental .hdr-btn{min-width:44} in
	// a different breakpoint could falsely satisfy the assertion.
	coarseIdx := strings.Index(css, "@media (pointer: coarse)")
	if coarseIdx < 0 {
		t.Fatal("dashboard.html missing `@media (pointer: coarse)` block")
	}
	// Take a bounded slice of the block — real block is ~30 lines, 2KB
	// is plenty and avoids matching the next media block's contents.
	blockEnd := coarseIdx + 2000
	if blockEnd > len(css) {
		blockEnd = len(css)
	}
	coarseBlock := css[coarseIdx:blockEnd]

	// Invariant 1: .hdr-btn uses min-width:44 + min-height:44 inside the
	// coarse block. Regex allows the declaration order to flip.
	hdrBtnRx := regexp.MustCompile(`\.hdr-btn\{[^}]*min-width:\s*44px[^}]*min-height:\s*44px`)
	if !hdrBtnRx.MatchString(coarseBlock) {
		t.Error("`.hdr-btn` inside `@media (pointer: coarse)` must declare min-width:44px AND min-height:44px — WCAG 2.5.5 AAA + Material Touch Guidance")
	}

	// Invariant 2: .hdr-btns gap:8px inside the same block.
	if !strings.Contains(coarseBlock, ".hdr-btns{gap:8px}") {
		t.Error("`.hdr-btns{gap:8px}` must be declared inside `@media (pointer: coarse)` so adjacent 44px tap targets have an 8px interstitial")
	}

	// Invariant 3: base + 480px desktop-narrow rules keep compact sizing.
	// Scope each check to the specific rule string.
	if !strings.Contains(css, `.hdr-btn{background:none;border:1px solid var(--nz-border);border-radius:6px;color:var(--nz-text-mute);cursor:pointer;padding:0;position:relative;transition:all .15s;line-height:1;white-space:nowrap;width:32px;height:32px`) {
		t.Error("base `.hdr-btn` rule must stay at width:32px;height:32px — mouse users keep the compact density")
	}
	if !strings.Contains(css, ".hdr-btn{width:36px;height:36px}") {
		t.Error("`@media(max-width:480px)` `.hdr-btn{width:36px;height:36px}` override must stay — narrow desktop windows (mouse) get 36, not 44")
	}
}

// TestDashboardJS_R110P2_CronRunNowButton pins the "Run Now" button added to
// the cron card action row. Four invariants together guarantee the feature
// cannot silently regress:
//
//  1. cronJobCardHtml emits a run button that calls cronTriggerNow, gated on
//     !j.paused so the backend's ErrJobPaused 409 arm doesn't become the
//     click's only feedback.
//  2. The button is appended to the existing .cc-actions row (not rendered
//     as a new row / floating FAB) — shares .cc-btn styling with edit/pause.
//  3. cronTriggerNow handler exists and POSTs to /api/cron/trigger with the
//     id in the JSON body, which mirrors cronPause/cronResume's payload
//     shape. Auth header uses the same Bearer token path as other cron
//     mutations.
//  4. Error-path delegation goes through the localized helpers (showAPIError
//     + showNetworkError) with the Chinese action label '立即执行定时任务',
//     so a future untranslated toast regresses this test.
func TestDashboardJS_R110P2_CronRunNowButton(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// Invariant 1: run button gated on !j.paused. We don't match the entire
	// conditional string (prettier may reflow quotes/whitespace); we verify
	// the gate + the handler it invokes together.
	if !strings.Contains(js, "const runBtn = j.paused") {
		t.Error("cronJobCardHtml must gate run button on j.paused — paused jobs can't be triggered, showing the button would mislead")
	}
	// The run-button inline onclick must call cronTriggerNow with the escJs'd
	// job id. Pin the call-site literal to catch refactors that rename the
	// handler or drop escJs (which would let a malicious id break out of
	// the onclick attr).
	if !strings.Contains(js, "cronTriggerNow(\\'' + escJs(j.id) + '\\')") {
		t.Error("cronJobCardHtml run button must invoke cronTriggerNow(<escJs id>) — renaming this handler breaks the contract")
	}

	// Invariant 2: run button is placed inside .cc-actions — grep for the
	// runBtn token being injected before the edit button in the actions row.
	// Order is a UX call (positive actions before edit) so we pin it to
	// prevent accidental reordering that drops the run button to the
	// destructive-actions neighborhood.
	ccActionsIdx := strings.Index(js, "'<div class=\"cc-actions\" onclick=\"event.stopPropagation()\">' +")
	if ccActionsIdx < 0 {
		t.Fatal("cronJobCardHtml missing .cc-actions wrapper — card structure changed unexpectedly")
	}
	actionsWindow := js[ccActionsIdx:min2(ccActionsIdx+400, len(js))]
	runBtnIdx := strings.Index(actionsWindow, "runBtn +")
	editBtnIdx := strings.Index(actionsWindow, "editCronJob(")
	if runBtnIdx < 0 {
		t.Error(".cc-actions must include `runBtn +` — run button dropped out of the action row")
	}
	if editBtnIdx < 0 {
		t.Fatal("cronJobCardHtml missing edit button — card action row changed unexpectedly")
	}
	if runBtnIdx >= 0 && editBtnIdx >= 0 && runBtnIdx > editBtnIdx {
		t.Error("run button must precede edit in .cc-actions (positive-actions-first convention)")
	}

	// Invariant 3: handler + endpoint + method + payload shape.
	if !strings.Contains(js, "async function cronTriggerNow(id)") {
		t.Fatal("dashboard.js missing cronTriggerNow(id) handler")
	}
	if !strings.Contains(js, "fetch('/api/cron/trigger', { method: 'POST'") {
		t.Error("cronTriggerNow must POST to /api/cron/trigger — matches the backend route in dashboard.go")
	}
	if !strings.Contains(js, "body: JSON.stringify({ id })") {
		t.Error("cronTriggerNow must send {id} JSON body — backend decodes into {ID string `json:\"id\"`}")
	}
	// Auth header must use the Bearer token the same way cronPause/Resume do.
	triggerIdx := strings.Index(js, "async function cronTriggerNow(id)")
	if triggerIdx >= 0 {
		body := js[triggerIdx:min2(triggerIdx+1200, len(js))]
		if !strings.Contains(body, "headers['Authorization'] = 'Bearer ' + t") {
			t.Error("cronTriggerNow must set Bearer auth header — mutating cron jobs is a privileged action")
		}
	}

	// Invariant 4: localized error surface. The raw 409 / 404 / 5xx error
	// body is intentionally NOT used in the toast (that's localizeAPIError's
	// job); we only need to confirm showAPIError is wired with the Chinese
	// action label so a future untranslated path trips this test.
	if !strings.Contains(js, "showAPIError('立即执行定时任务'") {
		t.Error("cronTriggerNow must route 4xx/5xx through showAPIError with Chinese action '立即执行定时任务'")
	}
	if !strings.Contains(js, "showNetworkError('立即执行定时任务'") {
		t.Error("cronTriggerNow must route network errors through showNetworkError with Chinese action '立即执行定时任务'")
	}
}

// TestDashboardJS_FetchEventsConcurrencyGuard pins the R166 fix that added an
// in-flight guard to fetchEvents. The 1 s setInterval driver plus the
// on-demand `full` fetch (session switch / WS fallback / reconnect) could
// otherwise stack multiple requests against /api/sessions/events while the
// network was slow; responses could arrive out of order and produce a
// reordered appendEvents. loadEarlierEvents already used a `_earlierLoading`
// module flag for the same reason — this test makes sure the tail-poll
// fetch grew a parallel `_fetchEventsInFlight` flag with the same shape,
// the stale-selection bail-out after await, and the finally-release
// contract.
func TestDashboardJS_FetchEventsConcurrencyGuard(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// 1) Module-scoped declaration, mirroring the existing _earlierLoading
	//    gate (both are let-initialised to false). Keeping them declared
	//    the same way prevents a future reviewer from accidentally
	//    promoting one to a const or dropping the initialiser.
	if !strings.Contains(js, "let _fetchEventsInFlight = false;") {
		t.Error("dashboard.js missing module-scoped `let _fetchEventsInFlight = false;` — fetchEvents concurrency guard requires a toggleable flag")
	}

	// 2) Locate fetchEvents' body once; subsequent assertions all scope to
	//    this function so we don't false-positive on a distant source line.
	startIdx := strings.Index(js, "async function fetchEvents(full) {")
	if startIdx < 0 {
		t.Fatal("dashboard.js missing `async function fetchEvents(full)` — structural anchor for the rest of the assertions")
	}
	// Scan forward until the matching close brace at the same depth. A
	// naive forward-scan for `\n}` works here because fetchEvents is a
	// top-level async function and the declaration is followed by the
	// canonical `\n}\n\n// loadEarlierEvents` separator. We cap at 8 KiB
	// so a future 10x growth of the function doesn't silently disable
	// any of the below asserts.
	endIdx := strings.Index(js[startIdx:], "\n}\n")
	if endIdx < 0 || endIdx > 8192 {
		t.Fatalf("could not locate fetchEvents end brace within 8 KiB — body scan failed (endIdx=%d)", endIdx)
	}
	body := js[startIdx : startIdx+endIdx+2]

	// 3) Early bail-out: fetchEvents must return when a prior invocation is
	//    still in flight. The precise string `if (_fetchEventsInFlight) return;`
	//    pins the gate so a refactor that inverts the flag or routes it
	//    through a helper will trip this test.
	if !strings.Contains(body, "if (_fetchEventsInFlight) return;") {
		t.Error("fetchEvents must early-return when _fetchEventsInFlight is true — guards against overlapping polls")
	}

	// 4) Dispatch-time capture of selectedKey/selectedNode. appendEvents
	//    on the wrong session is the concrete harm we're preventing, so
	//    the snapshot must exist before the awaited fetch().
	for _, needed := range []string{
		"const dispatchKey = selectedKey;",
		"const dispatchNode = selectedNode;",
	} {
		if !strings.Contains(body, needed) {
			t.Errorf("fetchEvents missing pre-await selection snapshot %q — stale fetches could graft onto the wrong session", needed)
		}
	}

	// 5) URL construction must use the snapshot, not the live `selectedKey`.
	//    If a refactor loses this invariant, a mid-flight switch would hit
	//    `/api/sessions/events?key=<new>` with the old `after` cursor,
	//    showing seemingly impossible backlog gaps.
	for _, needed := range []string{
		"encodeURIComponent(dispatchKey)",
		"encodeURIComponent(dispatchNode)",
	} {
		if !strings.Contains(body, needed) {
			t.Errorf("fetchEvents URL must encode the dispatch-time snapshot %q, not live selectedKey/selectedNode", needed)
		}
	}

	// 6) Stale-response bail-out after await. The snapshot captured in
	//    step 4 must be re-checked once the fetch resolves, because
	//    selectedKey could have flipped during the suspend.
	if !strings.Contains(body, "if (selectedKey !== dispatchKey || selectedNode !== dispatchNode) return;") {
		t.Error("fetchEvents must drop stale responses after await — prevents appendEvents from targeting the wrong session")
	}

	// 7) finally-block must always release the flag. Without finally,
	//    a throw from renderEvents / appendEvents (e.g. malformed event
	//    payload) would leave the flag stuck at true and silently kill
	//    future polls for the life of the page.
	if !strings.Contains(body, "_fetchEventsInFlight = false;") {
		t.Error("fetchEvents must reset _fetchEventsInFlight to false — otherwise one throw kills all subsequent polls")
	}
	if !strings.Contains(body, "} finally {") {
		t.Error("fetchEvents must release the guard inside a `finally` block — a plain try/catch would leak the gate on thrown errors")
	}
}

// TestDashboardJS_R167_AgentPicker locks the Round 167 R110-P3 agent-picker
// contract. The feature lets operators pick an agent (e.g. sonnet / haiku /
// a custom config.yaml entry) when creating a new dashboard session, and
// rewrites the key schema so the server-side buildSessionOpts correctly
// resolves AgentOpts from parts[3].
//
// Invariants:
//  1. renderAgentPicker helper exists, emits a <select id="new-agent"> when
//     availableAgents.length > 1, and collapses to an empty string otherwise.
//  2. getSelectedAgent reads the <select>, defaults to "general", and
//     persists the pick to localStorage under `naozhi_last_agent`.
//  3. Three session-creation entry points call renderAgentPicker: the
//     no-projects modal in createNewSession, the palette in
//     openProjectPalette, and the Custom Workspace modal in pickPaletteCustom.
//  4. buildDashboardSessionKey emits 4-segment keys with agentID as parts[3]
//     (not projectName), and sanitizeKeySlug normalizes project/folder names.
//  5. doCreateInProject and doCreateSession construct keys via the new
//     helper — the legacy inline `'dashboard:direct:' + ts + ':' + proj`
//     form must not resurface.
//  6. keyTailDisplay provides a safe fallback for the sidebar/main-header
//     displayName so the schema change doesn't regress the empty-fallback
//     UI to showing raw agentIDs ("general") as session labels.
func TestDashboardJS_R167_AgentPicker(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// Invariant 1: renderAgentPicker helper.
	for _, need := range []string{
		"function renderAgentPicker()",
		"availableAgents.length <= 1",
		`id="new-agent"`,
		`localStorage.getItem('naozhi_last_agent')`,
	} {
		if !strings.Contains(js, need) {
			t.Errorf("renderAgentPicker invariant missing: %q", need)
		}
	}

	// Invariant 2: getSelectedAgent helper.
	for _, need := range []string{
		"function getSelectedAgent()",
		`document.getElementById('new-agent')`,
		`localStorage.setItem('naozhi_last_agent', v)`,
		"return v || 'general';",
	} {
		if !strings.Contains(js, need) {
			t.Errorf("getSelectedAgent invariant missing: %q", need)
		}
	}

	// Invariant 3: three call sites. Tolerate whitespace shuffles by
	// counting occurrences rather than matching exact formatting.
	if c := strings.Count(js, "renderAgentPicker()"); c < 4 {
		// 1 definition + 3 call sites = 4 minimum occurrences.
		t.Errorf("renderAgentPicker() must have at least 3 call sites (found %d total incl. definition)", c)
	}

	// Invariant 4: key builder.
	for _, need := range []string{
		"function sanitizeKeySlug(s)",
		"function buildDashboardSessionKey(timestamp, projectOrFolder, agentID)",
		"'dashboard:direct:' + timestamp + '-' + slug + ':' + agent",
	} {
		if !strings.Contains(js, need) {
			t.Errorf("key builder invariant missing: %q", need)
		}
	}

	// Invariant 5: legacy inline key form must not resurface in the
	// creation helpers. The `+ projectName` / `+ folderName` tails are
	// the precise shape we migrated away from. Scratch sessions
	// (dashboard_session.go:860) use a different Go-side construction
	// that does not appear in dashboard.js, so no server-side regression.
	forbidden := []string{
		"'dashboard:direct:' + ts + ':' + projectName",
		"'dashboard:direct:' + ts + ':' + folderName",
	}
	for _, bad := range forbidden {
		if strings.Contains(js, bad) {
			t.Errorf("legacy inline key form resurfaced: %q — route through buildDashboardSessionKey instead", bad)
		}
	}

	// Both creation helpers must use the new helper.
	if c := strings.Count(js, "buildDashboardSessionKey("); c < 2 {
		t.Errorf("buildDashboardSessionKey must be called from both doCreateInProject and doCreateSession (found %d call sites)", c)
	}

	// Invariant 6: displayName fallback defers to keyTailDisplay rather
	// than the legacy `keyParts[keyParts.length - 1]` tail-read, which
	// under the new schema would surface agentIDs ("general") as the
	// visible session label.
	if !strings.Contains(js, "function keyTailDisplay(keyParts)") {
		t.Error("keyTailDisplay helper missing — displayName fallbacks would regress to raw agentID under Round 167 schema")
	}
	// renderMainShell + downloadSessionMarkdown both migrated.
	if c := strings.Count(js, "keyTailDisplay(keyParts)"); c < 2 {
		t.Errorf("keyTailDisplay must be used at both displayName fallback sites (renderMainShell + Markdown export) (found %d)", c)
	}
	// Regex extraction anchors the ts prefix (YYYY-MM-DD-…) so the
	// trailing slug survives intact. Prettier may escape the shape; use
	// the raw character class as the anchor.
	if !strings.Contains(js, `^\d{4}-\d{2}-\d{2}-\d+-\d+-`) {
		t.Error("keyTailDisplay must anchor chatID prefix to `^\\d{4}-\\d{2}-\\d{2}-\\d+-\\d+-` so trailing slug is returned in Round 167 keys")
	}
}

// TestDashboardJS_R167_PendingAgentFromKey locks that fetchSessions's pending
// session merge reads the agent off the key tail instead of hardcoding
// "general". Without this, every pending card would show the wrong agent
// chip regardless of the palette choice, breaking visual feedback on the
// agent picker.
func TestDashboardJS_R167_PendingAgentFromKey(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// Narrow the search window to fetchSessions's pending-merge block so
	// unrelated "agent: 'general'" occurrences elsewhere (e.g. scratch
	// state defaults) don't trigger false failures.
	start := strings.Index(js, "// Merge pending dashboard sessions into data")
	if start < 0 {
		t.Fatal("fetchSessions pending-merge block missing — anchor comment moved?")
	}
	end := start + 2000
	if end > len(js) {
		end = len(js)
	}
	window := js[start:end]

	if !strings.Contains(window, "const pendingAgent = parts.length >= 4 && parts[3] ? parts[3] : 'general';") {
		t.Error("pending-merge block must derive agent from the key tail (parts[3]) under the Round 167 schema")
	}
	// The legacy hardcoded 'general' in the same block is gone.
	if strings.Contains(window, "agent: 'general',") {
		t.Error("legacy hardcoded agent: 'general' in pending-merge block must be replaced by pendingAgent lookup")
	}
}

// TestDashboardJS_R110P1_LongPressContextMenu locks the MVP of the mobile
// long-press context menu. The hover-only ×/✎ session-card buttons are
// invisible on touch devices, so without a discoverable alternative,
// operators have no way to rename or delete a session on mobile other
// than the swipe-to-delete gesture (which also isn't obvious). This
// contract captures the shape of the long-press + right-click affordance
// so future refactors don't silently regress it.
//
// Invariants:
//  1. Long-press constants are declared at module scope with values that
//     match the design (500ms trigger, >=8px movement cancels). These
//     constants are the contract between swipe-to-delete and long-press
//     — swipe's own 5px tracking gate and the 8px long-press cancel gate
//     must stay in the right order so a deliberate swipe always wins over
//     an accidental long-press.
//  2. showSessionContextMenu + closeContextMenu + openSessionContextMenu
//     helpers exist so the menu has a single entry point reachable from
//     both touchstart's long-press path and the contextmenu right-click
//     path.
//  3. initSwipeDelete wires a 500ms setTimeout on touchstart, cancels
//     it on touchmove when movement passes the threshold, and also
//     cancels on touchend + touchcancel — otherwise the menu would fire
//     after the user has already lifted their finger or the browser has
//     reclaimed the gesture.
//  4. The click bubble-up handler swallows the post-long-press click so
//     selectSession does not also fire under the menu. Capture phase is
//     critical — the onclick attribute fires during bubble, so capture
//     is the only sane spot to stop it.
//  5. Menu items array shape (label / icon / action / danger) is stable
//     so future extensions do not accidentally rename keys. The three
//     MVP items (重命名 / 复制 key / 删除) are present with their
//     respective actions wired to renameSession / copyStringToClipboard
//     / dismissSession.
func TestDashboardJS_R110P1_LongPressContextMenu(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// Invariant 1: module-level constants with correct values. 500ms
	// lines up with Android/iOS native defaults; 8px > swipe's 5px so
	// a deliberate swipe always kills long-press before it fires.
	if !strings.Contains(js, "const LONG_PRESS_MS = 500;") {
		t.Error("LONG_PRESS_MS must be 500ms (matches native long-press conventions)")
	}
	if !strings.Contains(js, "const LONG_PRESS_MOVE_CANCEL_PX = 8;") {
		t.Error("LONG_PRESS_MOVE_CANCEL_PX must be 8 so swipe's 5px gate doesn't accidentally cancel long-press")
	}
	if !strings.Contains(js, "let _longPressTimer = null;") {
		t.Error("_longPressTimer must be declared at module scope so touchstart/move/end share one handle")
	}
	if !strings.Contains(js, "let _longPressFired = false;") {
		t.Error("_longPressFired module flag must exist so the click-capture handler can distinguish post-long-press clicks")
	}

	// Invariant 2: three helpers exist so the menu has one entry
	// point shared between long-press and right-click.
	for _, need := range []string{
		"function closeContextMenu()",
		"function showSessionContextMenu(x, y, items)",
		"function openSessionContextMenu(card, x, y)",
		"function copyStringToClipboard(s)",
	} {
		if !strings.Contains(js, need) {
			t.Errorf("helper missing: %q", need)
		}
	}

	// Invariant 3: initSwipeDelete wires the timer + cancel paths.
	// Search within initSwipeDelete's body so other long-press uses
	// (if any added later) don't pollute the signal.
	idxInit := strings.Index(js, "function initSwipeDelete() {")
	if idxInit < 0 {
		t.Fatal("initSwipeDelete function missing")
	}
	// Conservative window — function body is ~80 lines including
	// long-press additions; 6KiB covers it with margin.
	end := idxInit + 6000
	if end > len(js) {
		end = len(js)
	}
	body := js[idxInit:end]

	if !strings.Contains(body, "_longPressTimer = setTimeout(") {
		t.Error("initSwipeDelete must schedule the long-press via setTimeout on touchstart")
	}
	if !strings.Contains(body, "LONG_PRESS_MS") {
		t.Error("setTimeout must reference LONG_PRESS_MS (not a magic number literal)")
	}
	if !strings.Contains(body, "LONG_PRESS_MOVE_CANCEL_PX") {
		t.Error("touchmove must reference LONG_PRESS_MOVE_CANCEL_PX to cancel long-press on directional intent")
	}
	if !strings.Contains(body, "list.addEventListener('touchcancel'") {
		t.Error("touchcancel handler missing — an interrupted gesture would leak _longPressTimer without it")
	}
	if !strings.Contains(body, "openSessionContextMenu(target, x, y);") {
		t.Error("long-press timer callback must invoke openSessionContextMenu")
	}
	// Cancel path must live in a reusable helper so every exit point
	// (timer fire / touchend / touchmove over threshold / touchcancel)
	// routes through the same cleanup.
	if !strings.Contains(body, "const cancelLongPress = () =>") {
		t.Error("cancelLongPress closure must exist so all exit paths share one cleanup routine")
	}

	// Invariant 4: capture-phase click handler swallows the post-long-press click.
	if !strings.Contains(body, "list.addEventListener('click', e => {") {
		t.Error("initSwipeDelete must attach a click listener to catch post-long-press bubble")
	}
	if !strings.Contains(body, "if (_longPressFired) {") {
		t.Error("click handler must gate on _longPressFired so short taps still reach selectSession")
	}
	// The third argument `true` = capture phase — required because
	// the onclick attribute on .session-card fires during bubble.
	if !strings.Contains(body, "e.stopPropagation();") {
		t.Error("post-long-press click must call stopPropagation to keep selectSession from firing")
	}
	// contextmenu for desktop right-click parity.
	if !strings.Contains(body, "list.addEventListener('contextmenu'") {
		t.Error("initSwipeDelete must also wire a contextmenu listener for desktop right-click")
	}

	// Invariant 5: menu item shape + Chinese labels for the three MVP actions.
	idxOpen := strings.Index(js, "function openSessionContextMenu(card, x, y)")
	if idxOpen < 0 {
		t.Fatal("openSessionContextMenu function missing")
	}
	endOpen := idxOpen + 4000
	if endOpen > len(js) {
		endOpen = len(js)
	}
	openBody := js[idxOpen:endOpen]
	for _, need := range []string{
		"label: '重命名'",
		"label: '复制 key'",
		"label: '删除'",
		"danger: true,",
		"renameSession();",
		"copyStringToClipboard(key)",
		"dismissSession(key, node);",
	} {
		// Labels may be prettier-escaped to \uXXXX in the final bundle;
		// Chinese labels embedded in a string literal stay raw UTF-8 in
		// modern Go embed, so the raw form is what lands in the embedded
		// file content. Keep the assertion simple.
		if !strings.Contains(openBody, need) {
			t.Errorf("openSessionContextMenu body missing item or action: %q", need)
		}
	}
}

// TestDashboardHTML_R110P1_ContextMenuStyles locks the CSS hooks that the JS
// side relies on for the long-press context menu. Missing any of these
// would leave the menu unstyled (unreadable text over transparent
// background) or unclickable (wrong z-index stacking under modal-overlay).
func TestDashboardHTML_R110P1_ContextMenuStyles(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)

	// All six CSS selectors are emitted by showSessionContextMenu;
	// absence of any single one would cause a visual regression.
	for _, need := range []string{
		".ctx-menu-overlay{",
		".ctx-menu{",
		".ctx-menu-item{",
		".ctx-menu-item.danger{",
		".session-card.long-pressing{",
	} {
		if !strings.Contains(html, need) {
			t.Errorf("CSS hook missing: %q", need)
		}
	}

	// z-index contract: menu (211) above overlay (210), both above
	// .modal-overlay (200). If the menu drops below modal-overlay a
	// stray confirmDialog would hide it.
	if !strings.Contains(html, ".ctx-menu-overlay{position:fixed;top:0;left:0;right:0;bottom:0;background:transparent;z-index:210}") {
		t.Error(".ctx-menu-overlay must have z-index:210 to sit above modal-overlay (z-index:200)")
	}
	if !strings.Contains(html, "z-index:211") {
		t.Error(".ctx-menu must have z-index:211 so it renders above its overlay")
	}

	// 44px min-height on items for WCAG 2.5.5 AAA touch target size —
	// same contract we enforced on .hdr-btn in the coarse pointer rule.
	if !strings.Contains(html, "min-height:44px") {
		t.Error(".ctx-menu-item min-height must remain 44px for touch target size parity with .hdr-btn")
	}
}

// TestDashboardJS_PasteImageRoutesToUpload pins the paste-handler image
// branch. Background: when the user pastes a screenshot (Cmd/Ctrl+V,
// or "copy image" from another app) into #msg-input, the clipboard
// typically carries ONLY an image File — no text/plain. The legacy
// paste handler short-circuited on `if (!text) return;` so:
//
//  1. the image never reached handleFiles / pendingFiles,
//  2. the browser's default paste embedded the image as <img src="data:...">
//     inside the contenteditable,
//  3. getMsgValue(input).trim() (innerText-based) returned ” for the
//     image-only paste, so sendMessage dispatched neither text nor
//     file_ids and Claude never saw the image.
//
// The fix routes image clipboard payloads to handleFiles BEFORE the text
// branch, mirroring the paperclip / drag-drop paths. This test pins the
// contract so a future "simplify paste handler" refactor can't silently
// reintroduce the silent-drop regression.
func TestDashboardJS_PasteImageRoutesToUpload(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// The image short-circuit must precede the text-plain branch — otherwise
	// a paste with BOTH text and image (rare but valid: "copy" from a rich
	// editor) would take the text path and silently drop the image.
	imgIdx := strings.Index(js, "imageFiles.length > 0")
	textIdx := strings.Index(js, "const text = cd.getData('text/plain');")
	if imgIdx < 0 {
		t.Fatal("paste handler missing imageFiles branch — image clipboard payloads would be dropped silently")
	}
	if textIdx < 0 {
		t.Fatal("paste handler missing text/plain branch — regression check cannot compare ordering")
	}
	if imgIdx > textIdx {
		t.Error("paste handler must check imageFiles BEFORE text/plain; reversing the order lets an image+text paste fall into the text branch and lose the image")
	}

	// Image branch must call handleFiles (the same entry point as the
	// paperclip button + drag-drop) so pendingFiles / upload / file_ids
	// all stay on one code path. Bypassing handleFiles would skip the
	// 40 MB cap, MIME filter, normalizeImage re-encode, and the 10-file
	// ceiling — all of which are enforced inside handleFiles.
	if !strings.Contains(js, "handleFiles(imageFiles)") {
		t.Error("paste image branch must route through handleFiles so pendingFiles / upload / file_ids stays on one code path")
	}

	// preventDefault on the image branch is mandatory: without it the
	// browser's default paste would also embed <img src=\"data:...\"> into
	// the contenteditable, producing a duplicate render (one in the input
	// preview row, another as ghost content the user can't easily remove).
	pasteIdx := strings.Index(js, "document.addEventListener('paste'")
	if pasteIdx < 0 {
		t.Fatal("paste handler not registered on document — image-paste bug cannot be fixed without this listener")
	}
	// Search within a bounded window to keep the assertion local.
	window := js[pasteIdx:min2(pasteIdx+2000, len(js))]
	if !strings.Contains(window, "e.preventDefault();\n    if (typeof handleFiles") {
		// Looser check: just ensure preventDefault appears before the
		// handleFiles call so the default embed is suppressed.
		pdIdx := strings.Index(window, "e.preventDefault()")
		hfIdx := strings.Index(window, "handleFiles(imageFiles)")
		if pdIdx < 0 || hfIdx < 0 || pdIdx > hfIdx {
			t.Error("image paste branch must call e.preventDefault() BEFORE handleFiles so browser doesn't also embed <img> into contenteditable")
		}
	}

	// Both cd.files and cd.items paths must be present: Chromium/Safari
	// expose the image via cd.files, but some legacy Firefox versions
	// surface it only via cd.items[i].getAsFile(). Supporting both keeps
	// the fix cross-browser without a user-agent sniff.
	if !strings.Contains(js, "cd.files") {
		t.Error("paste handler must check cd.files for images — Chromium/Safari deliver screenshot paste here")
	}
	if !strings.Contains(js, "cd.items") || !strings.Contains(js, "getAsFile()") {
		t.Error("paste handler must fall back to cd.items.getAsFile() — older Firefox paths only expose images there")
	}
}
