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

// TestBuildExcerpt_MarkerPlaceholderAtomicUnderCap pins R202606d-CR-001: the
// inert [EXCERPT_MARKER] placeholder must be emitted atomically with respect
// to the per-line cap. The previous shape emitted the 16-byte placeholder one
// rune at a time via writeRune, so when lineWritten was within <16 bytes of
// autoTitlerLineCapBytes a marker hit would leave a half-placeholder like
// "[EXCERPT_MARKE…" in the output — an incomplete token that can confuse the
// LLM. The output must contain EITHER the whole placeholder OR a clean
// ellipsis, never a partial placeholder fragment.
func TestBuildExcerpt_MarkerPlaceholderAtomicUnderCap(t *testing.T) {
	t.Parallel()

	// assertNoPartialPlaceholder fails if any proper non-empty prefix of the
	// placeholder (other than the full placeholder itself) appears in out.
	assertNoPartialPlaceholder := func(t *testing.T, out string) {
		t.Helper()
		// Strip every full placeholder occurrence; any remaining prefix is a
		// genuine partial leak.
		stripped := strings.ReplaceAll(out, excerptMarkerSafe, "")
		for n := len(excerptMarkerSafe) - 1; n >= 2; n-- {
			frag := excerptMarkerSafe[:n] // e.g. "[EXCERPT_MARKE"
			if strings.Contains(stripped, frag) {
				t.Errorf("partial placeholder fragment %q leaked into output: %q", frag, out)
				return
			}
		}
	}

	// Case A: pad so the marker is hit with the cap only a few bytes away.
	// len(placeholder)=16; pad to cap-5 means 16 won't fit (507+16>512) and the
	// emit must collapse to a single ellipsis, NOT "[EXCE…".
	padTooTight := strings.Repeat("a", autoTitlerLineCapBytes-5)
	gotTight := buildExcerpt(padTooTight + excerptBeginMarker + " tail")
	assertNoPartialPlaceholder(t, gotTight)
	if strings.Contains(gotTight, excerptBeginMarker) {
		t.Errorf("raw BEGIN marker survived: %q", gotTight)
	}

	// Case B: pad so the whole placeholder fits exactly at the cap
	// (lineWritten + 16 == 512). The full placeholder MUST appear.
	padExact := strings.Repeat("a", autoTitlerLineCapBytes-len(excerptMarkerSafe))
	gotExact := buildExcerpt(padExact + excerptEndMarker + " tail")
	assertNoPartialPlaceholder(t, gotExact)
	if !strings.Contains(gotExact, excerptMarkerSafe) {
		t.Errorf("full placeholder should fit exactly at cap but was missing: %q", gotExact)
	}

	// Case C: marker one byte beyond the fit boundary (lineWritten + 16 == 513)
	// must collapse to ellipsis with no fragment.
	padOneOver := strings.Repeat("a", autoTitlerLineCapBytes-len(excerptMarkerSafe)+1)
	gotOneOver := buildExcerpt(padOneOver + excerptBeginMarker + " tail")
	assertNoPartialPlaceholder(t, gotOneOver)
	if strings.Contains(gotOneOver, excerptBeginMarker) {
		t.Errorf("raw BEGIN marker survived at one-over boundary: %q", gotOneOver)
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
			name: "single-turn session is renamed (min_first_turns=1)",
			snap: session.SessionSnapshot{
				Key: "feishu:direct:u1:general", MessageCount: 1,
				LastPrompt: "帮我查个 bug", LastActive: now.UnixMilli(),
			},
			runnerResp: "查 bug",
		},
		{
			name: "below min first turns",
			snap: session.SessionSnapshot{
				Key: "feishu:direct:u1:general", MessageCount: 0,
				LastPrompt: "msg",
			},
			wantBucket: "min_first_turns",
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
			"REMINDER: Output one line, majority Chinese",
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

// TestAutoTitler_PromptRecognizabilityContract pins the recognizability
// redesign (#2115). The system prompt must instruct the model to (a) keep
// identifying tokens verbatim, (b) drop the current project/repo name, (c)
// cap the title at 24 chars, and (d) fall back to 未命名会话 ONLY for
// no-usable-text input. These are the wording changes that lifted titles
// from generic phrases ("自动命名失败排查") to distinguishing ones
// ("auto-titler 命名失败"). A future "tidy-up" that re-introduces the old
// "Han characters and Arabic digits only" / ≤16 / broad-fallback wording
// would silently regress naming quality with no test failure otherwise.
func TestAutoTitler_PromptRecognizabilityContract(t *testing.T) {
	t.Parallel()
	router := newSnapshotFakeRouter([]session.SessionSnapshot{
		{Key: "feishu:direct:u1:general", MessageCount: 5, LastPrompt: "auto-titler 命名失败"},
	})
	captured := make(chan string, 1)
	runner := &capturingRunner{captured: captured, resp: "auto-titler 命名失败"}
	a, _ := newAutoTitler(DaemonDeps{Router: wrapRouter(router), Runner: runner})
	if _, err := a.Tick(context.Background()); err != nil {
		t.Fatalf("Tick err: %v", err)
	}
	select {
	case prompt := <-captured:
		for _, want := range []string{
			"majority Chinese characters",
			"real, recognizable proper-noun identifiers",
			"Do NOT include the current project name or repository name",
			"at most 24 characters",
			"no usable topic",
		} {
			if !strings.Contains(prompt, want) {
				t.Errorf("prompt missing recognizability instruction %q\nfull:\n%s", want, prompt)
			}
		}
		// The old over-constraining wording must be gone — its presence is
		// exactly what suppressed proper nouns and tripped the broad
		// fallback. Guard against an accidental revert.
		for _, gone := range []string{
			"Han characters and Arabic digits only",
			"≤16 Chinese characters",
			"off-topic, or impossible to summarize",
		} {
			if strings.Contains(prompt, gone) {
				t.Errorf("prompt still contains retired over-constraint %q — recognizability regression", gone)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("runner.Run was not invoked")
	}
}

// TestAutoTitler_TitleRuneCeiling pins the autoTitlerMaxTitleRunes gate at
// its post-#2115 value (24) AND the clip-on-overflow behaviour. A title at
// or under the ceiling is written verbatim; an over-length title is clipped
// to the ceiling on a rune boundary and STILL written (not rejected) — the
// redesign instructs the model to keep rune-dense ASCII identifiers, so an
// overshoot must degrade to a usable label rather than a silent no-rename.
// Mixed CJK+ASCII is used because the gate counts runes, not bytes.
func TestAutoTitler_TitleRuneCeiling(t *testing.T) {
	t.Parallel()
	const key = "feishu:direct:u1:general"
	cases := []struct {
		name      string
		title     string
		wantLabel string // exact label expected to be written
	}{
		{"exactly at ceiling kept", strings.Repeat("a", autoTitlerMaxTitleRunes), strings.Repeat("a", autoTitlerMaxTitleRunes)},
		{"cjk at ceiling kept", strings.Repeat("好", autoTitlerMaxTitleRunes), strings.Repeat("好", autoTitlerMaxTitleRunes)},
		{"mixed under ceiling kept", "auto-titler 命名失败排查", "auto-titler 命名失败排查"},
		{"ascii over ceiling clipped", strings.Repeat("a", autoTitlerMaxTitleRunes+5), strings.Repeat("a", autoTitlerMaxTitleRunes)},
		{"cjk over ceiling clipped", strings.Repeat("好", autoTitlerMaxTitleRunes+5), strings.Repeat("好", autoTitlerMaxTitleRunes)},
		// R202606e-PERF-004: single-pass TruncateRunesNoEllipsis must clip a
		// mixed ASCII+multibyte title on the SAME rune boundary the prior
		// string([]rune(..)[:N]) slice produced. Here the 24th rune is a CJK
		// codepoint, so a byte-naive cut would split it; the rune-aware
		// stream must land exactly after the 24th rune (10 ASCII + 14 CJK).
		{"mixed over ceiling clipped on rune boundary",
			strings.Repeat("a", 10) + strings.Repeat("好", autoTitlerMaxTitleRunes),
			strings.Repeat("a", 10) + strings.Repeat("好", autoTitlerMaxTitleRunes-10)},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			router := newSnapshotFakeRouter([]session.SessionSnapshot{
				{Key: key, MessageCount: 5, LastPrompt: "msg"},
			})
			runner := &fakeRunner{resp: c.title}
			a, _ := newAutoTitler(DaemonDeps{Router: wrapRouter(router), Runner: runner})
			rep, err := a.Tick(context.Background())
			if err != nil {
				t.Fatalf("Tick err = %v, want nil (clip, not reject)", err)
			}
			if rep.Acted != 1 {
				t.Fatalf("Acted = %d, want 1 (title %q must be clipped+written)", rep.Acted, c.title)
			}
			got := router.fakeRouter.label(key)
			if got != c.wantLabel {
				t.Errorf("written label = %q, want %q", got, c.wantLabel)
			}
			if n := len([]rune(got)); n > autoTitlerMaxTitleRunes {
				t.Errorf("written label is %d runes, exceeds ceiling %d", n, autoTitlerMaxTitleRunes)
			}
		})
	}
}

// TestAutoTitler_KeepsAsciiIdentifierTitle locks the core recognizability
// win: a title containing verbatim ASCII identifiers (component + file +
// error tokens) must pass the full renameOne pipeline (ValidateUserLabel +
// the rune ceiling) and be written with origin=auto. Pre-#2115 the prompt
// forbade non-Han characters; even though ValidateUserLabel always allowed
// ASCII, the prompt suppressed it. This test asserts the pipeline itself
// admits such titles so the prompt change isn't silently undone by a
// validator tightening later.
func TestAutoTitler_KeepsAsciiIdentifierTitle(t *testing.T) {
	t.Parallel()
	const key = "feishu:direct:u1:general"
	router := newSnapshotFakeRouter([]session.SessionSnapshot{
		{Key: key, MessageCount: 5, LastPrompt: "NLB 拿不到真实 IP"},
	})
	runner := &fakeRunner{resp: "NLB 拿不到客户端真实IP"}
	a, _ := newAutoTitler(DaemonDeps{Router: wrapRouter(router), Runner: runner})
	rep, err := a.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick err = %v", err)
	}
	if rep.Acted != 1 {
		t.Fatalf("Acted = %d, want 1", rep.Acted)
	}
	if got := router.fakeRouter.label(key); got != "NLB 拿不到客户端真实IP" {
		t.Errorf("written label = %q, want the verbatim ASCII-bearing title", got)
	}
}

// TestAutoTitler_ClipTrimsTrailingSpace covers the clip boundary case where
// the rune-slice cut lands right after a space (#2115). Without the
// post-clip TrimSpace the published label would carry a trailing blank,
// which ValidateUserLabel tolerates (it only trims surrounding whitespace
// on entry, not after our slice) and which renders as a ragged sidebar
// title. Build a title whose 24th rune is a space so the naive slice would
// end in " ".
func TestAutoTitler_ClipTrimsTrailingSpace(t *testing.T) {
	t.Parallel()
	const key = "feishu:direct:u1:general"
	// 23 non-space runes + a space at rune index 23 (the 24th rune), then
	// more text that pushes past the ceiling. Slicing to 24 runes yields
	// "...<space>"; TrimSpace must drop it.
	title := strings.Repeat("好", 23) + " 多余的尾巴内容"
	if len([]rune(title)) <= autoTitlerMaxTitleRunes {
		t.Fatalf("test misconfigured: title is %d runes, need > %d", len([]rune(title)), autoTitlerMaxTitleRunes)
	}
	router := newSnapshotFakeRouter([]session.SessionSnapshot{
		{Key: key, MessageCount: 5, LastPrompt: "msg"},
	})
	runner := &fakeRunner{resp: title}
	a, _ := newAutoTitler(DaemonDeps{Router: wrapRouter(router), Runner: runner})
	rep, err := a.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick err = %v", err)
	}
	if rep.Acted != 1 {
		t.Fatalf("Acted = %d, want 1", rep.Acted)
	}
	got := router.fakeRouter.label(key)
	want := strings.Repeat("好", 23) // trailing space trimmed
	if got != want {
		t.Errorf("clipped label = %q, want %q (trailing space must be trimmed)", got, want)
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

// TestAutoTitler_FirstTitleUsesMinFirstTurns pins the two-threshold split:
// the FIRST title is gated by minFirstTurns (default 1), independent of the
// larger minUserTurns re-title throttle. A short session at exactly
// minFirstTurns turns — but below minUserTurns — must still be titled.
func TestAutoTitler_FirstTitleUsesMinFirstTurns(t *testing.T) {
	t.Parallel()
	// minFirstTurns=2, minUserTurns=5: a 2-turn session is below the
	// re-title throttle (5) but at/above the first-title floor (2), so it
	// must be renamed on the first tick. If gate 4 had used minUserTurns
	// (the old behaviour) this session would be skipped.
	snap := session.SessionSnapshot{
		Key: "feishu:direct:u1:general", MessageCount: 2,
		LastPrompt: "帮我看下登录报错", LastActive: time.Now().UnixMilli(),
	}
	router := newSnapshotFakeRouter([]session.SessionSnapshot{snap})
	runner := &fakeRunner{resp: "登录报错排查"}
	a, err := newAutoTitler(DaemonDeps{Router: wrapRouter(router), Runner: runner})
	if err != nil {
		t.Fatalf("newAutoTitler: %v", err)
	}
	if err := a.(Configurable).Configure(DaemonConfig{
		"min_first_turns": 2,
		"min_user_turns":  5,
	}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	rep, err := a.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick err = %v", err)
	}
	if rep.Acted != 1 {
		t.Fatalf("Acted = %d, want 1; report=%+v", rep.Acted, rep)
	}
	if rep.Skipped["min_user_turns"] != 0 || rep.Skipped["no_new_turns"] != 0 {
		t.Fatalf("first title must not hit the re-title throttle: %v", rep.Skipped)
	}
}

// TestAutoTitler_BelowMinFirstTurnsSkipped is the negative companion: a
// session with fewer turns than minFirstTurns is skipped under the
// min_first_turns bucket (never the legacy min_user_turns bucket).
func TestAutoTitler_BelowMinFirstTurnsSkipped(t *testing.T) {
	t.Parallel()
	snap := session.SessionSnapshot{
		Key: "feishu:direct:u1:general", MessageCount: 1,
		LastPrompt: "hi", LastActive: time.Now().UnixMilli(),
	}
	router := newSnapshotFakeRouter([]session.SessionSnapshot{snap})
	runner := &fakeRunner{resp: "x"}
	a, err := newAutoTitler(DaemonDeps{Router: wrapRouter(router), Runner: runner})
	if err != nil {
		t.Fatalf("newAutoTitler: %v", err)
	}
	if err := a.(Configurable).Configure(DaemonConfig{"min_first_turns": 3}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	rep, err := a.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick err = %v", err)
	}
	if rep.Acted != 0 {
		t.Fatalf("Acted = %d, want 0", rep.Acted)
	}
	if rep.Skipped["min_first_turns"] == 0 {
		t.Fatalf("expected min_first_turns skip, got %v", rep.Skipped)
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
	// The result must stay within the soft cap exactly.
	if len(got) > autoTitlerExcerptSoftCapBytes {
		t.Errorf("buildExcerptFromHistory exceeded soft cap: got %d bytes, max %d", len(got), autoTitlerExcerptSoftCapBytes)
	}
	// Truncation marker must be present so downstream review can spot
	// the cut.  Confirms the break-on-cap branch fired.
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected truncation ellipsis at tail, got tail %q", got[max(0, len(got)-32):])
	}
}

// TestBuildExcerptFromHistory_NoEarlyCap locks #1586: the per-entry "need"
// must NOT pre-charge the 3-byte "…" marker, which previously fired the cap
// ~one entry early for every entry that fit on its own.  We fill the buffer
// with entries whose newline-joined bytes land just under the cap and assert
// that every fitting entry is appended in full — i.e. truncation does NOT
// trigger when the real content (entries + joining newlines) fits.
func TestBuildExcerptFromHistory_NoEarlyCap(t *testing.T) {
	t.Parallel()
	const cap = autoTitlerExcerptSoftCapBytes
	// 100-byte entries, the typical-turn size called out in the issue.
	const perEntry = 100
	chunk := strings.Repeat("a", perEntry)
	// Each appended entry costs perEntry bytes plus one joining newline
	// (except the first). Pick a count whose total content fits strictly
	// under the cap so NOTHING should be truncated.
	//   total = n*perEntry + (n-1) newlines = n*(perEntry+1) - 1
	// Solve for the largest n with total <= cap, then back off by a few
	// to stay comfortably inside.
	count := cap/(perEntry+1) - 4
	if count <= 0 {
		t.Fatalf("test misconfigured: count=%d", count)
	}
	entries := make([]SystemEventEntry, 0, count)
	for i := 0; i < count; i++ {
		entries = append(entries, SystemEventEntry{Type: "user", Summary: chunk})
	}
	got := buildExcerptFromHistory(entries)
	// Expected full content: all entries joined by newlines, no marker.
	wantLen := count*perEntry + (count - 1)
	if len(got) != wantLen {
		t.Errorf("expected all %d entries appended (len %d), got len %d", count, wantLen, len(got))
	}
	if strings.HasSuffix(got, "…") {
		t.Errorf("cap fired early: content fit under cap but result was truncated")
	}
	// Sanity: confirm we genuinely exercised the regime where the OLD
	// per-entry 3-byte reserve would have truncated. The old need formula
	// added 3 bytes per entry, so old projected size was wantLen + 3*count,
	// which must exceed the cap for this test to be meaningful.
	if wantLen+3*count <= cap {
		t.Fatalf("test not exercising the early-cap regime: wantLen=%d count=%d cap=%d", wantLen, count, cap)
	}
}

// TestBuildExcerptFromHistory_SoftCapBoundary locks the hard upper bound: the
// builder must never write past autoTitlerExcerptSoftCapBytes, and when an
// entry would push past the cap the result is truncated with a visible "…".
// First entry fills the buffer to exactly cap; the second entry cannot fit
// (newline+byte would overflow) so truncation fires.  Because the buffer is
// already at the cap there is no room for the "\n…" marker, so the builder
// omits it rather than overshoot — the cap is a hard bound (#1586).
func TestBuildExcerptFromHistory_SoftCapBoundary(t *testing.T) {
	t.Parallel()

	const cap = autoTitlerExcerptSoftCapBytes
	// First entry fills the buffer to exactly cap bytes.
	fill := strings.Repeat("x", cap)
	// Second entry would require newline + byte = 2 more bytes, overflowing.
	entries := []SystemEventEntry{
		{Type: "user", Summary: fill},
		{Type: "user", Summary: "y"},
	}

	got := buildExcerptFromHistory(entries)

	if len(got) > cap {
		t.Errorf("result length %d exceeds soft cap %d", len(got), cap)
	}
	// Buffer was at the cap, so the marker is correctly omitted to stay
	// within bounds; only the first entry survives.
	if len(got) != cap {
		t.Errorf("expected result pinned at cap %d, got %d", cap, len(got))
	}
}

// TestBuildExcerptFromHistory_SoftCapBoundaryWithMarker verifies that when the
// buffer has room for the "\n…" marker at truncation time, the marker IS
// written and the result still stays within the cap.
func TestBuildExcerptFromHistory_SoftCapBoundaryWithMarker(t *testing.T) {
	t.Parallel()

	const cap = autoTitlerExcerptSoftCapBytes
	// First entry fills to cap-8, leaving 8 bytes of headroom.
	fill := strings.Repeat("x", cap-8)
	// Second entry is 16 bytes: newline+16 = 17 > 8 headroom → truncates.
	// At truncation sb.Len()=cap-8, marker "\n…" is 4 bytes, fits within cap.
	entries := []SystemEventEntry{
		{Type: "user", Summary: fill},
		{Type: "user", Summary: strings.Repeat("y", 16)},
	}

	got := buildExcerptFromHistory(entries)

	if len(got) > cap {
		t.Errorf("result length %d exceeds soft cap %d", len(got), cap)
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
			name:    "min_first_turns int64 instead of int",
			cfg:     DaemonConfig{"min_first_turns": int64(2)},
			wantKey: "min_first_turns",
		},
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
			gotFirstTurns := a.minFirstTurns
			gotMinTurns := a.minUserTurns
			gotInterval := a.minRenameInterval
			gotBatch := a.batchPerTick
			gotGroup := a.includeGroupChat

			if err := a.Configure(tc.cfg); err != nil {
				t.Fatalf("Configure: %v", err)
			}

			if a.minFirstTurns != gotFirstTurns || a.minUserTurns != gotMinTurns ||
				a.minRenameInterval != gotInterval ||
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
		"min_first_turns":     2,
		"min_user_turns":      9,
		"min_rename_interval": 42 * time.Minute,
		"batch_per_tick":      7,
		"include_group_chat":  true,
	}
	if err := a.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if a.minFirstTurns != 2 {
		t.Fatalf("minFirstTurns = %d, want 2", a.minFirstTurns)
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

// TestAutoTitler_CtxCancelPreservesFirstErr pins R053116-CR-4: when the first
// renameOne in a batch fails with an upstream error (firstErr ≠ nil) and ctx
// is cancelled before the second candidate is processed, Tick MUST return
// firstErr — not ctx.Err(). The upstream error must reach classifyError so it
// counts toward the circuit breaker; returning context.Canceled would silently
// mis-classify the failure as DaemonErrorClassCanceled and hide it from the
// breaker.
//
// Scenario:
//   - 2 candidates in the batch (2 sessions with enough turns)
//   - runner.Run returns a non-validation error on the first call (→ firstErr)
//     and also cancels the context after doing so
//   - ctx.Err() is non-nil before the second iteration
//   - Tick must return firstErr, not context.Canceled
func TestAutoTitler_CtxCancelPreservesFirstErr(t *testing.T) {
	t.Parallel()

	// errUpstream simulates a real upstream runner failure (not wrapped in
	// ErrValidation) so classifyError yields DaemonErrorClassUpstream.
	errUpstream := errors.New("runner: model overloaded")

	ctx, cancel := context.WithCancel(context.Background())

	// cancelAfterFirstRunner cancels ctx on the first Run call, then
	// returns errUpstream so the caller records it as firstErr.
	// On any subsequent call it would return ctx.Err(), but the
	// ctx.Err() check at the top of the loop fires before a second
	// Run, so only one call should ever happen.
	cancellingRunner := &cancelAfterFirstRunnerHelper{
		cancel:    cancel,
		errReturn: errUpstream,
	}

	snap1 := session.SessionSnapshot{
		Key: "feishu:direct:u1:general", MessageCount: 5,
		LastPrompt: "first prompt",
	}
	snap2 := session.SessionSnapshot{
		Key: "feishu:direct:u2:general", MessageCount: 5,
		LastPrompt: "second prompt",
	}
	router := newSnapshotFakeRouter([]session.SessionSnapshot{snap1, snap2})

	a, err := newAutoTitler(DaemonDeps{Router: wrapRouter(router), Runner: cancellingRunner})
	if err != nil {
		t.Fatalf("newAutoTitler: %v", err)
	}

	_, tickErr := a.Tick(ctx)

	if tickErr == nil {
		t.Fatal("Tick returned nil error; expected upstream error")
	}
	// The returned error MUST be the upstream error, not context.Canceled.
	if errors.Is(tickErr, context.Canceled) {
		t.Fatalf("Tick returned context.Canceled; want upstream firstErr — R053116-CR-4 fix may be missing")
	}
	if !errors.Is(tickErr, errUpstream) {
		t.Fatalf("Tick returned %v; want errUpstream (%v) — R053116-CR-4 fix may be missing", tickErr, errUpstream)
	}
}

// cancelAfterFirstRunnerHelper cancels its context on the first Run call and
// returns a caller-supplied error.  Subsequent calls (should not occur in the
// test scenario) return context.Canceled to surface any accidental second call.
type cancelAfterFirstRunnerHelper struct {
	cancel    context.CancelFunc
	errReturn error
	called    atomic.Int32
}

func (c *cancelAfterFirstRunnerHelper) Run(_ context.Context, _ string) (string, error) {
	n := c.called.Add(1)
	if n == 1 {
		c.cancel() // cancel the outer ctx so next iteration's ctx.Err() fires
		return "", c.errReturn
	}
	return "", context.Canceled
}
