package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTranscriptReader_PersistentFd_ReusesAcrossCalls pins R233-PERF-4 /
// R228-PERF-3: the prior open+ReadAll+close-per-call burned ~250
// fd-lifecycle syscalls/s under agent_tailer's 200ms × 50-tailer load.
// The persistent-fd path must keep r.f stable across Read/Tail invocations
// when the inode hasn't changed.
func TestTranscriptReader_PersistentFd_ReusesAcrossCalls(t *testing.T) {
	t.Parallel()
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`
	path := tmpFile(t, line+"\n")
	r := NewTranscriptReader(path)
	defer r.Close() //nolint:errcheck

	if _, err := r.Read(0, 100); err != nil {
		t.Fatalf("first Read: %v", err)
	}
	r.mu.Lock()
	first := r.f
	r.mu.Unlock()
	if first == nil {
		t.Fatal("expected r.f populated after first Read")
	}

	// Several more polls — the same fd must be retained.
	for i := 0; i < 5; i++ {
		if _, err := r.Tail(); err != nil {
			t.Fatalf("Tail #%d: %v", i, err)
		}
		r.mu.Lock()
		got := r.f
		r.mu.Unlock()
		if got != first {
			t.Errorf("Tail #%d: r.f changed (%p → %p); fd was not reused", i, first, got)
		}
	}
}

// TestTranscriptReader_PersistentFd_RotationResetsBookkeeping verifies
// that when the path is replaced (rm + create with new content) the next
// Read picks up the new inode, drops the cached fd, and resets offset/tail
// so we don't index into the new file with stale offsets.
func TestTranscriptReader_PersistentFd_RotationResetsBookkeeping(t *testing.T) {
	t.Parallel()
	line1 := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"first"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`
	line2 := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"second"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:01Z"}`

	path := tmpFile(t, line1+"\n")
	r := NewTranscriptReader(path)
	defer r.Close() //nolint:errcheck

	ents, err := r.Read(0, 100)
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if len(ents) != 1 || ents[0].Summary != "first" {
		t.Fatalf("first read: %+v", ents)
	}

	// Capture the original fd pointer to confirm it gets swapped.
	r.mu.Lock()
	priorFD := r.f
	priorOffset := r.offset
	r.mu.Unlock()
	if priorOffset == 0 {
		t.Fatal("offset should advance past line1")
	}

	// Rotation: remove the path and write a fresh file at the same name
	// containing only line2. rm + create guarantees a different inode on
	// every filesystem we run on.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.WriteFile(path, []byte(line2+"\n"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	ents2, err := r.Tail()
	if err != nil {
		t.Fatalf("post-rotation Tail: %v", err)
	}
	if len(ents2) != 1 || ents2[0].Summary != "second" {
		t.Fatalf("post-rotation Tail entries = %+v, want [{second}]", ents2)
	}
	r.mu.Lock()
	newFD := r.f
	r.mu.Unlock()
	if newFD == priorFD {
		t.Error("rotation should swap r.f to a new fd; it is unchanged")
	}
}

