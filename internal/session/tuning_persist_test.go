package session

// tuning_persist_test.go — sessions.json round-trip + load-time validation
// for the per-session TuningModel/TuningEffort overrides.
// docs/rfc/dashboard-model-effort-control.md §4.3 / §4.6 / §5 持久化 row.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTuningPersist_RoundTrip pins the save→load cycle: overrides written
// via the accessors must survive a router restart intact.
func TestTuningPersist_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "sessions.json")

	key := "dash:direct:tuner:general"
	src := newSessionWithID(key, "sess-tune-1")
	src.SetTuningModel("claude-haiku-4.5")
	src.SetTuningEffort("low")

	if err := saveStore(storePath, map[string]*ManagedSession{key: src}); err != nil {
		t.Fatalf("saveStore: %v", err)
	}

	// Wire shape: the keys must be present so an operator can audit the
	// override offline (RFC §7), and use the documented names.
	raw, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"tuning_model":"claude-haiku-4.5"`) ||
		!strings.Contains(string(raw), `"tuning_effort":"low"`) {
		t.Fatalf("persisted JSON missing tuning keys:\n%s", raw)
	}

	r := NewRouter(RouterConfig{MaxProcs: 3, StorePath: storePath})
	t.Cleanup(func() { r.Shutdown() })

	r.mu.RLock()
	got := r.ss.sessions[key]
	r.mu.RUnlock()
	if got == nil {
		t.Fatal("session not restored")
	}
	if got.TuningModel() != "claude-haiku-4.5" {
		t.Errorf("TuningModel = %q, want claude-haiku-4.5", got.TuningModel())
	}
	if got.TuningEffort() != "low" {
		t.Errorf("TuningEffort = %q, want low", got.TuningEffort())
	}
}

// TestTuningPersist_LoadRejectsInjectedValues pins §4.6: a hand-edited
// sessions.json carrying argv-shaped ("-flag") or out-of-set values must be
// dropped at load — these strings feed --model/--effort argv on the next
// spawn, and the store file is the only trust boundary between "operator can
// edit a JSON file" and "operator can inject CLI flags".
func TestTuningPersist_LoadRejectsInjectedValues(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "sessions.json")

	key := "dash:direct:mallory:general"
	entries := []map[string]any{{
		"key":           key,
		"session_id":    "sess-evil-1",
		"tuning_model":  "--dangerously-skip-permissions",
		"tuning_effort": "ultra", // not in the closed tier set
	}}
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(storePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	r := NewRouter(RouterConfig{MaxProcs: 3, StorePath: storePath})
	t.Cleanup(func() { r.Shutdown() })

	r.mu.RLock()
	got := r.ss.sessions[key]
	r.mu.RUnlock()
	if got == nil {
		t.Fatal("session not restored (the entry itself must survive — only the tuning fields drop)")
	}
	if got.TuningModel() != "" {
		t.Errorf("injected tuning_model survived load: %q", got.TuningModel())
	}
	if got.TuningEffort() != "" {
		t.Errorf("out-of-set tuning_effort survived load: %q", got.TuningEffort())
	}
	// The legitimate fields of the same entry must be unaffected.
	if got.getSessionID() != "sess-evil-1" {
		t.Errorf("sessionID = %q, want sess-evil-1", got.getSessionID())
	}
}
