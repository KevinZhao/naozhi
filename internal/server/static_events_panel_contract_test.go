// static_events_panel_contract_test.go — wiring pins for the session events
// panel fixes (rename wipe / same-ms watermark / stale page-back / stale
// header clears). dashboard.js has no JS unit runner, so these Go source-greps
// lock the shape the Playwright regressions (test/e2e/events_panel.test.js)
// exercise, and cover the two race fixes e2e cannot time deterministically.
//
//  1. renameSession repaints only the header (renderMainHeader) — a full
//     renderMainShell rebuilds #events-scroll empty and nothing on the rename
//     path refetches history.
//  2. onHistory / appendEvents gate on strict `<` and dedup same-ms events by
//     uuid: eventHtml(thinking) renders nothing yet advanced the cursor, so a
//     text block sharing the millisecond was swallowed by `<=`.
//  3. loadEarlierEvents stale-checks after every await and selectSession resets
//     its in-flight flag, so a page of session A never prepends into session B.
//  4. fetchSessionRuns / fetchGitState error branches stale-check BEFORE
//     clearing the header mounts that now belong to a different session.
package server

import (
	"strings"
	"testing"
)

// Function bodies are sliced with jsFuncBody (dashboard_sidebar_autocollapse_test.go).

func TestDashboardJS_RenameRepaintsHeaderOnly(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)

	for _, want := range []string{
		`function mainHeaderHtml(s) {`,
		`function renderMainHeader() {`,
		// Both shells must be built from the shared header builder.
		"  main.innerHTML =\n    mainHeaderHtml(s) +",
		`header.outerHTML = mainHeaderHtml(s);`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("dashboard.js missing header-only repaint wiring: %q", want)
		}
	}

	rename := jsFuncBody(t, js, "renameSession")
	if rename == "" {
		t.Fatal("renameSession not found")
	}
	if !strings.Contains(rename, "renderMainHeader();") {
		t.Error("renameSession must repaint via renderMainHeader()")
	}
	if strings.Contains(rename, "renderMainShell()") {
		t.Error("renameSession must NOT call renderMainShell() — it rebuilds #events-scroll empty and the conversation is not refetched")
	}

	// The header-only path must repaint the mounts the rebuild emptied, exactly
	// like renderMainShell's tail (git chip from cache, effort tag, run stats).
	hdr := jsFuncBody(t, js, "renderMainHeader")
	for _, want := range []string{"repaintGitChip();", "setHeaderEffortChip();", "fetchSessionRuns(selectedKey, selectedNode);"} {
		if !strings.Contains(hdr, want) {
			t.Errorf("renderMainHeader must call %q after replacing the header", want)
		}
	}
	// Fallback to a full rebuild when no shell is mounted.
	if !strings.Contains(hdr, "{ renderMainShell(); return; }") {
		t.Error("renderMainHeader must fall back to renderMainShell() when no header/scroller is mounted")
	}
}

func TestDashboardJS_SameMsEventsNotDroppedByWatermark(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)

	// The old inclusive gate must be gone from both render loops.
	if n := strings.Count(js, "if (e.time && e.time <= lastRenderedEventTime) return;"); n != 0 {
		t.Errorf("inclusive watermark gate `e.time <= lastRenderedEventTime` still present %d time(s) — same-ms thinking→text loses the text bubble", n)
	}
	// Strict gate + same-ms uuid dedup, once in onHistory and once in appendEvents.
	if n := strings.Count(js, "if (e.time && e.time < lastRenderedEventTime) return;"); n != 2 {
		t.Errorf("strict watermark gate must appear exactly twice (onHistory + appendEvents), got %d", n)
	}
	if n := strings.Count(js, "if (e.time && e.time === lastRenderedEventTime && eventAlreadyRendered(el, e.uuid)) return;"); n != 2 {
		t.Errorf("same-ms uuid dedup must appear exactly twice (onHistory + appendEvents), got %d", n)
	}

	// Cron live buffer: same treatment against the in-memory array.
	if !strings.Contains(js, "(e.time === lastTime && !(e.uuid && seen.has(e.uuid))));") {
		t.Error("onCronLiveHistory must admit same-ms events and dedup them by uuid against cronLive.events")
	}
	if strings.Contains(js, "if (ev.time && ev.time <= this.cronLive.lastEventTimeMs) return;") {
		t.Error("onCronLiveEvent still drops same-ms events on time alone")
	}
	if !strings.Contains(js, "if (ev.time && ev.time < this.cronLive.lastEventTimeMs) return;") {
		t.Error("onCronLiveEvent must gate on strict `<`")
	}

	// thinking must still render nothing (the dedup fix must not "solve" the
	// bug by painting thinking bubbles instead).
	if !strings.Contains(js, "if (e && e.type === 'thinking') return '';") {
		t.Error("eventHtml must keep returning '' for thinking events")
	}
}

