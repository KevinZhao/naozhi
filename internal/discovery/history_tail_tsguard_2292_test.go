package discovery

import (
	"testing"
)

// TestParseHistoryLine_DropsUnparseableTimestamp pins #2292: a user or
// assistant record whose timestamp is missing or unparseable must be dropped
// rather than emitted with Time=0. A Time=0 entry survives the strict-<
// pagination filter, sorts to chunk[0], and pins the LoadBefore cursor at 0 —
// degrading into a newest-tail re-read that repeats already-seen entries.
// codexjsonl/kirojsonl already drop ts<=0; the claude path must match.
func TestParseHistoryLine_DropsUnparseableTimestamp(t *testing.T) {
	t.Parallel()
	cases := []string{
		// user, empty timestamp
		`{"type":"user","timestamp":"","message":{"role":"user","content":"hello"}}`,
		// user, missing timestamp field
		`{"type":"user","message":{"role":"user","content":"hello"}}`,
		// user, garbage timestamp
		`{"type":"user","timestamp":"not-a-date","message":{"role":"user","content":"hello"}}`,
		// assistant, empty timestamp
		`{"type":"assistant","timestamp":"","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`,
		// assistant, garbage timestamp
		`{"type":"assistant","timestamp":"xyz","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`,
	}
	for _, line := range cases {
		if got, ok := parseHistoryLine([]byte(line)); ok {
			t.Errorf("expected line with bad timestamp to be dropped, but it parsed (%+v): %s", got, line)
		}
	}
}

// TestParseHistoryLine_KeepsValidTimestamp is the positive control: a valid
// timestamp still parses, proving the guard is a pure fast-negative for the
// ts<=0 case only.
func TestParseHistoryLine_KeepsValidTimestamp(t *testing.T) {
	t.Parallel()
	line := `{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"hello"}}`
	got, ok := parseHistoryLine([]byte(line))
	if !ok {
		t.Fatal("valid-timestamp line was dropped — guard too aggressive")
	}
	if len(got) != 1 || got[0].Time <= 0 {
		t.Fatalf("expected one entry with positive Time, got %+v", got)
	}
}
