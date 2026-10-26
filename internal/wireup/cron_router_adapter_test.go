// cron_router_adapter_test.go pins the cron.AgentOpts → session.AgentOpts
// translation and the spawn-error mapping. The cron/session ordinal pairs are
// pinned at compile time in cron_router_adapter.go.
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
	"github.com/naozhi/naozhi/internal/cli/clierr"
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

// TestWrapCronSpawnErr_NamesCLIExitCause: a spawn whose CLI exited in the
// Init handshake with a named cause reaches cron carrying that cause, the
// exit still in the chain; an unrecognised exit passes through.
func TestWrapCronSpawnErr_NamesCLIExitCause(t *testing.T) {
	t.Parallel()
	for _, tc := range cliExitCauses {
		exit := fmt.Errorf("session k: protocol init: acp session/new: %w", &clierr.ProcessExitedError{Code: 1, Class: tc.class})
		err := wrapCronSpawnErr(exit)
		var tf *cron.TurnFailedError
		if !errors.As(err, &tf) || tf.Cause != tc.cause {
			t.Errorf("wrapCronSpawnErr(class %d) = %v, want a TurnFailedError with %q", tc.class, err, tc.cause)
		}
		var pe *clierr.ProcessExitedError
		if !errors.As(err, &pe) || pe.Class != tc.class || errors.Is(err, cron.ErrSessionCapacity) {
			t.Errorf("wrapCronSpawnErr(class %d) = %v, want the exit kept and no capacity tag", tc.class, err)
		}
	}
	unknown := fmt.Errorf("protocol init: %w", &clierr.ProcessExitedError{Code: 1})
	if got := wrapCronSpawnErr(unknown); got != unknown {
		t.Errorf("unrecognised exit rewrapped: got %v", got)
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
