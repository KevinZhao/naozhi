package clierr

import "testing"

// Every ExitClass constant has its own non-empty wire name, and Wire falls
// back to "unknown" outside the declared range.
func TestExitClassWire(t *testing.T) {
	t.Parallel()
	// usermsg/classify.go and the dashboard match these literal strings
	// (STARTUP_FAILURE_CLASS, usermsg.Code*), so a swap between two classes
	// must fail here, not just a dropped or duplicated entry.
	want := map[ExitClass]string{
		ExitUnknown:        "unknown",
		ExitResumeNotFound: "resume_not_found",
		ExitAuth:           "auth",
		ExitMCPConfig:      "mcp_config",
		ExitMissingRuntime: "missing_runtime",
	}
	wires := AllExitClassWires()
	if len(wires) != len(want) {
		t.Fatalf("AllExitClassWires() = %q (%d), want one per ExitClass constant (%d)", wires, len(wires), len(want))
	}
	for c, w := range want {
		if got := c.Wire(); got != w {
			t.Errorf("ExitClass(%d).Wire() = %q, want %q", c, got, w)
		}
		if wires[c] != w {
			t.Errorf("AllExitClassWires()[%d] = %q, want %q", c, wires[c], w)
		}
	}
	for _, c := range []ExitClass{-1, ExitClass(len(want))} {
		if got := c.Wire(); got != ExitUnknown.Wire() {
			t.Errorf("ExitClass(%d).Wire() = %q, want %q", c, got, ExitUnknown.Wire())
		}
	}
}
