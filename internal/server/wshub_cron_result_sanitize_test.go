package server

import (
	"strings"
	"testing"
)

// TestBuildCronResultMsg_SanitizesAllOperatorVisibleFields locks the
// R246-SEC-7 (#811) contract: every cron_result field carrying upstream
// data must flow through SanitizeForLog so a job whose stdout / errMsg
// contains bidi / C0 / C1 / U+2028 / U+2029 cannot smuggle those bytes
// into the dashboard renderer via a SetEscapeHTML(false) JSON encoder.
//
// Older code paths (and any future producer that bypasses
// recordResultP0's SanitizeForLog pipeline) drive this same broadcast,
// so the safety net must live at the broadcast site, not at the
// producer.
func TestBuildCronResultMsg_SanitizesAllOperatorVisibleFields(t *testing.T) {
	t.Parallel()

	// hostile input mixes:
	//   \n        — log-injection newline
	//        — JS line separator (parses as newline in <script>)
	//   ‮    — right-to-left override (used to spoof filenames)
	//   \x07      — bell (C0 control)
	//       — NEL (C1 control)
	hostile := "ok\nfake_log inject‮evil\x07dingnel"

	got := buildCronResultMsg(hostile, hostile, hostile)

	if got.Type != "cron_result" {
		t.Errorf("Type = %q; want cron_result", got.Type)
	}
	for name, v := range map[string]string{
		"JobID":  got.JobID,
		"Result": got.Result,
		"Error":  got.Error,
	} {
		// Every dangerous rune above must be scrubbed. The exact replacement
		// rune is policy of osutil.SanitizeForLog ("_") — we only assert the
		// dangerous classes are gone so a future SanitizeForLog tweak (e.g.
		// switch to U+FFFD) does not break this test.
		for _, bad := range []string{"\n", " ", "‮", "\x07", ""} {
			if strings.Contains(v, bad) {
				t.Errorf("%s field still contains %q after sanitise: %q",
					name, bad, v)
			}
		}
	}
}

// TestBuildCronResultMsg_CapsRespected pins that the per-field byte caps
// (cronResultBroadcastResultMax / cronResultBroadcastErrorMax / 64) are
// honoured. Without these caps a hostile producer could wedge the
// broadcast hot path on a pathologically long sanitise walk and bloat
// the WS frame past every recipient's read buffer. The 64-byte JobID
// cap matches sanitizeHexIDForBroadcast / cron_run_started parity.
func TestBuildCronResultMsg_CapsRespected(t *testing.T) {
	t.Parallel()

	// 8 KiB result → must be clipped to 4 KiB.
	bigResult := strings.Repeat("A", 8*1024)
	// 4 KiB errMsg → must be clipped to 1 KiB.
	bigErr := strings.Repeat("B", 4*1024)
	// 256-char jobID → must be clipped to 64.
	bigID := strings.Repeat("c", 256)

	got := buildCronResultMsg(bigID, bigResult, bigErr)

	if len(got.JobID) > 64 {
		t.Errorf("JobID len = %d; want <= 64", len(got.JobID))
	}
	if len(got.Result) > cronResultBroadcastResultMax {
		t.Errorf("Result len = %d; want <= %d", len(got.Result), cronResultBroadcastResultMax)
	}
	if len(got.Error) > cronResultBroadcastErrorMax {
		t.Errorf("Error len = %d; want <= %d", len(got.Error), cronResultBroadcastErrorMax)
	}
}

// TestBuildCronResultMsg_PreservesCleanInput is the negative side of the
// sanitise contract: clean lowercase-hex jobIDs and ASCII result/errMsg
// pass through unchanged. SanitizeForLog has a fast path that returns the
// original string when no scrub is needed; this test guards a regression
// where a future tightening (e.g. dropping characters from the safe set)
// would garble a perfectly-valid cron_result and break the dashboard.
func TestBuildCronResultMsg_PreservesCleanInput(t *testing.T) {
	t.Parallel()

	jobID := "deadbeefcafef00d"
	result := "build succeeded; 12 tests passed"
	errMsg := ""

	got := buildCronResultMsg(jobID, result, errMsg)

	if got.JobID != jobID {
		t.Errorf("JobID = %q; want %q (clean hex must pass through)", got.JobID, jobID)
	}
	if got.Result != result {
		t.Errorf("Result = %q; want %q (clean ASCII must pass through)", got.Result, result)
	}
	if got.Error != errMsg {
		t.Errorf("Error = %q; want %q", got.Error, errMsg)
	}
}