// TestTranscriptReader_Close_Idempotent locks the Close contract: idempotent,
// safe to call without ever touching Read/Tail, and drops the cached fd so a
// subsequent Read on a fresh reader still works (lazy reopen).
func TestTranscriptReader_Close_Idempotent(t *testing.T) {
	t.Parallel()
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"x"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`
	path := tmpFile(t, line+"\n")

	// Close-without-open: must not panic, must not error.
	r := NewTranscriptReader(path)
	if err := r.Close(); err != nil {
		t.Fatalf("Close on never-opened reader: %v", err)
	}

	// Open via Read, then close, then close again.
	r2 := NewTranscriptReader(path)
	if _, err := r2.Read(0, 100); err != nil {
		t.Fatalf("Read: %v", err)
	}
	r2.mu.Lock()
	hadFD := r2.f != nil
	r2.mu.Unlock()
	if !hadFD {
		t.Fatal("expected r.f populated after Read")
	}
	if err := r2.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	r2.mu.Lock()
	postFD := r2.f
	r2.mu.Unlock()
	if postFD != nil {
		t.Error("Close did not clear r.f")
	}
	// Second Close must be a silent no-op.
	if err := r2.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	// Fresh reader after the original Close still works (reopen path).
	r3 := NewTranscriptReader(path)
	defer r3.Close() //nolint:errcheck
	if _, err := r3.Read(0, 100); err != nil {
		t.Fatalf("Read on fresh reader after prior Close: %v", err)
	}
}

// TestTranscriptReader_GrowingFile_ReusesFdWithoutReopen pins
// R20260607-PERF-3 (#1884): on the steady-state path (a live fd that just
// yielded fresh bytes) openOrReuse must reuse the cached fd with no reopen —
// the per-poll os.Stat(r.path) rotation probe only fires on a zero-byte poll.
// We assert the fd pointer is stable as the file grows across polls.
func TestTranscriptReader_GrowingFile_ReusesFdWithoutReopen(t *testing.T) {
	t.Parallel()
	mk := func(i int) string {
		return `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"line` +
			string(rune('0'+i)) + `"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:0` +
			string(rune('0'+i)) + `Z"}` + "\n"
	}
	path := tmpFile(t, mk(0))
	r := NewTranscriptReader(path)
	defer r.Close() //nolint:errcheck

	if _, err := r.Read(0, 100); err != nil {
		t.Fatalf("first Read: %v", err)
	}
	r.mu.Lock()
	fd := r.f
	r.mu.Unlock()
	if fd == nil {
		t.Fatal("expected r.f populated")
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	defer f.Close() //nolint:errcheck

	for i := 1; i <= 5; i++ {
		if _, err := f.WriteString(mk(i)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		ents, err := r.Tail()
		if err != nil {
			t.Fatalf("Tail %d: %v", i, err)
		}
		if len(ents) != 1 {
			t.Fatalf("Tail %d: got %d entries, want 1", i, len(ents))
		}
		r.mu.Lock()
		got := r.f
		r.mu.Unlock()
		if got != fd {
			t.Fatalf("Tail %d reopened fd (%p→%p); growing file must reuse the cached fd", i, fd, got)
		}
	}
}

// TestTranscriptReader_IdlePollThenRotation pins the rotation-after-idle
// branch of R20260607-PERF-3 (#1884): an idle (zero-byte) poll re-probes the
// inode via reprobeRotation, so a rm+create that lands while the reader is
// idle is still detected and the new content surfaced.
func TestTranscriptReader_IdlePollThenRotation(t *testing.T) {
	t.Parallel()
	line1 := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"old"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`
	line2 := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"new"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:01Z"}`
	path := tmpFile(t, line1+"\n")
	r := NewTranscriptReader(path)
	defer r.Close() //nolint:errcheck

	if ents, err := r.Read(0, 100); err != nil || len(ents) != 1 {
		t.Fatalf("first Read: ents=%+v err=%v", ents, err)
	}

	// Idle poll: no new bytes, no rotation. Must return empty and NOT reopen.
	r.mu.Lock()
	fdBefore := r.f
	r.mu.Unlock()
	if ents, err := r.Tail(); err != nil || len(ents) != 0 {
		t.Fatalf("idle Tail: ents=%+v err=%v, want empty", ents, err)
	}
	r.mu.Lock()
	fdAfterIdle := r.f
	r.mu.Unlock()
	if fdAfterIdle != fdBefore {
		t.Fatalf("idle poll with no rotation must not reopen fd (%p→%p)", fdBefore, fdAfterIdle)
	}

	// Now rotate: rm + create with new content.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.WriteFile(path, []byte(line2+"\n"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	ents, err := r.Tail()
	if err != nil {
		t.Fatalf("post-rotation Tail: %v", err)
	}
	if len(ents) != 1 || ents[0].Summary != "new" {
		t.Fatalf("post-rotation Tail = %+v, want [{new}]", ents)
	}
	r.mu.Lock()
	fdAfterRot := r.f
	r.mu.Unlock()
	if fdAfterRot == fdBefore {
		t.Error("rotation should swap r.f to a new fd")
	}
}

// TestTranscriptReader_PersistentFd_ENOENTPath verifies that when the path
// is missing the call returns os.IsNotExist-classifiable err and clears any
// cached fd, so the next call after the file reappears reopens cleanly
// (mirrors the slow-path semantics — agent_tailer relies on this for
// "agent terminated, jsonl gone" 404 surfacing).
func TestTranscriptReader_PersistentFd_ENOENTPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.jsonl")
	r := NewTranscriptReader(path)
	defer r.Close() //nolint:errcheck

	_, err := r.Read(0, 100)
	if err == nil {
		t.Fatal("Read on non-existent path: want error, got nil")
	}
	if !os.IsNotExist(err) {
		t.Errorf("Read err = %v, want os.IsNotExist-classifiable", err)
	}
	r.mu.Lock()
	if r.f != nil {
		t.Error("r.f should be nil after ENOENT")
	}
	r.mu.Unlock()

	// Path appears — next Read should succeed.
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"y"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`
	if err := os.WriteFile(path, []byte(line+"\n"), 0o644); err != nil {
		t.Fatalf("create: %v", err)
	}
	ents, err := r.Read(0, 100)
	if err != nil {
		t.Fatalf("Read after recreate: %v", err)
	}
	if len(ents) != 1 || !strings.Contains(ents[0].Summary, "y") {
		t.Errorf("entries = %+v", ents)
	}
}
