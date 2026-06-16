package dispatch

// #2148: a synthetic message (Feishu AskUserQuestion card click) carries an
// explicit AgentID with no /agent prefix in its text. prepareInbound must
// route it to that agent's session — NOT default to "general" — and only when
// the agent id is in the known-agent whitelist.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
)

func newAgentRouteDispatcher(t *testing.T) *Dispatcher {
	t.Helper()
	fp := &fakePlatform{}
	d, err := NewDispatcher(DispatcherConfig{
		Router:        session.NewRouter(session.RouterConfig{MaxProcs: 10}),
		Platforms:     map[string]platform.Platform{"fake": fp},
		Agents:        map[string]session.AgentOpts{},
		AgentCommands: map[string]string{"review": "code-reviewer"},
		Guard:         newFakeGuard(),
		Dedup:         platform.NewDedup(100),
		SendFn: func(_ context.Context, _ string, _ *session.ManagedSession, _ string, _ []cli.ImageData, _ cli.EventCallback) (*cli.SendResult, error) {
			return &cli.SendResult{Text: "ok"}, nil
		},
		TakeoverFn:            func(_ context.Context, _, _ string, _ session.AgentOpts) bool { return false },
		WatchdogNoOutputKills: new(atomic.Int64),
		WatchdogTotalKills:    new(atomic.Int64),
		NoOutputTimeout:       5 * time.Second,
		TotalTimeout:          30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	return d
}

func TestIsKnownAgent(t *testing.T) {
	t.Parallel()
	d := newAgentRouteDispatcher(t)
	cases := map[string]bool{
		"general":        true,
		"planner":        true,
		"code-reviewer":  true,  // reachable via the "review" command
		"security":       false, // not configured
		"":               false,
		"../etc/passwd":  false,
		"GENERAL":        false, // case-sensitive; agent ids are lowercase
		"code-reviewer ": false, // trailing space is not the canonical id
	}
	for id, want := range cases {
		if got := d.isKnownAgent(id); got != want {
			t.Errorf("isKnownAgent(%q) = %v, want %v", id, got, want)
		}
	}
}

// TestPrepareInbound_ExplicitAgentIDRoutesToAsker pins the #2148 fix: a card
// answer (no /agent prefix) with AgentID="code-reviewer" must resolve to the
// code-reviewer agent and a session key embedding it.
func TestPrepareInbound_ExplicitAgentIDRoutesToAsker(t *testing.T) {
	t.Parallel()
	d := newAgentRouteDispatcher(t)
	msg := platform.IncomingMessage{
		Platform: "fake", EventID: "evt-ans1",
		UserID: "u1", ChatID: "c1", ChatType: "direct",
		Text:      "Error style: Return an error.", // composeAskAnswerText shape, no /agent prefix
		MentionMe: true,
		AgentID:   "code-reviewer",
	}
	p, ok := d.prepareInbound(context.Background(), msg)
	if !ok {
		t.Fatal("prepareInbound dropped the card answer; want accepted")
	}
	if p.agentID != "code-reviewer" {
		t.Errorf("agentID = %q, want code-reviewer (#2148 misrouted to general)", p.agentID)
	}
	if want := "fake:direct:c1:code-reviewer"; p.key != want {
		t.Errorf("session key = %q, want %q", p.key, want)
	}
	// cleanText must be the full answer text — the override only swaps the
	// agent, it does not strip any prefix (there is none).
	if p.cleanText != msg.Text {
		t.Errorf("cleanText = %q, want %q", p.cleanText, msg.Text)
	}
}

// TestPrepareInbound_UnknownExplicitAgentIDFallsBack verifies an unknown /
// hostile AgentID is ignored: routing falls back to ResolveAgent → "general".
func TestPrepareInbound_UnknownExplicitAgentIDFallsBack(t *testing.T) {
	t.Parallel()
	d := newAgentRouteDispatcher(t)
	msg := platform.IncomingMessage{
		Platform: "fake", EventID: "evt-ans2",
		UserID: "u1", ChatID: "c1", ChatType: "direct",
		Text:      "Error style: Return an error.",
		MentionMe: true,
		AgentID:   "../../evil-agent",
	}
	p, ok := d.prepareInbound(context.Background(), msg)
	if !ok {
		t.Fatal("prepareInbound dropped the message; want accepted")
	}
	if p.agentID != "general" {
		t.Errorf("agentID = %q, want general (unknown AgentID must be ignored)", p.agentID)
	}
}
