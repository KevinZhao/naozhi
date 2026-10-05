package cli

import (
	"log/slog"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/eventlog/ring"
)

// A code_change_published frame reaches the session's hook and stays off the
// event timeline; an invalid one reaches neither.
func TestCodeChangeFrame_ReachesHookNotTimeline(t *testing.T) {
	t.Parallel()
	proto := &ClaudeProtocol{}
	p := &Process{eventLog: ring.NewEventLog(16), eventCh: make(chan clievent.Event, 4), killCh: make(chan struct{})}
	var got []clievent.CodeChange
	p.SetOnCodeChange(func(c clievent.CodeChange) { got = append(got, c) })

	feed := func(line string) {
		t.Helper()
		evs, _, err := proto.ReadEventInto(line, nil)
		if err != nil {
			t.Fatalf("ReadEventInto(%s): %v", line, err)
		}
		for _, ev := range evs {
			p.dispatchProtocolEvent(ev, slog.New(slog.DiscardHandler))
		}
	}
	feed(`{"type":"system","subtype":"code_change_published","provider":"github","url":"https://github.com/o/r/pull/12","repo":"o/r","identifier":"12","action":"created","branch":"feat-x","uuid":"u1","session_id":"s1"}`)
	feed(`{"type":"system","subtype":"code_change_published","provider":"github","url":"javascript:alert(1)","identifier":"13"}`)
	feed(`{"type":"system","subtype":"vcs_state_changed","kind":"push","branch":"feat-x","cwd":"/w"}`)

	want := clievent.CodeChange{Provider: "github", URL: "https://github.com/o/r/pull/12", Repo: "o/r", Identifier: "12", Action: "created", Branch: "feat-x"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("hook got %+v, want exactly %+v", got, want)
	}
	if n := len(p.EventEntries()); n != 0 {
		t.Errorf("timeline has %d entries, want 0: %+v", n, p.EventEntries())
	}

	p.SetOnCodeChange(nil)
	feed(`{"type":"system","subtype":"code_change_published","url":"https://github.com/o/r/pull/14"}`)
	if len(got) != 1 {
		t.Errorf("cleared hook still fired: %+v", got)
	}
}
