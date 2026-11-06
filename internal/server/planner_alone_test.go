package server

import (
	"slices"
	"testing"

	"github.com/naozhi/naozhi/internal/planner/agent"
	"github.com/naozhi/naozhi/internal/session"
)

// A scout wake queued behind a running Planner turn is not merged with the
// user's message queued after it: each runs as its own turn, judged by its
// own source, so the user's dispatch is not refused as the scout turn's
// (Q21). In passthrough too: the Planner's requests never share the CLI.
func TestPlannerScoutWakeRunsAlone(t *testing.T) {
	for _, mode := range []string{"collect", "passthrough"} {
		t.Run(mode, func(t *testing.T) {
			h := newParityHarness(t, parityOpts{mode: mode})
			turns := h.session(agent.Key, mode == "passthrough")
			e := h.engine()
			submit := func(src agent.Source, text string) {
				t.Helper()
				if _, err := e.submitPlanner(agent.Key, src, text, nil, session.AgentOpts{}, nil); err != nil {
					t.Fatalf("submit %s: %v", src, err)
				}
			}
			submit(agent.SourceUser, "先看看")
			turns.next(t, "the running turn")
			submit(agent.SourceScout, "巡检发现")
			submit(agent.SourceUser, "派两个任务")
			turns.answer(okTurn("R1"))
			for _, want := range []struct {
				text string
				src  agent.Source
			}{{"巡检发现", agent.SourceScout}, {"派两个任务", agent.SourceUser}} {
				c := turns.next(t, string(want.src)+" turn")
				if c.Text != want.text {
					t.Errorf("turn text = %q, want %q alone", c.Text, want.text)
				}
				if got := e.plannerTurns.sources(agent.Key); !slices.Equal(got, []agent.Source{want.src}) {
					t.Errorf("%q judged by %q, want [%s]", want.text, got, want.src)
				}
				turns.answer(okTurn("R"))
			}
			h.waitEngineIdle()
		})
	}
}
