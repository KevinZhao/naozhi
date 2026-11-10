package agent

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/eventlog/persist"
)

// A lookup's answer is outside content: its turn is judged like a scout's.
func TestLookupSourceOutside(t *testing.T) {
	if !SourceLookup.Outside() || outsideLabels[SourceLookup] == "" {
		t.Fatal("a lookup's answer turn is not an outside turn")
	}
}

// TestTaintDeny: a session marked tainted while the Planner runs puts a deny
// rule for its event log into the spawn options at once and has the idle
// process released, to be resumed with it (§15.12 ⑨).
func TestTaintDeny(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.Local)
	sess := &fakeSessions{st: SessionState{Exists: true, Alive: true, SessionID: "s1"}}
	a, _ := newTestAgent(t, sess, &now)
	var mu sync.Mutex
	var tainted []string
	a.cfg.Tainted = func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(tainted) }
	a.turns = &fakeTurns{}
	ctx := context.Background()
	a.reconcile(ctx)
	if !sess.st.Alive {
		t.Fatal("released with nothing tainted")
	}
	key := "dashboard:direct:u:general"
	mu.Lock()
	tainted = []string{key}
	mu.Unlock()
	rule := absRule("Read", filepath.Join(a.cfg.Layout.EventsRoot(), persist.KeyHash(key)+"*"))
	a.reconcile(ctx)
	if !slices.Contains(a.Opts().PermissionsDeny, rule) {
		t.Fatalf("deny rules lack %s: %q", rule, a.Opts().PermissionsDeny)
	}
	if sess.st.Alive {
		t.Fatal("idle Planner without the taint rule not released")
	}
	a.reconcile(ctx)
	a.reconcile(ctx)
	if !sess.st.Alive {
		t.Fatal("not resumed, or released again, on the new rules")
	}
}
