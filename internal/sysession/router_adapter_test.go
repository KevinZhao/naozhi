package sysession

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// rawRouterStub is a minimal RawSystemSessionRouter used to exercise the
// routerAdapter projection in isolation (R260528-ARCH-9 / #1370). Only
// EventEntriesForKey carries behaviour; the rest record that the
// pass-through reached the raw router.
type rawRouterStub struct {
	entries           map[string][]cli.EventEntry
	visitCalled       bool
	setLabelArgs      [3]string
	clearKey          string
	registerStubKey   string
	setLabelResult    bool
	clearResultCalled bool
}

func (s *rawRouterStub) VisitSessions(fn func(session.SessionSnapshot) bool) {
	s.visitCalled = true
}

func (s *rawRouterStub) SetUserLabelWithOrigin(key, label, origin string) bool {
	s.setLabelArgs = [3]string{key, label, origin}
	return s.setLabelResult
}

func (s *rawRouterStub) ClearUserLabelOrigin(key string) bool {
	s.clearKey = key
	s.clearResultCalled = true
	return true
}

func (s *rawRouterStub) RegisterSystemStub(key, workspace, lastPrompt string) {
	s.registerStubKey = key
}

func (s *rawRouterStub) EventEntriesForKey(key string) []cli.EventEntry {
	return s.entries[key]
}

// TestRouterAdapter_EventEntriesProjection verifies the adapter copies
// only the Type/Summary fields onto SystemEventEntry, preserves order, and
// (R20260602-PERF-1 / #1578) filters down to the type=="user" turns with a
// non-blank summary that AutoTitler actually consumes — dropping assistant
// text / tool_use / blank-summary entries at the single conversion point
// rather than re-filtering them in buildExcerptFromHistory.
func TestRouterAdapter_EventEntriesProjection(t *testing.T) {
	t.Parallel()
	raw := &rawRouterStub{
		entries: map[string][]cli.EventEntry{
			"sys:auto:1": {
				{Type: "user", Summary: "问题一", Time: 100, Detail: "ignored"},
				{Type: "text", Summary: "回复", Tool: "Read"},
				{Type: "tool_use", Summary: "Read file"},
				{Type: "user", Summary: "  "}, // blank user summary dropped
				{Type: "user", Summary: "问题二"},
			},
		},
	}
	a := wrapRouter(raw)
	got := a.EventEntriesForKey("sys:auto:1")
	want := []SystemEventEntry{
		{Type: "user", Summary: "问题一"},
		{Type: "user", Summary: "问题二"},
	}
	if len(got) != len(want) {
		t.Fatalf("len: got %d want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d: got %+v want %+v", i, got[i], want[i])
		}
	}

	// Equivalence guard: feeding the adapter output to buildExcerptFromHistory
	// must produce the same excerpt as feeding the unfiltered projection,
	// proving the pushed-down filter is behaviour-preserving.
	unfiltered := []SystemEventEntry{
		{Type: "user", Summary: "问题一"},
		{Type: "text", Summary: "回复"},
		{Type: "tool_use", Summary: "Read file"},
		{Type: "user", Summary: "  "},
		{Type: "user", Summary: "问题二"},
	}
	if a, b := buildExcerptFromHistory(got), buildExcerptFromHistory(unfiltered); a != b {
		t.Errorf("filtered excerpt %q != unfiltered excerpt %q", a, b)
	}
}

// TestRouterAdapter_AllNonUserCollapsesToNil verifies that when no entry
// survives the user/blank filter, the adapter returns nil so the
// empty-seed contract (buildExcerptFromHistory("")=="") is preserved
// (R20260602-PERF-1 / #1578).
func TestRouterAdapter_AllNonUserCollapsesToNil(t *testing.T) {
	t.Parallel()
	raw := &rawRouterStub{entries: map[string][]cli.EventEntry{
		"sys:auto:1": {
			{Type: "text", Summary: "回复"},
			{Type: "tool_use", Summary: "Read"},
			{Type: "user", Summary: "   "},
		},
	}}
	a := wrapRouter(raw)
	if got := a.EventEntriesForKey("sys:auto:1"); got != nil {
		t.Errorf("all-non-user slice: got %v want nil", got)
	}
}

// TestRouterAdapter_EmptyAndNil verifies both nil-key and empty-slice
// cases collapse to nil (the only distinction buildExcerptFromHistory
// observes).
func TestRouterAdapter_EmptyAndNil(t *testing.T) {
	t.Parallel()
	raw := &rawRouterStub{entries: map[string][]cli.EventEntry{
		"empty": {},
	}}
	a := wrapRouter(raw)
	if got := a.EventEntriesForKey("missing"); got != nil {
		t.Errorf("missing key: got %v want nil", got)
	}
	if got := a.EventEntriesForKey("empty"); got != nil {
		t.Errorf("empty slice: got %v want nil", got)
	}
}

// TestWrapRouter_Nil ensures wrapping a nil raw router yields nil so the
// Manager's nil-Router guard stays meaningful.
func TestWrapRouter_Nil(t *testing.T) {
	t.Parallel()
	if got := wrapRouter(nil); got != nil {
		t.Errorf("wrapRouter(nil): got %v want nil", got)
	}
}

// TestRouterAdapter_PassThrough verifies the non-event methods forward to
// the raw router unchanged.
func TestRouterAdapter_PassThrough(t *testing.T) {
	t.Parallel()
	raw := &rawRouterStub{setLabelResult: true}
	a := wrapRouter(raw)

	a.VisitSessions(func(session.SessionSnapshot) bool { return true })
	if !raw.visitCalled {
		t.Error("VisitSessions did not reach raw router")
	}
	if ok := a.SetUserLabelWithOrigin("k", "label", "auto"); !ok {
		t.Error("SetUserLabelWithOrigin should return raw result true")
	}
	if raw.setLabelArgs != [3]string{"k", "label", "auto"} {
		t.Errorf("SetUserLabelWithOrigin args not forwarded: %v", raw.setLabelArgs)
	}
	a.ClearUserLabelOrigin("ck")
	if raw.clearKey != "ck" || !raw.clearResultCalled {
		t.Error("ClearUserLabelOrigin not forwarded")
	}
	a.RegisterSystemStub("rk", "/ws", "prompt")
	if raw.registerStubKey != "rk" {
		t.Error("RegisterSystemStub not forwarded")
	}
}
