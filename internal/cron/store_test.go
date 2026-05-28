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
	validID := mustGenerateID()
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

// TestLoadJobsDuplicateIDWarn pins R260528-BUG-8: a hand-edited
// cron_jobs.json carrying two Job entries with the same ID must keep
// the first occurrence and drop the rest. Pre-fix, the loop's
// `m[j.ID] = j` silently let the second entry overwrite the first,
// masking the duplicate from operators while quietly losing whichever
// version came earlier in the file.
func TestLoadJobsDuplicateIDWarn(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	path := filepath.Join(tmp, "cron_jobs.json")

	dupID := mustGenerateID()
	first := &Job{
		ID:        dupID,
		Schedule:  "@every 1m",
		Prompt:    "first wins",
		Platform:  "feishu",
		ChatID:    "chat-A",
		ChatType:  "private",
		CreatedBy: "tester",
	}
	second := &Job{
		ID:        dupID,
		Schedule:  "@every 5m",
		Prompt:    "second loses",
		Platform:  "feishu",
		ChatID:    "chat-B",
		ChatType:  "private",
		CreatedBy: "tester",
	}
	firstBytes, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal first: %v", err)
	}
	secondBytes, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("marshal second: %v", err)
	}
	payload := []byte("[" + string(firstBytes) + "," + string(secondBytes) + "]")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write store: %v", err)
	}

	m, err := loadJobs(path)
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	if got, want := len(m), 1; got != want {
		t.Fatalf("loaded jobs = %d, want %d (duplicate IDs must collapse)", got, want)
	}
	got, ok := m[dupID]
	if !ok {
		t.Fatalf("dupID %q missing from loaded map", dupID)
	}
	if got.Prompt != "first wins" {
		t.Errorf("loaded job prompt = %q, want %q (first occurrence must win)",
			got.Prompt, "first wins")
	}
	if got.ChatID != "chat-A" {
		t.Errorf("loaded job chat_id = %q, want %q (first occurrence must win)",
			got.ChatID, "chat-A")
	}
}
