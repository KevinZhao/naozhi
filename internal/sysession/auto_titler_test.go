package sysession

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// snapshotFakeRouter extends fakeRouter so VisitSessions iterates
// through caller-supplied snapshots.  Used by AutoTitler tests to
// exercise the candidate-selection logic without spinning up a real
// session.Router.
//
// Per-key event-log entries can be supplied via `entries`.  When a key
// is missing from `entries`, EventEntriesForKey falls back to a single
// synthetic user entry derived from the snapshot's LastPrompt — this
// preserves backward-compatibility with table-driven tests written
// before the full-history change.
type snapshotFakeRouter struct {
	*fakeRouter
	snaps   []session.SessionSnapshot
	entries map[string][]cli.EventEntry

	// rejectAuto, when true, makes SetUserLabelWithOrigin return false
	// for origin="auto" — simulates the race-window guard firing.
	rejectAuto atomic.Bool
}

func newSnapshotFakeRouter(snaps []session.SessionSnapshot) *snapshotFakeRouter {
	return &snapshotFakeRouter{fakeRouter: newFakeRouter(), snaps: snaps}
}

func (s *snapshotFakeRouter) VisitSessions(fn func(session.SessionSnapshot) bool) {
	for _, snap := range s.snaps {
		if !fn(snap) {
			return
		}
	}
}

func (s *snapshotFakeRouter) SetUserLabelWithOrigin(key, label, origin string) bool {
	if origin == "auto" && s.rejectAuto.Load() {
		return false
	}
	return s.fakeRouter.SetUserLabelWithOrigin(key, label, origin)
}

func (s *snapshotFakeRouter) EventEntriesForKey(key string) []cli.EventEntry {
	if e, ok := s.entries[key]; ok {
		return e
	}
	// Backward-compat: if the test only set LastPrompt on the snapshot,
	// surface it as a single user entry so renameOne sees a non-empty
	// excerpt and the old assertions still pass.
	for _, snap := range s.snaps {
		if snap.Key == key && snap.LastPrompt != "" {
			return []cli.EventEntry{{Type: "user", Summary: snap.LastPrompt}}
		}
	}
	return nil
}

func TestBuildExcerpt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty in empty out", "", ""},
		{"trims whitespace", "  hi  ", "hi"},
		{"drops control chars", "ab\x00\x07c", "abc"},
		{"keeps newlines", "a\nb\nc", "a\nb\nc"},
		{"caps line length", strings.Repeat("a", autoTitlerLineCapBytes+50) + "\nshort", strings.Repeat("a", autoTitlerLineCapBytes) + "…\nshort"},
		// R232-PERF-7: invalid UTF-8 byte (lone 0xC3 continuation start
		// without follow-up) is dropped while surrounding ASCII survives.
		{"strips invalid utf-8 bytes", "ab\xc3xy", "abxy"},
		// CJK multi-byte runes survive intact (3 bytes each) — guards
		// the single-pass DecodeRuneInString from over-eager byte skip.
		{"keeps cjk runes", "你好", "你好"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := buildExcerpt(c.in)
			if got != c.want {
				t.Errorf("buildExcerpt = %q, want %q", got, c.want)
			}
		})
	}
}

// TestBuildExcerpt_NoTotalCap: the previous 8 KiB total-byte ceiling
// was deliberately removed so AutoTitler can review entire long
// conversations.  Verify that a >>8 KiB seed survives the sanitiser
// intact (line cap still applies — but here every line is short).
func TestBuildExcerpt_NoTotalCap(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	for i := 0; i < 5000; i++ {
		sb.WriteString("hello\n")
	}
	in := sb.String()
	got := buildExcerpt(in)
	if len(got) < 8*1024 {
		t.Errorf("excerpt len %d unexpectedly small; want >= 8 KiB after total-cap removal", len(got))
	}
	// Trailing newline gets trimmed by TrimSpace; otherwise content matches.
	want := strings.TrimSpace(in)
	if got != want {
		t.Errorf("excerpt content drift; len got=%d want=%d", len(got), len(want))
	}
}

