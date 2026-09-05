package shim

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// loadGolden copies a testdata fixture to a 0600 temp file (git checkouts are
// 0644 and ReadStateFile refuses group/world-readable state) and reads it
// through the production reader.
func loadGolden(t *testing.T, name string) State {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := ReadStateFile(path)
	if err != nil {
		t.Fatalf("ReadStateFile(%s): %v — an old release's state file no longer parses; "+
			"zero-downtime restart against live shims from that release is broken", name, err)
	}
	return st
}

// TestStateGoldenCompat_V0081 pins the pre-SpawnOverlay on-disk shape
// (#2543): a state file written by a v0.0.81 shim must load, every field
// must survive (a renamed json tag or a dropped omitempty field reads back
// as the zero value and fails the comparison below), and SpawnOverlay must
// come back nil — the "written by a shim predating the field" semantics the
// reconnect path branches on.
func TestStateGoldenCompat_V0081(t *testing.T) {
	t.Parallel()
	st := loadGolden(t, "state_v0.0.81.json")

	want := State{
		Version:         1,
		SchemaVersion:   0, // absent in the file; readers treat zero as v1
		ShimPID:         40001,
		CLIPID:          40002,
		Socket:          "/run/user/1000/naozhi/aabbccdd.sock",
		AuthToken:       "R29sZGVuRml4dHVyZVRva2VuLXYwLjAuODEtMzJieXRlcw==",
		Key:             "feishu:p2p:golden-81",
		SessionID:       "0e12f5a4-9d1e-4c7b-9f43-3a5a1b2c3d4e",
		Workspace:       "/home/ec2-user/workspace/naozhi",
		Backend:         "claude",
		CLIAlive:        true,
		StartedAt:       "2026-08-20T09:00:00Z",
		LastConnectedAt: "2026-08-21T10:00:00Z",
		BufferCount:     3,
	}
	if st.SpawnOverlay != nil {
		t.Errorf("SpawnOverlay = %+v, want nil (pre-#2494 file must read as 'unknown')", st.SpawnOverlay)
	}
	if len(st.CLIArgs) != 14 || st.CLIArgs[0] != "-p" || st.CLIArgs[len(st.CLIArgs)-1] != "0e12f5a4-9d1e-4c7b-9f43-3a5a1b2c3d4e" {
		t.Errorf("CLIArgs did not round-trip: %v", st.CLIArgs)
	}
	st.CLIArgs = nil
	st.SpawnOverlay = nil
	if !reflect.DeepEqual(st, want) {
		t.Errorf("v0.0.81 state fields did not survive:\n got  %+v\n want %+v", st, want)
	}
}

// TestStateGoldenCompat_V0082 pins the SpawnOverlay-era shape: all five
// overlay fields plus the top-level schema_version must survive a read.
func TestStateGoldenCompat_V0082(t *testing.T) {
	t.Parallel()
	st := loadGolden(t, "state_v0.0.82.json")

	if st.Version != 1 || st.SchemaVersion != 1 {
		t.Errorf("versions = %d/%d, want 1/1", st.Version, st.SchemaVersion)
	}
	if st.Key != "dashboard:direct:2026-09-01-120000-1:myproject" ||
		st.Backend != "kiro" ||
		st.ShimPID != 50001 || st.CLIPID != 50002 ||
		st.Socket != "/run/user/1000/naozhi/eeff0011.sock" ||
		st.AuthToken != "R29sZGVuRml4dHVyZVRva2VuLXYwLjAuODItMzJieXRlcw==" ||
		st.SessionID != "1f23a6b5-8c2f-4d8c-a054-4b6b2c3d4e5f" ||
		st.Workspace != "/home/ec2-user/workspace/myproject" ||
		!st.CLIAlive || st.BufferCount != 0 ||
		st.StartedAt != "2026-09-01T12:00:00Z" ||
		st.LastConnectedAt != "2026-09-02T08:30:00Z" {
		t.Errorf("v0.0.82 top-level fields did not survive: %+v", st)
	}
	ov := st.SpawnOverlay
	if ov == nil {
		t.Fatal("SpawnOverlay = nil, want populated")
	}
	if ov.Model != "claude-fable-5" || ov.Effort != "high" ||
		ov.AccessProfile != "team-bedrock" ||
		ov.AppendSystemPrompt != "回答保持简短。" ||
		len(ov.ExtraArgs) != 1 || ov.ExtraArgs[0] != "--debug" {
		t.Errorf("SpawnOverlay fields did not survive: %+v", ov)
	}
}

// TestStateGoldenCompat_AllFixturesLoad walks the whole matrix so a new
// release's archived golden is picked up without touching this file.
func TestStateGoldenCompat_AllFixturesLoad(t *testing.T) {
	t.Parallel()
	matches, err := filepath.Glob(filepath.Join("testdata", "state_*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) < 2 {
		t.Fatalf("golden matrix needs at least the current and previous release, found %v", matches)
	}
	for _, m := range matches {
		st := loadGolden(t, filepath.Base(m))
		if st.Key == "" || st.Socket == "" || st.AuthToken == "" {
			t.Errorf("%s: identity fields empty after read: %+v", m, st)
		}
	}
}

// schemaProtocolAllowlist pins the linkage between the state-file schema and
// the socket protocol: bumping maxSupportedSchemaVersion without adding an
// entry here (with the protocol floor that understands it) fails the test —
// the two version tracks may not drift apart silently (#2543).
var schemaProtocolAllowlist = map[int]int{
	1: 1, // schema v1 readable from protocol v1 onwards
}

func TestSchemaProtocolLinkage(t *testing.T) {
	t.Parallel()
	minProto, ok := schemaProtocolAllowlist[maxSupportedSchemaVersion]
	if !ok {
		t.Fatalf("maxSupportedSchemaVersion=%d has no schemaProtocolAllowlist entry — a schema bump must land together with its protocol pairing", maxSupportedSchemaVersion)
	}
	if ProtocolVersion < minProto {
		t.Errorf("ProtocolVersion=%d < %d, the floor allowlisted for schema %d", ProtocolVersion, minProto, maxSupportedSchemaVersion)
	}
}
