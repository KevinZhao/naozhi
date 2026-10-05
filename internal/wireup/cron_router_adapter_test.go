// cron_router_adapter_test.go pins the cron.AgentOpts → session.AgentOpts
// translation and the cron.InterruptOutcome / cron.SessionStatus ordinals
// against their session counterparts. Without these tests the init() panic in
// cron_router_adapter.go is the only protection against ordinal drift, and it
// only fires at boot — silent miscasts in CI / sandbox builds slip through.
//
// Moved here from cmd/naozhi with the adapter (R260528-ARCH-23 / #1382).

package wireup

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/session"
)

// TestToSessionAgentOpts_ExtraArgsCloned verifies that mutating the cron-side
// ExtraArgs after toSessionAgentOpts returns does NOT corrupt the session-side
// slice. The aliasing contract in internal/session/router_lifecycle.go:267
// says callers populating AgentOpts must own ExtraArgs exclusively; the
// adapter clones to honour that.
func TestToSessionAgentOpts_ExtraArgsCloned(t *testing.T) {
	t.Parallel()
	cronArgs := []string{"--debug", "--verbose"}
	in := cron.AgentOpts{
		Backend:   "claude",
		Model:     "opus",
		Workspace: "/tmp/x",
		ExtraArgs: cronArgs,
		Exempt:    true,
	}
	out := toSessionAgentOpts(in)
	if len(out.ExtraArgs) != 2 || out.ExtraArgs[0] != "--debug" || out.ExtraArgs[1] != "--verbose" {
		t.Fatalf("ExtraArgs not copied: got %#v", out.ExtraArgs)
	}
	// Mutate the cron source — session copy must stay unchanged.
	cronArgs[0] = "--mutated"
	if out.ExtraArgs[0] != "--debug" {
		t.Errorf("ExtraArgs aliased: out[0] = %q after mutating cron source, want %q",
			out.ExtraArgs[0], "--debug")
	}
}

// TestToSessionAgentOpts_NilExtraArgs ensures a nil/empty cron ExtraArgs
// translates to a nil session ExtraArgs, not an empty non-nil slice that could
// surprise downstream nil checks.
func TestToSessionAgentOpts_NilExtraArgs(t *testing.T) {
	t.Parallel()
	out := toSessionAgentOpts(cron.AgentOpts{Backend: "claude"})
	if out.ExtraArgs != nil {
		t.Errorf("empty ExtraArgs: got %#v, want nil", out.ExtraArgs)
	}
}

// TestToSessionAgentOpts_Effort closes the cron → session half of the
// thinking-effort round-trip. Dropping the field here makes a cron job spawn on
// the backend default instead of the tier configured for its agent — silently,
// since nothing else reads it. The config → cron half is asserted in
// cmd/naozhi's TestBuildAgentOpts. docs/rfc/kiro-effort-control.md
func TestToSessionAgentOpts_Effort(t *testing.T) {
	t.Parallel()
	if got := toSessionAgentOpts(cron.AgentOpts{Backend: "kiro", Effort: "max"}).Effort; got != "max" {
		t.Errorf("Effort = %q, want max", got)
	}
	// Unset stays unset so the backend default applies.
	if got := toSessionAgentOpts(cron.AgentOpts{Backend: "kiro"}).Effort; got != "" {
		t.Errorf("Effort = %q, want empty when the cron opts carry none", got)
	}
}

// TestToSessionAgentOpts_AccessProfile closes the cron → session half of the
// agent access-profile hop (#3106): without it a job for an agent pinned to a
// profile spawns on the default account. The config → cron half is asserted in
// cmd/naozhi's TestBuildAgentOpts.
func TestToSessionAgentOpts_AccessProfile(t *testing.T) {
	t.Parallel()
	if got := toSessionAgentOpts(cron.AgentOpts{Backend: "claude", AccessProfile: "personal"}).AccessProfile; got != "personal" {
		t.Errorf("AccessProfile = %q, want personal", got)
	}
	// Unset stays unset so the router's default_access_profile tier applies.
	if got := toSessionAgentOpts(cron.AgentOpts{Backend: "claude"}).AccessProfile; got != "" {
		t.Errorf("AccessProfile = %q, want empty when the cron opts carry none", got)
	}
}

