package session

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestManagedState_DerivedStates pins the R176-ARCH-N4 (#432) state-machine
// derivation so the single ManagedState accessor stays the source of truth
// for the inference that consumers used to open-code.
func TestManagedState_DerivedStates(t *testing.T) {
	t.Run("stub: no proc, no session id, no history", func(t *testing.T) {
		s := &ManagedSession{key: "dashboard:direct:user:general"}
		if got := s.ManagedState(); got != StateStub {
			t.Fatalf("ManagedState = %v, want StateStub", got)
		}
	})

	t.Run("alive: live process attached", func(t *testing.T) {
		s := &ManagedSession{key: "k"}
		s.storeProcess(newIdleProc())
		if got := s.ManagedState(); got != StateAlive {
			t.Fatalf("ManagedState = %v, want StateAlive", got)
		}
	})

	t.Run("suspended: no live proc but resumable session id", func(t *testing.T) {
		s := &ManagedSession{key: "k"}
		s.setSessionID("sess-123")
		if got := s.ManagedState(); got != StateSuspended {
			t.Fatalf("ManagedState = %v, want StateSuspended", got)
		}
	})

	t.Run("suspended: dead proc attached but session id captured", func(t *testing.T) {
		s := &ManagedSession{key: "k"}
		s.storeProcess(newDeadProc())
		s.setSessionID("sess-456")
		if got := s.ManagedState(); got != StateSuspended {
			t.Fatalf("ManagedState = %v, want StateSuspended", got)
		}
	})

	t.Run("dead: no live proc, no session id, but has history", func(t *testing.T) {
		s := &ManagedSession{key: "k"}
		s.persistedHistory = append(s.persistedHistory, cli.EventEntry{Type: "user", Summary: "hi"})
		if got := s.ManagedState(); got != StateDead {
			t.Fatalf("ManagedState = %v, want StateDead", got)
		}
	})

	t.Run("exempt wins over liveness", func(t *testing.T) {
		s := &ManagedSession{key: "k", exempt: true}
		s.storeProcess(newIdleProc())
		s.setSessionID("sess-789")
		if got := s.ManagedState(); got != StateExempt {
			t.Fatalf("ManagedState = %v, want StateExempt", got)
		}
	})
}

func TestManagedState_String(t *testing.T) {
	cases := map[ManagedState]string{
		StateStub:        "stub",
		StateAlive:       "alive",
		StateSuspended:   "suspended",
		StateDead:        "dead",
		StateExempt:      "exempt",
		ManagedState(99): "unknown",
	}
	for st, want := range cases {
		if got := st.String(); got != want {
			t.Errorf("ManagedState(%d).String() = %q, want %q", int(st), got, want)
		}
	}
}
