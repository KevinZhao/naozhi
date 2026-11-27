package dispatch

import (
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestInterruptChat_BroadcastsAcrossAgents pins #1944: /stop must interrupt
// the in-flight turn of any agent session the chat owns (general/planner plus
// agent-command targets like /review→code-reviewer), not just the hard-coded
// "general" key, and probe an agent reachable through two commands once. With
// every agent idle the outcome folds to NoSession ("no reply in progress").
func TestInterruptChat_BroadcastsAcrossAgents(t *testing.T) {
	t.Parallel()
	const reviewer = "im:direct:u1:code-reviewer"
	for _, live := range []bool{true, false} {
		var mu sync.Mutex
		counts := map[string]int{}
		fake := &fakeSessionRouter{
			interruptViaControl: func(key string) session.InterruptOutcome {
				mu.Lock()
				counts[key]++
				mu.Unlock()
				if live && key == reviewer {
					return session.InterruptSent
				}
				return session.InterruptNoSession
			},
		}
		d := &Dispatcher{
			router:        fake,
			resolver:      session.NewKeyResolver(map[string]session.AgentOpts{"general": {}}, nil),
			agentCommands: map[string]string{"review": "code-reviewer", "cr": "code-reviewer"},
		}
		want := session.InterruptNoSession
		if live {
			want = session.InterruptSent
		}
		if got := d.interruptChat("im", "direct", "u1"); got != want {
			t.Errorf("live=%v: interruptChat = %v, want %v", live, got, want)
		}
		mu.Lock()
		if c := counts[reviewer]; c != 1 {
			t.Errorf("live=%v: code-reviewer key probed %d times, want 1; probed=%v", live, c, counts)
		}
		mu.Unlock()
	}
}