// TestBuildExcerpt_MarkerSplitByLineCap pins R235-GO-4 (#1004): the
// per-line cap (autoTitlerLineCapBytes) MUST NOT leave a literal
// EXCERPT delimiter visible to the LLM, even when an attacker pads
// the line so the marker straddles the cap. The previous shape did
// the marker scrub as a final ReplaceAll on the rune-walk output —
// once truncation cut "---BEGIN CONVERSATION EXCERPT---" between
// "---BEGIN CONVERS" + "ATION EXCERPT---" the post-pass missed both
// fragments and a real BEGIN delimiter survived in the prompt,
// poisoning the structural boundary the system prompt relies on.
//
// The fix neutralises markers on the raw seed before truncation so
// no cap split can re-emerge a partial marker. The placeholder is
// shorter than either marker (16 vs 30/32 bytes) so the cap math
// stays conservative.
func TestBuildExcerpt_MarkerSplitByLineCap(t *testing.T) {
	t.Parallel()
	// Pad with enough leading bytes that the BEGIN marker straddles
	// the autoTitlerLineCapBytes boundary. Pick a pad length ≥
	// (cap - len(marker)/2) so the truncation point lands inside the
	// marker text.
	padLen := autoTitlerLineCapBytes - 10
	pad := strings.Repeat("a", padLen)
	seedBegin := pad + excerptBeginMarker + " trailing tail"
	gotBegin := buildExcerpt(seedBegin)
	if strings.Contains(gotBegin, excerptBeginMarker) {
		t.Errorf("BEGIN marker survived in output despite line-cap split; got: %q", gotBegin)
	}
	if strings.Contains(gotBegin, "BEGIN CONVERSATION EXCERPT") {
		// Even a partial-marker fragment is a structural-boundary risk.
		// The replacement happens BEFORE truncation, so the cap should
		// only ever see the inert placeholder fragments.
		t.Errorf("BEGIN-marker substring survived in output: %q", gotBegin)
	}

	// Same shape for the END marker.
	seedEnd := pad + excerptEndMarker + " trailing tail"
	gotEnd := buildExcerpt(seedEnd)
	if strings.Contains(gotEnd, excerptEndMarker) {
		t.Errorf("END marker survived in output despite line-cap split; got: %q", gotEnd)
	}
	if strings.Contains(gotEnd, "END CONVERSATION EXCERPT") {
		t.Errorf("END-marker substring survived in output: %q", gotEnd)
	}

	// Sanity: an inline marker on a short line still gets neutralised
	// (existing Sec-MEDIUM-1 contract).
	seedShort := excerptBeginMarker + " short tail"
	gotShort := buildExcerpt(seedShort)
	if strings.Contains(gotShort, excerptBeginMarker) {
		t.Errorf("inline BEGIN marker survived without line-cap split: %q", gotShort)
	}
	if !strings.Contains(gotShort, excerptMarkerSafe) {
		t.Errorf("expected placeholder %q in output, got: %q", excerptMarkerSafe, gotShort)
	}
}

