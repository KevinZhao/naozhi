package cron

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestJob_BackendPersisted: a dashboard-picked backend survives a restart via
// cron_jobs.json, and a job without one keeps the pre-backend on-disk shape
// (no "backend" key) so a rolled-back binary still reads the file.
func TestJob_BackendPersisted(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(Job{ID: "j", Schedule: "@hourly", Backend: "kiro"})
	if err != nil {
		t.Fatal(err)
	}
	var got Job
	if err := json.Unmarshal(raw, &got); err != nil || got.Backend != "kiro" {
		t.Fatalf("Backend round-trip: got %q (err %v), want kiro; json %s", got.Backend, err, raw)
	}
	raw, err = json.Marshal(Job{ID: "legacy", Schedule: "@hourly"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"backend"`) {
		t.Errorf("job without a backend marshalled a backend key: %s", raw)
	}
}
