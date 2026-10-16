package cli

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// Under `--permission-mode auto` the CLI (2.1.296) brackets every classifier
// check with two permission_check_status frames; they must stay off the
// timeline, while a classifier denial must still render as a row.
func TestPermissionCheckFrames_TimelineRows(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		line string
		want int
	}{
		{"checking", `{"type":"system","subtype":"permission_check_status","tool_use_id":"toolu_1","status":"checking","uuid":"u1","session_id":"s1"}`, 0},
		{"done", `{"type":"system","subtype":"permission_check_status","tool_use_id":"toolu_1","status":"done","uuid":"u2","session_id":"s1"}`, 0},
		{"denied", `{"type":"system","subtype":"permission_denied","tool_name":"Bash","tool_use_id":"toolu_2","decision_reason_type":"classifier","decision_reason":"pushes to an untrusted repo","uuid":"u3","session_id":"s1"}`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			evs, _, err := (&ClaudeProtocol{}).ReadEvent(tc.line)
			if err != nil || len(evs) != 1 {
				t.Fatalf("ReadEvent = %v, %v; want one event", evs, err)
			}
			entries := EventEntriesFromEventAt(evs[0], 1)
			if len(entries) != tc.want {
				t.Fatalf("entries = %+v, want %d", entries, tc.want)
			}
			if tc.want == 1 && entries[0].Type != clievent.KindSystem {
				t.Errorf("permission_denied entry type = %q, want %q", entries[0].Type, clievent.KindSystem)
			}
		})
	}
}
