package cron

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestJob_BackendRoundTrip pins the JSON wire shape for Job.Backend:
// Marshal → Unmarshal must preserve a non-empty value losslessly so a
// dashboard-picked backend survives a Scheduler.Stop()/Start() cycle via
// cron_jobs.json. This is the contract Sprint 6c relies on for "user
// picks Kiro for one cron, Claude for another, both stick across
// restarts" — break it and every persisted job silently snaps back to
// router default after reboot.
func TestJob_BackendRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []string{
		"claude",
		"kiro",
		"some-backend_v2",
		"X1",
	}
	for _, want := range cases {
		want := want
		t.Run(want, func(t *testing.T) {
			t.Parallel()
			orig := Job{
				ID:       "round-trip",
				Schedule: "@hourly",
				Prompt:   "x",
				Platform: "p",
				ChatID:   "c",
				Backend:  want,
			}
			raw, err := json.Marshal(orig)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got Job
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.Backend != want {
				t.Fatalf("Backend round-trip: got %q, want %q", got.Backend, want)
			}
			// Defensive: the on-disk shape must include the backend key
			// when set, because the persistence path relies on the JSON
			// tag (json.Marshal of a Job with Backend != "" always emits
			// "backend":"…"). A future refactor swapping the tag to
			// `json:"-"` would silently drop the field — this assertion
			// catches that drift at test time.
			if !strings.Contains(string(raw), `"backend":"`+want+`"`) {
				t.Errorf("marshalled JSON missing backend field: %s", raw)
			}
		})
	}
}

// TestJob_BackendOmitemptyForLegacy locks the omitempty contract: an old
// cron_jobs.json file produced before Sprint 6c never wrote a "backend"
// key. The new field must keep that on-disk shape stable for legacy jobs
// (Backend == "") so:
//
//  1. older naozhi binaries can still read the file if the user rolls
//     back (forward-compat is paid for by omitempty);
//  2. operator-facing diffs of cron_jobs.json don't gain a noisy "":""
//     line for every existing job after upgrade.
//
// Reverse: an explicitly-set backend MUST produce the key (covered by
// TestJob_BackendRoundTrip), so the two tests together pin both sides
// of the omitempty contract.
func TestJob_BackendOmitemptyForLegacy(t *testing.T) {
	t.Parallel()
	legacy := Job{
		ID:       "legacy",
		Schedule: "@hourly",
		Prompt:   "x",
		Platform: "p",
		ChatID:   "c",
		// Backend deliberately left zero-value to model an older file.
	}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"backend"`) {
		t.Errorf("legacy job marshalled with backend key (omitempty broken): %s", raw)
	}
}

// TestJob_LegacyJSONUnmarshalsToEmptyBackend models the upgrade path: a
// cron_jobs.json file written by a pre-Sprint-6c naozhi has no "backend"
// key. Unmarshalling it must succeed and leave Job.Backend == "" so the
// scheduler routes the job through the router default at execute time
// (zero migration). Without this guarantee, an operator upgrading the
// binary while the persistence file already exists on disk would crash
// at scheduler startup or worse — silently re-route every job.
func TestJob_LegacyJSONUnmarshalsToEmptyBackend(t *testing.T) {
	t.Parallel()
	legacyJSON := []byte(`{
		"id": "abc123",
		"schedule": "@every 30m",
		"prompt": "report yesterday",
		"platform": "feishu",
		"chat_id": "chat-xyz",
		"created_by": "user",
		"created_at": "2026-04-01T00:00:00Z",
		"paused": false
	}`)
	var j Job
	if err := json.Unmarshal(legacyJSON, &j); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if j.Backend != "" {
		t.Fatalf("legacy unmarshal Backend = %q, want empty", j.Backend)
	}
	// Sanity check: surrounding fields still parsed normally.
	if j.ID != "abc123" || j.Schedule != "@every 30m" {
		t.Fatalf("legacy unmarshal lost other fields: id=%q schedule=%q", j.ID, j.Schedule)
	}
}
