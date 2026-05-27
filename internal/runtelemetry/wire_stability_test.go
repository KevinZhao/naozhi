package runtelemetry

import "testing"

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
		ErrClassNone:             "",
		ErrClassDeadlineExceeded: "deadline_exceeded",
		ErrClassCanceled:         "canceled",
		ErrClassPanic:            "panic",

		ErrClassCronSessionError:       "session_error",
		ErrClassCronSendError:          "send_error",
		ErrClassCronWorkDirUnreachable: "workdir_unreachable",
		ErrClassCronWorkDirOutsideRoot: "workdir_outside_root",
		ErrClassCronOverlapSkipped:     "overlap_skipped",

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