// TestInterruptOutcome_Ordinals duplicates the init() panic check at test time
// so a divergence is caught by `go test` in CI even before the binary is
// booted. Without this, a refactor that reorders session.InterruptOutcome
// would only fail at first boot of the naozhi binary, which CI does not
// execute.
func TestInterruptOutcome_Ordinals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		c    int
		s    int
	}{
		{"Sent", int(cron.InterruptSent), int(session.InterruptSent)},
		{"NoSession", int(cron.InterruptNoSession), int(session.InterruptNoSession)},
		{"NoTurn", int(cron.InterruptNoTurn), int(session.InterruptNoTurn)},
		{"Unsupported", int(cron.InterruptUnsupported), int(session.InterruptUnsupported)},
		{"Error", int(cron.InterruptError), int(session.InterruptError)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.c != tc.s {
				t.Errorf("%s ordinal diverged: cron=%d, session=%d", tc.name, tc.c, tc.s)
			}
		})
	}
}

// TestInterruptOutcome_CountDrift guards the gap the per-member ordinal table
// (TestInterruptOutcome_Ordinals) and the init() panic cannot catch: a NEW
// case appended to cron.InterruptOutcome or session.InterruptOutcome without
// updating the alignment list (R260528-ARCH-17 / #1378). Adding InterruptFoo
// AFTER InterruptError leaves every existing pair equal — the ordinal table
// still passes — yet one side now has a member the other lacks, which silently
// miscasts in cronSessionAdapter.InterruptViaControl.
//
// We pin two invariants:
//  1. InterruptError is ordinal 4 on BOTH sides (an insert BEFORE Error shifts
//     it and fails here).
//  2. The first ordinal PAST Error (5) is undefined on the session side —
//     session.InterruptOutcome.String() renders it as "unknown(5)". An append
//     AFTER Error on the session side would give 5 a real name and break this.
//     cron has no String(); its append is caught by the count pin below
//     (knownInterruptOutcomes must equal Error+1).
//
// If a fifth real outcome is ever added it MUST be mirrored on both sides AND
// this test bumped deliberately — exactly the human review gate #1378 asks for.
func TestInterruptOutcome_CountDrift(t *testing.T) {
	t.Parallel()

	const lastOrdinal = 4 // InterruptError
	if int(cron.InterruptError) != lastOrdinal {
		t.Errorf("cron.InterruptError ordinal = %d, want %d — a case was inserted before it; mirror on session side and bump this test",
			int(cron.InterruptError), lastOrdinal)
	}
	if int(session.InterruptError) != lastOrdinal {
		t.Errorf("session.InterruptError ordinal = %d, want %d — a case was inserted before it; mirror on cron side and bump this test",
			int(session.InterruptError), lastOrdinal)
	}

	// One past the last known member must be undefined on the session side.
	if got := session.InterruptOutcome(lastOrdinal + 1).String(); got != "unknown(5)" {
		t.Errorf("session.InterruptOutcome(5).String() = %q, want %q — a new outcome was appended after InterruptError; mirror it on cron side and bump this test",
			got, "unknown(5)")
	}

	// Count pin: the known cron members are exactly Sent..Error (5 values).
	// Encoded as a slice so a `go vet`-friendly exhaustive review is forced
	// when the const block grows; len must equal Error+1 (dense iota).
	knownCron := []cron.InterruptOutcome{
		cron.InterruptSent, cron.InterruptNoSession, cron.InterruptNoTurn,
		cron.InterruptUnsupported, cron.InterruptError,
	}
	if len(knownCron) != int(cron.InterruptError)+1 {
		t.Errorf("cron InterruptOutcome count drift: listed %d members but max ordinal is %d — update knownCron and the alignment list in cron_router_adapter.go init()",
			len(knownCron), int(cron.InterruptError))
	}
}

