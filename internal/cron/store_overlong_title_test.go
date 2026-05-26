package cron

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestR250GO12_LoadJobsDropsOverlongTitle pins #1075 / R250-GO-12: a
// cron_jobs.json hand-edit (or a file persisted before MaxCronTitleLen
// existed) carrying a Title whose rune count exceeds MaxCronTitleLen
// must be dropped at load time. Without this defence-in-depth gate
// the entry would round-trip back to disk on every persist and
// inflate every /api/cron list broadcast.
//
// The write path (addJobAcquiringLock + UpdateJob) already enforces
// the same cap; this test asserts loadJobs mirrors it.
func TestR250GO12_LoadJobsDropsOverlongTitle(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	path := filepath.Join(tmp, "cron_jobs.json")

	overlongID := mustGenerateID()
	overlong := &Job{
		ID:        overlongID,
		Schedule:  "@every 1m",
		Prompt:    "ping",
		Title:     strings.Repeat("a", MaxCronTitleLen+1),
		Platform:  "feishu",
		ChatID:    "chat-1",
		ChatType:  "private",
		CreatedBy: "tester",
	}
	validID := mustGenerateID()
	valid := &Job{
		ID:        validID,
		Schedule:  "@every 1m",
		Prompt:    "ping",
		Title:     "ok",
		Platform:  "feishu",
		ChatID:    "chat-1",
		ChatType:  "private",
		CreatedBy: "tester",
	}

	payload, err := json.Marshal([]*Job{overlong, valid})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write store: %v", err)
	}

	m, err := loadJobs(path)
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	if _, ok := m[overlongID]; ok {
		t.Errorf("overlong-title job (%d runes) must be dropped, but was loaded",
			MaxCronTitleLen+1)
	}
	if _, ok := m[validID]; !ok {
		t.Errorf("valid job missing — drop logic must skip the offender, not abort the load")
	}
}

// TestR250GO12_LoadJobsAcceptsExactlyAtCap asserts the rune-count gate is
// inclusive of MaxCronTitleLen — exactly cap runes is allowed, only
// strictly greater than cap is rejected. Mirrors AddJob's check
// (`> MaxCronTitleLen`).
func TestR250GO12_LoadJobsAcceptsExactlyAtCap(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	path := filepath.Join(tmp, "cron_jobs.json")

	id := mustGenerateID()
	j := &Job{
		ID:        id,
		Schedule:  "@every 1m",
		Prompt:    "ping",
		Title:     strings.Repeat("a", MaxCronTitleLen),
		Platform:  "feishu",
		ChatID:    "chat-1",
		ChatType:  "private",
		CreatedBy: "tester",
	}
	payload, err := json.Marshal([]*Job{j})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write store: %v", err)
	}
	m, err := loadJobs(path)
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	if _, ok := m[id]; !ok {
		t.Errorf("Title at exactly MaxCronTitleLen must load — gate is `>` not `>=`")
	}
}
