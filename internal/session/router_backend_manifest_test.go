package session

import (
	"encoding/json"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestBackendsManifest_ShapeAndDefault pins the manifest assembly the
// dashboard handler and the reverse-RPC branch both render from: backends
// ordered default-first, the default field set, and detected coerced to a
// non-nil array so the frontend's Array.isArray guard holds.
func TestBackendsManifest_ShapeAndDefault(t *testing.T) {
	claudeW := &cli.Wrapper{BackendID: "claude", CLIName: "claude-code", CLIVersion: "2.1.100"}
	kiroW := &cli.Wrapper{BackendID: "kiro", CLIName: "kiro", CLIVersion: "2.12.0"}
	r := NewRouter(RouterConfig{
		Wrappers:       map[string]*cli.Wrapper{"claude": claudeW, "kiro": kiroW},
		DefaultBackend: "kiro",
	})

	m := r.BackendsManifest(nil)

	if m.Default != "kiro" {
		t.Errorf("Default = %q, want kiro", m.Default)
	}
	if len(m.Backends) != 2 || m.Backends[0].ID != "kiro" {
		t.Fatalf("Backends = %+v, want default-first [kiro, claude]", m.Backends)
	}
	// nil detected must serialise to [] (not null) so the frontend guard holds.
	if m.Detected == nil {
		t.Error("Detected = nil, want non-nil empty slice")
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	var probe struct {
		Detected json.RawMessage `json:"detected"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("unmarshal probe: %v", err)
	}
	if string(probe.Detected) != "[]" {
		t.Errorf("detected JSON = %s, want []", probe.Detected)
	}
}

// TestBackendsManifest_PassesDetectedThrough confirms a caller-supplied
// detected list is relayed verbatim (the handler pre-probes it once at
// startup and threads it in).
func TestBackendsManifest_PassesDetectedThrough(t *testing.T) {
	w := &cli.Wrapper{BackendID: "claude", CLIName: "claude-code", CLIVersion: "2.1.100"}
	r := NewRouter(RouterConfig{Wrapper: w})

	detected := []cli.BackendInfo{{ID: "codex", Available: true}}
	m := r.BackendsManifest(detected)

	if len(m.Detected) != 1 || m.Detected[0].ID != "codex" {
		t.Errorf("Detected = %+v, want passed-through [codex]", m.Detected)
	}
}

// TestBackendsList_UnavailableWhenNoVersion guards the Available flag: a
// wrapper whose EffectiveVersion is empty (binary present but --version parse
// failed) must report Available=false so the dashboard greys it out.
func TestBackendsList_UnavailableWhenNoVersion(t *testing.T) {
	w := &cli.Wrapper{BackendID: "claude", CLIName: "claude-code"} // no version
	r := NewRouter(RouterConfig{Wrapper: w})

	list := r.BackendsList()
	if len(list) != 1 {
		t.Fatalf("BackendsList len = %d, want 1", len(list))
	}
	if list[0].Available {
		t.Error("Available = true for a wrapper with empty effective version, want false")
	}
}