// TestSessionStatus_Cast verifies cron.SessionStatus(int(...)) round-trip
// preserves the four known states. cron does not branch on SessionStatus
// today, but a future caller comparing against SessionExisting /
// SessionResumed / SessionNew / SessionResumeLost relies on the cast staying
// identity.
func TestSessionStatus_Cast(t *testing.T) {
	t.Parallel()
	if int(cron.SessionExisting) != int(session.SessionExisting) {
		t.Errorf("SessionExisting ordinal: cron=%d, session=%d",
			cron.SessionExisting, session.SessionExisting)
	}
	if int(cron.SessionResumed) != int(session.SessionResumed) {
		t.Errorf("SessionResumed ordinal: cron=%d, session=%d",
			cron.SessionResumed, session.SessionResumed)
	}
	if int(cron.SessionNew) != int(session.SessionNew) {
		t.Errorf("SessionNew ordinal: cron=%d, session=%d",
			cron.SessionNew, session.SessionNew)
	}
	if int(cron.SessionResumeLost) != int(session.SessionResumeLost) {
		t.Errorf("SessionResumeLost ordinal: cron=%d, session=%d",
			cron.SessionResumeLost, session.SessionResumeLost)
	}
}

// TestWrapCronSpawnErr_CapacityRefusals: the router's capacity refusals, in the
// wrapped forms reserveSpawn returns, reach cron as ErrSessionCapacity with
// the session sentinel still in the chain; any other spawn error passes
// through untouched so it stays a session_error.
func TestWrapCronSpawnErr_CapacityRefusals(t *testing.T) {
	t.Parallel()
	for _, sentinel := range []error{session.ErrMaxExemptSessions, session.ErrMaxProcs} {
		routerErr := fmt.Errorf("%w: cron namespace (12)", sentinel)
		got := wrapCronSpawnErr(routerErr)
		if !errors.Is(got, cron.ErrSessionCapacity) {
			t.Errorf("%v: errors.Is(ErrSessionCapacity) = false", routerErr)
		}
		if !errors.Is(got, sentinel) {
			t.Errorf("%v: session sentinel lost from the chain", routerErr)
		}
	}
	other := errors.New("spawn boom")
	if got := wrapCronSpawnErr(other); got != other {
		t.Errorf("non-capacity error rewrapped: got %v", got)
	}
}

// TestCronRouterAdapter_GetOrCreateTagsCapacity drives the real router into
// the refusal cron actually meets: every spawn is exempt, so the limit is the
// exempt cap, filled here with live injected cron sessions well past it.
func TestCronRouterAdapter_GetOrCreateTagsCapacity(t *testing.T) {
	r := session.NewRouter(session.RouterConfig{})
	t.Cleanup(r.Shutdown)
	for i := range 64 {
		r.InjectSession(fmt.Sprintf("cron:fill-%d", i), session.NewTestProcess()).MarkExemptForTest()
	}

	_, _, err := newCronRouterAdapter(r).GetOrCreate(context.Background(), "cron:job-cap", cron.AgentOpts{Exempt: true})
	if !errors.Is(err, session.ErrMaxExemptSessions) {
		t.Fatalf("GetOrCreate err = %v, want the router's ErrMaxExemptSessions (test premise)", err)
	}
	if !errors.Is(err, cron.ErrSessionCapacity) {
		t.Errorf("GetOrCreate err = %v, want it tagged cron.ErrSessionCapacity", err)
	}
}

