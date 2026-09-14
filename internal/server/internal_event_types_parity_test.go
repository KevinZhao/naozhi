package server

import (
	"regexp"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// internalSetRe extracts the element list from
//
//	const INTERNAL_EVENT_TYPES = new Set(['tool_use','result',...]);
//
// in dashboard.js so the test can compare it element-by-element against the
// server-side clievent.IsInternalEventType predicate.
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

// This file reads dashboard.js, and deliberately keeps doing so: a JS Set has no
// enumerator reachable from Go, and #2547 lists exactly this kind of cheap
// cross-language reconciliation among the tests worth keeping. What it does NOT
// do is assert on the file's prose or on how the code is spelled — it parses out
// the Set's elements and compares them, member by member, against a Go
// predicate.
//
// The auto-page-back half of this file moved to test/e2e/auto_pageback.test.js:
// it pinned six substrings, one of which was a comment, and two of which the
// probes recorded there show it cannot distinguish (#2547).
//
// TestInternalEventTypes_JSGoParity is the load-bearing guard for the
// visible-aware history fix. The server's EventLastNVisibleCtx counts entries
// clievent.IsInternalEventType reports false for; the dashboard hides exactly the
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
		if !clievent.IsInternalEventType(ty) {
			t.Errorf("dashboard.js hides %q but clievent.IsInternalEventType(%q)=false — the server would count it as a visible bubble and mis-size the initial page", ty, ty)
		}
	}

	// Direction 2: every Go-internal type must be hidden by the JS set. We
	// don't have a public enumerator for the Go map, so probe the known
	// universe of types the dashboard renders plus the internal ones; any
	// type the Go side calls internal but JS doesn't hide is a drift.
	for _, ty := range allKnownEventTypes {
		if clievent.IsInternalEventType(ty) && !jsSet[ty] {
			t.Errorf("clievent.IsInternalEventType(%q)=true but dashboard.js INTERNAL_EVENT_TYPES does not hide it — the server would skip past it while the UI still renders it", ty)
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
