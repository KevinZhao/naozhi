package runtelemetry

import (
	"encoding/json"
	"testing"
	"time"
)

// TestRunState_WireStable freezes the wire string for every RunState.
// Changing a value here without coordinating with dashboard.js + cron
// runs/<id>.json on disk + sysession runRing JSON breaks the contract.
//
// Adding a new RunState: extend this map; the duplicate-detector at the
// bottom of the test guards against accidental wire collisions.
func TestRunState_WireStable(t *testing.T) {
	t.Parallel()
	want := map[RunState]string{
		RunStateSucceeded: "succeeded",
		RunStateFailed:    "failed",
		RunStateSkipped:   "skipped",
		RunStateTimedOut:  "timed_out",
		RunStateCanceled:  "canceled",
	}
	for c, w := range want {
		if string(c) != w {
			t.Errorf("RunState %q wire = %q, want %q", c, string(c), w)
		}
	}
	assertNoDuplicateWireValues(t, "RunState", stringValuesRunState(want))
}

// TestErrorClass_WireStable freezes wire strings for every ErrorClass.
// Cross-subsystem (canceled / deadline_exceeded / panic / "") and
// subsystem-specific values are both pinned. The duplicate-detector
// catches a future "ErrClassCronUpstream = upstream" addition that
// would silently collide with ErrClassSysessionUpstream.
func TestErrorClass_WireStable(t *testing.T) {
	t.Parallel()
	want := map[ErrorClass]string{
		ErrClassCronInterrupted:  "interrupted",
		ErrClassNone:             "",
		ErrClassDeadlineExceeded: "deadline_exceeded",
		ErrClassCanceled:         "canceled",
		ErrClassPanic:            "panic",

		ErrClassCronSessionError:       "session_error",
		ErrClassCronSendError:          "send_error",
		ErrClassCronWorkDirUnreachable: "workdir_unreachable",
		ErrClassCronWorkDirOutsideRoot: "workdir_outside_root",
		ErrClassCronOverlapSkipped:     "overlap_skipped",
		ErrClassCronSandboxFailed:      "sandbox_failed",
		ErrClassCronSandboxTransport:   "sandbox_transport",
		ErrClassCronSandboxUnavailable: "sandbox_unavailable",

		ErrClassSysessionUpstream:   "upstream",
		ErrClassSysessionValidation: "validation",
	}
	for c, w := range want {
		if string(c) != w {
			t.Errorf("ErrorClass %q wire = %q, want %q", c, string(c), w)
		}
	}
	assertNoDuplicateWireValues(t, "ErrorClass", stringValuesErrorClass(want))
}

// TestTriggerKind_WireStable freezes wire strings for every TriggerKind.
func TestTriggerKind_WireStable(t *testing.T) {
	t.Parallel()
	want := map[TriggerKind]string{
		TriggerScheduled: "scheduled",
		TriggerManual:    "manual",
		TriggerCatchup:   "catchup",
	}
	for c, w := range want {
		if string(c) != w {
			t.Errorf("TriggerKind %q wire = %q, want %q", c, string(c), w)
		}
	}
	assertNoDuplicateWireValues(t, "TriggerKind", stringValuesTriggerKind(want))
}

// TestSubsystem_WireStable freezes wire strings for every Subsystem.
func TestSubsystem_WireStable(t *testing.T) {
	t.Parallel()
	want := map[Subsystem]string{
		SubsystemCron:      "cron",
		SubsystemSysession: "sysession",
		SubsystemSession:   "session",
	}
	for c, w := range want {
		if string(c) != w {
			t.Errorf("Subsystem %q wire = %q, want %q", c, string(c), w)
		}
	}
	assertNoDuplicateWireValues(t, "Subsystem", stringValuesSubsystem(want))
}

// assertNoDuplicateWireValues guards against two named constants of the
// same type accidentally sharing a wire string. ErrClassNone is empty
// by design and excluded — every other duplicate is a bug.
func assertNoDuplicateWireValues(t *testing.T, typeName string, values []string) {
	t.Helper()
	seen := make(map[string]int, len(values))
	for _, v := range values {
		if v == "" {
			// "" is reserved for "no error class" and not a wire collision.
			continue
		}
		seen[v]++
	}
	for v, n := range seen {
		if n > 1 {
			t.Errorf("%s: wire string %q appears %d times — wire collision",
				typeName, v, n)
		}
	}
}