// ReleaseProcess closes an idle cron session's process, keeps the session and
// bumps the list version (so the dashboard re-renders the row as released);
// an unknown key or a running turn is refused without a bump.
func TestCronRouterAdapter_ReleaseProcess(t *testing.T) {
	t.Parallel()
	r := session.NewRouter(session.RouterConfig{MaxProcs: 1})
	t.Cleanup(r.Shutdown)
	a := cronRouterAdapter{r: r}
	const key = "cron:job-adapter-release"

	if a.ReleaseProcess(key) {
		t.Fatal("ReleaseProcess = true for a key with no session")
	}

	running := session.NewTestProcess()
	running.SetState(cli.StateRunning)
	r.InjectSession(key, running).MarkExemptForTest()
	_, v0 := r.ListSessionsWithVersion()
	if a.ReleaseProcess(key) {
		t.Fatal("ReleaseProcess = true while the turn is running")
	}
	if !running.Alive() {
		t.Fatal("a refused release closed the running process")
	}
	if _, v := r.ListSessionsWithVersion(); v != v0 {
		t.Error("a refused release bumped the list version")
	}

	idle := session.NewTestProcess()
	s := r.InjectSession(key, idle)
	s.MarkExemptForTest()
	_, v1 := r.ListSessionsWithVersion()
	if !a.ReleaseProcess(key) {
		t.Fatal("ReleaseProcess refused an idle exempt cron session")
	}
	if idle.Alive() {
		t.Error("released process is still alive")
	}
	if r.SessionFor(key) != s {
		t.Error("release dropped the session")
	}
	if _, v := r.ListSessionsWithVersion(); v == v1 {
		t.Error("release did not bump the list version; the dashboard keeps showing the process alive")
	}
}

// TestCronSessionAdapter_BackendPassesThrough: cron's ledger labels a run with
// the backend the session recorded at spawn, "" included.
func TestCronSessionAdapter_BackendPassesThrough(t *testing.T) {
	t.Parallel()
	r := session.NewRouter(session.RouterConfig{})
	t.Cleanup(r.Shutdown)
	ms := r.InjectSession("cron:job-backend", session.NewTestProcess())
	a := cronSessionAdapter{s: ms}
	if got := a.Backend(); got != "" {
		t.Errorf("Backend before SetBackend = %q, want empty", got)
	}
	ms.SetBackend("kiro")
	if got := a.Backend(); got != "kiro" {
		t.Errorf("Backend = %q, want kiro", got)
	}
}

// TestCronSessionAdapter_CostWindowReachesTheSession: the window cron opens
// through the adapter is the session's, so a result inside it comes back as
// the run's increment and writes no row, and the next one outside it is a row.
func TestCronSessionAdapter_CostWindowReachesTheSession(t *testing.T) {
	t.Parallel()
	r := session.NewRouter(session.RouterConfig{StorePath: filepath.Join(t.TempDir(), "sessions.json")})
	t.Cleanup(r.Shutdown)
	cumulative := []float64{0.4, 1.0}
	proc := session.NewTestProcess()
	proc.SendFunc = func(context.Context, string, []clievent.Attachment, clievent.EventCallback) (*clievent.SendResult, error) {
		res := &clievent.SendResult{Text: "ok", SessionID: "sess-1", CostUSD: cumulative[0]}
		cumulative = cumulative[1:]
		return res, nil
	}
	var a cron.Session = cronSessionAdapter{s: r.InjectSession("cron:job-cost", proc)}
	cw, ok := a.(cron.CostWindow)
	if !ok {
		t.Fatal("cronSessionAdapter does not offer cron.CostWindow")
	}

	cw.BeginCostWindow()
	if _, err := a.Send(context.Background(), "ping"); err != nil {
		t.Fatal(err)
	}
	inc := cw.EndCostWindow()
	if _, err := a.Send(context.Background(), "ping"); err != nil {
		t.Fatal(err)
	}

	if math.Abs(inc.USD-0.4) > 1e-9 {
		t.Errorf("window increment = %v, want 0.4", inc.USD)
	}
	ledger := r.Runs().CostLedger()
	ledger.Close()
	ents, err := ledger.Entries(costledger.Query{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour)}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Source != costledger.SourceSession || math.Abs(ents[0].Amount-0.6) > 1e-9 {
		t.Fatalf("entries = %+v, want one session row of the 0.6 outside the window", ents)
	}
}
