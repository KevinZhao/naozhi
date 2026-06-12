package session

import (
	"testing"
)

// TestSnapshot_LiveVersion_SurfacesAndMirrors pins R20260612-live-version:
// when the live process self-reports a CLI binary version (system/init
// claude_code_version) the mirroring Snapshot must (1) surface it in
// snap.CLIVersion and (2) mirror it back into the persisted cliVersion so the
// dashboard reflects the binary THIS process exec'd — authoritative even after
// the host claude was upgraded under a long-lived naozhi.
func TestSnapshot_LiveVersion_SurfacesAndMirrors(t *testing.T) {
	t.Parallel()

	s := &ManagedSession{key: "test:direct:alice:general"}
	s.SetCLIVersion("2.1.100") // spawn-time value, detected once at startup
	proc := NewTestProcess()
	proc.LiveVersionVal = "2.1.174" // process actually exec'd a newer binary
	s.storeProcess(proc)

	if got := s.Snapshot().CLIVersion; got != "2.1.174" {
		t.Fatalf("Snapshot.CLIVersion = %q, want live 2.1.174", got)
	}
	// The live value is mirrored into the persisted field for sessions.json.
	if got := s.CLIVersion(); got != "2.1.174" {
		t.Fatalf("persisted CLIVersion = %q, want mirrored 2.1.174", got)
	}
}

// TestSnapshot_LiveVersion_FallsBackToPersisted guards the init-not-yet-arrived
// and ACP (never self-reports) cases: an empty live version must leave the
// spawn-time persisted value intact rather than blanking the dashboard.
func TestSnapshot_LiveVersion_FallsBackToPersisted(t *testing.T) {
	t.Parallel()

	s := &ManagedSession{key: "test:direct:bob:general"}
	s.SetCLIVersion("2.1.100")
	proc := NewTestProcess()
	proc.LiveVersionVal = "" // init frame not seen / ACP backend
	s.storeProcess(proc)

	if got := s.Snapshot().CLIVersion; got != "2.1.100" {
		t.Fatalf("Snapshot.CLIVersion = %q, want persisted 2.1.100", got)
	}
	if got := s.CLIVersion(); got != "2.1.100" {
		t.Fatalf("persisted CLIVersion = %q, want untouched 2.1.100", got)
	}
}

// TestSnapshotReadOnly_LiveVersion_NoWrite locks the read-path discipline:
// snapshotReadOnly (used by VisitSessions under RLock) must surface the live
// version in the returned view but never write it back into the persisted
// field — matching the model mirror's read-only behaviour.
func TestSnapshotReadOnly_LiveVersion_NoWrite(t *testing.T) {
	t.Parallel()

	s := &ManagedSession{key: "test:direct:carol:general"}
	s.SetCLIVersion("2.1.100")
	proc := NewTestProcess()
	proc.LiveVersionVal = "2.1.174"
	s.storeProcess(proc)

	if got := s.snapshotReadOnly().CLIVersion; got != "2.1.174" {
		t.Fatalf("snapshotReadOnly().CLIVersion = %q, want live 2.1.174", got)
	}
	// Persisted value must stay at the spawn-time value: no read-path write.
	if got := s.CLIVersion(); got != "2.1.100" {
		t.Fatalf("snapshotReadOnly mutated persisted CLIVersion to %q; read path must be side-effect free", got)
	}
}
