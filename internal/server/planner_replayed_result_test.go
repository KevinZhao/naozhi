package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/planner/agent"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// TestPlannerResultAcrossRestart: a Planner turn the shim carried over a
// restart reaches the result sink exactly once, whether it ended while
// naozhi was down (its result is in the backlog) or after the reconnect but
// before the sink was set; a backlog ending in the previous turn's result,
// delivered by the last run, is not handed on again.
func TestPlannerResultAcrossRestart(t *testing.T) {
	const (
		initLine = `{"type":"system","subtype":"init","session_id":"s1"}`
		toolUse  = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}]},"session_id":"s1"}`
		result   = `{"type":"result","subtype":"success","result":"done","session_id":"s1"}`
	)
	mark := func(seq int64) string { return cli.TurnWatermark{ShimPID: session.FakeShimPID, Seq: seq}.String() }
	// Backlogs start at seq 10, so a result in one sits at seq 11.
	tests := []struct {
		name    string
		backlog []string
		live    string // emitted after the reconnect, before the sink is set
		after   string // the carried-over request's watermark
		want    []string
	}{
		{"turn ended while down", []string{initLine, result}, "", mark(10), []string{"done"}},
		{"turn ended before the sink was set", []string{initLine, toolUse}, result, mark(10), []string{"done"}},
		{"backlog ends in the previous turn's result", []string{initLine, result}, "", mark(11), nil},
		{"request carried no watermark", []string{initLine, result}, "", "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := session.StartFakeShimForTest(t, agent.Key, 10, tc.backlog)
			r := session.NewRouter(fs.Config)
			t.Cleanup(r.Shutdown)
			r.ReconnectShimsCtx(context.Background())
			fs.Attached(t)
			if tc.live != "" {
				fs.Emit(t, tc.live)
				testhelper.Eventually(t, func() bool {
					p, _ := r.AdoptInFlight(agent.Key, cli.TurnWatermark{}, false)
					return p != nil && !p.AdoptedTurnPending()
				}, 5*time.Second, "the live result never reached the process")
			}

			path := filepath.Join(t.TempDir(), "turns.json")
			ledger, _ := json.Marshal(plannerTurnsFile{V: 1, Turns: []plannerTurn{
				{Key: agent.Key, Src: agent.SourceUser, Began: time.Now(), After: tc.after},
			}})
			if err := os.WriteFile(path, ledger, 0o600); err != nil {
				t.Fatal(err)
			}
			e := newSendEngine(sendEngineOpts{Router: r, PlannerTurnsPath: path})

			var got []string
			r.SetResultSink(func(key, text string) {
				if key == agent.Key {
					got = append(got, text)
				}
			})
			if !slices.Equal(got, tc.want) {
				t.Errorf("results = %q, want %q", got, tc.want)
			}
			e.plannerTurns.redeliver(e.deliverReplayed)
			if !slices.Equal(got, tc.want) {
				t.Errorf("after a second pass: results = %q, want %q", got, tc.want)
			}
		})
	}
}
