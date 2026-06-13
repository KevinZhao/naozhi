package cron

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSandboxEventLog(t *testing.T, storePath, jobID, runID string, lines []string) {
	t.Helper()
	dir := filepath.Join(filepath.Dir(storePath), "sandboxevents", jobID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := strings.Join(lines, "\n")
	if len(lines) > 0 {
		body += "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, runID+".ndjson"), []byte(body), 0o600); err != nil {
		t.Fatalf("write event log: %v", err)
	}
}

func TestSandboxRunEvents_ReadsAllLines(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)

	lines := []string{
		`{"kind":"boot","msg":"materialized"}`,
		`{"kind":"cli","line":{"type":"result","is_error":false}}`,
		`{"kind":"meta","image_version":"phase2"}`,
		`{"kind":"exit","code":0}`,
	}
	writeSandboxEventLog(t, storePath, "0123456789abcdef", "feedfacefeedface", lines)

	got, truncated, err := s.SandboxRunEvents("0123456789abcdef", "feedfacefeedface", 100)
	if err != nil {
		t.Fatalf("SandboxRunEvents: %v", err)
	}
	if truncated {
		t.Fatal("must not truncate when under cap")
	}
	if len(got) != len(lines) {
		t.Fatalf("got %d lines, want %d", len(got), len(lines))
	}
	for i := range lines {
		if string(got[i]) != lines[i] {
			t.Fatalf("line %d = %q, want %q", i, got[i], lines[i])
		}
	}
}

func TestSandboxRunEvents_MissingFileIsEmpty(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)

	// No event log written — a local run / events-disabled deploy. Must
	// return (nil, false, nil), not an error, so the UI renders empty.
	got, truncated, err := s.SandboxRunEvents("0123456789abcdef", "feedfacefeedface", 100)
	if err != nil || got != nil || truncated {
		t.Fatalf("missing file: got=%v truncated=%v err=%v, want nil/false/nil", got, truncated, err)
	}
}

func TestSandboxRunEvents_TruncatesAtCap(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)

	lines := make([]string, 10)
	for i := range lines {
		lines[i] = `{"kind":"cli","line":{"type":"assistant"}}`
	}
	writeSandboxEventLog(t, storePath, "0123456789abcdef", "feedfacefeedface", lines)

	got, truncated, err := s.SandboxRunEvents("0123456789abcdef", "feedfacefeedface", 3)
	if err != nil {
		t.Fatalf("SandboxRunEvents: %v", err)
	}
	if !truncated {
		t.Fatal("must report truncated when over cap")
	}
	if len(got) != 3 {
		t.Fatalf("got %d lines, want cap 3", len(got))
	}
}

func TestSandboxRunEvents_ExactlyCapNotTruncated(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)

	lines := make([]string, 3)
	for i := range lines {
		lines[i] = `{"kind":"cli","line":{"type":"assistant"}}`
	}
	writeSandboxEventLog(t, storePath, "0123456789abcdef", "feedfacefeedface", lines)

	got, truncated, err := s.SandboxRunEvents("0123456789abcdef", "feedfacefeedface", 3)
	if err != nil {
		t.Fatalf("SandboxRunEvents: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d lines, want 3", len(got))
	}
	// A file with EXACTLY maxLines valid lines must NOT report truncated —
	// nothing was dropped (review PR-3 off-by-one).
	if truncated {
		t.Fatal("exactly-cap file must not be flagged truncated")
	}
}

func TestSandboxRunEvents_SkipsCorruptLine(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)

	writeSandboxEventLog(t, storePath, "0123456789abcdef", "feedfacefeedface", []string{
		`{"kind":"cli"}`,
		`{not json`, // corrupt tail (e.g. crash mid-write)
		`{"kind":"exit","code":0}`,
	})

	got, _, err := s.SandboxRunEvents("0123456789abcdef", "feedfacefeedface", 100)
	if err != nil {
		t.Fatalf("SandboxRunEvents: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d valid lines, want 2 (corrupt line skipped)", len(got))
	}
}

func TestSandboxRunEvents_RejectsBadIDs(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)

	if _, _, err := s.SandboxRunEvents("../etc", "feedfacefeedface", 10); err == nil {
		t.Fatal("must reject non-hex jobID (path traversal guard)")
	}
	if _, _, err := s.SandboxRunEvents("0123456789abcdef", "../../x", 10); err == nil {
		t.Fatal("must reject non-hex runID")
	}
}

// TestSandboxRunEvents_BusyWhenSemSaturated verifies the concurrency gate
// [R20260613-SEC-5 / #2066]: when all sandboxEventsSemCap slots are held, a
// further read fails fast with ErrSandboxEventsBusy instead of allocating
// another scanner buffer.
func TestSandboxRunEvents_BusyWhenSemSaturated(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)
	writeSandboxEventLog(t, storePath, "0123456789abcdef", "feedfacefeedface",
		[]string{`{"kind":"boot"}`})

	// Saturate the package-level semaphore, then restore it on cleanup so the
	// gate does not leak into sibling tests sharing the process-wide channel.
	for i := 0; i < sandboxEventsSemCap; i++ {
		sandboxEventsSem <- struct{}{}
	}
	t.Cleanup(func() {
		for i := 0; i < sandboxEventsSemCap; i++ {
			<-sandboxEventsSem
		}
	})

	if _, _, err := s.SandboxRunEvents("0123456789abcdef", "feedfacefeedface", 10); !errors.Is(err, ErrSandboxEventsBusy) {
		t.Fatalf("saturated sem: err = %v, want ErrSandboxEventsBusy", err)
	}
}

// TestSandboxRunEvents_ReleasesSemOnReturn verifies the semaphore slot is
// freed once a read completes, so back-to-back reads (the common case) all
// succeed rather than the gate latching after the first.
func TestSandboxRunEvents_ReleasesSemOnReturn(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)
	writeSandboxEventLog(t, storePath, "0123456789abcdef", "feedfacefeedface",
		[]string{`{"kind":"boot"}`})

	for i := 0; i < sandboxEventsSemCap+2; i++ {
		if _, _, err := s.SandboxRunEvents("0123456789abcdef", "feedfacefeedface", 10); err != nil {
			t.Fatalf("read %d: unexpected err %v (slot not released?)", i, err)
		}
	}
}

// TestSandboxRunEvents_LineSizeCap verifies a line exceeding
// sandboxEventsMaxLineSize is handled as a scan error rather than silently
// growing the buffer to the old 16 MB ceiling [R20260613-SEC-5 / #2066].
func TestSandboxRunEvents_LineSizeCap(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)

	// One valid small line followed by an oversize line (> 1 MB cap).
	big := `{"kind":"huge","data":"` + strings.Repeat("x", sandboxEventsMaxLineSize+1) + `"}`
	writeSandboxEventLog(t, storePath, "0123456789abcdef", "feedfacefeedface",
		[]string{`{"kind":"boot"}`, big})

	got, _, err := s.SandboxRunEvents("0123456789abcdef", "feedfacefeedface", 100)
	if err == nil {
		t.Fatal("oversize line must surface a scan error (bufio.ErrTooLong)")
	}
	// The healthy head line is still returned alongside the error.
	if len(got) != 1 || string(got[0]) != `{"kind":"boot"}` {
		t.Fatalf("got %d lines %q, want the single head line", len(got), got)
	}
}
