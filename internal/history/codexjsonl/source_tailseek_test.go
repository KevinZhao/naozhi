package codexjsonl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSource_LoadBefore_TailSeekSurfacesNewest pins #2288: for a rollout file
// larger than maxFileBytes the source must read the LAST maxFileBytes (the
// newest turns), not the first. Reading from offset 0 surfaced only the oldest
// turns, so a long codex session's latest messages were never visible in the
// dashboard. We bracket a >16 MiB filler region with an old user message at
// the head and a fresh agent message at the tail and assert the tail wins.
func TestSource_LoadBefore_TailSeekSurfacesNewest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "019ee988-da7f-7821-b6d1-7b74a7db62d7"

	var sb strings.Builder
	// Oldest record at offset 0 — must NOT survive the tail window.
	sb.WriteString(`{"timestamp":"2026-06-21T09:00:00.000Z","type":"event_msg","payload":{"type":"user_message","message":"OLDEST should be dropped"}}` + "\n")
	// Filler to push the file past maxFileBytes (16 MiB). Each line is a
	// renderable user_message so a regression (reading from offset 0) would
	// still return entries — but never the newest one below.
	filler := strings.Repeat("x", 4096)
	line := `{"timestamp":"2026-06-21T09:10:00.000Z","type":"event_msg","payload":{"type":"user_message","message":"` + filler + `"}}` + "\n"
	for written := 0; written < (17 << 20); written += len(line) {
		sb.WriteString(line)
	}
	// Newest record at the tail — must survive.
	sb.WriteString(`{"timestamp":"2026-06-21T09:59:59.000Z","type":"event_msg","payload":{"type":"agent_message","message":"NEWEST must be visible"}}` + "\n")

	bucketDir := filepath.Join(dir, "2026", "06", "21")
	if err := os.MkdirAll(bucketDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	name := "rollout-2026-06-21T09-00-00-" + sid + ".jsonl"
	if err := os.WriteFile(filepath.Join(bucketDir, name), []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	src := New(dir, func() string { return sid })
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no entries returned from oversized rollout")
	}
	newest := got[len(got)-1]
	if newest.Summary != "NEWEST must be visible" {
		t.Fatalf("newest entry not surfaced from >16MiB rollout; got %q (%d entries)", newest.Summary, len(got))
	}
	for _, e := range got {
		if strings.Contains(e.Summary, "OLDEST") {
			t.Errorf("oldest head record leaked into tail window: %+v", e)
		}
	}
}
