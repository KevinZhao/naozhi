package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestCapHistoryBatch locks in R68-PERF-H1: eventPushLoop must truncate
// large backlogs to the most recent maxHistoryPushEntries so a slow
// subscriber doesn't trigger a single multi-MB push frame that starves
// the WS send channel for all connected clients. Older entries remain
// reachable via the paginated /api/sessions/events?before= path.
func TestCapHistoryBatch(t *testing.T) {
	cases := []struct {
		name    string
		inLen   int
		wantLen int
		// Head value after capping — for oversize inputs we keep the tail,
		// so the head should equal `inLen-maxHistoryPushEntries` when
		// inLen > maxHistoryPushEntries (entries are int64-timed here so
		// the test uses Time as an index surrogate).
		wantHead int64
	}{
		{"empty", 0, 0, 0},
		{"at cap", maxHistoryPushEntries, maxHistoryPushEntries, 0},
		{"under cap", maxHistoryPushEntries - 1, maxHistoryPushEntries - 1, 0},
		{"just over cap", maxHistoryPushEntries + 1, maxHistoryPushEntries, 1},
		{"ten times cap", 10 * maxHistoryPushEntries, maxHistoryPushEntries, int64(9 * maxHistoryPushEntries)},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := make([]cli.EventEntry, c.inLen)
			for i := 0; i < c.inLen; i++ {
				in[i] = cli.EventEntry{Time: int64(i)}
			}
			got := capHistoryBatch(in)
			if len(got) != c.wantLen {
				t.Fatalf("len = %d, want %d", len(got), c.wantLen)
			}
			if c.wantLen > 0 && got[0].Time != c.wantHead {
				t.Errorf("head Time = %d, want %d (tail should be preserved)", got[0].Time, c.wantHead)
			}
			// Last element must always be the most recent, regardless of cap.
			if c.wantLen > 0 {
				wantTail := int64(c.inLen - 1)
				if got[len(got)-1].Time != wantTail {
					t.Errorf("tail Time = %d, want %d", got[len(got)-1].Time, wantTail)
				}
			}
		})
	}
}

// TestValidateProjectName locks in R68-SEC-M3: project `name` query param
// must be gated at the HTTP boundary so oversized or control-character
// inputs cannot reach slog attrs.
func TestValidateProjectName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"empty rejected", "", false},
		{"simple ASCII", "myproj", true},
		{"Chinese passes", "项目甲", true},
		{"128 bytes exact", string(make([]byte, maxProjectNameLen)), false}, // NUL chars reject
		{"128 printable exact", repeatByte('a', maxProjectNameLen), true},
		{"129 bytes rejected", repeatByte('a', maxProjectNameLen+1), false},
		{"NUL rejected", "foo\x00bar", false},
		{"LF rejected", "foo\nbar", false},
		{"CR rejected", "foo\rbar", false},
		{"tab rejected", "foo\tbar", false},
		{"ESC rejected", "foo\x1bbar", false},
		{"DEL rejected", "foo\x7fbar", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateProjectName(c.in)
			if c.ok && err != nil {
				t.Errorf("unexpected err: %v", err)
			}
			if !c.ok && err == nil {
				t.Errorf("expected error for %q", c.in)
			}
		})
	}
}

func repeatByte(b byte, n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return string(out)
}
