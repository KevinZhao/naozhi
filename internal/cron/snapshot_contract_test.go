package cron

import (
	"reflect"
	"sort"
	"testing"
)

// snapshotJobJobFields lists every Job field that snapshotJob is
// REQUIRED to capture. New fields added to Job land in one of three
// places, asserted by TestJobSnapshot_FieldCoverage:
//
//  1. snapshotJobMustCapture — the field affects executeOpt's send /
//     notify pipeline and therefore needs to be read under s.mu so a
//     concurrent UpdateJob cannot tear it.
//  2. snapshotJobIgnored — the field is either runtime-only (entryID),
//     metadata that executeOpt never consults (CreatedAt / CreatedBy /
//     Paused — Paused is rechecked separately under s.mu just before
//     dispatch, see scheduler.go ~line 1801), or last-state fields that
//     the recordResult path writes back rather than reads at execute
//     time (LastResult / LastRunAt / LastError / LastErrorClass /
//     LastSessionID / RunCounters).
//  3. New unknown field → test fails. The reviewer who added the field
//     must classify it into one of the two lists above; merely
//     extending snapshotJobIgnored without thinking is the failure mode
//     this test exists to catch.
//
// R236-ARCH-19: Without this contract test, jobSnapshot's field set
// drifted from Job whenever a new Job field was added — the reviewer
// got no compile-time signal because executeOpt held *Job and could
// silently dereference the new field outside s.mu, racing with
// UpdateJob writes.
var snapshotJobMustCapture = map[string]struct{}{
	"ID":             {}, // → jobID
	"Schedule":       {},
	"Prompt":         {},
	"Platform":       {}, // → platName
	"ChatID":         {},
	"Title":          {}, // → label (via jobTitleOrFallback)
	"WorkDir":        {},
	"Backend":        {},
	"NotifyPlatform": {}, // → notifyPlat
	"NotifyChatID":   {}, // → notifyChat
	"Notify":         {},
	"FreshContext":   {}, // → fresh
}

var snapshotJobIgnored = map[string]struct{}{
	"ChatType":       {}, // executeOpt sends to ChatID directly; ChatType is for routing only
	"CreatedBy":      {},
	"CreatedAt":      {},
	"Paused":         {}, // re-checked under s.mu in the cron-callback wrapper, not in snapshot
	"LastResult":     {},
	"LastRunAt":      {},
	"LastError":      {},
	"LastErrorClass": {},
	"LastSessionID":  {}, // recordResult-only: written, not read at execute time
	"RunCounters":    {},
	"entryID":        {}, // robfig/cron handle, not a job-content field
}

func TestJobSnapshot_FieldCoverage(t *testing.T) {
	// Reflect-walk every field on Job and assert each lives in exactly
	// one of the two classification maps. A new Job field with no
	// classification fails this test, forcing the reviewer to choose:
	// "snapshotJob must read this under s.mu" → add to snapshotJobMustCapture
	// AND extend snapshotJob.
	// "executeOpt never reads this" → add to snapshotJobIgnored.
	rt := reflect.TypeOf(Job{})
	seen := map[string]struct{}{}
	for i := 0; i < rt.NumField(); i++ {
		seen[rt.Field(i).Name] = struct{}{}
	}

	// Every name in our maps must correspond to a real Job field; if a
	// field is renamed/removed without updating the map the test fails
	// rather than silently drifting.
	for name := range snapshotJobMustCapture {
		if _, ok := seen[name]; !ok {
			t.Errorf("snapshotJobMustCapture lists %q but Job has no such field — update both", name)
		}
	}
	for name := range snapshotJobIgnored {
		if _, ok := seen[name]; !ok {
			t.Errorf("snapshotJobIgnored lists %q but Job has no such field — update both", name)
		}
	}

	// Every Job field must be classified.
	var unclassified []string
	for name := range seen {
		_, must := snapshotJobMustCapture[name]
		_, ignore := snapshotJobIgnored[name]
		if must && ignore {
			t.Errorf("Job field %q is in BOTH snapshotJobMustCapture and snapshotJobIgnored — pick one", name)
			continue
		}
		if !must && !ignore {
			unclassified = append(unclassified, name)
		}
	}
	sort.Strings(unclassified)
	if len(unclassified) > 0 {
		t.Fatalf(
			"new Job field(s) %v have no classification; add each to snapshotJobMustCapture (and update snapshotJob in scheduler.go) or to snapshotJobIgnored if executeOpt does not read it under s.mu",
			unclassified,
		)
	}
}
