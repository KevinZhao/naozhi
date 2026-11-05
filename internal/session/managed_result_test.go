package session

import (
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/testhelper"
)

// TestReattachedTurn: how the turn in flight at the last shutdown stands on
// the reattached process, which is what settles the Planner's carried-over
// requests (server.SettlePlannerTurns).
func TestReattachedTurn(t *testing.T) {
	result := func(text string) string {
		return `{"type":"result","subtype":"success","result":"` + text + `","session_id":"s1"}`
	}
	for _, tc := range []struct {
		name           string
		backlog        []string
		text           string
		ended, running bool
	}{
		{"ended while down", []string{adoptAssistantLine, result("down")}, "down", true, false},
		{"still running", []string{result("earlier"), adoptAssistantLine}, "", false, true},
		{"failed while down", []string{adoptAssistantLine,
			`{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s1"}`}, "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const key = "planner:main"
			r, _, _ := reconnectToFakeShim(t, key, 1, tc.backlog)
			text, ended, running := r.ReattachedTurn(key)
			if text != tc.text || ended != tc.ended || running != tc.running {
				t.Errorf("ReattachedTurn = (%q, %v, %v), want (%q, %v, %v)", text, ended, running, tc.text, tc.ended, tc.running)
			}
			if _, ended, running := r.ReattachedTurn("planner:none"); ended || running {
				t.Error("a key with no session reports a turn")
			}
		})
	}
}

// TestResultSink_HeldUntilSet: startup reattaches shims long before the
// result sink's consumers exist; a turn finishing in that window still
// reaches the sink once it is set, and later results go straight to it.
func TestResultSink_HeldUntilSet(t *testing.T) {
	const key = "planner:main"
	r, proc, emit := reconnectToFakeShim(t, key, 1, []string{adoptAssistantLine})
	emit(`{"type":"result","subtype":"success","result":"late answer","session_id":"s1"}`)
	testhelper.Eventually(t, func() bool { return !proc.AdoptedTurnPending() },
		5*time.Second, "the live result never arrived")

	got := make(chan string, 4)
	r.SetResultSink(func(k, text string) { got <- k + "=" + text })
	select {
	case s := <-got:
		if s != key+"=late answer" {
			t.Errorf("held result = %q, want the late answer", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a result that came before the sink was set never reached it")
	}
	r.resultFor(r.ss.Load(key))("next")
	if s := <-got; s != key+"=next" {
		t.Errorf("result after the sink = %q, want next", s)
	}
}
