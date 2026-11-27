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
	"reflect"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/session"
)

// TestToSessionAgentOpts: every field crosses the cron -> session hop;
// dropping one makes a cron job spawn unlike any other session of its agent,
// silently: SystemPrompt (#2493), Effort (docs/rfc/kiro-effort-control.md),
// AccessProfile (#3106, else the default account). The config -> cron half is
// asserted in cmd/naozhi's TestBuildAgentOpts. ExtraArgs is cloned (the router
// treats AgentOpts.ExtraArgs as exclusively owned) and stays nil when empty.
func TestToSessionAgentOpts(t *testing.T) {
	t.Parallel()
	cronArgs := []string{"--debug", "--verbose"}
	out := toSessionAgentOpts(cron.AgentOpts{Backend: "kiro", Model: "opus", Workspace: "/tmp/x", Effort: "max",
		SystemPrompt: "You are a code review expert.", AccessProfile: "personal", ExtraArgs: cronArgs, Exempt: true})
	want := session.AgentOpts{Backend: "kiro", Model: "opus", Workspace: "/tmp/x", Effort: "max",
		SystemPrompt: "You are a code review expert.", AccessProfile: "personal", ExtraArgs: []string{"--debug", "--verbose"}, Exempt: true}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("toSessionAgentOpts = %+v, want %+v", out, want)
	}
	cronArgs[0] = "--mutated"
	if out.ExtraArgs[0] != "--debug" {
		t.Errorf("ExtraArgs aliased: out[0] = %q after mutating the cron source", out.ExtraArgs[0])
	}
	if got := toSessionAgentOpts(cron.AgentOpts{Backend: "claude"}); !reflect.DeepEqual(got, session.AgentOpts{Backend: "claude"}) {
		t.Errorf("unset opts = %+v, want only the backend (nil ExtraArgs, the rest left to the router's defaults)", got)
	}
}

// TestWrapCronSpawnErr: the router's capacity refusals, in the wrapped forms
// reserveSpawn returns, reach cron as ErrSessionCapacity with the session
// sentinel still in the chain; a CLI exit in the Init handshake with a named
// cause reaches cron carrying that cause (the classes: TestExitFailure), the
// exit kept; any other spawn error passes through untouched so it stays a
// session_error.
func TestWrapCronSpawnErr(t *testing.T) {
	t.Parallel()
	for _, sentinel := range []error{session.ErrMaxExemptSessions, session.ErrMaxProcs} {
		routerErr := fmt.Errorf("%w: cron namespace (12)", sentinel)
		if got := wrapCronSpawnErr(routerErr); !errors.Is(got, cron.ErrSessionCapacity) || !errors.Is(got, sentinel) {
			t.Errorf("%v: got %v, want ErrSessionCapacity with the session sentinel kept", routerErr, got)
		}
	}
	exit := fmt.Errorf("session k: protocol init: acp session/new: %w", &clierr.ProcessExitedError{Code: 1, Class: clierr.ExitAuth})
	err := wrapCronSpawnErr(exit)
	var tf *cron.TurnFailedError
	var pe *clierr.ProcessExitedError
	if !errors.As(err, &tf) || tf.Cause != cron.TurnCauseBackendAuth || !errors.As(err, &pe) || errors.Is(err, cron.ErrSessionCapacity) {
		t.Errorf("wrapCronSpawnErr(auth exit) = %v, want a TurnFailedError with the exit kept and no capacity tag", err)
	}
	for _, other := range []error{errors.New("spawn boom"), fmt.Errorf("protocol init: %w", &clierr.ProcessExitedError{Code: 1})} {
		if got := wrapCronSpawnErr(other); got != other {
			t.Errorf("wrapCronSpawnErr(%v) rewrapped: got %v", other, got)
		}
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
