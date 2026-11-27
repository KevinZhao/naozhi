package server

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// An empty Backend builds a server (nil maps tolerated) and resolves to the
// claude reply tag; kiro keeps its own.
func TestNewWithOptions_DefaultBackendIsClaude(t *testing.T) {
	if srv := NewWithOptions(ServerOptions{Addr: ":0", Router: session.NewRouter(session.RouterConfig{})}); srv == nil {
		t.Fatal("NewWithOptions returned nil server for minimal opts")
	}
	for id, want := range map[string]string{"": "cc", "kiro": "kiro"} {
		if tag := replyTagForBackend(id); tag != want {
			t.Errorf("replyTagForBackend(%q) = %q, want %q", id, tag, want)
		}
	}
}

// A nil Router panics at the constructor entry, naming the field (#3638).
func TestNewWithOptions_NilRouterPanicsAtEntry(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("buildServerWithHandlers(ServerOptions{}) did not panic")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "Router is required") {
			t.Fatalf("panic = %v, want it to name the missing Router", r)
		}
	}()
	buildServerWithHandlers(ServerOptions{Addr: ":0"})
}
