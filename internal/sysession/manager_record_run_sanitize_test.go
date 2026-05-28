package sysession

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestRecordRun_ErrorMsgSanitised pins R260528-BUG-21: recordRun must
// SanitizeForLog the error.Error() output before persisting it on
// DaemonRun.ErrorMsg. runner.go already sanitises stderr before
// fmt.Errorf wraps it, but other err sources reaching recordRun
// (timeouts, panics-as-error, validation errors carrying user-supplied
// strings) bypass that hop — a control rune or oversized payload
// otherwise lands in run-history JSONL and propagates to the dashboard.
//
// Pre-fix recordRun did dr.ErrorMsg = err.Error() unconditionally.
func TestRecordRun_ErrorMsgSanitised(t *testing.T) {
	t.Parallel()

	// Build the minimum daemonRecord shape recordRun needs. We don't
	// need a real daemon — recordRun reads only Name(), runs.Append, and
	// the consecutive-failure counters. signalDaemon is sufficient.
	d := &signalDaemon{name: "auto-titler"}
	rec := &daemonRecord{
		daemon:           d,
		tick:             time.Second,
		processStartedAt: time.Now(),
		runs:             newRunRing(),
	}

	// Manager with no callbacks — recordRun walks the path without any
	// dashboard side-effects.
	m := &Manager{}

	// Error message containing C0 control runes + an oversized tail —
	// SanitizeForLog should strip the controls and clip the tail.
	rawTail := strings.Repeat("a", 4096)
	rawErr := errors.New("upstream\x00\x07pizza\x1b[31m boom: " + rawTail)

	m.recordRun(rec, "run-1", DaemonTriggerScheduled,
		time.Now().Add(-time.Millisecond), TickReport{}, rawErr, false)

	runs := rec.runs.Snapshot()
	if len(runs) != 1 {
		t.Fatalf("expected 1 run record, got %d", len(runs))
	}
	got := runs[0].ErrorMsg
	if got == "" {
		t.Fatalf("ErrorMsg unexpectedly empty for non-nil err")
	}
	// No raw control bytes survive (NUL, BEL, ESC).
	for _, c := range []byte{0x00, 0x07, 0x1b} {
		if strings.IndexByte(got, c) >= 0 {
			t.Errorf("ErrorMsg retains control byte 0x%02x: %q", c, got)
		}
	}
	// Oversized tail truncated to the 1024-byte cap.
	if len(got) > 1024 {
		t.Errorf("ErrorMsg len=%d exceeds 1024-byte cap", len(got))
	}
	// Sanity: original prefix's printable bytes survive.
	if !strings.HasPrefix(got, "upstream") {
		t.Errorf("ErrorMsg lost the leading printable prefix: %q", got)
	}
}
