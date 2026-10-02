package server

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/runhistory"
)

// TestSessionRouterView_CLIFactsFromBackends: the adapter's two CLI facts are
// the default backend's, and the version follows a live observation the way
// the dashboard banner needs (R20260612-global-version).
func TestSessionRouterView_CLIFactsFromBackends(t *testing.T) {
	w := cli.NewWrapper("/nonexistent/cli-binary", &cli.ClaudeProtocol{}, "claude")
	w.CLIName = "claude-code"
	w.CLIVersion = "2.1.100"
	r := session.NewRouter(session.RouterConfig{Wrapper: w, MaxProcs: 1})
	t.Cleanup(r.Shutdown)
	v := sessionRouterView{r}

	if got := v.CLIName(); got != "claude-code" {
		t.Errorf("CLIName() = %q, want claude-code", got)
	}
	if got := v.CLIVersion(); got != "2.1.100" {
		t.Errorf("CLIVersion() = %q, want the spawn-time 2.1.100", got)
	}
	w.ObserveLiveVersion("2.1.174")
	if got := v.CLIVersion(); got != "2.1.174" {
		t.Errorf("CLIVersion() after a live observation = %q, want 2.1.174", got)
	}
}

// TestSessionRouterView_RunsFromRunLedger: GET /api/sessions/runs reads
// through the adapter's two run-history forwarders, so they must reach the
// router's real run store with limit and before intact. The dashboard tests
// use a fake router and never run these bodies.
func TestSessionRouterView_RunsFromRunLedger(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	const key = "feishu:direct:alice:general"
	base := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	seed := runhistory.NewStore(datadir.ForStore(storePath).SessionRunsRoot(), 0, 0)
	for i, d := range []int64{100, 200, 600} {
		start := base.Add(time.Duration(i) * time.Hour)
		seed.Append(runhistory.SessionRun{
			RunID: fmt.Sprintf("%016x", i+1), SessionKey: key,
			StartedAt: start, EndedAt: start.Add(time.Duration(d) * time.Millisecond),
			DurationMS: d, Outcome: runhistory.OutcomeCompleted,
		})
	}
	seed.Close()

	w := cli.NewWrapper("/nonexistent/cli-binary", &cli.ClaudeProtocol{}, "claude")
	r := session.NewRouter(session.RouterConfig{Wrapper: w, MaxProcs: 1, StorePath: storePath})
	t.Cleanup(r.Shutdown)
	v := sessionRouterView{r}

	if got := v.SessionRuns(key, 2, time.Time{}); len(got) != 2 || got[0].DurationMS != 600 || got[1].DurationMS != 200 {
		t.Errorf("SessionRuns(limit=2) = %+v, want the two newest (600ms, 200ms)", got)
	}
	if got := v.SessionRuns(key, 10, base.Add(time.Hour)); len(got) != 1 || got[0].DurationMS != 100 {
		t.Errorf("SessionRuns(before=2nd run) = %+v, want only the oldest (100ms)", got)
	}
	st := v.SessionRunStats(key)
	if st.Count != 3 || st.TotalMS != 900 || st.MaxMS != 600 || st.CompletedCnt != 3 {
		t.Errorf("SessionRunStats = %+v, want count=3 total=900 max=600 completed=3", st)
	}
	if want := r.Runs().Stats(key); st != want {
		t.Errorf("SessionRunStats = %+v, want Runs().Stats %+v", st, want)
	}
}