// TestBuildExcerpt_MarkerFoldedIntoWalk pins R20260602-PERF-1 (#1578):
// folding the delimiter neutralisation into the single rune walk (instead
// of the prior 2×Contains + 2×ReplaceAll pre-pass) must stay
// behaviour-equivalent — every literal marker becomes the inert
// placeholder, surrounding content is preserved verbatim, and multiple
// / adjacent markers are all neutralised.
func TestBuildExcerpt_MarkerFoldedIntoWalk(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"inline begin marker with surrounding text",
			"before " + excerptBeginMarker + " after",
			"before " + excerptMarkerSafe + " after",
		},
		{
			"inline end marker with surrounding text",
			"x" + excerptEndMarker + "y",
			"x" + excerptMarkerSafe + "y",
		},
		{
			"both markers on one line",
			excerptBeginMarker + "mid" + excerptEndMarker,
			excerptMarkerSafe + "mid" + excerptMarkerSafe,
		},
		{
			"adjacent identical markers",
			excerptBeginMarker + excerptBeginMarker,
			excerptMarkerSafe + excerptMarkerSafe,
		},
		{
			"marker across newlines preserves line structure",
			"a\n" + excerptBeginMarker + "\nb",
			"a\n" + excerptMarkerSafe + "\nb",
		},
		{
			"no marker is untouched",
			"plain content 你好",
			"plain content 你好",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := buildExcerpt(c.in); got != c.want {
				t.Errorf("buildExcerpt(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestAutoTitler_CandidateFilter pins the candidate-selection rules
// from §7.1.  Each row in the table sets up one snapshot and asserts
// which "skipped_*" bucket it falls into (or that it gets renamed).
func TestAutoTitler_CandidateFilter(t *testing.T) {
	t.Parallel()
	now := time.Now()
	cases := []struct {
		name         string
		snap         session.SessionSnapshot
		runnerResp   string
		wantBucket   string // "" means we expect Acted=1
		includeGroup bool
	}{
		{
			name: "fresh user session is renamed",
			snap: session.SessionSnapshot{
				Key: "feishu:direct:u1:general", MessageCount: 5,
				LastPrompt: "讨论 deploy 流程", LastActive: now.UnixMilli(),
			},
			runnerResp: "讨论部署流程",
		},
		{
			name: "reserved namespace skipped",
			snap: session.SessionSnapshot{
				Key: "cron:job-1", MessageCount: 5, LastPrompt: "cron stuff",
			},
			wantBucket: "reserved_namespace",
		},
		{
			name: "group chat default skipped",
			snap: session.SessionSnapshot{
				Key: "feishu:group:c1:general", ChatType: "group",
				MessageCount: 10, LastPrompt: "group chat",
			},
			wantBucket: "group_chat",
		},
		{
			name: "user-labeled session left alone",
			snap: session.SessionSnapshot{
				Key: "feishu:direct:u1:general", MessageCount: 10,
				UserLabel: "Existing", LabelOrigin: "user",
				LastPrompt: "msg",
			},
			wantBucket: "origin_user",
		},
		{
			name: "legacy non-empty label without origin is left alone",
			snap: session.SessionSnapshot{
				Key: "feishu:direct:u1:general", MessageCount: 10,
				UserLabel: "Existing", LastPrompt: "msg",
			},
			wantBucket: "origin_user",
		},
		{
			name: "auto-labeled session can be re-touched",
			snap: session.SessionSnapshot{
				Key: "feishu:direct:u1:general", MessageCount: 10,
				UserLabel: "Old auto", LabelOrigin: "auto",
				LastPrompt: "new content",
			},
			runnerResp: "新主题",
		},
		{
			name: "below min user turns",
			snap: session.SessionSnapshot{
				Key: "feishu:direct:u1:general", MessageCount: 1,
				LastPrompt: "msg",
			},
			wantBucket: "min_user_turns",
		},
		{
			name: "group chat respected when enabled",
			snap: session.SessionSnapshot{
				Key: "feishu:group:c1:general", ChatType: "group",
				MessageCount: 5, LastPrompt: "group msg",
			},
			runnerResp:   "群聊主题",
			includeGroup: true,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			router := newSnapshotFakeRouter([]session.SessionSnapshot{c.snap})
			runner := &fakeRunner{resp: c.runnerResp}
			a, err := newAutoTitler(DaemonDeps{Router: wrapRouter(router), Runner: runner})
			if err != nil {
				t.Fatalf("newAutoTitler: %v", err)
			}
			if c.includeGroup {
				if err := a.(Configurable).Configure(DaemonConfig{"include_group_chat": true}); err != nil {
					t.Fatalf("configure: %v", err)
				}
			}
			rep, err := a.Tick(context.Background())
			if err != nil {
				t.Fatalf("Tick err = %v", err)
			}
			if c.wantBucket != "" {
				if rep.Skipped[c.wantBucket] == 0 {
					t.Errorf("expected Skipped[%q] >= 1, got %v", c.wantBucket, rep.Skipped)
				}
				if rep.Acted != 0 {
					t.Errorf("Acted = %d, want 0", rep.Acted)
				}
				return
			}
			if rep.Acted != 1 {
				t.Errorf("Acted = %d, want 1; report=%+v", rep.Acted, rep)
			}
			if runner.calls.Load() != 1 {
				t.Errorf("runner.calls = %d, want 1", runner.calls.Load())
			}
		})
	}
}

// TestAutoTitler_PromptStructure asserts the LLM prompt has the
// expected three-layer shape (system header + EXCERPT block + reminder
// tail).  Locks the key prompt-injection defence against accidental
// regression.
func TestAutoTitler_PromptStructure(t *testing.T) {
	t.Parallel()
	router := newSnapshotFakeRouter([]session.SessionSnapshot{
		{Key: "feishu:direct:u1:general", MessageCount: 5, LastPrompt: "请把工作流改成 docker"},
	})
	captured := make(chan string, 1)
	runner := &capturingRunner{captured: captured, resp: "工作流改 docker"}
	a, _ := newAutoTitler(DaemonDeps{Router: wrapRouter(router), Runner: runner})
	if _, err := a.Tick(context.Background()); err != nil {
		t.Fatalf("Tick err: %v", err)
	}
	select {
	case prompt := <-captured:
		for _, want := range []string{
			"You are a session title extractor",
			"CRITICAL RULES",
			"---BEGIN CONVERSATION EXCERPT---",
			"---END CONVERSATION EXCERPT---",
			"REMINDER: Output only the Chinese title",
			"请把工作流改成 docker",
		} {
			if !strings.Contains(prompt, want) {
				t.Errorf("prompt missing %q\nfull:\n%s", want, prompt)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("runner.Run was not invoked")
	}
}

// TestAutoTitler_RaceWindowDeferralIsValidationError covers the case
// where the user changed the label between Snapshot and write — the
// router rejects the daemon write, and the daemon must surface this
// as a validation error (not upstream) so the breaker doesn't trip.
func TestAutoTitler_RaceWindowDeferralIsValidationError(t *testing.T) {
	t.Parallel()
	router := newSnapshotFakeRouter([]session.SessionSnapshot{
		{Key: "feishu:direct:u1:general", MessageCount: 5, LastPrompt: "stuff"},
	})
	router.rejectAuto.Store(true)
	runner := &fakeRunner{resp: "新标题"}
	a, _ := newAutoTitler(DaemonDeps{Router: wrapRouter(router), Runner: runner})
	_, err := a.Tick(context.Background())
	if !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want wrapped ErrValidation", err)
	}
}

// TestAutoTitler_HighwaterPreventsRapidRename validates the rename
// interval gate:  a session that was just renamed must not be renamed
// again until minRenameInterval elapses.
func TestAutoTitler_HighwaterPreventsRapidRename(t *testing.T) {
	t.Parallel()
	snap := session.SessionSnapshot{
		Key: "feishu:direct:u1:general", MessageCount: 5,
		LastPrompt: "msg",
	}
	router := newSnapshotFakeRouter([]session.SessionSnapshot{snap})
	runner := &fakeRunner{resp: "first title"}
	a, _ := newAutoTitler(DaemonDeps{Router: wrapRouter(router), Runner: runner})

	// First tick: renames.
	rep, err := a.Tick(context.Background())
	if err != nil {
		t.Fatalf("first Tick err: %v", err)
	}
	if rep.Acted != 1 {
		t.Errorf("first tick: Acted = %d, want 1", rep.Acted)
	}

	// Second tick immediately after: must skip due to interval gate.
	rep2, err := a.Tick(context.Background())
	if err != nil {
		t.Fatalf("second Tick err: %v", err)
	}
	if rep2.Acted != 0 {
		t.Errorf("second tick within interval: Acted = %d, want 0", rep2.Acted)
	}
	if rep2.Skipped["min_rename_interval"] == 0 {
		t.Errorf("expected min_rename_interval skip, got %v", rep2.Skipped)
	}
	if runner.calls.Load() != 1 {
		t.Errorf("runner called %d times, want 1", runner.calls.Load())
	}
}

// TestBuildExcerptFromHistory verifies the helper concatenates only
// type=="user" entries (in chronological order) and ignores assistant
// text, thinking, tool_use and other event types.
func TestBuildExcerptFromHistory(t *testing.T) {
	t.Parallel()
	entries := []SystemEventEntry{
		{Type: "user", Summary: "第一个问题"},
		{Type: "text", Summary: "助手回复 A"},
		{Type: "thinking", Summary: "思考"},
		{Type: "tool_use", Summary: "Read file"},
		{Type: "user", Summary: "第二个问题"},
		{Type: "text", Summary: "助手回复 B"},
		{Type: "user", Summary: "第三个问题"},
		// Empty user summary should be skipped, not blank-line
		// pollute the output.
		{Type: "user", Summary: "  "},
	}
	got := buildExcerptFromHistory(entries)
	want := "第一个问题\n第二个问题\n第三个问题"
	if got != want {
		t.Errorf("buildExcerptFromHistory:\n got %q\nwant %q", got, want)
	}
}

// TestBuildExcerptFromHistory_SoftCap verifies R238-GO-15 (#806):
// thousands-of-turns sessions can no longer drive the builder past
// autoTitlerExcerptSoftCapBytes.  We feed entries whose summed length
// exceeds the cap and assert the result stays bounded and ends with the
// truncation marker.
func TestBuildExcerptFromHistory_SoftCap(t *testing.T) {
	t.Parallel()
	// Use a 4 KiB summary repeated enough times to exceed the 1 MiB cap.
	const perEntry = 4 * 1024
	bigChunk := strings.Repeat("a", perEntry)
	// Want at least cap/perEntry + 8 entries so the loop hits the
	// truncation path and keeps iterating with no further appends.
	count := (autoTitlerExcerptSoftCapBytes / perEntry) + 8
	entries := make([]SystemEventEntry, 0, count)
	for i := 0; i < count; i++ {
		entries = append(entries, SystemEventEntry{
			Type: "user", Summary: bigChunk,
		})
	}
	got := buildExcerptFromHistory(entries)
	// After R171023-CR-004 the "…" byte width is included in the need
	// calculation, so the result must stay within the soft cap exactly.
	if len(got) > autoTitlerExcerptSoftCapBytes {
		t.Errorf("buildExcerptFromHistory exceeded soft cap: got %d bytes, max %d", len(got), autoTitlerExcerptSoftCapBytes)
	}
	// Truncation marker must be present so downstream review can spot
	// the cut.  Confirms the break-on-cap branch fired.
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected truncation ellipsis at tail, got tail %q", got[max(0, len(got)-32):])
	}
}

// TestBuildExcerptFromHistory_SoftCapBoundary locks R171023-CR-004: the "…"
// rune width (3 bytes) must be included in the need calculation so the
// builder never writes past autoTitlerExcerptSoftCapBytes.  We construct an
// input whose last entry, if fully appended, would push the buffer to exactly
// cap+1, triggering truncation.  After truncation the result must be
// ≤ autoTitlerExcerptSoftCapBytes and end with "…".
func TestBuildExcerptFromHistory_SoftCapBoundary(t *testing.T) {
	t.Parallel()

	const cap = autoTitlerExcerptSoftCapBytes
	// First entry fills the buffer to cap-4 bytes (leaves exactly 4 bytes
	// of headroom: 1 newline + 3 for "…").
	fill := strings.Repeat("x", cap-4)
	// Second entry is 1 byte — newline + entry would be 2 bytes which,
	// together with the 3-byte "…", sums to 5, exceeding the 4-byte
	// headroom and triggering truncation.
	entries := []SystemEventEntry{
		{Type: "user", Summary: fill},
		{Type: "user", Summary: "y"},
	}

	got := buildExcerptFromHistory(entries)

	if len(got) > cap {
		t.Errorf("result length %d exceeds soft cap %d (R171023-CR-004)", len(got), cap)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected truncation ellipsis at tail, got %q", got[max(0, len(got)-16):])
	}
}

// TestEvictOldestHighwater_DropsOldestFirst verifies R238-GO-16 (#808)
// hard-cap eviction order:  the helper must keep the most recently
// renamed entries and drop the oldest ones first so the bounded-size
// map still gates useful min_rename_interval decisions.
func TestEvictOldestHighwater_DropsOldestFirst(t *testing.T) {
	t.Parallel()
	now := time.Now()
	m := map[string]autoTitlerHighwater{
		"oldest":   {lastRenamedAt: now.Add(-3 * time.Hour)},
		"middle":   {lastRenamedAt: now.Add(-2 * time.Hour)},
		"recent":   {lastRenamedAt: now.Add(-1 * time.Hour)},
		"freshest": {lastRenamedAt: now},
	}
	evictOldestHighwater(m, 2)
	if len(m) != 2 {
		t.Fatalf("expected len=2 after evict, got %d", len(m))
	}
	if _, ok := m["freshest"]; !ok {
		t.Errorf("freshest must survive eviction")
	}
	if _, ok := m["recent"]; !ok {
		t.Errorf("recent must survive eviction")
	}
	if _, ok := m["oldest"]; ok {
		t.Errorf("oldest must be evicted first")
	}
}

// TestEvictOldestHighwaterTieBreakDeterministic pins R260528-BUG-9: when
// two entries share lastRenamedAt the eviction must be deterministic
// across runs — pre-fix the slices.SortFunc returned 0 on ties and Go's
// randomised map iteration order picked the survivor at random, which
// surfaced as flaky tests and arbitrary tenant eviction in production.
// We seed five entries with identical timestamps and assert the same
// keys survive on every iteration.
func TestEvictOldestHighwaterTieBreakDeterministic(t *testing.T) {
	t.Parallel()
	mkSeed := func() map[string]autoTitlerHighwater {
		ts := time.Unix(1_700_000_000, 0)
		// Wide alphabet so map iteration order would otherwise pick
		// different keys on different runs.
		return map[string]autoTitlerHighwater{
			"alpha":   {lastRenamedAt: ts},
			"bravo":   {lastRenamedAt: ts},
			"charlie": {lastRenamedAt: ts},
			"delta":   {lastRenamedAt: ts},
			"echo":    {lastRenamedAt: ts},
		}
	}
	const keep = 2
	// Run many iterations — Go's map iteration randomisation seeds per
	// range so even within a single test pass the order changes; the
	// determinism assertion holds only because the sort tie-break uses
	// the key, not the iteration order.
	const iterations = 200
	var firstSurvivors map[string]struct{}
	for i := 0; i < iterations; i++ {
		m := mkSeed()
		evictOldestHighwater(m, keep)
		if len(m) != keep {
			t.Fatalf("iter %d: len=%d want %d", i, len(m), keep)
		}
		survivors := make(map[string]struct{}, len(m))
		for k := range m {
			survivors[k] = struct{}{}
		}
		if firstSurvivors == nil {
			firstSurvivors = survivors
			continue
		}
		if len(survivors) != len(firstSurvivors) {
			t.Fatalf("iter %d: survivor set size diverged", i)
		}
		for k := range survivors {
			if _, ok := firstSurvivors[k]; !ok {
				t.Errorf("iter %d: non-deterministic eviction — survivor %q not in first run %v",
					i, k, firstSurvivors)
			}
		}
	}
	// With ascending key tie-break, the two largest keys must survive
	// (oldest-first eviction drops the smallest keys when timestamps tie).
	if _, ok := firstSurvivors["delta"]; !ok {
		t.Errorf("expected 'delta' to survive (largest key not yet evicted), got %v", firstSurvivors)
	}
	if _, ok := firstSurvivors["echo"]; !ok {
		t.Errorf("expected 'echo' to survive (largest key), got %v", firstSurvivors)
	}
}

// TestEvictOldestHighwater_NoOpWhenUnderCap covers the fast-path:
// when len(m) <= keep, no entry should be touched.
func TestEvictOldestHighwater_NoOpWhenUnderCap(t *testing.T) {
	t.Parallel()
	m := map[string]autoTitlerHighwater{
		"a": {lastRenamedAt: time.Now()},
		"b": {lastRenamedAt: time.Now()},
	}
	evictOldestHighwater(m, 5)
	if len(m) != 2 {
		t.Errorf("expected unchanged len=2, got %d", len(m))
	}
}

// TestCommitHighwater_HardCapEnforcedOnEarlyStop pins R238-GO-16 (#808):
// even when earlyStop=true keeps blocking the regular prune path, a
// long-running tick stream must NOT grow highwater past
// autoTitlerHighwaterMaxEntries.  We seed an oversized map directly
// then call commitHighwater with earlyStop=true and a single new write.
func TestCommitHighwater_HardCapEnforcedOnEarlyStop(t *testing.T) {
	t.Parallel()
	a := &autoTitler{}
	a.highwater.Store(&map[string]autoTitlerHighwater{})
	// Seed past the cap so commitHighwater MUST evict.
	overflow := autoTitlerHighwaterMaxEntries + 50
	seed := make(map[string]autoTitlerHighwater, overflow)
	for i := 0; i < overflow; i++ {
		seed[fmtKey(i)] = autoTitlerHighwater{
			lastRenamedAt: time.Unix(int64(i), 0),
		}
	}
	a.highwater.Store(&seed)
	// Single new write; earlyStop=true blocks the prune-by-observed
	// path, so the only thing keeping the map bounded is the hard cap.
	writes := map[string]autoTitlerHighwater{
		"new-write": {lastRenamedAt: time.Now()},
	}
	a.commitHighwater(writes, nil, true)
	got := *a.highwater.Load()
	if len(got) > autoTitlerHighwaterMaxEntries {
		t.Errorf("hard cap not enforced: got %d entries, max %d", len(got), autoTitlerHighwaterMaxEntries)
	}
	// The freshest write must survive; the oldest seeds must be gone.
	if _, ok := got["new-write"]; !ok {
		t.Errorf("most-recent write was evicted, expected to survive")
	}
	if _, ok := got[fmtKey(0)]; ok {
		t.Errorf("oldest seed entry was not evicted")
	}
}

func fmtKey(i int) string {
	return "key-" + strconv.Itoa(i)
}

// TestAutoTitler_PromptIncludesAllUserTurns ensures the rename prompt
// reviews every user turn in the conversation, not just the LastPrompt
// cached on SessionSnapshot. The previous implementation only saw the
// most recent user message which produced misleading titles for long
// conversations whose theme had drifted away from the latest exchange.
func TestAutoTitler_PromptIncludesAllUserTurns(t *testing.T) {
	t.Parallel()
	key := "feishu:direct:u1:general"
	snap := session.SessionSnapshot{
		Key: key, MessageCount: 5,
		// LastPrompt only carries the latest message — the visitor
		// no longer reads it; the candidate is rebuilt from the full
		// event log via EventEntriesForKey below.
		LastPrompt: "第三个问题",
	}
	history := []cli.EventEntry{
		{Time: 100, Type: "user", Summary: "讨论 deploy 流程"},
		{Time: 110, Type: "text", Summary: "好的"},
		{Time: 200, Type: "user", Summary: "切到 docker"},
		{Time: 300, Type: "user", Summary: "第三个问题"},
	}
	router := newSnapshotFakeRouter([]session.SessionSnapshot{snap})
	router.entries = map[string][]cli.EventEntry{key: history}

	captured := make(chan string, 1)
	runner := &capturingRunner{captured: captured, resp: "部署流程讨论"}
	a, _ := newAutoTitler(DaemonDeps{Router: wrapRouter(router), Runner: runner})
	if _, err := a.Tick(context.Background()); err != nil {
		t.Fatalf("Tick err: %v", err)
	}
	select {
	case prompt := <-captured:
		// Every user turn should land in the prompt.
		for _, want := range []string{"讨论 deploy 流程", "切到 docker", "第三个问题"} {
			if !strings.Contains(prompt, want) {
				t.Errorf("prompt missing user-turn excerpt %q\nfull:\n%s", want, prompt)
			}
		}
		// Assistant text should NOT leak in.
		if strings.Contains(prompt, "好的") {
			t.Errorf("prompt unexpectedly contains assistant text\nfull:\n%s", prompt)
		}
	case <-time.After(time.Second):
		t.Fatal("runner.Run was not invoked")
	}
}

// TestAutoTitler_LongConversationNotTruncated locks in the operator
// decision to remove the 8 KiB total-byte EXCERPT cap. A conversation
// with hundreds of user turns must reach the runner intact (line cap
// still applies per individual line, but the total budget is unbounded).
func TestAutoTitler_LongConversationNotTruncated(t *testing.T) {
	t.Parallel()
	key := "feishu:direct:u1:general"
	snap := session.SessionSnapshot{
		Key: key, MessageCount: 200, LastPrompt: "marker-tail",
	}
	history := make([]cli.EventEntry, 0, 200)
	for i := 0; i < 200; i++ {
		history = append(history, cli.EventEntry{
			Time:    int64(i + 1),
			Type:    "user",
			Summary: "marker-" + strings.Repeat("x", 32),
		})
	}
	router := newSnapshotFakeRouter([]session.SessionSnapshot{snap})
	router.entries = map[string][]cli.EventEntry{key: history}

	captured := make(chan string, 1)
	runner := &capturingRunner{captured: captured, resp: "长对话标题"}
	a, _ := newAutoTitler(DaemonDeps{Router: wrapRouter(router), Runner: runner})
	if _, err := a.Tick(context.Background()); err != nil {
		t.Fatalf("Tick err: %v", err)
	}
	select {
	case prompt := <-captured:
		// 200 marker lines should all survive (8 KiB cap was the
		// previous truncation point — at 32-byte payload + prefix
		// that would have stopped well before the 200th line).
		got := strings.Count(prompt, "marker-")
		if got != 200 {
			t.Errorf("excerpt truncated: got %d marker lines, want 200", got)
		}
	case <-time.After(time.Second):
		t.Fatal("runner.Run was not invoked")
	}
}

// capturingRunner records the prompt argument for assertion in tests.
type capturingRunner struct {
	captured chan<- string
	resp     string
	mu       sync.Mutex
}

func (c *capturingRunner) Run(_ context.Context, prompt string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case c.captured <- prompt:
	default:
	}
	return c.resp, nil
}

// TestAutoTitler_BatchPerTickClamp asserts that Configure clamps a
// pathologically large `batch_per_tick` cfg to autoTitlerMaxBatchPerTick
// (R236-QA-09). The candidate slice pre-allocates batchPerTick*4, so an
// unbounded value would also blow visit memory; serialised Phase 2
// rename calls plus typical 3 s/rename latency means 100/tick already
// implies ~5 min stall and is the practical operational ceiling.
func TestAutoTitler_BatchPerTickClamp(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  int
		want int
	}{
		{name: "small value passes through", cfg: 5, want: 5},
		{name: "boundary value passes through", cfg: autoTitlerMaxBatchPerTick, want: autoTitlerMaxBatchPerTick},
		{name: "oversized value clamped", cfg: 10_000, want: autoTitlerMaxBatchPerTick},
		{name: "just-over-cap clamped", cfg: autoTitlerMaxBatchPerTick + 1, want: autoTitlerMaxBatchPerTick},
		{name: "zero ignored (default kept)", cfg: 0, want: autoTitlerDefaultBatchPerTick},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, err := newAutoTitler(DaemonDeps{
				Router: wrapRouter(newFakeRouter()),
				Runner: &capturingRunner{},
			})
			if err != nil {
				t.Fatalf("newAutoTitler: %v", err)
			}
			a, ok := d.(*autoTitler)
			if !ok {
				t.Fatalf("newAutoTitler returned %T, want *autoTitler", d)
			}
			cfg := DaemonConfig{}
			if tc.cfg > 0 {
				cfg["batch_per_tick"] = tc.cfg
			}
			if err := a.Configure(cfg); err != nil {
				t.Fatalf("Configure: %v", err)
			}
			if a.batchPerTick != tc.want {
				t.Fatalf("batchPerTick = %d, want %d", a.batchPerTick, tc.want)
			}
		})
	}
}

