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
				if _, err := e.submitPlanner(agent.Key, src, nil, text, nil, session.AgentOpts{}, nil); err != nil {
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

// A watched session's turn end queued behind a running Planner turn (D138)
// is not merged with the user's message queued after it either: its turn
// runs only note, notify and front, so merged it would refuse the dispatch
// the user asked for; and the user's turn is judged as theirs alone.
func TestPlannerWatchWakeRunsAlone(t *testing.T) {
	for _, src := range []agent.Source{agent.SourceWatch, agent.SourceWatchOutside} {
		t.Run(string(src), func(t *testing.T) {
			h := newParityHarness(t, parityOpts{mode: "collect"})
			turns := h.session(agent.Key, false)
			e := h.engine()
			submit := func(src agent.Source, text string) {
				t.Helper()
				if _, err := e.submitPlanner(agent.Key, src, nil, text, nil, session.AgentOpts{}, nil); err != nil {
					t.Fatalf("submit %s: %v", src, err)
				}
			}
			submit(agent.SourceUser, "先看看")
			turns.next(t, "the running turn")
			submit(src, "<naozhi-watch>")
			submit(agent.SourceUser, "派两个任务")
			turns.answer(okTurn("R1"))
			for _, want := range []struct {
				text  string
				src   agent.Source
				entry string
			}{{"<naozhi-watch>", src, "watch"}, {"派两个任务", agent.SourceUser, ""}} {
				c := turns.next(t, string(want.src)+" turn")
				if c.Text != want.text || c.Source != want.entry {
					t.Errorf("turn = %q with entry source %q, want %q alone with %q", c.Text, c.Source, want.text, want.entry)
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

// A Planner turn's transcript entry carries what started it, so the
// Planner page lays out its timeline without reading the text (§15.14 ②,
// D113): your message and a session you handed over are the user's own
// (""), your answer and every turn the gateway started name their source,
// and a dispatch's first message is the Planner's.
func TestPlannerTurnEntrySource(t *testing.T) {
	h := newParityHarness(t, parityOpts{mode: "collect"})
	turns := h.session(agent.Key, false)
	e := h.engine()
	for _, tc := range []struct {
		src  agent.Source
		want string
	}{
		{agent.SourceUser, ""},
		{agent.SourceHandover, ""},
		{agent.SourceAnswer, "answer"},
		{agent.SourceReceipt, "receipt"},
		{agent.SourceUnrun, "unrun"},
		{agent.SourceScout, "scout"},
		{agent.SourceBriefing, "briefing"},
		{agent.SourceLookup, "lookup"},
		{agent.SourceRefusal, "refusal"},
		{agent.SourceWatch, "watch"},
		{agent.SourceWatchOutside, "watch"},
		{agent.SourceWatchRefusal, "refusal"},
		{agent.SourceShift, "shift"},
	} {
		if _, err := e.submitPlanner(agent.Key, tc.src, nil, "m-"+string(tc.src), nil, session.AgentOpts{}, nil); err != nil {
			t.Fatalf("submit %s: %v", tc.src, err)
		}
		c := turns.next(t, string(tc.src)+" turn")
		if c.Source != tc.want {
			t.Errorf("%s turn: entry source = %q, want %q", tc.src, c.Source, tc.want)
		}
		turns.answer(okTurn("R"))
		h.waitEngineIdle()
	}
}
