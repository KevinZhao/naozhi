package session

import (
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/planner/activity"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// TestReattachedTurn: how the turn in flight at the last shutdown stands on
// the reattached process, which is what settles the Planner's carried-over
// requests (server.SettlePlannerTurns).
func TestReattachedTurn(t *testing.T) {
	result := func(text string) string {
		return `{"type":"result","subtype":"success","result":"` + text + `","session_id":"s1"}`
	}
	// after is the request's watermark seq (backlog lines are seqs 1, 2, …);
	// -1 records none.
	for _, tc := range []struct {
		name           string
		backlog        []string
		after          int64
		text           string
		ended, running bool
	}{
		{"ended while down", []string{adoptAssistantLine, result("down")}, 1, "down", true, false},
		// The request's own turn never started: the backlog's result is the
		// previous turn's, which the last run already acted on.
		{"previous turn's result", []string{adoptAssistantLine, result("earlier")}, 2, "", false, false},
		{"no watermark", []string{adoptAssistantLine, result("down")}, -1, "", false, false},
		{"still running", []string{result("earlier"), adoptAssistantLine}, 1, "", false, true},
		{"failed while down", []string{adoptAssistantLine,
			`{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s1"}`}, 1, "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const key = "planner:main"
			r, proc, _ := reconnectToFakeShim(t, key, 1, tc.backlog)
			now, ok := proc.TurnWatermark()
			if !ok {
				t.Fatal("no watermark on the reattached process")
			}
			after, known := cli.TurnWatermark{ShimPID: now.ShimPID, Seq: tc.after}, tc.after >= 0
			text, ended, running := r.ReattachedTurn(key, after, known)
			if text != tc.text || ended != tc.ended || running != tc.running {
				t.Errorf("ReattachedTurn = (%q, %v, %v), want (%q, %v, %v)", text, ended, running, tc.text, tc.ended, tc.running)
			}
			if _, ended, running := r.ReattachedTurn("planner:none", after, known); ended || running {
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
	r.resultFor(r.ss.Load(key))(clievent.SendResult{Text: "next"})
	if s := <-got; s != key+"=next" {
		t.Errorf("result after the sink = %q, want next", s)
	}
}

type turnSink struct{ turns []string }

func (*turnSink) Record(string, string, activity.Activity) {}
func (t *turnSink) RecordTurn(key, dir string, o activity.TurnOutcome) {
	t.turns = append(t.turns, key+" "+dir+" "+string(o))
}

// Every result frame is one turn's end journaled through the activity sink,
// with how it ended: an abort first (older claude reports one as an error),
// then an error; only an answer reaches the result sink.
func TestResultForRecordsEveryTurnEnd(t *testing.T) {
	const key = "dashboard:direct:host:general"
	for _, tc := range []struct {
		name string
		res  clievent.SendResult
		want activity.TurnOutcome
	}{
		{"answered", clievent.SendResult{Text: "好的"}, activity.TurnDone},
		{"empty answer", clievent.SendResult{}, activity.TurnDone},
		{"failed", clievent.SendResult{IsError: true, SubType: "error_during_execution"}, activity.TurnFailed},
		{"stopped by the user", clievent.SendResult{Aborted: true, IsError: true, SubType: "error_during_execution"}, activity.TurnAborted},
		{"aborted by the CLI", clievent.SendResult{TerminalReason: "aborted_tools"}, activity.TurnAborted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Router{}
			sink := &turnSink{}
			r.SetActivitySink(sink)
			var replies []string
			r.SetResultSink(func(_, text string) { replies = append(replies, text) })
			s := &ManagedSession{key: key}
			s.setWorkspace("/w")
			r.resultFor(s)(tc.res)
			if want := []string{key + " /w " + string(tc.want)}; len(sink.turns) != 1 || sink.turns[0] != want[0] {
				t.Errorf("turns = %q, want %q", sink.turns, want)
			}
			if got := len(replies) == 1; got != (tc.want == activity.TurnDone) {
				t.Errorf("replies = %q, want one only for an answer", replies)
			}
		})
	}
	// No sink yet: the result still reaches the result sink.
	r := &Router{}
	var n int
	r.SetResultSink(func(string, string) { n++ })
	r.resultFor(&ManagedSession{key: key})(clievent.SendResult{Text: "x"})
	if n != 1 {
		t.Errorf("result sink got %d replies without an activity sink, want 1", n)
	}
}
