package kirojsonl

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSource_LoadBefore_TailSeekSurfacesNewest pins #2288: for a kiro session
// file larger than maxFileBytes the source must read the LAST maxFileBytes
// (the newest prompts), not the first. Reading from offset 0 surfaced only the
// oldest turns. We bracket a >16 MiB filler region with an old prompt at the
// head and a fresh prompt at the tail and assert the tail wins.
func TestSource_LoadBefore_TailSeekSurfacesNewest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sid := "kiro-session-tailseek"

	var sb strings.Builder
	// Oldest prompt at offset 0 — must NOT survive the tail window.
	sb.WriteString(promptLine("OLDEST should be dropped", 1_700_000_000) + "\n")
	// Filler prompts to push the file past maxFileBytes (16 MiB).
	filler := strings.Repeat("x", 4096)
	fillerLine := fmt.Sprintf(
		`{"version":"v1","kind":"Prompt","data":{"message_id":"filler","content":[{"kind":"text","data":%q}],"meta":{"timestamp":1700000100}}}`+"\n",
		filler,
	)
	for written := 0; written < (17 << 20); written += len(fillerLine) {
		sb.WriteString(fillerLine)
	}
	// Newest prompt at the tail — must survive.
	sb.WriteString(promptLine("NEWEST must be visible", 1_700_009_999) + "\n")

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}

	src := New(dir, func() string { return sid })
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no entries returned from oversized session")
	}
	newest := got[len(got)-1]
	if newest.Summary != "NEWEST must be visible" {
		t.Fatalf("newest entry not surfaced from >16MiB session; got %q (%d entries)", newest.Summary, len(got))
	}
	for _, e := range got {
		if strings.Contains(e.Summary, "OLDEST") {
			t.Errorf("oldest head record leaked into tail window: %+v", e)
		}
	}
}
