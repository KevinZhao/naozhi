// static_events_panel_p2_contract_test.go — wiring pins for the #2430 P2 fixes
// on the session events panel. dashboard.js has no JS unit runner, so these
// source-greps lock the shape the Playwright regressions
// (test/e2e/events_panel_p2.test.js) exercise:
//
//  1. downloadSessionMarkdown pages the whole history through the `before=`
//     cursor (fetchAllSessionEvents) instead of one bare
//     `/api/sessions/events?key=` call that only returns the in-memory ring
//     (≤500). A bounded pager + a truncation warning in the toast.
//  2. fetchEvents(full) is never swallowed by the tail-poll in-flight gate; a
//     generation counter drops the stale tail instead so a WS-down session
//     switch still renders the paged first page (and mounts "load earlier").
//  3. AskUserQuestion cards lock when a `user` event arrives incrementally
//     (WS onEvent / onHistory backfill / poll appendEvents) and the poll full
//     path (renderEvents) hydrates the answered-set like onHistory does.
package server

import (
	"strings"
	"testing"
)

// jsMethodBody slices a `  name(args) {` object-literal method (wsm.onEvent
// style) up to its two-space-indented closing `},`. jsFuncBody only handles
// `function name(` declarations.
func jsMethodBody(t *testing.T, js, name string) string {
	t.Helper()
	decl := "\n  " + name + "(msg) {"
	start := strings.Index(js, decl)
	if start < 0 {
		t.Fatalf("could not find method %q in dashboard.js", name)
	}
	end := strings.Index(js[start:], "\n  },")
	if end < 0 {
		t.Fatalf("could not bound %s body", name)
	}
	return js[start : start+end]
}

func TestDashboardJS_ExportPagesFullHistory(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)

	dl := jsFuncBody(t, js, "downloadSessionMarkdown")
	if !strings.Contains(dl, "fetchAllSessionEvents(") {
		t.Error("downloadSessionMarkdown must fetch through fetchAllSessionEvents (before= pager), not one bare ring read")
	}
	if strings.Contains(dl, "const r = await fetch(url, { headers });") {
		t.Error("downloadSessionMarkdown still issues the single un-paged /api/sessions/events fetch (ring-only, ≤500 events)")
	}
	// Truncation must be surfaced, never silent.
	if !strings.Contains(dl, "truncated") || !strings.Contains(dl, "已截断") {
		t.Error("downloadSessionMarkdown must warn in the toast when the export was truncated")
	}
	// Re-entrancy guard: a double click must not launch two pagers.
	if !strings.Contains(dl, "_exportInFlight") {
		t.Error("downloadSessionMarkdown must be guarded by _exportInFlight")
	}

	pager := jsFuncBody(t, js, "fetchAllSessionEvents")
	for _, want := range []string{
		// Re-admit the watermark ms: ring + disk sources are strictly `< before`,
		// so `before = oldest` would lose same-ms siblings split by a page edge.
		"'&before=' + (oldest + 1) + '&limit=' + EXPORT_PAGE_LIMIT",
		"const seen = new Set(events.map(exportEventKey));", // dedup across overlapping pages
		"if (fresh.length === 0) {",                         // progress = new entries after dedup
		"EXPORT_MAX_PAGES",                                  // hard upper bound
		"truncated = true",                                  // cap / malformed / stalled full page flag truncation
		"if (!Array.isArray(page)) { truncated = true; break; }",
		// Remote relay ignores before/limit → ring-only; a ring-sized slice is
		// probably incomplete and must not toast as a full export.
		"if (remote) return { events, truncated: events.length >= EXPORT_PAGE_LIMIT };",
	} {
		if !strings.Contains(pager, want) {
			t.Errorf("fetchAllSessionEvents missing %q", want)
		}
	}
	if strings.Contains(pager, "'&before=' + oldest + '&limit='") {
		t.Error("fetchAllSessionEvents still uses the strict `before = oldest` cursor — same-ms siblings at a page edge are lost")
	}
	// Untimed head: concat first, then stop (never drop the page).
	if !strings.Contains(pager, "events = fresh.concat(events);\n    const pageOldest = (fresh[0] && fresh[0].time) || 0;\n    if (!pageOldest) break;") {
		t.Error("fetchAllSessionEvents must concat the page before breaking on an untimed head")
	}
	keyFn := jsFuncBody(t, js, "exportEventKey")
	if !strings.Contains(keyFn, "e.uuid") || !strings.Contains(keyFn, "e.detail") {
		t.Error("exportEventKey must key on uuid, falling back to (time,type,detail)")
	}
	if !strings.Contains(js, "const EXPORT_PAGE_LIMIT = 500;") {
		t.Error("EXPORT_PAGE_LIMIT must equal the server's maxEventsPageLimit (500)")
	}
}

