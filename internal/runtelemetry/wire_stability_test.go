package runtelemetry

import (
	"encoding/json"
	"testing"
	"time"
)

// The frozen wire string of every enum constant, one map per type.
var (
	wireRunStates = map[RunState]string{
		RunStateSucceeded: "succeeded",
		RunStateFailed:    "failed",
		RunStateSkipped:   "skipped",
		RunStateTimedOut:  "timed_out",
		RunStateCanceled:  "canceled",
	}
	wireErrorClasses = map[ErrorClass]string{
		ErrClassNone:             "",
		ErrClassDeadlineExceeded: "deadline_exceeded",
		ErrClassCanceled:         "canceled",
		ErrClassPanic:            "panic",

		ErrClassCronSessionError:       "session_error",
		ErrClassCronSendError:          "send_error",
		ErrClassCronWorkDirUnreachable: "workdir_unreachable",
		ErrClassCronWorkDirOutsideRoot: "workdir_outside_root",
		ErrClassCronOverlapSkipped:     "overlap_skipped",
		ErrClassCronSessionCapacity:    "session_capacity",
		ErrClassCronBudgetExceeded:     "budget_exceeded",
		ErrClassCronTurnFailed:         "turn_failed",
		ErrClassCronInterrupted:        "interrupted",
		ErrClassCronConfigDrift:        "config_drift",
		ErrClassCronRouterMissing:      "router_missing",
		ErrClassCronPausedConcurrent:   "paused_concurrent",
		ErrClassCronDeletedConcurrent:  "deleted_concurrent",

		ErrClassSysessionUpstream:   "upstream",
		ErrClassSysessionValidation: "validation",
	}
	wireTriggerKinds = map[TriggerKind]string{
		TriggerScheduled: "scheduled",
		TriggerManual:    "manual",
		TriggerCatchup:   "catchup",
	}
	wireSubsystems = map[Subsystem]string{
		SubsystemCron:      "cron",
		SubsystemSysession: "sysession",
		SubsystemSession:   "session",
	}
)

// TestEnums_WireStable freezes the wire string of every enum constant:
// dashboard.js, cron runs/<id>.json on disk and the sysession run ring read
// them. Two constants sharing a value would already fail to compile here
// (duplicate map key).
func TestEnums_WireStable(t *testing.T) {
	t.Parallel()
	for c, w := range wireRunStates {
		if string(c) != w {
			t.Errorf("RunState %q, want %q", c, w)
		}
	}
	for c, w := range wireErrorClasses {
		if string(c) != w {
			t.Errorf("ErrorClass %q, want %q", c, w)
		}
	}
	for c, w := range wireTriggerKinds {
		if string(c) != w {
			t.Errorf("TriggerKind %q, want %q", c, w)
		}
	}
	for c, w := range wireSubsystems {
		if string(c) != w {
			t.Errorf("Subsystem %q, want %q", c, w)
		}
	}
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
