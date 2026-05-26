package cron

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestLoadJobs_NilEntry verifies loadJobs survives a tampered or hand-edited
// cron_jobs.json that contains JSON null entries in the top-level array.
// Without the nil-guard, the first nil-entry deref (j.ID == "") panics with
// a NPE and the scheduler crashes on startup.
//
// R20260526-CR-001 — internal/cron/store.go:134.
func TestLoadJobs_NilEntry(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	path := filepath.Join(tmp, "cron_jobs.json")

	// Build a payload mixing a nil entry with one valid Job. We construct
	// the JSON by hand to ensure the nil literal is preserved literally as
	// `null` rather than being elided by encoding/json.
	validID := generateID()
	valid := &Job{
		ID:        validID,
		Schedule:  "@every 1m",
		Prompt:    "ping",
		Platform:  "feishu",
		ChatID:    "chat-1",
		ChatType:  "private",
		CreatedBy: "tester",
	}
	validBytes, err := json.Marshal(valid)
	if err != nil {
		t.Fatalf("marshal valid job: %v", err)
	}
	payload := []byte("[null," + string(validBytes) + ",null]")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write store: %v", err)
	}

	// loadJobs MUST NOT panic on the nil entries; valid entry survives.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("loadJobs panicked on nil entry: %v", r)
		}
	}()
	m, err := loadJobs(path)
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	if got, want := len(m), 1; got != want {
		t.Fatalf("loaded jobs = %d, want %d (nil entries should be skipped)", got, want)
	}
	if _, ok := m[validID]; !ok {
		t.Fatalf("valid job %q missing from loaded map", validID)
	}
}
