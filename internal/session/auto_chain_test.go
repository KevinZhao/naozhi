package session

import (
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/discovery"
)

// fakeExcluder lets tests script "is this id excluded" without spinning
// up a router or cron scheduler. set is consulted by IsExcluded.
type fakeExcluder struct {
	set map[string]bool
}

func (f fakeExcluder) IsExcluded(id string) bool {
	if f.set == nil {
		return false
	}
	return f.set[id]
}

// fakePolicy gives tests cheap control over the AutoChainPolicy without
// constructing a Router.
type fakePolicy struct {
	enabled bool
	window  time.Duration
	cap     int
}

func (f fakePolicy) Enabled(string) bool         { return f.enabled }
func (f fakePolicy) Window(string) time.Duration { return f.window }
func (f fakePolicy) Cap(string) int              { return f.cap }

// fakeFilter is a discovery.RecentSessionsFilter for the
// recentFilterAsExcluder adapter test.
type fakeFilter struct {
	skipIDs   map[string]bool
	skipWS    string
	wsCalls   int
	idCalls   int
	skipWSAns bool
}

func (f *fakeFilter) SkipWorkspace(ws string) bool {
	f.wsCalls++
	return f.skipWSAns && (f.skipWS == "" || ws == f.skipWS)
}

func (f *fakeFilter) SkipSessionID(id string) bool {
	f.idCalls++
	return f.skipIDs[id]
}

// listFn synthesises a deterministic ListWorkspaceJSONL output for the
// given (id, mtime-offset-seconds) pairs. Mtime is "now - secAgo" so
// tests can assert window cutoff behaviour.
func listFn(now time.Time, pairs ...struct {
	id     string
	secAgo int
}) func(string) []discovery.WorkspaceJSONL {
	return func(string) []discovery.WorkspaceJSONL {
		out := make([]discovery.WorkspaceJSONL, len(pairs))
		for i, p := range pairs {
			out[i] = discovery.WorkspaceJSONL{
				SessionID: p.id,
				Mtime:     now.Add(-time.Duration(p.secAgo) * time.Second).UnixMilli(),
			}
		}
		return out
	}
}

func TestPickWorkspaceChain_HappyPath_OrderedByMtime(t *testing.T) {
	now := time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC)
	list := listFn(now,
		struct {
			id     string
			secAgo int
		}{"00000000-0000-4000-8000-000000000003", 30},
		struct {
			id     string
			secAgo int
		}{"00000000-0000-4000-8000-000000000001", 90},
		struct {
			id     string
			secAgo int
		}{"00000000-0000-4000-8000-000000000002", 60},
	)
	got := pickWorkspaceChain("/ws", list, fakeExcluder{}, fakePolicy{enabled: true, window: time.Hour, cap: 32}, now)
	want := []string{
		"00000000-0000-4000-8000-000000000001",
		"00000000-0000-4000-8000-000000000002",
		"00000000-0000-4000-8000-000000000003",
	}
	if len(got) != len(want) {
		t.Fatalf("len got=%d want=%d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %s want %s", i, got[i], want[i])
		}
	}
}

func TestPickWorkspaceChain_RespectsWindow(t *testing.T) {
	now := time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC)
	list := listFn(now,
		struct {
			id     string
			secAgo int
		}{"00000000-0000-4000-8000-000000000001", 30},
		struct {
			id     string
			secAgo int
		}{"00000000-0000-4000-8000-000000000002", 7200}, // out of 1h window
	)
	got := pickWorkspaceChain("/ws", list, fakeExcluder{}, fakePolicy{enabled: true, window: time.Hour, cap: 32}, now)
	if len(got) != 1 || got[0] != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("expected only ID 1 (in-window), got %v", got)
	}
}

func TestPickWorkspaceChain_ExcludesUsedIDs(t *testing.T) {
	now := time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC)
	list := listFn(now,
		struct {
			id     string
			secAgo int
		}{"00000000-0000-4000-8000-000000000001", 30},
		struct {
			id     string
			secAgo int
		}{"00000000-0000-4000-8000-000000000002", 60},
	)
	excl := fakeExcluder{set: map[string]bool{
		"00000000-0000-4000-8000-000000000001": true,
	}}
	got := pickWorkspaceChain("/ws", list, excl, fakePolicy{enabled: true, window: time.Hour, cap: 32}, now)
	if len(got) != 1 || got[0] != "00000000-0000-4000-8000-000000000002" {
		t.Fatalf("expected only ID 2 (ID 1 excluded), got %v", got)
	}
}

