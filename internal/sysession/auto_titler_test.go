package sysession

import (
	"context"
	"errors"
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
			a, err := newAutoTitler(DaemonDeps{Router: router, Runner: runner})
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
	a, _ := newAutoTitler(DaemonDeps{Router: router, Runner: runner})
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
	a, _ := newAutoTitler(DaemonDeps{Router: router, Runner: runner})
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
	a, _ := newAutoTitler(DaemonDeps{Router: router, Runner: runner})

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
	entries := []cli.EventEntry{
		{Time: 100, Type: "user", Summary: "第一个问题"},
		{Time: 110, Type: "text", Summary: "助手回复 A"},
		{Time: 120, Type: "thinking", Summary: "思考"},
		{Time: 130, Type: "tool_use", Summary: "Read file"},
		{Time: 200, Type: "user", Summary: "第二个问题"},
		{Time: 210, Type: "text", Summary: "助手回复 B"},
		{Time: 300, Type: "user", Summary: "第三个问题"},
		// Empty user summary should be skipped, not blank-line
		// pollute the output.
		{Time: 310, Type: "user", Summary: "  "},
	}
	got := buildExcerptFromHistory(entries)
	want := "第一个问题\n第二个问题\n第三个问题"
	if got != want {
		t.Errorf("buildExcerptFromHistory:\n got %q\nwant %q", got, want)
	}
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
	a, _ := newAutoTitler(DaemonDeps{Router: router, Runner: runner})
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
	a, _ := newAutoTitler(DaemonDeps{Router: router, Runner: runner})
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
