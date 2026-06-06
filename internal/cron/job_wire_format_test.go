package cron

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	robfigcron "github.com/robfig/cron/v3"
)

// TestJobRuntimeFieldsNotPersisted pins the wire-format contract that the
// runtime-only fields on Job (entryID / cachedPeriod / cachedSched) never
// reach cron_jobs.json. These fields are unexported, so encoding/json drops
// them today; the test exists to catch a future refactor that promotes one
// of them to an exported / json-tagged field (the exact schema-drift hazard
// flagged in #1176 — every new "execute receipt"-shaped field risks growing
// the persistence schema or breaking the wire-format on rename).
//
// If a runtime-only field is ever exported, this test fails loudly at the
// marshal boundary instead of silently writing transient state to disk.
func TestJobRuntimeFieldsNotPersisted(t *testing.T) {
	j := &Job{
		ID:       "abcd1234abcd1234",
		Schedule: "@every 1m",
		Prompt:   "do the thing",
		// Populate every runtime-only field with a non-zero sentinel so an
		// accidental promotion to an exported field surfaces in the JSON.
		entryID:      robfigcron.EntryID(424242),
		cachedPeriod: 7 * time.Minute,
		cachedSched:  robfigcron.ConstantDelaySchedule{Delay: time.Minute},
	}

	raw, err := json.Marshal(j)
	if err != nil {
		t.Fatalf("json.Marshal(Job) error: %v", err)
	}
	got := string(raw)

	// Sentinels that would only appear if a runtime-only field leaked into
	// the persisted form. 424242 is the entryID; 420000000000 is the
	// nanosecond encoding of 7m had cachedPeriod become a json field.
	for _, sentinel := range []string{"424242", "420000000000", "ConstantDelay"} {
		if strings.Contains(got, sentinel) {
			t.Errorf("persisted Job JSON contains runtime-only sentinel %q; a runtime-only field was promoted to the wire-format (schema drift, #1176). JSON: %s", sentinel, got)
		}
	}

	// Round-trip: the runtime-only fields must come back zero after a
	// marshal/unmarshal cycle (they are reconstructed by registerJob, never
	// loaded from disk).
	var back Job
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("json.Unmarshal(Job) error: %v", err)
	}
	if back.entryID != 0 {
		t.Errorf("entryID survived JSON round-trip = %d, want 0 (runtime-only must not persist)", back.entryID)
	}
	if back.cachedPeriod != 0 {
		t.Errorf("cachedPeriod survived JSON round-trip = %v, want 0", back.cachedPeriod)
	}
	if back.cachedSched != nil {
		t.Errorf("cachedSched survived JSON round-trip = %v, want nil", back.cachedSched)
	}
}
