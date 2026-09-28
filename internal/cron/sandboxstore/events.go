package sandboxstore

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/naozhi/naozhi/internal/limits"
)

// EventSink opens the per-run event log
// (<root>/sandboxevents/<jobID>/<runID>.ndjson) and returns a sink
// writing one envelope per line, plus a closer. Streaming to disk means the
// events received before a mid-job stream break are already durable. On open
// failure the sink degrades to a no-op with one WARN (the run is more valuable
// than its event log). Deliberately separate from the runStore's runs/ tree.
func (st Store) EventSink(jobID, runID string, lg *slog.Logger) (sink func([]byte) error, closer func()) {
	if st.Root == "" {
		return func([]byte) error { return nil }, func() {}
	}
	dir := st.Subtree("sandboxevents", jobID)
	if err := st.MkdirSubtree(dir); err != nil {
		lg.Warn("cron sandbox: event log dir create failed; events not persisted", "err", err)
		return func([]byte) error { return nil }, func() {}
	}
	// runID is scheduler-generated hex, path-safe by construction; join
	// defensively anyway.
	f, err := os.OpenFile(filepath.Join(dir, runID+".ndjson"),
		os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		lg.Warn("cron sandbox: event log open failed; events not persisted", "err", err)
		return func([]byte) error { return nil }, func() {}
	}
	w := bufio.NewWriterSize(f, 64*1024)
	// Write failures degrade to a no-op sink (one WARN): a naozhi-side disk error
	// must not abort a healthy run — propagating it would classify the run
	// failed-transport and Stop a microVM whose stream is fine. Per-line Flush
	// keeps crash durability to at most the line being written.
	degraded := false
	sink = func(line []byte) error {
		if degraded {
			return nil
		}
		// The reader (RunEvents) caps a token at EventsMaxLineSize; a
		// line reaching it would make the scanner hit ErrTooLong and drop every later
		// event, so drop just the oversized line with a WARN instead (#2083). `>=`
		// keeps line+'\n' <= cap.
		if len(line) >= EventsMaxLineSize {
			lg.Warn("cron sandbox: oversized event line dropped; will not be readable by scanner",
				"len", len(line))
			return nil
		}
		_, werr := w.Write(line)
		if werr == nil {
			werr = w.WriteByte('\n')
		}
		if werr == nil {
			werr = w.Flush()
		}
		if werr != nil {
			degraded = true
			lg.Warn("cron sandbox: event log write failed; further events not persisted", "err", werr)
		}
		return nil
	}
	// Single fd-release path, idempotent via sync.Once so callers can order the
	// explicit flush before the RunEnded broadcast AND `defer closeSink()` as a
	// panic-safe fallback without double-closing (#2317).
	var closeOnce sync.Once
	closer = func() {
		closeOnce.Do(func() {
			if err := w.Flush(); err != nil && !degraded {
				lg.Warn("cron sandbox: event log flush failed", "err", err)
			}
			if err := f.Close(); err != nil {
				lg.Warn("cron sandbox: event log close failed", "err", err)
			}
		})
	}
	return sink, closer
}

