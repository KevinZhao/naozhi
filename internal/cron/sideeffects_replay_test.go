package cron

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestJob_SideEffectsWireTag pins the side_effects JSON tag + tri-state.
func TestJob_SideEffectsWireTag(t *testing.T) {
	// nil → key omitted (legacy default, zero-migration for old stores).
	data, _ := json.Marshal(&Job{ID: "a"})
	if strings.Contains(string(data), "side_effects") {
		t.Fatalf("nil SideEffects must omit the key: %s", data)
	}
	// explicit true/false → key present with the bool.
	data, _ = json.Marshal(&Job{ID: "a", SideEffects: boolPtr(true)})
	if !strings.Contains(string(data), `"side_effects":true`) {
		t.Fatalf("explicit SideEffects=true must serialise: %s", data)
	}
	data, _ = json.Marshal(&Job{ID: "a", SideEffects: boolPtr(false)})
	if !strings.Contains(string(data), `"side_effects":false`) {
		t.Fatalf("explicit SideEffects=false must serialise (distinct from unset): %s", data)
	}
}

// TestJobUpdate_AppliesSideEffects pins the patch path: nil omits, pointer
// writes the explicit tri-state (deep-copied, not aliasing the caller's).
func TestJobUpdate_AppliesSideEffects(t *testing.T) {
	j := &Job{ID: "a"}

	JobUpdate{}.applyTo(j)
	if j.SideEffects != nil {
		t.Fatal("nil SideEffects update must leave the field unchanged")
	}

	src := true
	JobUpdate{SideEffects: &src}.applyTo(j)
	if j.SideEffects == nil || *j.SideEffects != true {
		t.Fatalf("SideEffects=true update not applied: %v", j.SideEffects)
	}
	// applyTo must deep-copy: mutating the source pointer must not bleed in.
	src = false
	if *j.SideEffects != true {
		t.Fatal("applyTo aliased the caller's *bool instead of copying")
	}

	JobUpdate{SideEffects: boolPtr(false)}.applyTo(j)
	if *j.SideEffects != false {
		t.Fatalf("SideEffects=false update not applied: %v", *j.SideEffects)
	}
}

// TestSnapshot_SideEffectsCollapsesToBool: snapshot flattens the tri-state
// pointer to a bool (nil→false) for the executor's §6.2 fence decision.
func TestSnapshot_SideEffectsCollapsesToBool(t *testing.T) {
	cases := []struct {
		name string
		in   *bool
		want bool
	}{
		{"nil is false", nil, false},
		{"explicit false", boolPtr(false), false},
		{"explicit true", boolPtr(true), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			snap := snapshotJobLocked(&Job{ID: "a", SideEffects: c.in})
			if snap.sideEffects != c.want {
				t.Fatalf("sideEffects = %v, want %v", snap.sideEffects, c.want)
			}
		})
	}
}

// TestCronRun_ReplayOfWireTagAndSummary pins replay_of on both CronRun and
// the summary projection (the list view renders the chain badge too).
func TestCronRun_ReplayOfWireTagAndSummary(t *testing.T) {
	r := &CronRun{RunID: "b", JobID: "j", State: RunStateSucceeded, ReplayOf: "a"}

	data, _ := json.Marshal(r)
	if !strings.Contains(string(data), `"replay_of":"a"`) {
		t.Fatalf("CronRun must carry replay_of: %s", data)
	}
	// Original runs (empty ReplayOf) omit the key.
	data, _ = json.Marshal(&CronRun{RunID: "a", JobID: "j", State: RunStateSucceeded})
	if strings.Contains(string(data), "replay_of") {
		t.Fatalf("original run must omit replay_of: %s", data)
	}
	// Summary carries it (UI list draws the chain off the summary).
	sdata, _ := json.Marshal(r.summary())
	if !strings.Contains(string(sdata), `"replay_of":"a"`) {
		t.Fatalf("CronRunSummary must carry replay_of: %s", sdata)
	}
}
