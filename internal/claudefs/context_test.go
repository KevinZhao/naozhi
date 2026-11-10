package claudefs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLastContextTokens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	lines := []string{
		`{"type":"user","message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":100,"cache_creation_input_tokens":5,"output_tokens":7}}}`,
		`{"type":"assistant","message":{"usage":{"input_tokens":3,"cache_read_input_tokens":200000,"cache_creation_input_tokens":60000,"output_tokens":9}}}`,
		// A sub-agent's line and a trailing non-assistant line are skipped.
		`{"type":"assistant","isSidechain":true,"message":{"usage":{"input_tokens":1,"cache_read_input_tokens":1,"cache_creation_input_tokens":1}}}`,
		`{"type":"system","subtype":"turn_duration"}`,
		`{"type":"assistant","message":{"usage":`, // mid-write
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	n, found, err := LastContextTokens(path)
	if err != nil || !found || n != 260003 {
		t.Fatalf("LastContextTokens = %d, %v, %v; want 260003, true, nil", n, found, err)
	}

	if err := os.WriteFile(path, []byte(lines[0]+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, found, err := LastContextTokens(path); err != nil || found || n != 0 {
		t.Fatalf("no assistant line: %d, %v, %v", n, found, err)
	}
	if _, _, err := LastContextTokens(filepath.Join(dir, "missing.jsonl")); err == nil {
		t.Fatal("missing file: want an error")
	}
}