// RunEvents reads the persisted event log for one sandbox run
// (sandboxevents/<jobID>/<runID>.ndjson) and returns up to maxLines raw NDJSON
// lines (no trailing newline) for the dashboard run-detail event stream.
//
// Returns (nil, nil) when the file does not exist (local run, events disabled,
// sink degraded on open) so the caller renders an empty stream. jobID/runID
// are re-validated defensively for path safety. maxLines keeps the FIRST
// maxLines (boot + early turns are the most useful for "where did it break");
// the truncated flag lets the UI show "… N more events".
func (st Store) RunEvents(jobID, runID string, maxLines int) ([][]byte, bool, error) {
	if st.Root == "" {
		return nil, false, nil
	}
	if !validID(jobID) || !validID(runID) {
		return nil, false, fmt.Errorf("cron sandbox: invalid jobID/runID")
	}
	if maxLines <= 0 {
		maxLines = eventsDefaultMax
	}
	// Bound concurrent reads: a non-blocking acquire fails fast with
	// ErrEventsBusy rather than letting a burst pin unbounded scanner
	// buffers (#2066).
	select {
	case eventsSem <- struct{}{}:
		defer func() { <-eventsSem }()
	default:
		return nil, false, ErrEventsBusy
	}
	path := st.Subtree("sandboxevents", jobID, runID+".ndjson")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil // no event log — render empty stream
		}
		return nil, false, fmt.Errorf("cron sandbox: open event log: %w", err)
	}
	defer f.Close()

	out := make([][]byte, 0, maxLines)
	sc := bufio.NewScanner(f)
	// Cap a single line at EventsMaxLineSize so a concurrent burst cannot
	// pin gigabytes of scanner buffers.
	sc.Buffer(make([]byte, 64*1024), EventsMaxLineSize)
	truncated := false
	for sc.Scan() {
		line := sc.Bytes()
		if !json.Valid(line) {
			continue // skip any partial/corrupt tail line
		}
		cp := make([]byte, len(line))
		copy(cp, line)
		out = append(out, cp)
		// A file with exactly maxLines valid lines must NOT report truncated: peek
		// for a further valid line first.
		if len(out) >= maxLines {
			if hasMoreValidJSON(sc) {
				truncated = true
			}
			break
		}
	}
	if err := sc.Err(); err != nil {
		// Return the partial head plus the error; a missing tail means truncated, so
		// the UI signals an incomplete stream.
		return out, true, fmt.Errorf("cron sandbox: scan event log: %w", err)
	}
	return out, truncated, nil
}

// hasMoreValidJSON advances the scanner looking for one more valid-JSON line
// after the cap was hit, so truncated reflects "real events were dropped"
// rather than "the file ended exactly at the cap". Trailing blank/corrupt
// lines do not count as a dropped event. The scanner is already consumed by
// the caller's break, so advancing it here is safe.
func hasMoreValidJSON(sc *bufio.Scanner) bool {
	for sc.Scan() {
		if json.Valid(sc.Bytes()) {
			return true
		}
	}
	return false
}

// eventsDefaultMax bounds RunEvents when the caller passes a
// non-positive cap. 2000 frames covers a typical run's opening comfortably
// while keeping the response well under a megabyte for the dashboard.
const eventsDefaultMax = 2000

// EventsMaxLineSize caps a single NDJSON line on the sandbox event
// wire. It must equal agentcore.MaxEnvelopeLineBytes (the SSE decoder's
// ceiling) so the writer's accept ceiling and this reader's scanner token limit
// never drift: a writer/reader split let lines write but never read back,
// silently dropping every later event (#2083). Neither cron nor this package
// may import internal/agentcore (AWS SDK; cron's no_agentcore_import_test.go
// walks the closure, which includes this package), so both derive from
// limits.MaxStreamJSONLine + 64KiB. Reader memory is bounded
// by eventsSemCap, not by shrinking this cap.
const EventsMaxLineSize = limits.MaxStreamJSONLine + (64 << 10)

// eventsSemCap bounds concurrent RunEvents reads (mirrors the
// dashboard transcriptSem). Each read holds up to maxLines×64KB plus a scanner
// buffer; without the gate one authenticated client could exhaust memory
// (#2066). Non-blocking acquire fails fast rather than parking goroutines.
const eventsSemCap = 8

// eventsSem limits concurrent RunEvents reads process-wide. Package-level (not
// per-Store) because the bound protects the host's memory, of which there is one
// regardless of how many schedulers, and so Stores, exist in a process.
var eventsSem = make(chan struct{}, eventsSemCap)

// DeleteJobEvents removes a deleted job's sandboxevents subtree.
// Best-effort: a missing tree is fine. A 60-minute run can emit several MB, so
// leaving it orphaned would be an observable disk leak.
func (st Store) DeleteJobEvents(jobID string) {
	if st.Root == "" || !validID(jobID) {
		return
	}
	dir := st.Subtree("sandboxevents", jobID)
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("cron sandbox: event subtree delete failed", "job_id", jobID, "err", err)
	}
}

// ErrEventsBusy is returned by RunEvents when the concurrency
// semaphore is saturated. The dashboard handler maps it to HTTP 503 so a
// burst fails fast instead of allocating unbounded scanner buffers.
var ErrEventsBusy = errors.New("cron sandbox: event reads busy")
