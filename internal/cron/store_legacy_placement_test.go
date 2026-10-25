package cron

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadJobs_PausesRemovedSandboxPlacement: a job saved for the removed
// cloud-sandbox placement loads paused rather than silently running on this
// host; a local job and an unknown placement value are untouched, and the
// next persist drops the key.
func TestLoadJobs_PausesRemovedSandboxPlacement(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "cron_jobs.json")
	body := `[
		{"id":"aaaaaaaaaaaaaaaa","schedule":"@every 1h","prompt":"p","placement":"sandbox","side_effects":true},
		{"id":"bbbbbbbbbbbbbbbb","schedule":"@every 1h","prompt":"p","placement":"local"},
		{"id":"cccccccccccccccc","schedule":"@every 1h","prompt":"p"}
	]`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	jobs, err := loadJobs(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 {
		t.Fatalf("loaded %d jobs, want 3", len(jobs))
	}
	if j := jobs["aaaaaaaaaaaaaaaa"]; !j.Paused {
		t.Error("sandbox-placement job must load paused")
	}
	for _, id := range []string{"bbbbbbbbbbbbbbbb", "cccccccccccccccc"} {
		if jobs[id].Paused {
			t.Errorf("job %s must stay active", id)
		}
	}
	out, err := json.Marshal(jobs["aaaaaaaaaaaaaaaa"])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "placement") || strings.Contains(string(out), "side_effects") {
		t.Errorf("persist kept a removed key: %s", out)
	}
}