func TestDashboardJS_FullFetchNotSwallowedByInFlightGate(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)

	fe := jsFuncBody(t, js, "fetchEvents")
	if strings.Contains(fe, "\n  if (_fetchEventsInFlight) return;") {
		t.Error("fetchEvents still gates `full` fetches on _fetchEventsInFlight — a WS-down session switch during a slow tail poll drops the paged first page")
	}
	if !strings.Contains(fe, "if (!full && _fetchEventsInFlight) return;") {
		t.Error("fetchEvents must only coalesce tail polls (`!full`) behind the in-flight flag")
	}
	// Generation guard: a full fetch invalidates whatever tail is still in
	// flight so the stale response can neither append nor release the flag.
	for _, want := range []string{
		"if (full) _fetchEventsGen++;",
		"const gen = _fetchEventsGen;",
		"gen !== _fetchEventsGen",
		"if (gen === _fetchEventsGen) _fetchEventsInFlight = false;",
	} {
		if !strings.Contains(fe, want) {
			t.Errorf("fetchEvents missing generation guard piece %q", want)
		}
	}
	if !strings.Contains(js, "let _fetchEventsGen = 0;") {
		t.Error("_fetchEventsGen declaration missing")
	}
}

func TestDashboardJS_AskCardLocksOnIncrementalUserEvent(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)

	lock := jsFuncBody(t, js, "lockRenderedAskCards")
	for _, want := range []string{
		".event.ask_question[data-tool-use-id]",
		"_askAnswered.add(tuid);",
		"b.disabled = true;",
		"'ask-status'",
		"indexOf('发送失败') === 0", // stale failure copy is overwritten with 已回答
	} {
		if !strings.Contains(lock, want) {
			t.Errorf("lockRenderedAskCards missing %q", want)
		}
	}

	// WS push path.
	if !strings.Contains(jsMethodBody(t, js, "onEvent"), "lockRenderedAskCards(") {
		t.Error("wsm.onEvent must lock rendered AskUserQuestion cards when a `user` event lands")
	}
	// WS backfill (non-initial history frame) path.
	if !strings.Contains(jsMethodBody(t, js, "onHistory"), "lockRenderedAskCards(el);") {
		t.Error("wsm.onHistory incremental branch must lock rendered AskUserQuestion cards on a `user` event")
	}
	// Poll fallback paths.
	if !strings.Contains(jsFuncBody(t, js, "appendEvents"), "lockRenderedAskCards(el);") {
		t.Error("appendEvents must lock rendered AskUserQuestion cards when a `user` event lands")
	}
	app := jsFuncBody(t, js, "appendEvents")
	if !strings.Contains(app, "hydrateAskAnsweredFromHistory(events);") {
		t.Error("appendEvents must hydrate the answered-set for ask→user pairs inside one poll batch")
	}
	if !strings.Contains(jsFuncBody(t, js, "renderEvents"), "hydrateAskAnsweredFromHistory(events);") {
		t.Error("renderEvents (poll full path) must hydrate the answered-set like onHistory")
	}
}
