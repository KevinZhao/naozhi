package subagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestRawScanSubagentsDir_EntInfo_SkipsOversized pins R250531-PERF-3: the
// size-cap guard must use ent.Info() (one syscall from the DirEntry cache)
// rather than os.Stat (an extra syscall per file). Functionally, a file
// exceeding maxMetaBytes (8 KiB) must be silently skipped, and a file
// within the limit with valid JSON must be returned.
func TestRawScanSubagentsDir_EntInfo_SkipsOversized(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Write a valid small meta file.
	validHex := "aabbccdd11223344a"
	validMeta := map[string]string{"agentType": "subagent"}
	writeMetaJSON(t, dir, validHex, validMeta)

	// Write an oversized meta file (>8 KiB) — must be skipped.
	oversizedHex := "bbccddee22334455b"
	big := make([]byte, 9*1024)
	for i := range big {
		big[i] = 'x'
	}
	// Wrap in braces so it parses as JSON object but agentType is missing;
	// however the size check should reject it before JSON parse.
	p := filepath.Join(dir, "agent-"+oversizedHex+".meta.json")
	if err := os.WriteFile(p, big, 0o600); err != nil {
		t.Fatalf("write oversized: %v", err)
	}

	entries := rawScanSubagentsDir(dir)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (oversized skipped), got %d", len(entries))
	}
	if entries[0].hex != validHex {
		t.Errorf("got hex=%q, want %q", entries[0].hex, validHex)
	}
	if entries[0].agentType != "subagent" {
		t.Errorf("got agentType=%q, want %q", entries[0].agentType, "subagent")
	}
}

func writeMetaJSON(t *testing.T, dir, hex string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	p := filepath.Join(dir, "agent-"+hex+".meta.json")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write meta: %v", err)
	}
}