func TestPickWorkspaceChain_DisabledReturnsNil(t *testing.T) {
	now := time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC)
	list := listFn(now, struct {
		id     string
		secAgo int
	}{"00000000-0000-4000-8000-000000000001", 30})
	got := pickWorkspaceChain("/ws", list, fakeExcluder{}, fakePolicy{enabled: false, window: time.Hour, cap: 32}, now)
	if got != nil {
		t.Fatalf("expected nil when disabled, got %v", got)
	}
}

func TestPickWorkspaceChain_CapZeroDefaultsToMax(t *testing.T) {
	now := time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC)
	list := listFn(now, struct {
		id     string
		secAgo int
	}{"00000000-0000-4000-8000-000000000001", 30})
	got := pickWorkspaceChain("/ws", list, fakeExcluder{}, fakePolicy{enabled: true, window: time.Hour, cap: 0}, now)
	if len(got) != 1 {
		t.Fatalf("cap=0 must fall through to maxPrevSessionIDs (32), got %v", got)
	}
}

func TestPickWorkspaceChain_WindowZeroDefaultsTo7d(t *testing.T) {
	now := time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC)
	// 6 days ago → still inside default 7d, must be returned.
	list := listFn(now, struct {
		id     string
		secAgo int
	}{"00000000-0000-4000-8000-000000000001", 6 * 24 * 3600})
	got := pickWorkspaceChain("/ws", list, fakeExcluder{}, fakePolicy{enabled: true, window: 0, cap: 32}, now)
	if len(got) != 1 {
		t.Fatalf("window=0 must default to 7d; got %v", got)
	}
}

func TestPickWorkspaceChain_RespectsCap_KeepsNewest(t *testing.T) {
	now := time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC)
	// 5 candidates, cap=3 → keep 2 (cap-1 reserved for current session).
	pairs := []struct {
		id     string
		secAgo int
	}{
		{"00000000-0000-4000-8000-000000000001", 500},
		{"00000000-0000-4000-8000-000000000002", 400},
		{"00000000-0000-4000-8000-000000000003", 300},
		{"00000000-0000-4000-8000-000000000004", 200},
		{"00000000-0000-4000-8000-000000000005", 100},
	}
	list := listFn(now, pairs...)
	got := pickWorkspaceChain("/ws", list, fakeExcluder{}, fakePolicy{enabled: true, window: time.Hour, cap: 3}, now)
	want := []string{
		"00000000-0000-4000-8000-000000000004",
		"00000000-0000-4000-8000-000000000005",
	}
	if len(got) != len(want) {
		t.Fatalf("len got=%d want=%d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %s want %s", i, got[i], want[i])
		}
	}
}

func TestPickWorkspaceChain_EmptyWorkspace(t *testing.T) {
	got := pickWorkspaceChain("", nil, fakeExcluder{}, fakePolicy{enabled: true}, time.Now())
	if got != nil {
		t.Fatalf("empty workspace must return nil, got %v", got)
	}
}

func TestPickWorkspaceChain_NilPolicy(t *testing.T) {
	got := pickWorkspaceChain("/ws", nil, fakeExcluder{}, nil, time.Now())
	if got != nil {
		t.Fatalf("nil policy must return nil, got %v", got)
	}
}

func TestPickWorkspaceChain_NilListFn(t *testing.T) {
	got := pickWorkspaceChain("/ws", nil, fakeExcluder{}, fakePolicy{enabled: true, cap: 32}, time.Now())
	if got != nil {
		t.Fatalf("nil listJSONL must return nil, got %v", got)
	}
}

func TestPickWorkspaceChain_CapOne_NoPrevAllowed(t *testing.T) {
	now := time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC)
	list := listFn(now, struct {
		id     string
		secAgo int
	}{"00000000-0000-4000-8000-000000000001", 30})
	got := pickWorkspaceChain("/ws", list, fakeExcluder{}, fakePolicy{enabled: true, window: time.Hour, cap: 1}, now)
	if got != nil {
		t.Fatalf("cap=1 reserves the only slot for the current session; got %v", got)
	}
}

