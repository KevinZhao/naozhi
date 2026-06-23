package cron

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/agentcore"
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

// TestSandboxEventSink_CloserIdempotent pins #2317: executeSandbox now
// `defer closeSink()` as a panic-safe fd-release fallback in addition to the
// explicit ordered close before the terminal broadcast. That double-invoke is
// only safe if the closer is idempotent (sync.Once) — otherwise the second
// f.Close() would log a spurious "close failed" WARN. Here we drive the real
// sink, flush a line, then call the closer twice and assert the event log was
// written exactly once and the second close is a no-op.
func TestSandboxEventSink_CloserIdempotent(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)

	jobID, runID := "0123456789abcdef", "feedfacefeedface"
	sink, closeSink := s.sandboxEventSink(jobID, runID, slog.Default())

	if err := sink([]byte(`{"kind":"boot"}`)); err != nil {
		t.Fatalf("sink write: %v", err)
	}

	// Two closes must not panic and must not corrupt the flushed file. The
	// second is the defer-fallback firing after the explicit close already ran.
	closeSink()
	closeSink()

	got, _, err := s.SandboxRunEvents(jobID, runID, 100)
	if err != nil {
		t.Fatalf("SandboxRunEvents: %v", err)
	}
	if len(got) != 1 || string(got[0]) != `{"kind":"boot"}` {
		t.Fatalf("event log after double-close = %v, want exactly the one flushed line", got)
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

// TestSandboxEventSink_OversizeLineDropped verifies R20260613-ARCH-2: when
// the sink receives a line >= sandboxEventsMaxLineSize it is silently
// discarded (not written to the NDJSON log) and subsequent normal-size
// lines are still written and readable by SandboxRunEvents. Without this
// guard an oversized line written to disk causes bufio.Scanner to return
// ErrTooLong, which terminates the scan and silently drops all lines that
// follow — turning one big line into a full tail loss.
func TestSandboxEventSink_OversizeLineDropped(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)

	jobID := "0123456789abcdef"
	runID := "feedfacefeedface"

	sink, closer := s.sandboxEventSink(jobID, runID, slog.Default())

	// Send an oversized line (>= sandboxEventsMaxLineSize bytes).
	oversized := make([]byte, sandboxEventsMaxLineSize)
	for i := range oversized {
		oversized[i] = 'x'
	}
	if err := sink(oversized); err != nil {
		t.Fatalf("sink returned error for oversized line, want nil (degrade gracefully): %v", err)
	}

	// Send a normal-size line after the oversized one.
	normal := []byte(`{"kind":"boot","msg":"ok"}`)
	if err := sink(normal); err != nil {
		t.Fatalf("sink returned error for normal line: %v", err)
	}
	closer()

	// SandboxRunEvents must return only the normal line; the oversized line
	// was never written so there is no ErrTooLong to interrupt the scan.
	got, _, err := s.SandboxRunEvents(jobID, runID, 100)
	if err != nil {
		t.Fatalf("SandboxRunEvents returned error (oversized line must not break reader): %v", err)
	}
	if len(got) != 1 || string(got[0]) != string(normal) {
		t.Fatalf("got lines %q, want exactly the normal line %q", got, normal)
	}
}

// TestSandboxRunEvents_LineSizeCap verifies a line exceeding
// sandboxEventsMaxLineSize is handled as a scan error rather than silently
// growing the scanner buffer without bound. The cap is the shared
// agentcore.MaxEnvelopeLineBytes ceiling [#2083; concurrency bounded by
// sandboxEventsSemCap, R20260613-SEC-5 / #2066].
func TestSandboxRunEvents_LineSizeCap(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)

	// One valid small line followed by a line that exceeds the shared cap.
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

// TestSandboxEvents_LargeLineWriteReadRoundTrip is the #2083 regression: a
// single tool-result line in the 1–16MB band that the SSE decoder accepts
// must also be writable by the sink AND readable back by SandboxRunEvents.
// Before the cap was unified (16MB writer / 1MB reader) such a line wrote to
// disk but the reader's scanner hit bufio.ErrTooLong and silently dropped it
// plus every later event. Now both ends share agentcore.MaxEnvelopeLineBytes,
// so a 4MB line (well above the old 1MB reader cap, well below the shared
// 16MB ceiling) survives the full round trip with the following line intact.
func TestSandboxEvents_LargeLineWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)

	jobID := "0123456789abcdef"
	runID := "feedfacefeedface"

	// 4MB JSON line: above the pre-#2083 1MB reader cap, below the 16MB
	// shared ceiling. Valid JSON so it is not skipped as a corrupt line.
	const payloadLen = 4 << 20
	big := []byte(`{"kind":"cli","line":{"type":"tool_result","data":"` +
		strings.Repeat("x", payloadLen) + `"}}`)
	if len(big) >= sandboxEventsMaxLineSize {
		t.Fatalf("test payload %d must stay below the shared cap %d",
			len(big), sandboxEventsMaxLineSize)
	}

	sink, closer := s.sandboxEventSink(jobID, runID, slog.Default())
	if err := sink(big); err != nil {
		t.Fatalf("sink rejected an in-band large line: %v", err)
	}
	follow := []byte(`{"kind":"exit","code":0}`)
	if err := sink(follow); err != nil {
		t.Fatalf("sink rejected the following line: %v", err)
	}
	closer()

	got, truncated, err := s.SandboxRunEvents(jobID, runID, 100)
	if err != nil {
		t.Fatalf("SandboxRunEvents errored on an in-band large line (the #2083 bug): %v", err)
	}
	if truncated {
		t.Fatal("must not report truncated; both lines fit under maxLines")
	}
	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2 (large line + follower both readable)", len(got))
	}
	if string(got[0]) != string(big) {
		t.Fatalf("large line did not round-trip: got %d bytes, want %d", len(got[0]), len(big))
	}
	if string(got[1]) != string(follow) {
		t.Fatalf("follower line lost after large line: got %q", got[1])
	}
}

// TestSandboxEventsCap_SharesAgentcoreCeiling pins the single-source-of-truth
// invariant (#2083): the cron reader's line cap must equal the agentcore SSE
// decoder's accept ceiling. If a future edit forks one end, this fails before
// the silent-drop bug can recur.
func TestSandboxEventsCap_SharesAgentcoreCeiling(t *testing.T) {
	if sandboxEventsMaxLineSize != agentcore.MaxEnvelopeLineBytes {
		t.Fatalf("reader cap %d != agentcore wire ceiling %d — the two ends have drifted (#2083)",
			sandboxEventsMaxLineSize, agentcore.MaxEnvelopeLineBytes)
	}
}