func stringValuesRunState(m map[RunState]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func stringValuesErrorClass(m map[ErrorClass]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func stringValuesTriggerKind(m map[TriggerKind]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func stringValuesSubsystem(m map[Subsystem]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// TestRunRecord_WireStable pins RunRecord's JSON byte-for-byte (#2540). The
// record is the meeting point of every run surface — the WS frames mirror its
// fields, the DTO merge will serve it — so a silently renamed json tag here
// desynchronises consumers that no compiler connects to this struct.
func TestRunRecord_WireStable(t *testing.T) {
	rec := RunRecord{
		Subsystem:  SubsystemCron,
		OwnerID:    "0123456789abcdef",
		RunID:      "fedcba9876543210",
		State:      RunStateSucceeded,
		Trigger:    TriggerManual,
		StartedAt:  time.UnixMilli(1700000000000).UTC(),
		EndedAt:    time.UnixMilli(1700000001200).UTC(),
		DurationMS: 1200,
		SessionID:  "11111111-2222-3333-4444-555555555555",
		ErrorClass: ErrClassNone,
		ErrorMsg:   "",
		Fresh:      true,
		CostUSD:    0.25,
	}
	got, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"subsystem":"cron","owner_id":"0123456789abcdef","run_id":"fedcba9876543210","state":"succeeded","trigger":"manual","started_at":"2023-11-14T22:13:20Z","ended_at":"2023-11-14T22:13:21.2Z","duration_ms":1200,"session_id":"11111111-2222-3333-4444-555555555555","fresh":true,"cost_usd":0.25}`
	if string(got) != want {
		t.Errorf("RunRecord wire shape drifted:\n got  %s\n want %s", got, want)
	}

	// The zero-value ended_at must be absent, not "0001-01-01T00:00:00Z": a
	// started-but-not-ended record is the WS run_started projection, and a
	// bogus timestamp there reads as "ended before the epoch".
	openRec, err := json.Marshal(RunRecord{Subsystem: SubsystemSysession, OwnerID: "d", RunID: "r", StartedAt: time.UnixMilli(1700000000000).UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if string(openRec) != `{"subsystem":"sysession","owner_id":"d","run_id":"r","started_at":"2023-11-14T22:13:20Z"}` {
		t.Errorf("open RunRecord wire shape drifted: %s", openRec)
	}
}

// TestEventRecordProjections pins that both events project every one of their
// fields into the record — a field added to an event but not to Record()
// reaches the producer's internal path and silently misses every common
// surface.
func TestEventRecordProjections(t *testing.T) {
	started := RunStartedEvent{
		Subsystem: SubsystemCron, OwnerID: "o", RunID: "r",
		Trigger: TriggerScheduled, StartedAt: time.UnixMilli(1), SessionID: "s", Fresh: true,
	}
	sr := started.Record()
	if sr.Subsystem != started.Subsystem || sr.OwnerID != started.OwnerID || sr.RunID != started.RunID ||
		sr.Trigger != started.Trigger || !sr.StartedAt.Equal(started.StartedAt) ||
		sr.SessionID != started.SessionID || sr.Fresh != started.Fresh {
		t.Errorf("RunStartedEvent.Record dropped a field: %+v from %+v", sr, started)
	}

	ended := RunEndedEvent{
		Subsystem: SubsystemSysession, OwnerID: "o", RunID: "r", State: RunStateFailed,
		StartedAt: time.UnixMilli(1), EndedAt: time.UnixMilli(2), DurationMS: 1,
		Trigger: TriggerManual, SessionID: "s", ErrorClass: ErrClassPanic, ErrorMsg: "m",
	}
	er := ended.Record()
	if er.State != ended.State || !er.EndedAt.Equal(ended.EndedAt) || er.DurationMS != ended.DurationMS ||
		er.ErrorClass != ended.ErrorClass || er.ErrorMsg != ended.ErrorMsg {
		t.Errorf("RunEndedEvent.Record dropped a field: %+v from %+v", er, ended)
	}
}