func TestCombinedExcluder_ShortCircuits(t *testing.T) {
	called := 0
	hit := fakeFnExcluder{fn: func(id string) bool { called++; return true }}
	never := fakeFnExcluder{fn: func(id string) bool { called++; return false }}

	c := combinedExcluder{inner: []SessionIDExcluder{never, hit, never}}
	if !c.IsExcluded("x") {
		t.Fatal("expected combined to exclude on hit")
	}
	if called != 2 {
		t.Errorf("expected short-circuit at 2 calls, got %d", called)
	}
}

func TestCombinedExcluder_NilEntriesIgnored(t *testing.T) {
	c := combinedExcluder{inner: []SessionIDExcluder{nil, nil}}
	if c.IsExcluded("x") {
		t.Fatal("nil entries must not exclude")
	}
}

func TestSelfExcluder(t *testing.T) {
	se := selfExcluder{set: map[string]bool{"a": true}}
	if !se.IsExcluded("a") {
		t.Fatal("a must be excluded")
	}
	if se.IsExcluded("b") {
		t.Fatal("b must not be excluded")
	}
	zero := selfExcluder{}
	if zero.IsExcluded("a") {
		t.Fatal("nil set must not exclude anything")
	}
}

func TestFilterByExcluder_NoExclusions_ReturnsInput(t *testing.T) {
	in := []string{"a", "b", "c"}
	out := filterByExcluder(in, fakeExcluder{})
	// fast path: returns same backing array
	if &in[0] != &out[0] {
		t.Errorf("expected non-allocating fast path on no-drop case")
	}
}

func TestFilterByExcluder_DropsMidElement(t *testing.T) {
	in := []string{"a", "b", "c"}
	excl := fakeExcluder{set: map[string]bool{"b": true}}
	out := filterByExcluder(in, excl)
	want := []string{"a", "c"}
	if len(out) != len(want) || out[0] != "a" || out[1] != "c" {
		t.Errorf("got %v want %v", out, want)
	}
}

func TestFilterByExcluder_DropsAll(t *testing.T) {
	in := []string{"a", "b"}
	excl := fakeExcluder{set: map[string]bool{"a": true, "b": true}}
	out := filterByExcluder(in, excl)
	if len(out) != 0 {
		t.Errorf("expected empty result, got %v", out)
	}
}

func TestRecentFilterAsExcluder_ForwardsSkipSessionID(t *testing.T) {
	f := &fakeFilter{skipIDs: map[string]bool{"a": true}}
	a := recentFilterAsExcluder{f: f}
	if !a.IsExcluded("a") {
		t.Fatal("expected adapter to forward SkipSessionID(a)=true")
	}
	if a.IsExcluded("b") {
		t.Fatal("expected adapter to forward SkipSessionID(b)=false")
	}
	if f.idCalls != 2 {
		t.Errorf("expected 2 SkipSessionID calls, got %d", f.idCalls)
	}
	// SkipWorkspace must NOT be invoked through the adapter.
	if f.wsCalls != 0 {
		t.Errorf("expected adapter to ignore SkipWorkspace, got %d calls", f.wsCalls)
	}
}

func TestRecentFilterAsExcluder_NilFilterReturnsFalse(t *testing.T) {
	a := recentFilterAsExcluder{f: nil}
	if a.IsExcluded("anything") {
		t.Fatal("nil filter must return false")
	}
}

func TestAsExcluder_PublicFactory(t *testing.T) {
	f := &fakeFilter{skipIDs: map[string]bool{"x": true}}
	e := AsExcluder(f)
	if !e.IsExcluded("x") {
		t.Fatal("AsExcluder(f) should forward SkipSessionID(x)=true")
	}
}

// fakeFnExcluder is a callback-shaped SessionIDExcluder for asserting
// short-circuit behaviour in combinedExcluder tests.
type fakeFnExcluder struct {
	fn func(string) bool
}

func (f fakeFnExcluder) IsExcluded(id string) bool { return f.fn(id) }
