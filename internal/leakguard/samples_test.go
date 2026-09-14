package leakguard

import (
	"encoding/json"
	"os"
	"testing"
)

// leakSample is one detection-boundary case from testdata/samples.json.
//
// The file is shared with test/e2e/leaked_toolcall_fold.test.js, which drives
// the same texts through the dashboard and checks whether the bubble folded.
// Both sides reading one file is what makes JS ⇄ Go parity behavioural: the
// retired internal/server/static_leaked_toolcall_test.go established it by
// asserting that leakguard.Anchor appeared verbatim inside dashboard.js, which
// is satisfied by an identical regex literal that is never applied, and broken
// by a JS rewrite that is exactly equivalent (#2547).
type leakSample struct {
	Name string `json:"name"`
	Text string `json:"text"`
	Leak bool   `json:"leak"`
}

func loadLeakSamples(t *testing.T) []leakSample {
	t.Helper()
	data, err := os.ReadFile("testdata/samples.json")
	if err != nil {
		t.Fatalf("read testdata/samples.json: %v", err)
	}
	var out []leakSample
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("parse testdata/samples.json: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("testdata/samples.json is empty; this test would check nothing")
	}
	return out
}

// TestDetect_BoundarySamples pins the detection boundary. A false positive here
// is worse than the bug it guards: it would fold legitimate technical prose
// that merely quotes tool-call syntax, so the table carries as many
// must-not-flag cases as must-flag ones.
func TestDetect_BoundarySamples(t *testing.T) {
	t.Parallel()
	samples := loadLeakSamples(t)
	var leaks, clean int
	for _, s := range samples {
		if s.Leak {
			leaks++
		} else {
			clean++
		}
	}
	// A table that drifted to one-sided would still pass every case while
	// testing only half the boundary.
	if leaks == 0 || clean == 0 {
		t.Fatalf("samples.json must carry both verdicts; got %d leak / %d clean", leaks, clean)
	}
	for _, s := range samples {
		t.Run(s.Name, func(t *testing.T) {
			t.Parallel()
			if got := Detect(s.Text); got != s.Leak {
				t.Errorf("Detect(%q) = %v, want %v", s.Text, got, s.Leak)
			}
		})
	}
}