// TestAutoTitler_MistypedKnobWarnsAndKeepsDefault asserts R250531-ARCH-01
// (#1505): a daemon knob supplied with the wrong dynamic type (e.g. int64
// vs the expected int) must NOT silently retain the default — it logs a
// slog.Warn naming the key + want/got types so the misconfiguration is
// diagnosable from logs. The default value is still retained (we never
// coerce an attacker/operator-controlled wrong type).
func TestAutoTitler_MistypedKnobWarnsAndKeepsDefault(t *testing.T) {
	tests := []struct {
		name    string
		cfg     DaemonConfig
		wantKey string // substring expected in the warn log
	}{
		{
			name:    "min_user_turns int64 instead of int",
			cfg:     DaemonConfig{"min_user_turns": int64(7)},
			wantKey: "min_user_turns",
		},
		{
			name:    "batch_per_tick float64 instead of int",
			cfg:     DaemonConfig{"batch_per_tick": float64(50)},
			wantKey: "batch_per_tick",
		},
		{
			name:    "min_rename_interval int instead of Duration",
			cfg:     DaemonConfig{"min_rename_interval": 30},
			wantKey: "min_rename_interval",
		},
		{
			name:    "include_group_chat string instead of bool",
			cfg:     DaemonConfig{"include_group_chat": "true"},
			wantKey: "include_group_chat",
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
			defer slog.SetDefault(prev)

			d, err := newAutoTitler(DaemonDeps{
				Router: wrapRouter(newFakeRouter()),
				Runner: &capturingRunner{},
			})
			if err != nil {
				t.Fatalf("newAutoTitler: %v", err)
			}
			a := d.(*autoTitler)

			// Snapshot defaults so we can assert the mistyped value did
			// not get applied.
			gotMinTurns := a.minUserTurns
			gotInterval := a.minRenameInterval
			gotBatch := a.batchPerTick
			gotGroup := a.includeGroupChat

			if err := a.Configure(tc.cfg); err != nil {
				t.Fatalf("Configure: %v", err)
			}

			if a.minUserTurns != gotMinTurns || a.minRenameInterval != gotInterval ||
				a.batchPerTick != gotBatch || a.includeGroupChat != gotGroup {
				t.Fatalf("mistyped knob mutated config: %+v", a)
			}

			logged := buf.String()
			if !strings.Contains(logged, "mistyped daemon knob") {
				t.Fatalf("expected mistyped-knob warning, got log: %q", logged)
			}
			if !strings.Contains(logged, tc.wantKey) {
				t.Fatalf("warning missing key %q, got log: %q", tc.wantKey, logged)
			}
		})
	}
}