func TestDashboardJS_LoadEarlierStaleGuard(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)

	body := jsFuncBody(t, js, "loadEarlierEvents")
	if body == "" {
		t.Fatal("loadEarlierEvents not found")
	}
	for _, want := range []string{
		"const key = selectedKey;",
		"const node = selectedNode;",
		"const gen = _earlierGen;",
		"const stale = () => selectedKey !== key || selectedNode !== node || gen !== _earlierGen;",
		// The URL must be built from the captured identity, not the live globals.
		"encodeURIComponent(key) +",
		// finally keys on the generation only: selectedKey=null paths (dismiss /
		// pending create) never reset the flag, so a full stale() would stick it.
		"if (gen === _earlierGen) _earlierLoading = false;",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("loadEarlierEvents missing stale guard piece: %q", want)
		}
	}
	// Two stale checks (after fetch, after json) must precede prependEvents.
	prepend := strings.Index(body, "prependEvents(events);")
	if prepend < 0 {
		t.Fatal("loadEarlierEvents must still call prependEvents(events)")
	}
	if n := strings.Count(body[:prepend], "if (stale()) return;"); n < 2 {
		t.Errorf("loadEarlierEvents needs a stale check after each await before prependEvents, found %d", n)
	}
	if strings.Contains(body, "encodeURIComponent(selectedKey)") {
		t.Error("loadEarlierEvents must not read the live selectedKey after capture")
	}
	if strings.Contains(body, "if (!stale()) _earlierLoading = false;") {
		t.Error("loadEarlierEvents finally must not gate the flag release on stale() — selectedKey=null switch paths never reset it")
	}

	// Mobile long-press rename must go through selectSession so renderMainHeader
	// repaints the shell that actually belongs to the renamed session.
	if strings.Contains(js, "        selectedKey = key;\n        selectedNode = node;\n        renameSession();") {
		t.Error("long-press rename must not flip selectedKey/selectedNode directly before renameSession()")
	}
	if !strings.Contains(js, "        selectSession(key, node);\n        renameSession();") {
		t.Error("long-press rename must call selectSession(key, node) before renameSession()")
	}

	// selectSession resets the flag + bumps the generation, right after the
	// cursor resets it already performs.
	if !strings.Contains(js, "_autoPageBackCount = 0; // reset the blank-page recovery budget per session\n  // Invalidate any in-flight \"load earlier\" page of the previous session and\n  // free the flag so the new session can page back immediately.\n  _earlierGen++;\n  _earlierLoading = false;\n") {
		t.Error("selectSession must bump _earlierGen and reset _earlierLoading next to the cursor resets")
	}
	if !strings.Contains(js, "let _earlierGen = 0;") {
		t.Error("_earlierGen must be declared at module scope")
	}
}

func TestDashboardJS_HeaderFetchErrorPathsStaleChecked(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)

	runs := jsFuncBody(t, js, "fetchSessionRuns")
	if runs == "" {
		t.Fatal("fetchSessionRuns not found")
	}
	if !strings.Contains(runs, "if (!resp.ok) { if (selectedKey !== key) return; panel.hidden = true; setHeaderRunStats(''); return; }") {
		t.Error("fetchSessionRuns !resp.ok branch must stale-check selectedKey before clearing #header-runstats")
	}
	if !strings.Contains(runs, "} catch (_) {\n    if (selectedKey !== key) return;\n    panel.hidden = true;\n    setHeaderRunStats('');") {
		t.Error("fetchSessionRuns catch branch must stale-check selectedKey before clearing #header-runstats")
	}

	git := jsFuncBody(t, js, "fetchGitState")
	if git == "" {
		t.Fatal("fetchGitState not found")
	}
	if !strings.Contains(git, "if (!resp.ok) { delete gitStateCache[cacheKey]; if (selectedKey !== key || selectedNode !== node) return; setHeaderGitChip(''); return; }") {
		t.Error("fetchGitState !resp.ok branch must stale-check key+node before clearing #header-git (cache delete stays unconditional)")
	}
	if !strings.Contains(git, "} catch (_) {\n    delete gitStateCache[cacheKey];\n    if (selectedKey !== key || selectedNode !== node) return;\n    setHeaderGitChip('');") {
		t.Error("fetchGitState catch branch must stale-check key+node before clearing #header-git")
	}
}
