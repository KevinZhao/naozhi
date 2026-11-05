package wireup

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// The user's last message is the latest "user" entry custody or the
// Planner did not send; a cross-session message is not one.
func TestLastUserAt(t *testing.T) {
	user := func(ms int64, src string) clievent.EventEntry {
		return clievent.EventEntry{Time: ms, Type: clievent.KindUser, Source: src}
	}
	for _, c := range []struct {
		name    string
		entries []clievent.EventEntry
		want    int64
	}{
		{"none", nil, 0},
		{"the user's", []clievent.EventEntry{user(1000, ""), {Time: 2000, Type: "text"}}, 1000},
		{"custody's after it", []clievent.EventEntry{user(1000, ""), user(2000, clievent.SourceCustody)}, 1000},
		{"the dispatch only", []clievent.EventEntry{user(1000, clievent.SourcePlanner), {Time: 2000, Type: clievent.KindPeerMessage}}, 0},
		{"the latest", []clievent.EventEntry{user(1000, ""), user(3000, "")}, 3000},
	} {
		got := lastUserAt(c.entries)
		if want := unixMilli(c.want); !got.Equal(want) {
			t.Errorf("%s: lastUserAt = %v, want %v", c.name, got, want)
		}
	}
}
