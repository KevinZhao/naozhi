package discovery

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestReadBoundedSessionFile_OK pins the happy-path contract: a small
// file is read in full and returned byte-for-byte.
func TestReadBoundedSessionFile_OK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "ok.json")
	want := []byte(`{"pid":42,"session_id":"abc"}`)
	if err := os.WriteFile(path, want, 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readBoundedSessionFile(path)
	if err != nil {
		t.Fatalf("readBoundedSessionFile: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("data mismatch: got %q, want %q", got, want)
	}
}

// TestReadBoundedSessionFile_OversizedRejected pins R237-PERF-7 (#676):
// an oversized session file must be rejected without returning the
// partial contents (so the caller cannot json.Unmarshal a truncated
// payload).
func TestReadBoundedSessionFile_OversizedRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.json")
	// Write maxSessionFileBytes+1 bytes — explicitly past the cap.
	big := make([]byte, maxSessionFileBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	if err := os.WriteFile(path, big, 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := readBoundedSessionFile(path)
	if err == nil {
		t.Fatalf("expected error for oversized file, got data len=%d", len(data))
	}
	if data != nil {
		t.Fatalf("data must be nil on oversize-reject, got len=%d", len(data))
	}
}

// TestReadBoundedSessionFile_Missing pins that a missing path surfaces
// the error rather than crashing.
func TestReadBoundedSessionFile_Missing(t *testing.T) {
	t.Parallel()
	if _, err := readBoundedSessionFile(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected error for missing file")
	}
}