// TestAutoTitler_CorrectlyTypedKnobsApplyNoWarn is the negative companion
// to TestAutoTitler_MistypedKnobWarnsAndKeepsDefault: correctly-typed
// knobs must apply AND emit no mistyped-knob warning.
func TestAutoTitler_CorrectlyTypedKnobsApplyNoWarn(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	d, err := newAutoTitler(DaemonDeps{
		Router: wrapRouter(newFakeRouter()),
		Runner: &capturingRunner{},
	})
	if err != nil {
		t.Fatalf("newAutoTitler: %v", err)
	}
	a := d.(*autoTitler)

	cfg := DaemonConfig{
		"min_user_turns":      9,
		"min_rename_interval": 42 * time.Minute,
		"batch_per_tick":      7,
		"include_group_chat":  true,
	}
	if err := a.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if a.minUserTurns != 9 {
		t.Fatalf("minUserTurns = %d, want 9", a.minUserTurns)
	}
	if a.minRenameInterval != 42*time.Minute {
		t.Fatalf("minRenameInterval = %v, want 42m", a.minRenameInterval)
	}
	if a.batchPerTick != 7 {
		t.Fatalf("batchPerTick = %d, want 7", a.batchPerTick)
	}
	if !a.includeGroupChat {
		t.Fatalf("includeGroupChat = false, want true")
	}
	if strings.Contains(buf.String(), "mistyped daemon knob") {
		t.Fatalf("unexpected mistyped-knob warning for valid cfg: %q", buf.String())
	}
}
