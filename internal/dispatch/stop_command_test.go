package dispatch

import (
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestInterruptChat_BroadcastsAcrossAgents pins #1944: /stop must interrupt
// the in-flight turn of any agent session the chat owns (general/planner plus
// agent-command targets like /review→code-reviewer), not just the hard-coded
// "general" key. Before the fix, an active code-reviewer turn was never
// interrupted and the user got a misleading "no reply in progress".
func TestInterruptChat_BroadcastsAcrossAgents(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var probed []string
	fake := &fakeSessionRouter{
		interruptViaControl: func(key string) session.InterruptOutcome {
			mu.Lock()
			probed = append(probed, key)
			mu.Unlock()
			// Only the code-reviewer session has an in-flight turn; general /
			// planner are idle (NoSession).
			if key == "im:direct:u1:code-reviewer" {
				return session.InterruptSent
			}
			return session.InterruptNoSession
		},
	}
	var _ SessionRouter = fake

	d := &Dispatcher{
		router:        fake,
		resolver:      session.NewKeyResolver(map[string]session.AgentOpts{"general": {}}, nil),
		agentCommands: map[string]string{"review": "code-reviewer"},
	}

	got := d.interruptChat("im", "direct", "u1")
	if got != session.InterruptSent {
		t.Fatalf("interruptChat folded outcome = %v, want InterruptSent (code-reviewer turn must win over idle general/planner)", got)
	}

	mu.Lock()
	defer mu.Unlock()
	var sawReviewer bool
	for _, k := range probed {
		if k == "im:direct:u1:code-reviewer" {
			sawReviewer = true
		}
	}
	if !sawReviewer {
		t.Fatalf("code-reviewer key was never probed; /stop still hard-codes general. probed=%v", probed)
	}
}

// TestInterruptChat_NoLiveSession verifies the all-idle case still folds to
// NoSession so handleStopCommand replies "no reply in progress".
func TestInterruptChat_NoLiveSession(t *testing.T) {
	t.Parallel()

	fake := &fakeSessionRouter{
		interruptViaControl: func(string) session.InterruptOutcome {
			return session.InterruptNoSession
		},
	}
	d := &Dispatcher{
		router:        fake,
		resolver:      session.NewKeyResolver(map[string]session.AgentOpts{"general": {}}, nil),
		agentCommands: map[string]string{"review": "code-reviewer"},
	}
	if got := d.interruptChat("im", "direct", "u1"); got != session.InterruptNoSession {
		t.Fatalf("interruptChat = %v, want InterruptNoSession when no agent has a live turn", got)
	}
}

// TestInterruptChat_DeduplicatesKeys ensures the same agentID reachable via
// multiple slash commands is only probed once (avoids double-interrupt noise).
func TestInterruptChat_DeduplicatesKeys(t *testing.T) {
	t.Parallel()

	counts := map[string]int{}
	var mu sync.Mutex
	fake := &fakeSessionRouter{
		interruptViaControl: func(key string) session.InterruptOutcome {
			mu.Lock()
			counts[key]++
			mu.Unlock()
			return session.InterruptNoSession
		},
	}
	d := &Dispatcher{
		router:   fake,
		resolver: session.NewKeyResolver(map[string]session.AgentOpts{"general": {}}, nil),
		// Two commands map to the same agentID.
		agentCommands: map[string]string{"review": "code-reviewer", "cr": "code-reviewer"},
	}
	_ = d.interruptChat("im", "direct", "u1")

	mu.Lock()
	defer mu.Unlock()
	if c := counts["im:direct:u1:code-reviewer"]; c != 1 {
		t.Fatalf("code-reviewer key probed %d times, want exactly 1 (dedup broken)", c)
	}
}
