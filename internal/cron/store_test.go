package cron

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadJobs_DropsBadEntries: a tampered or hand-edited cron_jobs.json must
// load what it can and drop the rest without aborting:
//   - JSON null entries are skipped, not dereferenced (R20260526-CR-001: the
//     first nil entry used to panic the scheduler on startup);
//   - a duplicate ID keeps the first occurrence (R260528-BUG-8: the second
//     used to overwrite it silently);
//   - a Title longer than MaxCronTitleLen runes is dropped, one exactly at the
//     cap is kept (#1075 — the write path's cap, mirrored at load).
func TestLoadJobs_DropsBadEntries(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "cron_jobs.json")

	job := func(id, prompt, chat, title string) string {
		b, err := json.Marshal(&Job{ID: id, Schedule: "@every 1m", Prompt: prompt, Title: title,
			Platform: "feishu", ChatID: chat, ChatType: "private", CreatedBy: "tester"})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	dupID, overlongID, atCapID := mustGenerateID(), mustGenerateID(), mustGenerateID()
	payload := "[null," + strings.Join([]string{
		job(dupID, "first wins", "chat-A", ""),
		job(dupID, "second loses", "chat-B", ""),
		job(overlongID, "ping", "chat-1", strings.Repeat("a", MaxCronTitleLen+1)),
		job(atCapID, "ping", "chat-1", strings.Repeat("a", MaxCronTitleLen)),
	}, ",") + ",null]"
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatalf("write store: %v", err)
	}

	m, err := loadJobs(path)
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	if len(m) != 2 {
		t.Fatalf("loaded %d jobs, want 2 (dup collapsed, overlong title dropped, nulls skipped)", len(m))
	}
	if got := m[dupID]; got == nil || got.Prompt != "first wins" || got.ChatID != "chat-A" {
		t.Errorf("duplicate ID loaded as %+v, want the first occurrence", got)
	}
	if _, ok := m[overlongID]; ok {
		t.Error("overlong-title job must be dropped")
	}
	if _, ok := m[atCapID]; !ok {
		t.Error("title at exactly MaxCronTitleLen must load — the gate is `>` not `>=`")
	}
}
