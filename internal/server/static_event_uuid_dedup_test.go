package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_EventHtmlEmitsDataUUID pins that eventHtml stamps the
// backend's authoritative entry uuid onto the rendered .event element as
// data-uuid. This attribute is the idempotency key that stops a post-restart
// re-subscribe history replay from painting the same user message twice
// (the two-identical-bubbles bug). See
// docs/rfc/dashboard-event-uuid-idempotent-render.md.
func TestDashboardJS_EventHtmlEmitsDataUUID(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)

	// The attribute must be derived from e.uuid and escaped (escAttr), and
	// omitted when absent so uuid-less CLI-synthesised events stay
	// distinguishable rather than colliding on data-uuid="".
	if !strings.Contains(js, "const uuidAttr = e.uuid ? ' data-uuid=\"' + escAttr(e.uuid) + '\"' : '';") {
		t.Error("eventHtml must build uuidAttr from e.uuid via escAttr, omitting the attribute when uuid is empty")
	}
	// The main .event return must include uuidAttr alongside timeAttr.
	if !strings.Contains(js, "esc(e.type||'') + '\"' + timeAttr + uuidAttr + '>'") {
		t.Error("eventHtml's main return must append uuidAttr to the .event element so user bubbles carry data-uuid")
	}
}

// TestDashboardJS_EventAlreadyRenderedHelper pins the DOM-as-source-of-truth
// dedup helper. Critically it must NOT introduce a parallel JS Set/Map —
// querying the live DOM keeps the dedup set automatically consistent with
// trimEventsScroll() eviction and full innerHTML rebuilds. A second source of
// truth is the exact class of bug this fix removes.
func TestDashboardJS_EventAlreadyRenderedHelper(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)

	if !strings.Contains(js, "function eventAlreadyRendered(scrollEl, uuid) {") {
		t.Fatal("eventAlreadyRendered(scrollEl, uuid) helper must exist")
	}
	// Empty/absent uuid must never match (would otherwise swallow uuid-less events).
	if !strings.Contains(js, "if (!scrollEl || !uuid) return false;") {
		t.Error("eventAlreadyRendered must return false for empty/absent uuid so uuid-less events are never deduped")
	}
	// Selector must query the DOM by data-uuid (single source of truth).
	if !strings.Contains(js, "scrollEl.querySelector('.event[data-uuid=\"' + sel + '\"]')") {
		t.Error("eventAlreadyRendered must query the live DOM by .event[data-uuid=...] — no parallel JS Set")
	}
	// CSS.escape guards the attribute selector boundary.
	if !strings.Contains(js, "CSS.escape") {
		t.Error("eventAlreadyRendered must guard the selector with CSS.escape")
	}
}

// TestDashboardJS_UserBubbleDedupScopedToUser pins that uuid dedup is applied
// ONLY to user events, never to streaming text. Measured: a single session
// log had 586 text uuids that re-emit (same uuid, multiple times) — a generic
// "skip if seen" would freeze streaming output. The bug only manifests on user
// bubbles, which are immutable, so the dedup is deliberately user-scoped.
func TestDashboardJS_UserBubbleDedupScopedToUser(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)

	// onEvent: the uuid dedup must sit inside the isUser branch.
	idxIsUser := strings.Index(js, "const isUser = ev.type === 'user';")
	if idxIsUser < 0 {
		t.Fatal("onEvent must keep the isUser gate")
	}
	idxDedup := strings.Index(js[idxIsUser:], "if (eventAlreadyRendered(el, ev.uuid)) {")
	if idxDedup < 0 {
		t.Error("onEvent must dedup user events on ev.uuid inside the isUser branch")
	}
	// onHistory incremental: dedup must sit inside the e.type === 'user' branch.
	if !strings.Contains(js, "if (eventAlreadyRendered(el, e.uuid)) {") {
		t.Error("onHistory incremental loop must dedup user events on e.uuid as the time-cursor backstop")
	}

	// Guard against regression to a type-blind dedup: there must be no
	// uuid-dedup call that is NOT guarded by a user-type check. We approximate
	// this by asserting the comment that records the user-only invariant
	// survives — a reviewer removing the scope would have to remove this too.
	if !strings.Contains(js, "Scope is user-only by design") {
		t.Error("the user-only dedup scope rationale must remain so a future edit doesn't extend uuid-skip to streaming text")
	}
}

// TestDashboardJS_OnEventAdvancesCursorOnDedup pins the secondary fix: when
// onEvent renders a real user event it previously did NOT advance
// lastRenderedEventTime, so the time-gated onHistory path could not tell the
// event had already been shown. On a dedup hit (and on first render) the
// cursor must advance so both paths agree.
func TestDashboardJS_OnEventAdvancesCursorOnDedup(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)

	// Inside the onEvent dedup-hit branch, the cursor must advance before bail.
	idx := strings.Index(js, "if (eventAlreadyRendered(el, ev.uuid)) {")
	if idx < 0 {
		t.Fatal("onEvent uuid dedup branch missing")
	}
	window := js[idx:]
	end := strings.Index(window, "return;")
	if end < 0 {
		t.Fatal("onEvent dedup branch must bail with return")
	}
	if !strings.Contains(window[:end], "if (t && t > lastRenderedEventTime) lastRenderedEventTime = t;") {
		t.Error("onEvent dedup-hit branch must advance lastRenderedEventTime before returning")
	}
}

func readDashboardJS(t *testing.T) string {
	t.Helper()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	return string(data)
}
