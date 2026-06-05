package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

func TestAgentIDList(t *testing.T) {
	countID := func(ids []string, want string) int {
		n := 0
		for _, id := range ids {
			if id == want {
				n++
			}
		}
		return n
	}

	t.Run("general configured does not duplicate", func(t *testing.T) {
		agents := map[string]session.AgentOpts{
			"general": {},
			"coder":   {},
		}
		ids := agentIDList(agents)
		if got := countID(ids, "general"); got != 1 {
			t.Fatalf("expected exactly one \"general\", got %d in %v", got, ids)
		}
		if len(ids) == 0 || ids[0] != "general" {
			t.Fatalf("expected \"general\" first, got %v", ids)
		}
		if len(ids) != len(agents) {
			t.Fatalf("expected %d ids, got %d: %v", len(agents), len(ids), ids)
		}
	})

	t.Run("general not configured behaves unchanged", func(t *testing.T) {
		agents := map[string]session.AgentOpts{
			"coder":  {},
			"writer": {},
		}
		ids := agentIDList(agents)
		if len(ids) == 0 || ids[0] != "general" {
			t.Fatalf("expected \"general\" first, got %v", ids)
		}
		if got := countID(ids, "general"); got != 1 {
			t.Fatalf("expected exactly one \"general\", got %d in %v", got, ids)
		}
		if len(ids) != len(agents)+1 {
			t.Fatalf("expected %d ids, got %d: %v", len(agents)+1, len(ids), ids)
		}
		for _, want := range []string{"coder", "writer"} {
			if countID(ids, want) != 1 {
				t.Fatalf("expected %q present once, got %v", want, ids)
			}
		}
	})

	t.Run("empty agents yields only general", func(t *testing.T) {
		ids := agentIDList(map[string]session.AgentOpts{})
		if len(ids) != 1 || ids[0] != "general" {
			t.Fatalf("expected [general], got %v", ids)
		}
	})
}
