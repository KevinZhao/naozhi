package sysession

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/session"
)

// snapshotFakeRouter extends fakeRouter so VisitSessions iterates
// through caller-supplied snapshots.  Used by AutoTitler tests to
// exercise the candidate-selection logic without spinning up a real
// session.Router.
type snapshotFakeRouter struct {
	*fakeRouter
	snaps []session.SessionSnapshot

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
		{"caps line length", strings.Repeat("a", autoTitlerLineCapBytes+50) + "\nshort", strings.Repeat("a", autoTitlerLineCapBytes) + "\nshort"},
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

func TestBuildExcerpt_TotalCap(t *testing.T) {
	t.Parallel()
	// Force the excerpt cap by feeding many short lines.
	var sb strings.Builder
	for i := 0; i < 1000; i++ {
		sb.WriteString("hello\n")
	}
	got := buildExcerpt(sb.String())
	if len(got) > autoTitlerExcerptCapBytes {
		t.Errorf("excerpt len %d exceeds cap %d", len(got), autoTitlerExcerptCapBytes)
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
